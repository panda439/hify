package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"hify/internal/db/gen"
	"hify/internal/platform"
	"hify/internal/platform/apperr"
	"hify/internal/user"
)

// extraction_ops.go 是抽取的操作接口（010 T029）。
//
// ⭐ 两件最容易被忽略的事：
//   - **路径归属**：必须核对 :docId 真的属于 :id 那个知识库，不能拿请求里的
//     kbID 直接当授权依据。否则知道文档 ID 的人可以借一个自己有权限的知识库
//     去操作别人的文档，而每一步鉴权看起来都做了。
//   - **幂等键**：同键重放返回原结果、同键异 body 返回 409。不管的话，
//     用户手抖点两次「重新开始」就会开出两个 run 同时花钱。

const (
	maxIdempotencyKeyRunes = 128

	// uploadKeyPrefix 是系统在"上传时就开启抽取"这条路径上保留的幂等键前缀。
	// ⚠️ 用户不得使用它：占用之后，系统那次自动创建会被当成重放而静默跳过，
	// 用户勾了开关却什么都没发生。
	uploadKeyPrefix = "upload:"
)

var (
	ErrExtractionNotEnabled = apperr.Conflict(
		"knowledge.extraction_not_enabled", "该文档尚未开启关系抽取，请先开启")

	ErrIdempotencyKeyConflict = apperr.Conflict(
		"knowledge.idempotency_key_conflict",
		"相同的幂等键已经用于另一个请求，请换一个键或使用完全相同的参数重试")

	ErrExtractionAlreadyRunning = apperr.Conflict(
		"knowledge.extraction_already_running", "该文档已有正在进行的抽取")
)

// ExtractionStatus 是对外的抽取状态。
//
// ⚠️ 未知值用指针表示，序列化成 null。填 0 的后果很具体：初始化还没完成时
// total_items = 0，前端显示"0/0 已完成"——一个看起来已经跑完的进度条，
// 而实际上一条都还没开始。
type ExtractionStatus struct {
	Enabled         bool
	JobID           *string
	State           *string
	StopReason      *string
	DocumentVersion *int64
	ModelID         *string

	TotalItems     *int
	SucceededItems *int
	FailedItems    *int

	ConfirmedCalls      *int
	PossibleCalls       *int
	UnknownUsageAttempt *int
	ActiveMs            *int64

	RemainingCalls    *int
	RemainingItems    *int
	RemainingActiveMs *int64

	// CostKind 恒为 not_applicable（本地模型无金钱计费），CostAmount 恒为 nil。
	// ⚠️ 不写 0——0 的意思是"花了零元"，而真实情况是这个口径不适用。
	CostKind   string
	CostAmount *string
}

// ExtractionOperation 是所有写操作的公共入参。
type ExtractionOperation struct {
	IdempotencyKey string
	ModelID        string

	AdditionalItems     int
	AdditionalCalls     int
	AdditionalActiveSec int
	AdditionalRetries   int
}

// documentScope 是一次操作的授权与归属结果。
type documentScope struct {
	DocumentID      string
	KnowledgeBaseID string
	Status          string
	Version         int64
	IsNarrative     bool
	Enabled         bool
	ActiveJobID     string
}

// resolveDocumentScope 核对归属与权限，一次做完。
//
// ⭐ 归属检查（文档属于路径里那个知识库）必须在权限检查**之前**，
// 并且失败时返回 404 而不是 403：告诉调用方"你没权限操作这份文档"等于
// 确认了这份文档存在，而他本来就不该知道。
func (r *Repository) resolveDocumentScope(ctx context.Context, kbID, docID, userID, role string) (documentScope, error) {
	row, err := r.queries.GetDocumentWithKnowledgeBase(ctx, docID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return documentScope{}, ErrDocumentNotFound
		}
		return documentScope{}, fmt.Errorf("knowledge: load document scope: %w", err)
	}
	if row.KnowledgeBaseID != kbID {
		return documentScope{}, ErrDocumentNotFound
	}
	if row.CreatedBy != userID && role != user.RoleAdmin {
		return documentScope{}, ErrForbidden
	}
	return documentScope{
		DocumentID: row.ID, KnowledgeBaseID: row.KnowledgeBaseID,
		Status: row.Status, Version: row.Version,
		IsNarrative: row.IsNarrative, Enabled: row.IsRelationExtractionEnabled,
		ActiveJobID: row.ActiveRelationJobID.String,
	}, nil
}

