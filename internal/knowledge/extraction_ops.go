package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"hify/internal/db/gen"
	"hify/internal/platform"
	"hify/internal/platform/apperr"
)

// extraction_ops.go 是抽取的控制操作：暂停、续跑、追加额度、重开一次
// （010 T029/T030 的领域与数据侧）。
//
// ⭐ 这里每一个操作都必须是**幂等**的，而且幂等的粒度是"这一次用户动作"，
// 不是"这个状态"。理由很具体：追加额度是累加的。用户点了一下"追加 500 次
// 调用"，网络超时，前端重试——按状态判重（"现在是 paused，那就恢复"）会让
// 第二次请求再加 500 次，账上多出一倍额度，而两个响应都显示成功。
// 所以每一次操作都带一个键，键和请求体的摘要一起存进 budget_operations。
//
// ⚠️ 同样重要的是**不做**什么：
//   - pause/disable 不抹掉已经发生的计费尝试——那些钱真的花过;
//   - restart 不删旧 run 的账目，只把它标成 superseded;
//   - enable 不自动恢复一个 paused 的作业——用户暂停过，就必须由用户
//     显式 resume，否则"我明明停了它"和"它又开始花钱"会同时成立。

// maxBudgetOperationsPerRun 是每个 run 的控制操作上限（data-model §jobs）。
//
// ⭐ 上限存在的理由不是性能，是**这一列会被无限追加**：一个每天被追加一点
// 额度的作业，budget_operations 会一直长下去，而它每次控制操作都要整列读写。
// 超过就要求 restart——旧账目不清零，新 run 从干净的操作账开始。
const maxBudgetOperationsPerRun = 100

// maxIdempotencyKeyLen 与契约一致。
const maxIdempotencyKeyLen = 128

// autoOperationKeyPrefix 是系统自动开启抽取时用的键前缀。
// ⚠️ 用户键禁止用这个前缀：否则用户可以构造一个键，去顶掉（或重放）
// 系统在上传路径上建的那个作业。
const autoOperationKeyPrefix = "upload:"

var (
	// ErrExtractionNotStarted：这份文档还没有任何抽取作业。
	ErrExtractionNotStarted = apperr.Conflict("knowledge.extraction_not_started",
		"该文档还没有开始关系抽取")

	// ErrExtractionOperationConflict：同一个幂等键配了不同的请求体。
	// ⚠️ 这必须报错而不是"按新的来"：两个不同的请求用了同一个键，
	// 说明调用方那边的键生成有问题，而静默接受会让其中一个请求消失。
	ErrExtractionOperationConflict = apperr.Conflict("knowledge.extraction_operation_conflict",
		"同一个操作标识对应了不同的请求内容")

	// ErrTooManyExtractionOperations：控制操作次数用尽，只能 restart。
	ErrTooManyExtractionOperations = apperr.Conflict("knowledge.extraction_too_many_operations",
		"本次抽取的控制操作次数已达上限，请重新开始一次抽取")

	// ErrExtractionNotResumable：不在可续跑的状态。
	ErrExtractionNotResumable = apperr.Conflict("knowledge.extraction_not_resumable",
		"当前状态无法续跑，请检查抽取是否已结束或被关闭")

	// ErrExtractionNothingToResume：没有额度可用又没追加。
	// ⚠️ 与 ErrExtractionNotResumable 分开：前者是状态不对，后者是没钱了，
	// 用户要做的事完全不同。
	ErrExtractionNothingToResume = apperr.Conflict("knowledge.extraction_nothing_to_resume",
		"额度已用尽，续跑前请先追加额度")

	// ErrReservedOperationKey：用户键用了系统保留前缀。
	ErrReservedOperationKey = apperr.InvalidInput("knowledge.extraction_reserved_operation_key",
		"操作标识使用了系统保留的前缀")

	// ErrInvalidOperationKey：键为空或过长。
	ErrInvalidOperationKey = apperr.InvalidInput("knowledge.extraction_invalid_operation_key",
		"操作标识不能为空且不超过 128 个字符")
)

// budgetOperation 是 budget_operations 里的一条记录。
//
// ⚠️ 记的是**增量**不是结果值：记结果值的话，两条记录之间的差要靠减法
// 还原，而中间只要漏记一条（或者顺序反了），后面每一条的解读都是错的。
type budgetOperation struct {
	KeyHash       string    `json:"key_hash"`
	RequestHash   string    `json:"request_hash"`
	Op            string    `json:"op"`
	AddedCalls    int       `json:"added_calls"`
	AddedActiveMs int64     `json:"added_active_ms"`
	AddedItems    int       `json:"added_items"`
	AddedRounds   int       `json:"added_rounds"`
	At            time.Time `json:"at"`
}

