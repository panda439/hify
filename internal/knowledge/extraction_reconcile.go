package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"hify/internal/db/gen"
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
type ReconcileResult struct {
	JobsRequeued         int
	ReservationsResolved int
}

// reconcileRelationExtractions 跑一轮恢复。
//
// ⭐ 两件事必须在**同一个周期任务**里：捡回无人认领的作业，以及把停留过久
// 的预留改判 unknown。分开的话，一个再也不会被接手的作业上的孤儿预留会永远
// 停在 reserved，而它代表的那笔"可能花掉的钱"就永远不在账上。
//
// ⚠️ 它**只把作业重新丢进队列**，不在这里直接跑——恢复扫描是个短任务，
// 在里面同步跑几百次模型调用会让下一次扫描迟迟不来，而租约还在滴答。
func (r *Repository) reconcileRelationExtractions(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	fixed, err := r.reconcileStaleReservedAttempts(ctx, staleReservationThreshold, reconcileBatchSize)
	res.ReservationsResolved = fixed
	if err != nil {
		return res, err
	}

	n, err := r.walkRecoverableJobs(ctx, reconcileBatchSize, func(job recoverableJob) {
		// ⚠️ 只在这里记账与计数；真正的接手由调用方（Service）负责入队，
		// repository 不该知道 asynq 的存在。
		slog.Info("knowledge: extraction job needs recovery",
			"job_id", job.ID, "state", job.State,
			"initialized", job.InitializationComplete)
	})
	res.JobsRequeued = n
	return res, err
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