// validateOperation 校验幂等键与必填项。
func validateOperation(op ExtractionOperation, needModel bool) error {
	key := strings.TrimSpace(op.IdempotencyKey)
	if key == "" {
		return apperr.InvalidInput("knowledge.idempotency_key_required", "缺少幂等键")
	}
	if len([]rune(key)) > maxIdempotencyKeyRunes {
		return apperr.InvalidInput("knowledge.idempotency_key_too_long",
			"幂等键过长（超过 128 个字符）")
	}
	if strings.HasPrefix(key, uploadKeyPrefix) {
		// ⚠️ 见 uploadKeyPrefix 的注释：占用它会让系统那次自动创建被当成
		// 重放而静默跳过，用户勾了开关却什么都没发生。
		return apperr.InvalidInput("knowledge.idempotency_key_reserved",
			"幂等键不能以 upload: 开头，那是系统保留的前缀")
	}
	if needModel && strings.TrimSpace(op.ModelID) == "" {
		return apperr.InvalidInput("knowledge.relation_model_required",
			"开启关系抽取必须指定一个可用的对话模型")
	}
	return nil
}

func operationHashes(op ExtractionOperation, action string) (keyHash, requestHash []byte) {
	k := sha256.Sum256([]byte(action + "\x00" + op.IdempotencyKey))
	// ⚠️ request_hash 覆盖**全部会影响结果的字段**。漏掉一个的表现是：
	// 用户改了那个字段、用同一个键重发，系统当成重放静默忽略——
	// 用户以为改生效了，实际还在用第一次那份。
	r := sha256.Sum256([]byte(strings.Join([]string{
		action, op.ModelID,
		itoa(op.AdditionalItems), itoa(op.AdditionalCalls),
		itoa(op.AdditionalActiveSec), itoa(op.AdditionalRetries),
	}, "\x00")))
	return k[:], r[:]
}

// --- Service 实现 ---

func (s *service) GetExtractionStatus(ctx context.Context, kbID, docID, userID, role string) (ExtractionStatus, error) {
	scope, err := s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	return s.repo.extractionStatus(ctx, scope)
}

func (r *Repository) extractionStatus(ctx context.Context, scope documentScope) (ExtractionStatus, error) {
	st := ExtractionStatus{Enabled: scope.Enabled, CostKind: costKindNotApplicable}
	if scope.ActiveJobID == "" {
		// ⚠️ 没有作业时**全部字段留 null**，不填零。
		return st, nil
	}
	job, err := r.queries.GetActiveExtractionJobForDocument(ctx, scope.DocumentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return st, nil
		}
		return st, fmt.Errorf("knowledge: load extraction status: %w", err)
	}
	totals, err := r.jobLedgerTotals(ctx, job.ID)
	if err != nil {
		return st, err
	}

	st.JobID, st.State = &job.ID, &job.State
	if job.StopReason.Valid {
		st.StopReason = &job.StopReason.String
	}
	version := int64(job.DocumentVersion)
	st.DocumentVersion, st.ModelID = &version, &job.ModelID

	// ⭐ 初始化没完成时 total_items 仍然是 null：那个 0 不是"没有片段"，
	// 是"还不知道有多少片段"。
	if job.InitializationComplete {
		total, ok, failed := int(job.TotalItems), int(job.SucceededItems), int(job.FailedItems)
		st.TotalItems, st.SucceededItems, st.FailedItems = &total, &ok, &failed
		remainingItems := total - ok - failed
		st.RemainingItems = &remainingItems
	}
	confirmed := int(totals.ConfirmedDispatches)
	possible := int(totals.ConfirmedDispatches + totals.UnknownAttempts)
	unknownUsage := int(totals.Attempts - totals.UsageKnownAttempts)
	st.ConfirmedCalls, st.PossibleCalls, st.UnknownUsageAttempt = &confirmed, &possible, &unknownUsage
	st.ActiveMs = &totals.ActiveMs

	remainingCalls := int(job.CallLimit) - int(job.ReservedCalls)
	remainingMs := job.ActiveMsLimit - totals.ActiveMs
	st.RemainingCalls, st.RemainingActiveMs = &remainingCalls, &remainingMs
	return st, nil
}

func (s *service) SetExtractionEnabled(ctx context.Context, kbID, docID, userID, role string, enabled bool, op ExtractionOperation) (ExtractionStatus, error) {
	scope, err := s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	if err := validateOperation(op, enabled); err != nil {
		return ExtractionStatus{}, err
	}
	if enabled && !scope.IsNarrative {
		return ExtractionStatus{}, ErrRelationExtractionRequiresNarrative
	}
	if err := s.repo.applyExtractionSwitch(ctx, scope, enabled, op); err != nil {
		return ExtractionStatus{}, err
	}
	// ⚠️ 必须**重新读一次** scope：首次 enable 会建出作业并把文档指向它，
	// 而手上这份 scope 是操作之前读的，ActiveJobID 还是空的。
	// 拿旧的去查状态，响应里的 job_id 会是 null——用户看到"已开启"
	// 却没有任何作业，而下一次请求又会走"首次开启"那条路。
	scope, err = s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	return s.repo.extractionStatus(ctx, scope)
}