// extractionOperation 是一次控制操作的输入。
type extractionOperation struct {
	Op          string // pause / resume / disable / enable
	Key         string
	RequestBody []byte // 参与幂等判重的请求体摘要来源

	AddCalls    int
	AddActiveMs int64
	AddItems    int
	AddRounds   int
}

// operationOutcome 说明这次操作实际做了什么。
type operationOutcome struct {
	// Replayed 为 true 表示这是同一个键的重放，什么都没改。
	// ⚠️ 调用方据此**不要**再入队一次 worker：重放一个 resume 却又入队，
	// 等于用户点两下就跑两个 worker。
	Replayed bool
	State    string
	// ShouldRun 为 true 表示这次操作之后作业应当被叫醒。
	ShouldRun bool
}

// applyExtractionOperation 在一个事务里做完"判重 → 改状态 → 追加额度 → 记账"。
func (r *Repository) applyExtractionOperation(ctx context.Context, jobID string, op extractionOperation) (operationOutcome, error) {
	var out operationOutcome
	keyHash := hashOperationKey(op.Key)
	requestHash := hashOperationKey(string(op.RequestBody))

	err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		row, err := q.LockRelationExtractionJob(ctx, jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrExtractionNotStarted
			}
			return fmt.Errorf("knowledge: lock extraction job: %w", err)
		}
		ops, err := decodeBudgetOperations(row.BudgetOperations)
		if err != nil {
			return err
		}
		for _, prev := range ops {
			if prev.KeyHash != keyHash {
				continue
			}
			if prev.RequestHash != requestHash {
				return ErrExtractionOperationConflict
			}
			// 同键同体：原样返回上一次的结果，什么都不改。
			out = operationOutcome{Replayed: true, State: row.State}
			return nil
		}
		if len(ops) >= maxBudgetOperationsPerRun {
			return ErrTooManyExtractionOperations
		}

		state, stopReason := row.State, row.StopReason
		switch op.Op {
		case opPause:
			if row.State != jobStateRunning && row.State != jobStateInitializing {
				// 已经暂停/已经结束：幂等返回当前状态，不报错。
				// ⚠️ 但这条**仍然要记进操作账**（走到下面的 append），
				// 否则同一个键第二次进来会被当成新操作。
				break
			}
			state, stopReason = jobStatePaused, nullString(stopReasonUserPaused)
		case opResume:
			if row.State != jobStatePaused {
				return ErrExtractionNotResumable
			}
			remainingCalls := int(row.CallLimit) + op.AddCalls - int(row.ReservedCalls)
			remainingMs := row.ActiveMsLimit + op.AddActiveMs - row.ActiveMsUsed
			if remainingCalls <= 0 || remainingMs <= 0 {
				return ErrExtractionNothingToResume
			}
			state, stopReason = jobStateRunning, sql.NullString{}
			out.ShouldRun = true
		case opDisable:
			if row.State == jobStateRunning || row.State == jobStateInitializing {
				state, stopReason = jobStatePaused, nullString(stopReasonUserDisabled)
			}
		case opEnable:
			// ⭐ 明确**不**自动恢复一个 paused 的作业（契约 §2）：
			// 用户暂停过就必须由用户显式 resume，否则"我明明停了它"和
			// "它又开始花钱"会同时成立。响应里告诉用户还需要 resume。
		default:
			return fmt.Errorf("knowledge: unknown extraction operation %q", op.Op)
		}

		ops = append(ops, budgetOperation{
			KeyHash: keyHash, RequestHash: requestHash, Op: op.Op,
			AddedCalls: op.AddCalls, AddedActiveMs: op.AddActiveMs,
			AddedItems: op.AddItems, AddedRounds: op.AddRounds,
			At: time.Now().UTC(),
		})
		encoded, err := json.Marshal(ops)
		if err != nil {
			return fmt.Errorf("knowledge: marshal budget operations: %w", err)
		}
		if _, err := q.ApplyExtractionOperation(ctx, gen.ApplyExtractionOperationParams{
			State: state, StopReason: stopReason,
			CallLimit:         row.CallLimit + int32(op.AddCalls),
			ActiveMsLimit:     row.ActiveMsLimit + op.AddActiveMs,
			ApprovedItemLimit: row.ApprovedItemLimit + int32(op.AddItems),
			RetryRounds:       row.RetryRounds + int32(op.AddRounds),
			BudgetOperations:  encoded, ID: jobID,
		}); err != nil {
			return fmt.Errorf("knowledge: apply extraction operation: %w", err)
		}
		out.State = state
		return nil
	})
	return out, err
}

