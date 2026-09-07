package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"hify/internal/db/gen"
	"hify/internal/platform"
	"hify/internal/platform/apperr"
	"hify/internal/provider"
)

// extraction_ledger.go 记每一次外部调用（010 T017）。
//
// ⭐ 这张表是「全书抽取成本」这个数字可信度的**全部**来源，而那正是本期
// 最重要的两个产出之一。
//
// ⚠️ 少记和多记的严重性不对称：多记会被人发现（数字比账单大，有人会查）；
// 少记不会——它让系统显得更便宜，没有人会去质疑一个好看的数字。
// 所以每一条"可能花了钱"的路径都必须落在账上，包括那些我们并不确定
// 到底有没有花钱的路径。

const (
	phaseExtract = "extract"
	phaseAlias   = "alias"

	// maxRawResponseBytes 与 data-model 的 64 KiB 上限一致。
	// ⚠️ 截断必须**显式记录**：一份被悄悄截短的响应会让后来的回放
	// 得出与当时不同的结论，而两次结论不同却找不到原因。
	maxRawResponseBytes = 64 * 1024

	// costKindNotApplicable：本地 Ollama 没有金钱计费。
	// ⚠️ 不是 0——0 的意思是"花了零元"，而真实情况是这个口径不适用，
	// 硬件电力成本没有测量。报告里必须照这个区分写。
	costKindNotApplicable = "not_applicable"
)

var (
	// ErrExtractionCallBudgetExhausted：调用额度用尽。
	ErrExtractionCallBudgetExhausted = apperr.Conflict(
		"knowledge.extraction_call_budget_exhausted",
		"本次抽取的调用额度已用尽，可以追加额度后继续")

	// ErrAttemptAlreadySettled：一次尝试只能结算一次。
	// ⚠️ 重复结算会让 usage、费用、活跃时长被重复累加进作业级聚合。
	ErrAttemptAlreadySettled = apperr.Conflict(
		"knowledge.attempt_already_settled", "该次调用已经结算过")
)

// attemptReservation 是预留一次尝试需要的全部输入。
type attemptReservation struct {
	JobID           string
	ItemID          string
	Epoch           int
	Phase           string
	RequestHash     []byte
	MaxOutputTokens int
}

// reservedAttempt 是已经预留、尚未结算的一次尝试。
type reservedAttempt struct {
	ID    string
	JobID string
	Phase string
	// AttemptNumber 由**预留事务自己分配**（010 R6-03），不是调用方传进来的。
	// 理由见 reserveExtractionAttempt 的注释。
	AttemptNumber int
	StartedAt     time.Time
}

// errAttemptsExhausted：这个阶段的重试次数已经用完。
//
// ⚠️ 它在预留事务**内部**被发现（分配到的编号超了上限），整个事务回滚——
// 额度没占、attempt 行没建、计数也没加。调用方据此放弃这个阶段。
var errAttemptsExhausted = errors.New("knowledge: attempts exhausted for phase")

