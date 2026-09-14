package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"hify/internal/db/gen"
	"hify/internal/platform"
)

// extraction_reconcile.go 是抽取作业的恢复扫描（010 T021）。
//
// ⭐ 这里最要紧的不是"能捡回来"，而是**哪些不该捡**。
// 自动恢复把用户暂停的作业重新跑起来，是系统擅自推翻一次显式决定——
// 用户会看到一个自己明明暂停过的作业又开始花钱，而系统认为自己很尽责。
// 预算耗尽同理：追加额度是用户的决定。

const (
	// staleReservationThreshold：预留停留超过这么久就改判 unknown。
	//
	// ⚠️ 必须显著大于「一次调用超时 + 结算耗时」，否则会把**正在进行中**
	// 的调用误判成孤儿，凭空多出一笔 unknown。取单次超时的 10 倍。
	staleReservationThreshold = 10 * extractionCallTimeout

	// reconcileBatchSize 是一次扫描处理多少条，防止单次运行处理量失控。
	reconcileBatchSize = 100
	// extractionAttemptRetention 是原始模型响应的审计保留期。作业级汇总
	// 永久保留；到期后只清理可再生的逐次明细。
	extractionAttemptRetention = 30 * 24 * time.Hour
)

// recoverableJob 是一条等着被接手的作业。
type recoverableJob struct {
	ID                     string
	DocumentID             string
	DocumentVersion        int64
	Epoch                  int
	State                  string
	InitializationComplete bool
}

// listRecoverableExtractionJobs 按 id 游标翻页列出需要接手的作业。
func (r *Repository) listRecoverableExtractionJobs(ctx context.Context, afterID string, limit int) ([]recoverableJob, error) {
	rows, err := r.queries.ListRecoverableExtractionJobs(ctx, gen.ListRecoverableExtractionJobsParams{
		LeaseUntil: nullTimeNow(), ID: afterID, Limit: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: list recoverable extraction jobs: %w", err)
	}
	out := make([]recoverableJob, 0, len(rows))
	for _, row := range rows {
		out = append(out, recoverableJob{
			ID: row.ID, DocumentID: row.DocumentID,
			DocumentVersion: int64(row.DocumentVersion), Epoch: int(row.Epoch),
			State: row.State, InitializationComplete: row.InitializationComplete,
		})
	}
	return out, nil
}

// ReconcileResult 是一次恢复扫描的结果，供日志与运维观察。
//
// ⚠️ JobsNeedingRecovery 与 JobsRequeued 是两个数，不是一个：入队可能失败
// （Redis 不可用）。两者不等就是"有作业没能被叫醒"，而那正是要看见的事——
// 合并成一个数会让这种情况变得不可观测。
type ReconcileResult struct {
	JobsNeedingRecovery  int
	JobsRequeued         int
	ReservationsResolved int
	AttemptsArchived     int
	StaleJobsCleaned     int
	// JobIDsNeedingRecovery 交给 Service 去入队。
	// ⚠️ repository 不该知道 asynq 的存在（分层），所以这里只把 ID 带出去。
	JobIDsNeedingRecovery []string
}

// reconcileRelationExtractions 跑一轮恢复。
//
// ⭐ 两件事必须在**同一个周期任务**里：捡回无人认领的作业，以及把停留过久
// 的预留改判 unknown。分开的话，一个再也不会被接手的作业上的孤儿预留会永远
// 停在 reserved，而它代表的那笔"可能花掉的钱"就永远不在账上。
//
// ⚠️ 它绝不在这里直接跑作业——恢复扫描是个短任务，在里面同步跑几百次
// 模型调用会让下一次扫描迟迟不来，而租约还在滴答。
//
// ⚠️ 入队本身不在这里做：repository 不该知道 asynq 的存在。这里只把需要
// 接手的作业 ID 带出去，由 Service.ReconcileRelationExtractions 入队。
func (r *Repository) reconcileRelationExtractions(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	fixed, err := r.reconcileStaleReservedAttempts(ctx, staleReservationThreshold, reconcileBatchSize)
	res.ReservationsResolved = fixed
	if err != nil {
		return res, err
	}
	cleaned, err := r.cleanupStaleExtractionJobs(ctx, reconcileBatchSize)
	res.StaleJobsCleaned = cleaned
	if err != nil {
		return res, err
	}
	archived, err := r.archiveExpiredExtractionAttempts(ctx, time.Now().UTC().Add(-extractionAttemptRetention), reconcileBatchSize)
	res.AttemptsArchived = archived
	if err != nil {
		return res, err
	}

	// ⚠️ 逐条走 Debug 而不是 Info：扫描不夺租约，同一条作业在被真正接手
	// 之前每一轮都会被扫到。按分钟级的周期打 Info，一条卡住的作业就能把
	// 日志刷成噪音，真正的新问题反而看不见。总数由调用方打一条 Info。
	n, err := r.walkRecoverableJobs(ctx, reconcileBatchSize, func(job recoverableJob) {
		slog.Debug("knowledge: extraction job needs recovery",
			"job_id", job.ID, "state", job.State,
			"initialized", job.InitializationComplete)
		res.JobIDsNeedingRecovery = append(res.JobIDsNeedingRecovery, job.ID)
	})
	res.JobsNeedingRecovery = n
	return res, err
}

// cleanupStaleExtractionJobs 删除不再属于当前文档版本的派生关系数据。attempt
// 不在这里删：它们仍要保留到审计期届满，由 archiveExpiredExtractionAttempts
// 单独归档，不能因为关系结果失效就把已发生的调用成本抹掉。
func (r *Repository) cleanupStaleExtractionJobs(ctx context.Context, limit int) (int, error) {
	ids, err := r.queries.ListStaleRelationExtractionJobs(ctx, int32(limit))
	if err != nil {
		return 0, fmt.Errorf("knowledge: list stale extraction jobs: %w", err)
	}
	for _, id := range ids {
		if err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
			q := r.queries.WithTx(tx)
			if err := q.DeleteJobRelationEvidence(ctx, id); err != nil {
				return err
			}
			if err := q.DeleteJobRelations(ctx, id); err != nil {
				return err
			}
			if err := q.DeleteJobAliases(ctx, id); err != nil {
				return err
			}
			if err := q.DeleteJobCharacters(ctx, id); err != nil {
				return err
			}
			if err := q.DeleteJobItems(ctx, id); err != nil {
				return err
			}
			// 只有 parent 已删且所有 attempt 已过审计期时才删 job 本身。
			// superseded 的旧 run 仍是有效文档的历史成本，不会命中这条删除。
			_, err := q.DeleteExtractionJobIfDocumentMissingAndNoAttempts(ctx, id)
			return err
		}); err != nil {
			return 0, fmt.Errorf("knowledge: clean stale extraction job %s: %w", id, err)
		}
	}
	return len(ids), nil
}