const (
	opPause   = "pause"
	opResume  = "resume"
	opDisable = "disable"
	opEnable  = "enable"

	stopReasonUserPaused   = "user_paused"
	stopReasonUserDisabled = "user_disabled"
	stopReasonRestarted    = "restarted"
)

func decodeBudgetOperations(raw any) ([]budgetOperation, error) {
	// CAST(... AS CHAR) 在 sqlc 里是 interface{}：驱动给回 []byte 或 nil。
	b, ok := raw.([]byte)
	if !ok || len(b) == 0 {
		return nil, nil
	}
	var ops []budgetOperation
	if err := json.Unmarshal(b, &ops); err != nil {
		// ⚠️ 解析不出来时**报错**，不当成空数组。当成空的话，历史操作键
		// 全部失效，用户的重试会被当成新操作，额度被重复追加。
		return nil, fmt.Errorf("knowledge: decode budget operations: %w", err)
	}
	return ops, nil
}

// operationKeyBytes 是幂等键的原始摘要（写进 BINARY(32) 列）。
func operationKeyBytes(key string) []byte {
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}

func hashOperationKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// validateOperationKey 守幂等键本身。
func validateOperationKey(key string) error {
	if key == "" || len([]rune(key)) > maxIdempotencyKeyLen {
		return ErrInvalidOperationKey
	}
	if len(key) >= len(autoOperationKeyPrefix) && key[:len(autoOperationKeyPrefix)] == autoOperationKeyPrefix {
		return ErrReservedOperationKey
	}
	return nil
}

// ExtractionStatus 是抽取状态的对外视图（契约 §2）。
//
// ⚠️ 指针字段是"未知"，不是零。总数未知时报 null 而不是 0——
// 一个显示 0/0 的进度条看起来像"跑完了，没有内容"，而事实是"还不知道有多少"。
type ExtractionStatus struct {
	Enabled         bool
	JobID           string
	DocumentVersion int64
	State           string
	StopReason      string
	RunNumber       int
	ModelID         string

	TotalItems     *int
	SucceededItems int
	FailedItems    int
	// HasPartialEvidence：有成功也有失败——此时关系是**不完整**的，
	// 而不完整和空的区别必须让用户看见。
	HasPartialEvidence bool

	ConfirmedCalls int
	// PossibleCalls = 确定发生的 + 结局未知的。
	// ⚠️ 与 ConfirmedCalls 分开报，合并会让一个不确定的数字看起来像确定的。
	PossibleCalls        int
	UnknownUsageAttempts int
	ActiveMs             int64
	WallMs               *int64
	CostKind             string
	CostAmount           *float64

	RemainingCalls    int
	RemainingItems    *int
	RemainingActiveMs int64
	RetryRounds       int
}

// extractionStatus 读一份文档的抽取状态。
func (r *Repository) extractionStatus(ctx context.Context, doc Document) (ExtractionStatus, error) {
	st := ExtractionStatus{Enabled: doc.IsRelationExtractionEnabled}
	if doc.ActiveRelationJobID == "" {
		// 开关可能已经打开，但事实还没 ready，作业还不存在。
		// ⚠️ 这不是错误：状态就是"还没开始"，而 total_items 保持 null。
		return st, nil
	}
	job, err := r.queries.GetRelationExtractionJob(ctx, doc.ActiveRelationJobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("knowledge: load extraction job: %w", err)
	}
	st.JobID = job.ID
	st.DocumentVersion = int64(job.DocumentVersion)
	st.State = job.State
	st.StopReason = job.StopReason.String
	st.RunNumber = int(job.RunNumber)
	st.ModelID = job.ModelID
	st.SucceededItems = int(job.SucceededItems)
	st.FailedItems = int(job.FailedItems)
	st.HasPartialEvidence = job.FailedItems > 0 && job.SucceededItems > 0
	st.ConfirmedCalls = int(job.ConfirmedDispatches)
	st.PossibleCalls = int(job.ConfirmedDispatches) + int(job.UnknownAttempts)
	st.ActiveMs = job.ActiveMsUsed
	st.RetryRounds = int(job.RetryRounds)
	// 本地 Ollama 没有金钱计费：cost_kind 说明这个口径不适用，
	// ⚠️ cost_amount 保持 null 而不是 0——0 的意思是"花了零元"。
	st.CostKind = costKindNotApplicable
	st.RemainingCalls = int(job.CallLimit - job.ReservedCalls)
	st.RemainingActiveMs = job.ActiveMsLimit - job.ActiveMsUsed

	if job.InitializationComplete {
		total := int(job.TotalItems)
		st.TotalItems = &total
		remaining := total - int(job.SucceededItems) - int(job.FailedItems)
		st.RemainingItems = &remaining
	}
	if job.StartedAt.Valid {
		end := time.Now().UTC()
		if job.FinishedAt.Valid {
			end = job.FinishedAt.Time
		}
		ms := end.Sub(job.StartedAt.Time).Milliseconds()
		st.WallMs = &ms
	}
	unknownUsage, err := r.queries.CountJobUsageUnknownAttempts(ctx, job.ID)
	if err != nil {
		return st, fmt.Errorf("knowledge: count usage-unknown attempts: %w", err)
	}
	archivedUnknown, err := r.queries.GetArchivedUnknownUsageAttempts(ctx, job.ID)
	if err != nil {
		return st, fmt.Errorf("knowledge: load archived usage-unknown attempts: %w", err)
	}
	archivedCount, err := archivedUnknownUsageCount(archivedUnknown)
	if err != nil {
		return st, err
	}
	st.UnknownUsageAttempts = int(unknownUsage) + archivedCount
	return st, nil
}