// applyExtractionSwitch 开关 + 首次 enable 时登记意图，同一个事务。
//
// ⭐ enable 一个**已暂停**的作业只恢复可见开关，**不自动运行**。
// ⚠️ 自动跑起来等于系统替用户推翻了一次显式的暂停。响应里会说明需要 resume。
func (r *Repository) applyExtractionSwitch(ctx context.Context, scope documentScope, enabled bool, op ExtractionOperation) error {
	keyHash, reqHash := operationHashes(op, boolLabel(enabled, "enable", "disable"))
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		if _, err := q.LockDocumentForExtraction(ctx, scope.DocumentID); err != nil {
			return fmt.Errorf("knowledge: lock document: %w", err)
		}
		if scope.ActiveJobID != "" {
			if err := checkIdempotency(ctx, q, scope.ActiveJobID, keyHash, reqHash); err != nil {
				return err
			}
		}
		flag := int32(0)
		if enabled {
			flag = 1
		}
		n, err := q.SetExtractionEnabled(ctx, gen.SetExtractionEnabledParams{
			IsRelationExtractionEnabled: flag == 1, ID: scope.DocumentID, Column3: int64(flag),
		})
		if err != nil {
			return fmt.Errorf("knowledge: set extraction enabled: %w", err)
		}
		if n == 0 {
			return ErrRelationExtractionRequiresNarrative
		}
		if !enabled || scope.ActiveJobID != "" {
			return nil
		}
		// 首次开启：登记一个等待事实 ready 的作业意图，由 reconcile 补 items。
		// ⚠️ initialization_complete 保持 0，state 保持 pending——它还不是
		// 一个"跑到一半"的作业，只是一个意图。
		jobID := platform.NewID()
		if err := q.CreateRelationExtractionJobIntent(ctx, gen.CreateRelationExtractionJobIntentParams{
			ID: jobID, DocumentID: scope.DocumentID,
			KnowledgeBaseID: scope.KnowledgeBaseID, DocumentVersion: int32(scope.Version),
			RunNumber: 1, ModelID: op.ModelID,
			ConfigHash: reqHash, ConfigSnapshot: []byte(`{}`),
			ApprovedItemLimit: defaultApprovedItemLimit, CallLimit: defaultCallLimit,
			ActiveMsLimit:        defaultActiveMsLimit,
			OperationKeyHash:     nullBytes(keyHash),
			OperationRequestHash: nullBytes(reqHash),
		}); err != nil {
			return fmt.Errorf("knowledge: create extraction job intent: %w", err)
		}
		// ⭐ 必须**同事务**把文档指向它。第一版漏了这一步，结果是：
		// 作业建出来了但没人指向它，第二次 enable 仍然看到"没有作业"，
		// 于是再建一个 run_number=1 的——撞上唯一键，用户看到 500。
		// 而在唯一键之前，这就是一个悄悄开出两个 run 的路径。
		if _, err := q.SetDocumentRelationJob(ctx, gen.SetDocumentRelationJobParams{
			ActiveRelationJobID: sql.NullString{String: jobID, Valid: true},
			RelationModelID:     sql.NullString{String: op.ModelID, Valid: true},
			ID:                  scope.DocumentID, Version: scope.Version,
		}); err != nil {
			return fmt.Errorf("knowledge: point document at job intent: %w", err)
		}
		return nil
	})
}

// checkIdempotency 比对幂等键与请求体。
//
// ⭐ 同键同 body → 当成重放，什么都不做（调用方随后读当前状态返回）。
// ⭐ 同键异 body → 409。⚠️ 放行的话第二次请求的参数会被静默忽略，
// 用户以为改生效了，实际还在用第一次那份。
func checkIdempotency(ctx context.Context, q *gen.Queries, jobID string, keyHash, reqHash []byte) error {
	job, err := q.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("knowledge: load job for idempotency: %w", err)
	}
	if !job.OperationKeyHash.Valid || job.OperationKeyHash.String != string(keyHash) {
		return nil
	}
	if job.OperationRequestHash.Valid && job.OperationRequestHash.String != string(reqHash) {
		return ErrIdempotencyKeyConflict
	}
	return nil
}