// archiveExpiredExtractionAttempts 先把未被 job 汇总覆盖的 usage-unknown 数
// 累加到 archived_ledger_summary，再删 attempt。两步在同一事务中，事务回滚
// 时两者一起回滚，重试不会把同一条 attempt 算两遍。
func (r *Repository) archiveExpiredExtractionAttempts(ctx context.Context, before time.Time, limit int) (int, error) {
	ids, err := r.queries.ListExpiredExtractionAttemptIDs(ctx, gen.ListExpiredExtractionAttemptIDsParams{
		CreatedAt: before, Limit: int32(limit),
	})
	if err != nil {
		return 0, fmt.Errorf("knowledge: list expired extraction attempts: %w", err)
	}
	archived := 0
	for _, id := range ids {
		err := platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
			q := r.queries.WithTx(tx)
			attempt, err := q.LockExtractionAttemptForArchive(ctx, id)
			if err != nil {
				if err == sql.ErrNoRows {
					return nil // 被另一轮清理抢先删除，安全跳过。
				}
				return err
			}
			if _, err := q.LockRelationExtractionJob(ctx, attempt.JobID); err != nil {
				return err
			}
			if !attempt.UsageKnown {
				if err := q.IncrementArchivedUnknownUsageAttempts(ctx, gen.IncrementArchivedUnknownUsageAttemptsParams{
					Delta: 1, JobID: attempt.JobID,
				}); err != nil {
					return err
				}
			}
			return q.DeleteExtractionAttempt(ctx, id)
		})
		if err != nil {
			return archived, fmt.Errorf("knowledge: archive extraction attempt %s: %w", id, err)
		}
		archived++
	}
	return archived, nil
}

// walkRecoverableJobs 按 id 游标遍历全部待接手的作业。
//
// ⚠️ 单独抽出来并把批大小做成参数，是为了让**游标推进**这件事可测：
// 用生产批大小（100）写用例，要先造出 100 条以上的作业才能让游标不推进
// 这个缺陷显形，那样的夹具太贵，结果就是这段循环实际上没人验——
// 变异测试正是这么发现它的。游标不推进的表现是每一轮都只扫同一页，
// 排在后面的作业永远等不到人接手，而扫描本身每次都"成功"。
func (r *Repository) walkRecoverableJobs(ctx context.Context, batch int, visit func(recoverableJob)) (int, error) {
	after := ""
	total := 0
	for {
		jobs, err := r.listRecoverableExtractionJobs(ctx, after, batch)
		if err != nil {
			return total, err
		}
		if len(jobs) == 0 {
			return total, nil
		}
		for _, job := range jobs {
			visit(job)
			total++
		}
		after = jobs[len(jobs)-1].ID
		if len(jobs) < batch {
			return total, nil
		}
	}
}

func nullTimeNow() sql.NullTime {
	return sql.NullTime{Time: time.Now().UTC(), Valid: true}
}