func archivedUnknownUsageCount(value any) (int, error) {
	text := fmt.Sprint(value)
	if raw, ok := value.([]byte); ok {
		text = string(raw)
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("knowledge: invalid archived unknown usage count")
	}
	return n, nil
}

// supersedeExtractionJob 把旧 run 标成被取代（restart 用）。
func (r *Repository) supersedeExtractionJob(ctx context.Context, jobID string) error {
	if _, err := r.queries.SupersedeRelationExtractionJob(ctx, gen.SupersedeRelationExtractionJobParams{
		StopReason: nullString(stopReasonRestarted),
		FinishedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
		ID:         jobID,
	}); err != nil {
		return fmt.Errorf("knowledge: supersede extraction job: %w", err)
	}
	return nil
}

func (r *Repository) setRelationExtractionEnabled(ctx context.Context, documentID string, enabled bool) error {
	if _, err := r.queries.SetDocumentRelationExtractionEnabled(ctx,
		gen.SetDocumentRelationExtractionEnabledParams{
			IsRelationExtractionEnabled: enabled, ID: documentID,
		}); err != nil {
		return fmt.Errorf("knowledge: set relation extraction flag: %w", err)
	}
	return nil
}

// findJobByOperationKey 是 restart 的幂等重放：同一个键第二次进来时，
// 返回上一次那个 run，而不是再开一个。
func (r *Repository) findJobByOperationKey(ctx context.Context, documentID, key string) (string, bool, error) {
	row, err := r.queries.GetRelationExtractionJobByOperationKey(ctx,
		gen.GetRelationExtractionJobByOperationKeyParams{
			// ⚠️ 与 initializeExtractionJob 写进去的必须是**同一种表示**：
			// operation_key_hash 是 BINARY(32)，存的是 sha256 的原始字节，
			// 不是十六进制串。两边不一致的表现是幂等重放永远命不中——
			// 同一个键每次都开一个新 run。
			DocumentID: documentID, OperationKeyHash: nullBytes(operationKeyBytes(key)),
		})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("knowledge: find job by operation key: %w", err)
	}
	return row.ID, true, nil
}

// --- Service 侧的输入类型 ---
//
// ⚠️ 四个输入都带 KnowledgeBaseID：handler 必须校验**路径上的 kb 与文档
// 实际归属一致**，不能拿传进来的 kbID 直接当授权依据（契约 §0）。
// 拿路径 kb 当授权的表现是：知道文档 ID 的人换一个自己有权限的 kb 放进
// 路径，就能操作别人的文档。

type ExtractionControlInput struct {
	KnowledgeBaseID string
	DocumentID      string
	UserID          string
	Role            string
	// IdempotencyKey 是这一次用户动作的标识，不是这份文档的标识。
	IdempotencyKey string
}

type ExtractionEnableInput struct {
	ExtractionControlInput
	Enabled bool
	// ModelID 只在 enable 时可给；为空则用服务端配置的默认模型。
	ModelID string
}

type ExtractionResumeInput struct {
	ExtractionControlInput
	AdditionalCalls     int
	AdditionalChunks    int
	AdditionalActiveSec int
	AdditionalRounds    int
}

type ExtractionRestartInput struct {
	ExtractionControlInput
	ModelID string
}