// reserveExtractionAttempt 在**发出外部调用之前**占住额度并落下记录。
//
// ⭐ 顺序不可颠倒。颠倒的后果：进程在"已发出、未收到"之间崩掉，这次调用
// 不会留下任何痕迹，而它的钱已经花了。恢复扫描把停留过久的 reserved 改判
// unknown，于是"可能花了"这件事被如实记下来——这正是 unknown 存在的理由。
//
// ⚠️ 额度检查和额度占用是**同一条 UPDATE**。先读后写在两个 worker 之间
// 必然超发，而超发的表现是账单超了，不报错。
//
// ⭐ 编号分配、计数递增、额度占用、attempt 落行是**同一个事务**（010 R6-03）。
// ⚠️ 此前编号由调用方按"已用次数 + 1"算好传进来，而递增计数是预留提交
// **之后**的另一次提交。两次提交之间崩掉的话，attempt 行已经存在、
// item.extract_attempt_count 仍是 0——恢复之后仍然从 1 开始预留，
// 撞上 uk_rea_item_phase_attempt 重复键。而恢复扫描把旧 attempt 改判
// unknown **并不会修复计数**，于是这个 item 每一轮都撞同一个错，
// 永远好不了，且不再花钱也不再产出。
//
// 锁顺序 job → item，与其余事务一致（见 LockDocumentForExtraction）。
func (r *Repository) reserveExtractionAttempt(ctx context.Context, res attemptReservation) (reservedAttempt, error) {
	attempt := reservedAttempt{
		ID: platform.NewID(), JobID: res.JobID, Phase: res.Phase,
		StartedAt: time.Now().UTC(),
	}
	err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		n, err := q.AddJobCallReservation(ctx, gen.AddJobCallReservationParams{
			ID: res.JobID, Epoch: int32(res.Epoch),
		})
		if err != nil {
			return fmt.Errorf("knowledge: reserve call budget: %w", err)
		}
		if n == 0 {
			// 额度用尽、活跃时间用尽，或者 epoch 已经不是自己的。
			// 三者都必须停手，且**不留下孤儿 attempt 行**——整个事务回滚。
			// ⚠️ 再问一次是哪一维用尽：两者的下一步不同（调用额度可以追加，
			// 活跃时间用尽说明模型变慢了，追加未必解决问题）。
			state, qerr := q.ClassifyJobBudgetState(ctx, res.JobID)
			if qerr != nil {
				return fmt.Errorf("knowledge: classify budget state: %w", qerr)
			}
			if state.ActiveTimeExhausted {
				return ErrExtractionActiveTimeExhausted
			}
			return ErrExtractionCallBudgetExhausted
		}
		// 递增本阶段计数（UPDATE 顺带锁住 item 行），再把新值读回来
		// 当作这次的编号——⚠️ 编号必须来自**持久事实**，不是调用方的记忆。
		var extract, alias int32
		if res.Phase == phaseAlias {
			alias = 1
		} else {
			extract = 1
		}
		if _, err := q.BumpItemAttemptCount(ctx, gen.BumpItemAttemptCountParams{
			ExtractAttemptCount: extract, AliasAttemptCount: alias, ID: res.ItemID,
		}); err != nil {
			return fmt.Errorf("knowledge: bump attempt count: %w", err)
		}
		counts, err := q.GetItemAttemptCounts(ctx, res.ItemID)
		if err != nil {
			return fmt.Errorf("knowledge: read attempt counts: %w", err)
		}
		number := int(counts.ExtractAttemptCount)
		if res.Phase == phaseAlias {
			number = int(counts.AliasAttemptCount)
		}
		if number > maxAttemptsPerPhase {
			// ⚠️ 整个事务回滚：额度没占、行没建、计数也没加。
			return errAttemptsExhausted
		}
		attempt.AttemptNumber = number
		return q.ReserveExtractionAttempt(ctx, gen.ReserveExtractionAttemptParams{
			ID: attempt.ID, JobID: res.JobID, ItemID: res.ItemID,
			Epoch: int32(res.Epoch), Phase: res.Phase,
			AttemptNumber: int32(number),
			RequestHash:   res.RequestHash, MaxOutputTokens: int32(res.MaxOutputTokens),
		})
	})
	if err != nil {
		return reservedAttempt{}, err
	}
	return attempt, nil
}

// settleExtractionAttempt 把一次尝试的结局如实写下来，并计进作业级账目。
//
// ⚠️ 它**自己开一个事务**，与后续的关系发布事务完全分开。这不是疏忽：
// 发布失败会回滚，而那次调用的钱已经花了——账目跟着回滚就等于把一笔
// 真实成本抹掉。账先落，业务结果后落，顺序不可换。
func (r *Repository) settleExtractionAttempt(ctx context.Context, att reservedAttempt, res provider.ChatAttemptResult) error {
	finishedAt := time.Now().UTC()
	raw, truncated := capRawResponse(res.Message.Content)

	errorCode := res.ErrorCode
	if truncated {
		// ⚠️ 截断压过原有的错误码：一份被截短的响应是所有后续结论的
		// 前提条件，比"为什么失败"更需要被看见。
		errorCode = errorCodeResponseTruncated
	}

	var responseHash []byte
	if raw != "" {
		sum := sha256.Sum256([]byte(raw))
		responseHash = sum[:]
	}

	// ⚠️ usage 未知时写 NULL，不写 0。0 会让"没测到"和"真的没花"永久
	// 不可区分，而本期最重要的产出正是一个可复核的成本数字。
	var inTok, outTok sql.NullInt32
	if res.UsageKnown {
		inTok = sql.NullInt32{Int32: int32(res.InputTokens), Valid: true}
		outTok = sql.NullInt32{Int32: int32(res.OutputTokens), Valid: true}
	}

	confirmed, unknown := 0, 0
	switch res.Outcome {
	case provider.AttemptCompleted, provider.AttemptFailed:
		confirmed = 1
	case provider.AttemptUnknown:
		unknown = 1
	}

	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		n, err := q.SettleExtractionAttempt(ctx, gen.SettleExtractionAttemptParams{
			State:             string(res.Outcome),
			StartedAt:         sql.NullTime{Time: att.StartedAt, Valid: true},
			FinishedAt:        sql.NullTime{Time: finishedAt, Valid: true},
			ElapsedMs:         sql.NullInt64{Int64: res.ElapsedMs, Valid: true},
			DispatchConfirmed: res.Dispatched,
			UsageKnown:        res.UsageKnown,
			InputTokens:       inTok,
			OutputTokens:      outTok,
			FinishReason:      nullString(res.FinishReason),
			ErrorCode:         nullString(errorCode),
			RawResponse:       nullString(raw),
			ResponseHash:      nullBytes(responseHash),
			CostKind:          costKindNotApplicable,
			ID:                att.ID,
		})
		if err != nil {
			return fmt.Errorf("knowledge: settle attempt: %w", err)
		}
		if n == 0 {
			return ErrAttemptAlreadySettled
		}

		// ⭐ 只有**确定没发出去**的调用退还额度。unknown 绝不退——
		// 那笔钱可能已经花了，退还就是在系统性低估成本，
		// 而且偏差方向恒定：永远偏小。
		if res.Outcome == provider.AttemptNotDispatched {
			if _, err := q.RefundJobCallReservation(ctx, att.JobID); err != nil {
				return fmt.Errorf("knowledge: refund call reservation: %w", err)
			}
		}
		if _, err := q.RecordJobDispatchOutcome(ctx, gen.RecordJobDispatchOutcomeParams{
			ConfirmedDispatches: int32(confirmed),
			UnknownAttempts:     int32(unknown),
			ActiveMsUsed:        res.ElapsedMs,
			ID:                  att.JobID,
		}); err != nil {
			return fmt.Errorf("knowledge: record dispatch outcome: %w", err)
		}
		return nil
	})
}

