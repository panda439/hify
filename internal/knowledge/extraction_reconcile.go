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
//
// ⚠️ JobsNeedingRecovery 是「扫出来多少条需要有人接手」，**不是**「重新
// 入队了多少条」。这两个数在 Phase 3 的入队接上之前不相等，而且现在恒等于
// "全都没入队"。名字必须说实话：一个叫 JobsRequeued 的字段配上一条只写
// slog 的实现，会让运维看着非零的计数以为作业已经在跑了。
type ReconcileResult struct {
	JobsNeedingRecovery  int
	ReservationsResolved int
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
// 🚧 未完成（Phase 3）：扫出来的作业目前**只记账，没有重新入队**。
// 抽取的 asynq 任务类型还不存在（唯一的入口 validateUploadOptions 仍然
// 用 ErrRelationExtractionUnavailable 硬拒，所以现在一条作业都建不出来）。
// 接上入队之前，这个扫描对「崩掉的 worker 留下的作业」是无效的：它会每轮
// 扫到、每轮记一笔、然后什么都不发生。等 task type 落地后，在 Service 层
// （不是这里——repository 不该知道 asynq 的存在）把它们 Enqueue 出去。
func (r *Repository) reconcileRelationExtractions(ctx context.Context) (ReconcileResult, error) {
	var res ReconcileResult

	fixed, err := r.reconcileStaleReservedAttempts(ctx, staleReservationThreshold, reconcileBatchSize)
	res.ReservationsResolved = fixed
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
	})
	res.JobsNeedingRecovery = n
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
