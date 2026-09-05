package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"hify/internal/db/gen"
)

// extraction_lease.go 管作业的**归属权**（010 T016）。
//
// ⭐ 它防的是整个 Phase 3 里最贵的一种事故：租约过期被别的 worker 抢走，
// 而旧 worker 不知道，继续把同一批 chunk 送进模型。钱花两遍、账只记一遍，
// 而且**两个 worker 都工作正常**——没有报错、没有异常日志，只有账单不对。
//
// ⚠️ epoch 只约束**数据发布**。旧 worker 那次外部调用该花的钱已经花了，
// attempt 照记。把 epoch 当成"所以那次调用不算数"就是在系统性低估成本。

const (
	// extractionLeaseTTL 是一次租约的长度。
	// ⚠️ 它必须显著大于一次模型调用的上限（ChatOnce 默认 60s，硬上限 120s），
	// 否则一次正常的慢调用就会让自己的租约过期、被别人抢走，而那次调用
	// 已经发出去了——钱花了、结果被丢弃。
	extractionLeaseTTL = 180 * time.Second

	// extractionHeartbeatInterval 是续租频率。取 TTL 的 1/6：
	// 允许连续丢 5 次心跳（网络抖动、数据库瞬时不可用）仍不丢租约。
	extractionHeartbeatInterval = 30 * time.Second
)

// claimExtractionJob 抢占作业：epoch +1 并写下租约。
//
// 返回 ok=false 表示租约还在别人手里，**不是错误**——恢复扫描每分钟都会
// 撞上一堆正常持有中的作业，那是预期情形，不该产生错误日志。
func (r *Repository) claimExtractionJob(ctx context.Context, jobID string, ttl time.Duration) (int, bool, error) {
	now := time.Now().UTC()
	n, err := r.queries.ClaimRelationExtractionJob(ctx, gen.ClaimRelationExtractionJobParams{
		LeaseUntil:   sql.NullTime{Time: now.Add(ttl), Valid: true},
		HeartbeatAt:  sql.NullTime{Time: now, Valid: true},
		ID:           jobID,
		LeaseUntil_2: sql.NullTime{Time: now, Valid: true},
	})
	if err != nil {
		return 0, false, fmt.Errorf("knowledge: claim extraction job: %w", err)
	}
	if n == 0 {
		return 0, false, nil
	}
	job, err := r.queries.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		return 0, false, fmt.Errorf("knowledge: read claimed job: %w", err)
	}
	return int(job.Epoch), true, nil
}

// renewExtractionLease 续租。held=false 表示**自己已经出局**——这是持有者
// 唯一能主动发现这件事的信号，比"写数据被拒"早得多，而中间每一次模型调用
// 都是白花的钱。
func (r *Repository) renewExtractionLease(ctx context.Context, jobID string, epoch int, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	n, err := r.queries.RenewRelationExtractionLease(ctx, gen.RenewRelationExtractionLeaseParams{
		LeaseUntil:  sql.NullTime{Time: now.Add(ttl), Valid: true},
		HeartbeatAt: sql.NullTime{Time: now, Valid: true},
		ID:          jobID, Epoch: int32(epoch),
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: renew extraction lease: %w", err)
	}
	return n > 0, nil
}

func (r *Repository) releaseExtractionLease(ctx context.Context, jobID string, epoch int) error {
	if _, err := r.queries.ReleaseRelationExtractionLease(ctx, gen.ReleaseRelationExtractionLeaseParams{
		ID: jobID, Epoch: int32(epoch),
	}); err != nil {
		return fmt.Errorf("knowledge: release extraction lease: %w", err)
	}
	return nil
}

// jobIsStillCurrent 检查这个作业**仍然是文档当前认可的那一个**。
//
// ⭐ 只看 epoch 是不够的：restart 会建一个新 job 并把文档指针改过去，
// 而旧 job 的 epoch **一个字都没变**——旧 worker 会心安理得地跑完，
// 把一批属于旧版本的关系发布出去，全程零报错。
func (r *Repository) jobIsStillCurrent(ctx context.Context, jobID string) (bool, error) {
	job, err := r.queries.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("knowledge: load job: %w", err)
	}
	doc, err := r.queries.GetDocumentExtractionState(ctx, job.DocumentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("knowledge: load document: %w", err)
	}
	if doc.Status != StatusReady || doc.Version != int64(job.DocumentVersion) {
		return false, nil
	}
	if !doc.ActiveRelationJobID.Valid || doc.ActiveRelationJobID.String != jobID {
		return false, nil
	}
	return true, nil
}