// PauseExtraction 暂停当前 run。
//
// ⚠️ 可暂停的来源状态有两个：running（正在跑）和 pending（登记了意图但还没
// 开始）。只认 running 的话，用户在"等待文档就绪"阶段点暂停会毫无反应，
// 而文档一 ready 作业就自己跑起来了——用户明明按过暂停。
func (s *service) PauseExtraction(ctx context.Context, kbID, docID, userID, role string, op ExtractionOperation) (ExtractionStatus, error) {
	return s.transitionJob(ctx, kbID, docID, userID, role, op, "pause",
		[]string{jobStateRunning, jobStatePending, jobStateInitializing},
		jobStatePaused, "paused_by_user")
}

func (s *service) ResumeExtraction(ctx context.Context, kbID, docID, userID, role string, op ExtractionOperation) (ExtractionStatus, error) {
	scope, err := s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	if err := validateOperation(op, false); err != nil {
		return ExtractionStatus{}, err
	}
	// ⚠️ disabled 必须先 enable 再 resume——直接续跑等于绕过用户的关闭动作。
	if !scope.Enabled || scope.ActiveJobID == "" {
		return ExtractionStatus{}, ErrExtractionNotEnabled
	}
	keyHash, reqHash := operationHashes(op, "resume")
	err = platform.WithTx(ctx, s.repo.db, func(tx *sql.Tx) error {
		q := s.repo.queries.WithTx(tx)
		if _, err := q.LockDocumentForExtraction(ctx, scope.DocumentID); err != nil {
			return fmt.Errorf("knowledge: lock document: %w", err)
		}
		if err := checkIdempotency(ctx, q, scope.ActiveJobID, keyHash, reqHash); err != nil {
			return err
		}
		if op.AdditionalItems > 0 || op.AdditionalCalls > 0 ||
			op.AdditionalActiveSec > 0 || op.AdditionalRetries > 0 {
			// ⭐ 先记账再加额度，且**同一个事务**：加了额度却没记下来，
			// 最终成本里就有一部分无法解释——报告只能说"总共花了 N"，
			// 说不出"其中 M 是中途追加的"。而追加恰恰是最需要复盘的动作：
			// 一次实验追加了五轮，本身就说明第一次的预算估计错了。
			if err := recordBudgetOperation(ctx, q, scope.ActiveJobID, userID, op); err != nil {
				return err
			}
			if _, err := q.AddJobBudget(ctx, gen.AddJobBudgetParams{
				ApprovedItemLimit: int32(op.AdditionalItems),
				CallLimit:         int32(op.AdditionalCalls),
				ActiveMsLimit:     int64(op.AdditionalActiveSec) * 1000,
				RetryRounds:       int32(op.AdditionalRetries),
				ID:                scope.ActiveJobID,
			}); err != nil {
				return fmt.Errorf("knowledge: add job budget: %w", err)
			}
		}
		if _, err := q.SetJobState(ctx, gen.SetJobStateParams{
			State: jobStateRunning, StopReason: sql.NullString{},
			ID: scope.ActiveJobID, State_2: jobStatePaused,
		}); err != nil {
			return fmt.Errorf("knowledge: resume job: %w", err)
		}
		return nil
	})
	if err != nil {
		return ExtractionStatus{}, err
	}
	return s.repo.extractionStatus(ctx, scope)
}

func (s *service) RestartExtraction(ctx context.Context, kbID, docID, userID, role string, op ExtractionOperation) (ExtractionStatus, error) {
	scope, err := s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	if err := validateOperation(op, true); err != nil {
		return ExtractionStatus{}, err
	}
	if scope.Status != StatusReady {
		return ExtractionStatus{}, ErrExtractionSourceChanged
	}
	keyHash, reqHash := operationHashes(op, "restart")
	if scope.ActiveJobID != "" {
		if err := platform.WithTx(ctx, s.repo.db, func(tx *sql.Tx) error {
			return checkIdempotency(ctx, s.repo.queries.WithTx(tx), scope.ActiveJobID, keyHash, reqHash)
		}); err != nil {
			return ExtractionStatus{}, err
		}
	}
	spec := newExtractionJobSpec(platform.NewID(), scope.DocumentID, scope.Version, op.ModelID)
	spec.KnowledgeBaseID = scope.KnowledgeBaseID
	spec.RunNumber = nextRunNumber(ctx, s.repo, scope.DocumentID)
	spec.OperationKey, spec.RequestHash = keyHash, reqHash
	if _, err := s.repo.initializeExtractionJob(ctx, spec); err != nil {
		return ExtractionStatus{}, err
	}
	scope, err = s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	return s.repo.extractionStatus(ctx, scope)
}