// reconcileStaleReservedAttempts 把停留过久的 reserved 改判 unknown。
//
// ⭐ 改判 **unknown**，不是删掉、也不是标 failed：那次调用可能已经到达
// 服务端并被处理，说"没发生过"就是少记一笔真实成本。
func (r *Repository) reconcileStaleReservedAttempts(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	rows, err := r.queries.ListStaleReservedAttempts(ctx, gen.ListStaleReservedAttemptsParams{
		CreatedAt: time.Now().UTC().Add(-olderThan), Limit: int32(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("knowledge: list stale reserved attempts: %w", err)
	}
	var fixed int
	for _, row := range rows {
		err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
			q := r.queries.WithTx(tx)
			n, err := q.MarkAttemptUnknown(ctx, gen.MarkAttemptUnknownParams{
				ErrorCode:  nullString("abandoned_reservation"),
				FinishedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true},
				ID:         row.ID,
			})
			if err != nil {
				return err
			}
			if n == 0 {
				// 已经被别人结算了，什么都不做——**不能**再累加一次账目。
				return nil
			}
			fixed++
			_, err = q.RecordJobDispatchOutcome(ctx, gen.RecordJobDispatchOutcomeParams{
				ConfirmedDispatches: 0, UnknownAttempts: 1, ActiveMsUsed: 0, ID: row.JobID,
			})
			return err
		})
		if err != nil {
			return fixed, fmt.Errorf("knowledge: reconcile reserved attempt %s: %w", row.ID, err)
		}
	}
	return fixed, nil
}

// capRawResponse 把响应截到上限内，**并在 rune 边界上截**。
//
// ⚠️ 按字节硬切会把一个多字节汉字劈成两半，落库得到一段无效 UTF-8——
// 而这份原始响应是回放和人工复核的唯一依据，坏掉一个字节就可能让整段
// 无法解析，且报错发生在几天后的复核阶段。
// errorCodeResponseTruncated 标记一份**存不下全文**的响应。
//
// ⭐ 它同时是"这次调用的账要照记"和"这个结果不可用"两件事的标记
// （010 R6-06）。⚠️ 只写错误码而 state 仍是 completed，回放会把被截掉
// 内容的响应当成一次成功结果取回来——而它一定解析不出、或者解析出**少了
// 后半段**的结果，看起来完全正常。
const errorCodeResponseTruncated = "response_truncated"

// extractionResultUnusable 判断一次**成功返回**的响应是否不可用作结果。
//
// ⭐ 首次调用与恢复回放**必须共用这一个判据**（010 R6-06）。
// ⚠️ 分成两套的表现是：同一份响应第一次被拒、重启之后被接受，
// 或者反过来——而两条路径各自看起来都自洽。
func extractionResultUnusable(finishReason, body string) string {
	if finishReason == finishReasonLength {
		return "output_truncated"
	}
	if _, truncated := capRawResponse(body); truncated {
		return errorCodeResponseTruncated
	}
	return ""
}

func capRawResponse(s string) (string, bool) {
	if len(s) <= maxRawResponseBytes {
		return s, false
	}
	cut := maxRawResponseBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