// leaseOwner 是 leaseKeeper 需要的全部数据库能力。
//
// ⚠️ 抽成接口不是为了"可替换实现"——只有 *Repository 会实现它。
// 是为了让两条**只在故障时才走到**的路径可测：续租连续失败的容忍，
// 以及 Stop 必须等到正在进行的那次续租返回。这两条都无法靠真实数据库
// 制造（要么要杀连接、要么要卡住一次查询），而它们恰好是这段代码
// 最容易写错、且错了完全没有症状的地方。
type leaseOwner interface {
	renewExtractionLease(ctx context.Context, jobID string, epoch int, ttl time.Duration) (bool, error)
	jobIsStillCurrent(ctx context.Context, jobID string) (bool, error)
}

// leaseKeeper 在后台续租，并在自己出局时关闭 Lost()。
//
// ⚠️ 它**不做任何补救**——发现出局只负责报告。补救（停止调用、收尾）是工作
// 循环的事，因为只有那一层知道手上那次调用处在什么阶段。
type leaseKeeper struct {
	lostOnce sync.Once
	lost     chan struct{}
	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
	valid    atomic.Bool
}

// startLeaseKeeper 启动心跳。调用方**必须**在结束时调用 Stop——
// 宪法第 17 条：每个 goroutine 都要有明确的等待者。
func startLeaseKeeper(ctx context.Context, owner leaseOwner, jobID string, epoch int, ttl, interval time.Duration) *leaseKeeper {
	k := &leaseKeeper{lost: make(chan struct{}), stop: make(chan struct{})}
	k.valid.Store(true)

	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		// 宪法第 10 条：每个独立 goroutine 入口都要有 recover。
		// ⚠️ 心跳 goroutine panic 掉而进程不死，是最坏的形态：租约会一直
		// 有效到过期，工作循环全程以为自己还是持有者。
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("knowledge: extraction lease keeper panicked",
					"job_id", jobID, "panic", rec)
				k.markLost()
			}
		}()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				k.markLost()
				return
			case <-k.stop:
				return
			case <-ticker.C:
				held, err := owner.renewExtractionLease(ctx, jobID, epoch, ttl)
				if err != nil {
					// ⚠️ 一次续租失败**不**判定出局：数据库瞬时抖动很常见，
					// 而误判出局会白白丢掉一个正在正常工作的 worker。
					// 心跳频率是 TTL 的 1/6，允许连丢 5 次仍不丢租约；
					// 真的一直失败，租约会自然过期，由别人接手。
					slog.Warn("knowledge: extraction heartbeat failed",
						"job_id", jobID, "epoch", epoch, "error", err)
					continue
				}
				if !held {
					slog.Info("knowledge: extraction lease lost", "job_id", jobID, "epoch", epoch)
					k.markLost()
					return
				}
				current, err := owner.jobIsStillCurrent(ctx, jobID)
				if err != nil {
					slog.Warn("knowledge: extraction currency check failed",
						"job_id", jobID, "error", err)
					continue
				}
				if !current {
					slog.Info("knowledge: extraction job superseded", "job_id", jobID)
					k.markLost()
					return
				}
			}
		}
	}()
	return k
}

func (k *leaseKeeper) markLost() {
	k.valid.Store(false)
	k.lostOnce.Do(func() { close(k.lost) })
}

// Lost 在本 worker 不再是作业持有者时关闭。
func (k *leaseKeeper) Lost() <-chan struct{} { return k.lost }

// Valid 是工作循环在**每次模型调用之前**必须问的那一句。
// ⚠️ 失效之后每多打一次调用，就是一笔白花的钱，且账目上找不出异常。
func (k *leaseKeeper) Valid() bool { return k.valid.Load() }

// Stop 停止心跳并**等待 goroutine 真的退出**。重复调用安全。
//
// ⚠️ 不等的后果是调用方已经收摊、心跳还在写数据库，表现为随机的
// "use of closed connection"——一个只在 CI 上偶发、看不出根因的失败。
func (k *leaseKeeper) Stop() {
	k.stopOnce.Do(func() { close(k.stop) })
	k.wg.Wait()
}