func nextRunNumber(ctx context.Context, r *Repository, documentID string) int {
	var n sql.NullInt32
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(run_number) FROM relation_extraction_jobs WHERE document_id = ?`,
		documentID).Scan(&n); err != nil || !n.Valid {
		return 1
	}
	return int(n.Int32) + 1
}

// transitionJob 是 pause 这类"只改状态"的操作。
func (s *service) transitionJob(
	ctx context.Context, kbID, docID, userID, role string, op ExtractionOperation,
	action string, from []string, to, reason string,
) (ExtractionStatus, error) {
	scope, err := s.repo.resolveDocumentScope(ctx, kbID, docID, userID, role)
	if err != nil {
		return ExtractionStatus{}, err
	}
	if err := validateOperation(op, false); err != nil {
		return ExtractionStatus{}, err
	}
	if scope.ActiveJobID == "" {
		// ⚠️ 没有运行中的作业也**幂等返回当前状态**，不报错：
		// 用户点"暂停"时作业刚好自己结束了，不是一个需要他处理的问题。
		return s.repo.extractionStatus(ctx, scope)
	}
	keyHash, reqHash := operationHashes(op, action)
	err = platform.WithTx(ctx, s.repo.db, func(tx *sql.Tx) error {
		q := s.repo.queries.WithTx(tx)
		if err := checkIdempotency(ctx, q, scope.ActiveJobID, keyHash, reqHash); err != nil {
			return err
		}
		// ⚠️ 逐个来源状态试一遍，而不是无条件改：无条件改会让一条迟到的
		// pause 把已经结束的作业改回 paused，恢复扫描随后又把它捡起来。
		for _, f := range from {
			n, err := q.SetJobState(ctx, gen.SetJobStateParams{
				State: to, StopReason: nullString(reason),
				ID: scope.ActiveJobID, State_2: f,
			})
			if err != nil {
				return err
			}
			if n > 0 {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return ExtractionStatus{}, err
	}
	return s.repo.extractionStatus(ctx, scope)
}

// maxBudgetOperations 是每个 run 的控制操作上限。
//
// ⚠️ 上限存在的理由不是省空间：一个被追加了几十轮额度的 run，它的"预算"
// 已经和最初批准的那个数字没有关系了。到那个程度应该 restart 并重新说明
// 要花多少，而不是继续在同一个 run 上加。
const maxBudgetOperations = 100

// budgetOperation 是一次额度追加的记录。
type budgetOperation struct {
	IdempotencyKey  string `json:"idempotency_key"`
	ByUserID        string `json:"by_user_id"`
	AdditionalItems int    `json:"additional_chunks"`
	AdditionalCalls int    `json:"additional_calls"`
	AdditionalSecs  int    `json:"additional_active_seconds"`
	AdditionalRetry int    `json:"additional_retry_rounds"`
}

func recordBudgetOperation(ctx context.Context, q *gen.Queries, jobID, userID string, op ExtractionOperation) error {
	raw, err := q.GetJobBudgetOperations(ctx, jobID)
	if err != nil {
		return fmt.Errorf("knowledge: read budget operations: %w", err)
	}
	var ops []budgetOperation
	if blob := asString(raw); blob != "" {
		if err := json.Unmarshal([]byte(blob), &ops); err != nil {
			// ⚠️ 损坏的记录**报错**，不当成空数组重来。当成空的话，
			// 已经追加过的那些额度从记录里消失，而额度本身还在——
			// 一个总额对不上任何记录的 run。
			return fmt.Errorf("knowledge: parse budget operations for job %s: %w", jobID, err)
		}
	}
	if len(ops) >= maxBudgetOperations {
		return apperr.Conflict("knowledge.budget_operations_exhausted",
			"本次运行的额度追加次数已达上限，请重新开始一次抽取")
	}
	ops = append(ops, budgetOperation{
		IdempotencyKey: op.IdempotencyKey, ByUserID: userID,
		AdditionalItems: op.AdditionalItems, AdditionalCalls: op.AdditionalCalls,
		AdditionalSecs: op.AdditionalActiveSec, AdditionalRetry: op.AdditionalRetries,
	})
	blob, err := json.Marshal(ops)
	if err != nil {
		return fmt.Errorf("knowledge: marshal budget operations: %w", err)
	}
	if _, err := q.SetJobBudgetOperations(ctx, gen.SetJobBudgetOperationsParams{
		BudgetOperations: blob, ID: jobID,
	}); err != nil {
		return fmt.Errorf("knowledge: write budget operations: %w", err)
	}
	return nil
}

func boolLabel(b bool, yes, no string) string {
	if b {
		return yes
	}
	return no
}
