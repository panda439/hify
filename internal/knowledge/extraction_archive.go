package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"hify/internal/db/gen"
	"hify/internal/platform"
)

// extraction_archive.go 清理过期账目与失效派生记录（010 T023）。
//
// ⭐ 清理是这条链路上**唯一会让数据变少**的动作，也因此是唯一一个能把
// 已经花掉的钱从账上抹掉的地方。本期最重要的产出之一是一个可复核的成本
// 数字，而清理跑完之后那个数字必须一分不差。
//
// ⚠️ 两个方向的偏差都不报错：
//   - 汇总没加上就删 → 费用凭空变少，系统显得更便宜；
//   - 重复归档 → 费用凭空变多，而没人能解释多出来的部分从哪来。

const (
	// attemptRetention：attempt 明细保留多久。到期后汇总进 job 再删除。
	// 明细的用途是复核与回放，30 天足够覆盖一次实验从跑完到写完报告的周期。
	attemptRetention = 30 * 24 * time.Hour

	// derivedRowRetention：被取代/失败作业的派生记录保留多久。
	// 比 attempt 短：它们不可查询，留着只占空间；而 attempt 是账目凭据。
	derivedRowRetention = 7 * 24 * time.Hour
)

// archivedLedger 是被删掉的那些 attempt 的汇总。
//
// ⭐ UsageKnownAttempts 单独记：把未知用量当 0 相加，就是把"没测到"和
// "真的没花"混成一个数。归档之后原始行没了，这个区分**再也无法恢复**，
// 所以必须在汇总里带上，报告才能说出"token 数只覆盖 N/M 次调用"。
type archivedLedger struct {
	Attempts            int64  `json:"attempts"`
	ConfirmedDispatches int64  `json:"confirmed_dispatches"`
	UnknownAttempts     int64  `json:"unknown_attempts"`
	UsageKnownAttempts  int64  `json:"usage_known_attempts"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	ActiveMs            int64  `json:"active_ms"`
	CostAmount          string `json:"cost_amount"`
	LastArchivedAt      string `json:"last_archived_at"`
}

// ledgerTotals 是一个作业的完整账目。
//
// ⭐ 口径固定为「未归档明细 + 归档汇总」。
// ⚠️ **不能**改用 jobs 表上的 confirmed_dispatches / active_ms_used 再加汇总：
// 那两列是**全生命周期**计数，归档时并不减少，加上汇总就是把同一批调用算了
// 两遍。两条口径必须择一，这里择前者，因为它在清理之后仍然成立。
type ledgerTotals struct {
	Attempts            int64
	ConfirmedDispatches int64
	UnknownAttempts     int64
	UsageKnownAttempts  int64
	InputTokens         int64
	OutputTokens        int64
	ActiveMs            int64
}

// jobLedgerTotals 返回一个作业的完整账目（活账 + 归档汇总）。
func (r *Repository) jobLedgerTotals(ctx context.Context, jobID string) (ledgerTotals, error) {
	live, err := r.queries.SumLiveAttempts(ctx, jobID)
	if err != nil {
		return ledgerTotals{}, fmt.Errorf("knowledge: sum live attempts: %w", err)
	}
	archived, err := r.readArchivedLedger(ctx, jobID)
	if err != nil {
		return ledgerTotals{}, err
	}
	return ledgerTotals{
		Attempts:            live.Attempts + archived.Attempts,
		ConfirmedDispatches: asInt64(live.ConfirmedDispatches) + archived.ConfirmedDispatches,
		UnknownAttempts:     asInt64(live.UnknownAttempts) + archived.UnknownAttempts,
		UsageKnownAttempts:  asInt64(live.UsageKnownAttempts) + archived.UsageKnownAttempts,
		InputTokens:         asInt64(live.InputTokens) + archived.InputTokens,
		OutputTokens:        asInt64(live.OutputTokens) + archived.OutputTokens,
		ActiveMs:            asInt64(live.ActiveMs) + archived.ActiveMs,
	}, nil
}

func (r *Repository) readArchivedLedger(ctx context.Context, jobID string) (archivedLedger, error) {
	row, err := r.queries.GetRelationExtractionJobPayload(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return archivedLedger{}, nil
		}
		return archivedLedger{}, fmt.Errorf("knowledge: read job payload: %w", err)
	}
	blob := asString(row.ArchivedLedgerSummary)
	if blob == "" {
		return archivedLedger{}, nil
	}
	var out archivedLedger
	if err := json.Unmarshal([]byte(blob), &out); err != nil {
		// ⚠️ 损坏的汇总**报错**，不降级成零值。降级的表现是这个作业的历史
		// 费用突然归零，而报告照样出得来——一个凭空变便宜的数字，
		// 且没有任何地方说明它为什么变了。
		return archivedLedger{}, fmt.Errorf("knowledge: parse archived ledger for job %s: %w", jobID, err)
	}
	return out, nil
}

// archiveExpiredAttempts 把过期的 attempt 明细汇总进作业，然后删除。
//
// ⭐ 求和、写汇总、删除在**同一个事务**里。分开的后果：
//   - 先删后汇总，中间崩了 → 这批费用永远消失；
//   - 先汇总后删，中间崩了 → 下一轮再汇总一遍，费用翻倍。
//
// ⚠️ 分批的单位是**作业**，不是行：一个作业的 attempt 上限就是它的
// call_limit（默认 3000），一次事务处理这么多行可以接受；按行分批则要求
// "求和覆盖的行"与"删掉的行"严格对齐同一批，多出一整套游标对齐的复杂度，
// 而对齐不上的两种偏差（多删/少删）都不报错。
func (r *Repository) archiveExpiredAttempts(ctx context.Context, retention time.Duration, maxJobs int) (int, error) {
	cutoff := time.Now().UTC().Add(-retention)
	jobIDs, err := r.queries.ListJobsWithArchivableAttempts(ctx,
		gen.ListJobsWithArchivableAttemptsParams{
			FinishedAt: sql.NullTime{Time: cutoff, Valid: true}, Limit: int32(maxJobs)})
	if err != nil {
		return 0, fmt.Errorf("knowledge: list jobs with archivable attempts: %w", err)
	}

	archived := 0
	for _, jobID := range jobIDs {
		n, err := r.archiveOneJob(ctx, jobID, cutoff)
		if err != nil {
			return archived, err
		}
		archived += n
	}
	return archived, nil
}

func (r *Repository) archiveOneJob(ctx context.Context, jobID string, cutoff time.Time) (int, error) {
	var deleted int
	err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		sum, err := q.SumArchivableAttempts(ctx, gen.SumArchivableAttemptsParams{
			JobID: jobID, FinishedAt: sql.NullTime{Time: cutoff, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("knowledge: sum archivable attempts: %w", err)
		}
		if sum.Attempts == 0 {
			return nil
		}

		prev, err := r.readArchivedLedgerTx(ctx, q, jobID)
		if err != nil {
			return err
		}
		costAmount, err := addDecimalAmounts(prev.CostAmount, asString(sum.CostAmount))
		if err != nil {
			return fmt.Errorf("knowledge: merge archived cost: %w", err)
		}
		merged := archivedLedger{
			Attempts:            prev.Attempts + sum.Attempts,
			ConfirmedDispatches: prev.ConfirmedDispatches + asInt64(sum.ConfirmedDispatches),
			UnknownAttempts:     prev.UnknownAttempts + asInt64(sum.UnknownAttempts),
			UsageKnownAttempts:  prev.UsageKnownAttempts + asInt64(sum.UsageKnownAttempts),
			InputTokens:         prev.InputTokens + asInt64(sum.InputTokens),
			OutputTokens:        prev.OutputTokens + asInt64(sum.OutputTokens),
			ActiveMs:            prev.ActiveMs + asInt64(sum.ActiveMs),
			CostAmount:          costAmount,
			LastArchivedAt:      time.Now().UTC().Format(time.RFC3339),
		}
		blob, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("knowledge: marshal archived ledger: %w", err)
		}
		if _, err := q.UpdateJobArchivedSummary(ctx, gen.UpdateJobArchivedSummaryParams{
			ArchivedLedgerSummary: blob, ID: jobID,
		}); err != nil {
			return fmt.Errorf("knowledge: update archived ledger: %w", err)
		}

		// ⚠️ 谓词与 SumArchivableAttempts 逐字相同。不同的话，求和覆盖的行
		// 和删掉的行就不是同一批：多删的费用永远消失，少删的下一轮再加一遍。
		n, err := q.DeleteArchivableAttempts(ctx, gen.DeleteArchivableAttemptsParams{
			JobID: jobID, FinishedAt: sql.NullTime{Time: cutoff, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("knowledge: delete archived attempts: %w", err)
		}
		deleted = int(n)
		return nil
	})
	return deleted, err
}

func addDecimalAmounts(left, right string) (string, error) {
	if left == "" {
		left = "0"
	}
	if right == "" {
		right = "0"
	}
	a, ok := new(big.Rat).SetString(left)
	if !ok {
		return "", fmt.Errorf("invalid decimal %q", left)
	}
	b, ok := new(big.Rat).SetString(right)
	if !ok {
		return "", fmt.Errorf("invalid decimal %q", right)
	}
	return new(big.Rat).Add(a, b).FloatString(10), nil
}

func (r *Repository) readArchivedLedgerTx(ctx context.Context, q *gen.Queries, jobID string) (archivedLedger, error) {
	row, err := q.GetRelationExtractionJobPayload(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return archivedLedger{}, nil
		}
		return archivedLedger{}, fmt.Errorf("knowledge: read job payload: %w", err)
	}
	blob := asString(row.ArchivedLedgerSummary)
	if blob == "" {
		return archivedLedger{}, nil
	}
	var out archivedLedger
	if err := json.Unmarshal([]byte(blob), &out); err != nil {
		return archivedLedger{}, fmt.Errorf("knowledge: parse archived ledger for job %s: %w", jobID, err)
	}
	return out, nil
}

// cleanupDeadJobDerivedRows 删掉被取代 / 失败作业的人物、别名、关系、证据。
//
// ⭐ 删除顺序是子表在前（证据 → 关系 → 别名 → 人物），与外键方向一致——
// 本期故意没建跨表外键，但顺序仍按依赖方向来，这样中途崩溃留下的是
// "少了证据的关系"（可再删），而不是"指向不存在人物的关系"（更难解释）。
//
// ⚠️ **不动 relation_extraction_jobs / attempts 这两张账目表**：钱是真花过的，
// 作业记录和它的费用必须留下。清理派生记录只是让不可查询的数据不再占空间。
//
// ⚠️ 只清 superseded / failed。succeeded 的是用户现在查得到的关系数据；
// paused / budget_exhausted 随时可能被继续。
func (r *Repository) cleanupDeadJobDerivedRows(ctx context.Context, retention time.Duration, maxJobs int) (int, error) {
	cutoff := time.Now().UTC().Add(-retention)
	jobIDs, err := r.queries.ListDeadJobsWithDerivedRows(ctx,
		gen.ListDeadJobsWithDerivedRowsParams{
			FinishedAt: sql.NullTime{Time: cutoff, Valid: true}, Limit: int32(maxJobs),
		})
	if err != nil {
		return 0, fmt.Errorf("knowledge: list dead jobs: %w", err)
	}

	cleaned := 0
	for _, jobID := range jobIDs {
		err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
			q := r.queries.WithTx(tx)
			if _, err := q.DeleteJobEvidence(ctx, jobID); err != nil {
				return fmt.Errorf("knowledge: delete evidence: %w", err)
			}
			if _, err := q.DeleteJobRelations(ctx, jobID); err != nil {
				return fmt.Errorf("knowledge: delete relations: %w", err)
			}
			if _, err := q.DeleteJobAliases(ctx, jobID); err != nil {
				return fmt.Errorf("knowledge: delete aliases: %w", err)
			}
			if _, err := q.DeleteJobCharacters(ctx, jobID); err != nil {
				return fmt.Errorf("knowledge: delete characters: %w", err)
			}
			return nil
		})
		if err != nil {
			return cleaned, err
		}
		cleaned++
	}
	return cleaned, nil
}

// asString 把 sqlc 对 CAST(... AS CHAR) 的 interface{} 返回收敛成字符串。
// ⚠️ 可空 JSON 列在 Go 侧拿到的是 nil / []byte / string 三种形态之一，
// 取决于驱动与是否为 NULL；集中在这里处理，避免每个调用点各写一套断言。
func asString(v any) string {
	switch n := v.(type) {
	case nil:
		return ""
	case string:
		return n
	case []byte:
		return string(n)
	default:
		return ""
	}
}

// asInt64 把 sqlc 对 SUM/COUNT 的各种返回类型收敛成 int64。
// ⚠️ MySQL 的 SUM 返回 DECIMAL，sqlc 映射成 interface{}/string 视上下文而定；
// 这里集中处理，避免每个调用点各写一套断言。
func asInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case float64:
		return int64(n)
	case []byte:
		var out int64
		fmt.Sscanf(string(n), "%d", &out)
		return out
	case string:
		var out int64
		fmt.Sscanf(n, "%d", &out)
		return out
	default:
		return 0
	}
}
