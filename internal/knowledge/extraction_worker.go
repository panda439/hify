package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/platform"
	"hify/internal/provider"
)

// extraction_worker.go 是关系抽取的**生产工作循环**（010 R6-01）。
//
// ⭐ 在它之前，所有零件都齐了却没有人把它们串起来：恢复扫描只打日志、
// 没有入队，也没有任何任务类型会去跑一个作业。initializeExtractionJob、
// phaseRunner、processItem 全都没有生产调用方——测试里的工作循环写在测试
// 自己身上，证明的是零件的行为，不是"这条链路真的会跑"。
//
// ⚠️ 少了这一层的表现，恰恰是这个功能最怕的那一种：用户开启抽取，
// 界面显示"已开启"，恢复扫描每分钟报告"已重排 N 个作业"，
// 而**一次模型调用都不会发生**，进度永远是 0/0。每一层都认为自己工作正常。
//
// 一次运行的骨架，顺序不可调换：
//  1. 抢租约（拿到 epoch）——拿不到说明别人在跑，直接退出，不是错误；
//  2. 起心跳，并在整个循环里以它为准判断自己是否还是持有者；
//  3. 没初始化过就先初始化（文档还没 ready 就原样退出，等下一轮）；
//  4. 逐个 item 处理，每个 item 的失败都单独记账，不牵连别的；
//  5. 收尾时按事实把作业停在 succeeded / failed / budget_exhausted 上。

const (
	// extractionItemBatch 是一次取多少个待处理 item。
	// ⚠️ 不能一次全取：一份长篇小说有上千个 item，而它们中间任何一刻都
	// 可能因为租约丢失而必须停手——手上攥着一千行没有任何好处。
	extractionItemBatch = 50
)

// extractionRunResult 是一次运行的结局，只用于日志与测试断言。
type extractionRunResult struct {
	Claimed    bool
	Processed  int
	Succeeded  int
	Failed     int
	FinalState string
}

// RunRelationExtraction 跑一个抽取作业直到跑完、停下或失去租约。
//
// ⚠️ 返回 nil 不代表作业完成——抢不到租约、文档还没就绪、租约中途丢失
// 都是**正常结局**，由下一轮恢复扫描接手。返回 error 只表示这次运行
// 遇到了需要人看的问题。
func (s *service) RunRelationExtraction(ctx context.Context, jobID string) error {
	res, err := s.runExtractionJob(ctx, jobID)
	slog.Info("knowledge: extraction run finished",
		"job_id", jobID, "claimed", res.Claimed, "processed", res.Processed,
		"succeeded", res.Succeeded, "failed", res.Failed, "final_state", res.FinalState,
		"err", err)
	return err
}

func (s *service) runExtractionJob(ctx context.Context, jobID string) (extractionRunResult, error) {
	var res extractionRunResult

	job, err := s.repo.queries.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 作业被清理掉了。不是错误：没有什么可跑的。
			return res, nil
		}
		return res, fmt.Errorf("knowledge: load extraction job: %w", err)
	}
	switch job.State {
	case jobStatePending, jobStateInitializing, jobStateRunning:
	default:
		// ⭐ paused / budget_exhausted 是**用户或预算做出的决定**，
		// succeeded / failed / superseded 已经结束。都不该被自动跑起来。
		return res, nil
	}

	epoch, ok, err := s.repo.claimExtractionJob(ctx, jobID, extractionLeaseTTL)
	if err != nil {
		return res, fmt.Errorf("knowledge: claim extraction job: %w", err)
	}
	if !ok {
		// 别人正持有租约。⚠️ 这不是错误，也不该重试——重试只会
		// 让两个 worker 轮流抢同一个作业。
		return res, nil
	}
	res.Claimed = true

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	keeper := startLeaseKeeper(runCtx, s.repo, jobID, epoch,
		extractionLeaseTTL, extractionHeartbeatInterval)
	defer keeper.Stop()
	go func() {
		// ⚠️ 租约一丢就取消工作 context：正在进行的那次模型调用必须停手。
		// 不取消的话，一个已经出局的 worker 会把整个 item 跑完再发现自己
		// 出局了——那次调用的钱白花，而且它可能与新 worker 的调用重复。
		select {
		case <-keeper.Lost():
			cancel()
		case <-runCtx.Done():
		}
	}()
	defer func() {
		// 用独立 context 释放：runCtx 这会儿多半已经取消了。
		relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer relCancel()
		if err := s.repo.releaseExtractionLease(relCtx, jobID, epoch); err != nil {
			slog.Warn("knowledge: release extraction lease failed", "err", err, "job_id", jobID)
		}
	}()

	if !job.InitializationComplete {
		ready, err := s.initializeClaimedJob(runCtx, job, epoch)
		if err != nil {
			return res, err
		}
		if !ready {
			// 文档还没解析完。⭐ **退回 pending**，下一轮再来。
			// ⚠️ 留在 initializing 上，状态接口会一直显示"正在初始化"，
			// 用户以为卡住了；而真实情况是文档还在解析队列里排队。
			if _, rerr := s.repo.queries.ReturnJobToPending(runCtx,
				gen.ReturnJobToPendingParams{ID: jobID, Epoch: int32(epoch)}); rerr != nil {
				slog.Warn("knowledge: return job to pending failed", "err", rerr, "job_id", jobID)
			}
			res.FinalState = jobStatePending
			return res, nil
		}
		job, err = s.repo.queries.GetRelationExtractionJob(runCtx, jobID)
		if err != nil {
			return res, fmt.Errorf("knowledge: reload job after init: %w", err)
		}
	}

	pipeline := extractionPipeline{
		repo:   s.repo,
		runner: newPhaseRunner(s.repo),
		call:   s.newExtractionCaller(job.ModelID),
	}

	res, err = s.processJobItems(runCtx, job, epoch, pipeline, keeper, res)
	return res, err
}

// initializeClaimedJob 给一个已经存在的 pending 意图补齐 items。
//
// 返回 ready=false 表示文档还没到可以初始化的状态——**不是错误**。
func (s *service) initializeClaimedJob(ctx context.Context, job gen.GetRelationExtractionJobRow, epoch int) (bool, error) {
	doc, err := s.repo.queries.GetDocumentExtractionState(ctx, job.DocumentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("knowledge: load document for init: %w", err)
	}
	if doc.Status != StatusReady {
		return false, nil
	}
	if !doc.IsRelationExtractionEnabled {
		// 用户在这期间关掉了开关。
		return false, nil
	}

	spec := newExtractionJobSpec(job.ID, job.DocumentID, doc.Version, job.ModelID)
	spec.KnowledgeBaseID = job.KnowledgeBaseID
	spec.RunNumber = int(job.RunNumber)
	// ⭐ 作业行已经存在（enable/upload 登记的意图），只补 items 与完成标志。
	// ⚠️ 走 initializeExtractionJob 的建行分支会撞主键，而那条路径**从来
	// 没有生产调用方**——它本来就是给"从零开一个 run"设计的。
	spec.JobAlreadyExists = true
	if _, err := s.repo.initializeExtractionJob(ctx, spec); err != nil {
		if errors.Is(err, ErrExtractionSourceChanged) || errors.Is(err, ErrEmptyContent) ||
			errors.Is(err, ErrExtractionTooManyItems) ||
			errors.Is(err, ErrExtractionChunkMetadataMissing) {
			// ⭐ 这几种是**这份文档就是跑不了**，不是暂时性故障。
			// ⚠️ 留在 pending 会让恢复扫描每分钟重试一次，永远失败，
			// 而用户界面上只显示"等待中"。停成 failed 并记下原因。
			s.stopJob(ctx, job.ID, epoch, jobStateFailed, extractionStopReason(err))
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// processJobItems 是主循环。
func (s *service) processJobItems(
	ctx context.Context, job gen.GetRelationExtractionJobRow, epoch int,
	pipeline extractionPipeline, keeper *leaseKeeper, res extractionRunResult,
) (extractionRunResult, error) {
	consecutiveFailures := 0
	cursor := int32(-1)

	for {
		if !keeper.Valid() {
			// 出局：不写任何状态。⚠️ 这里去写 failed 会把一个正在被
			// 新 worker 正常处理的作业按死。
			res.FinalState = "lease_lost"
			return res, nil
		}
		items, err := s.repo.queries.ListPendingExtractionItems(ctx,
			gen.ListPendingExtractionItemsParams{
				JobID: job.ID, ChunkIndex: cursor, Limit: extractionItemBatch,
			})
		if err != nil {
			return res, fmt.Errorf("knowledge: list pending extraction items: %w", err)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			if !keeper.Valid() || ctx.Err() != nil {
				res.FinalState = "lease_lost"
				return res, nil
			}
			cursor = item.ChunkIndex

			err := s.processOneItem(ctx, job, epoch, pipeline, item)
			res.Processed++
			switch {
			case err == nil:
				res.Succeeded++
				consecutiveFailures = 0
			case errors.Is(err, ErrExtractionCallBudgetExhausted),
				errors.Is(err, ErrExtractionActiveTimeExhausted):
				// ⭐ 额度用完**不是** item 失败：不记 failed_items，
				// 因为这个 item 一条都没跑，用户追加额度后它还要跑。
				s.stopJob(ctx, job.ID, epoch, jobStateBudgetExhausted, "budget_exhausted")
				res.FinalState = jobStateBudgetExhausted
				return res, nil
			case errors.Is(err, ErrExtractionSourceChanged):
				// 文档在跑的中途改版了。停手，交给新一轮。
				res.FinalState = "source_changed"
				return res, nil
			default:
				res.Failed++
				consecutiveFailures++
				slog.Warn("knowledge: extraction item failed",
					"job_id", job.ID, "item_id", item.ID, "err", err)
				if merr := s.repo.markItemFailed(ctx, job.ID, item.ID, epoch,
					extractionStopReason(err)); merr != nil {
					return res, merr
				}
				if consecutiveFailures >= maxConsecutiveItemFailures {
					// ⚠️ 连续失败到上限就停手。不停的话，一个配置问题
					// （模型不存在、prompt 与解析器对不上）会把整本书
					// 一个 item 一个 item 地烧完额度，而每一条都失败。
					s.stopJob(ctx, job.ID, epoch, jobStateFailed, "too_many_consecutive_failures")
					res.FinalState = jobStateFailed
					return res, nil
				}
			}
		}
	}

	// 跑完了。⚠️ 以数据库里的计数为准判断"是不是真的跑完了"，
	// 不用本次循环处理了多少——上一轮已经处理过的 item 不在本次循环里。
	fresh, err := s.repo.queries.GetRelationExtractionJob(ctx, job.ID)
	if err != nil {
		return res, fmt.Errorf("knowledge: reload job for finish: %w", err)
	}
	if fresh.SucceededItems+fresh.FailedItems >= fresh.TotalItems && fresh.TotalItems > 0 {
		final := jobStateSucceeded
		if fresh.SucceededItems == 0 {
			// 一个都没成功：这不是"完成"。
			final = jobStateFailed
		}
		s.stopJob(ctx, job.ID, epoch, final, "")
		res.FinalState = final
	}
	return res, nil
}

func (s *service) processOneItem(
	ctx context.Context, job gen.GetRelationExtractionJobRow, epoch int,
	pipeline extractionPipeline, item gen.ListPendingExtractionItemsRow,
) error {
	rows, err := s.repo.pgQueries.GetPublishedNarrativeChunksByIDs(ctx,
		pggen.GetPublishedNarrativeChunksByIDsParams{
			DocumentID: job.DocumentID, DocumentVersion: int64(job.DocumentVersion),
			Column3: []string{item.ChunkID},
		})
	if err != nil {
		return fmt.Errorf("knowledge: load chunk for item %s: %w", item.ID, err)
	}
	if len(rows) == 0 {
		// ⭐ 片段不在当前已发布版本里 = 来源变了，整轮停手。
		// ⚠️ 当成这个 item 失败会让作业带着一批指向旧版本的结果继续跑完，
		// 而那些结果的引用全都对不上现在的文档。
		return ErrExtractionSourceChanged
	}
	meta, err := decodeNarrativeMetadata(rows[0].NarrativeMetadata)
	if err != nil {
		return fmt.Errorf("knowledge: chunk %s metadata: %w", item.ChunkID, err)
	}
	if meta == nil {
		return ErrExtractionChunkMetadataMissing
	}
	return pipeline.processItem(ctx, itemInput{
		JobID: job.ID, ItemID: item.ID, Epoch: epoch, ChunkID: item.ChunkID,
		DocumentVersion: int64(job.DocumentVersion),
		Content:         rows[0].Content, Metadata: *meta,
	})
}

// newExtractionCaller 是生产的 phaseCaller：把一段文本发给作业指定的模型。
//
// ⚠️ 每次调用都现解析模型与客户端，不在运行开始时解析一次并缓存：
// 供应商配置可能在一次长跑（一本书几千次调用）中间被改掉，
// 而缓存下来的客户端会一直用着旧的密钥或地址。
func (s *service) newExtractionCaller(modelID string) phaseCaller {
	return func(ctx context.Context, phase, input string, attempt int) (provider.ChatAttemptResult, error) {
		model, err := s.providerSvc.GetModel(ctx, modelID)
		if err != nil {
			return provider.ChatAttemptResult{}, fmt.Errorf("knowledge: extraction model %s: %w", modelID, err)
		}
		client, err := s.providerSvc.ResolveClient(ctx, model.ProviderID)
		if err != nil {
			return provider.ChatAttemptResult{}, fmt.Errorf("knowledge: extraction client: %w", err)
		}
		once, ok := client.(provider.SingleAttemptChatter)
		if !ok {
			// ⚠️ 明确报错而不是退回 Chat：只有 ChatOnce 保证"**恰好一次**
			// HTTP 请求"，而账目的全部可信度就建立在这条保证上。
			return provider.ChatAttemptResult{}, fmt.Errorf(
				"knowledge: provider client for model %s does not support single-attempt calls", modelID)
		}
		temp := 0.0
		return once.ChatOnce(ctx, provider.ChatRequest{
			Model:    model.ModelName,
			Messages: []provider.Message{{Role: provider.RoleUser, Content: input}},
			// ⭐ 温度固定 0：抽取要的是可复现，不是多样性（宪法第 V 条）。
			Temperature: &temp,
			MaxTokens:   maxOutputTokens,
		}, extractionCallTimeout)
	}
}

// stopJob 把作业停在一个终态上。失败只记日志：收尾写不进去不该盖掉
// 真正的原因，而下一轮恢复扫描会重新处理。
func (s *service) stopJob(ctx context.Context, jobID string, epoch int, state, reason string) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ctx
	if err := s.repo.setJobFinalState(stopCtx, jobID, epoch, state, reason); err != nil {
		slog.Error("knowledge: set extraction job final state failed",
			"err", err, "job_id", jobID, "state", state)
	}
}

// extractionStopReason 把错误压成一个短标识，落在 stop_reason /
// last_error_code 上。⚠️ 不落完整错误串：那里面可能有正文片段。
func extractionStopReason(err error) string {
	switch {
	case errors.Is(err, ErrExtractionResponseInvalid):
		return "response_invalid"
	case errors.Is(err, ErrExtractionInputTooLarge):
		return "input_too_large"
	case errors.Is(err, ErrExtractionSourceChanged):
		return "source_changed"
	case errors.Is(err, ErrExtractionChunkMetadataMissing):
		return "chunk_metadata_missing"
	case errors.Is(err, ErrExtractionTooManyItems):
		return "too_many_chunks"
	case errors.Is(err, ErrEmptyContent):
		return "empty_content"
	case errors.Is(err, ErrExtractionCallBudgetExhausted):
		return "budget_exhausted"
	case err == nil:
		return ""
	default:
		return "item_failed"
	}
}

// setJobFinalState 把作业停在终态上，按 epoch 守卫。
func (r *Repository) setJobFinalState(ctx context.Context, jobID string, epoch int, state, reason string) error {
	n, err := r.queries.SetJobFinalState(ctx, gen.SetJobFinalStateParams{
		State:      state,
		StopReason: sql.NullString{String: reason, Valid: reason != ""},
		ID:         jobID, Epoch: int32(epoch),
	})
	if err != nil {
		return fmt.Errorf("knowledge: set job final state: %w", err)
	}
	if n == 0 {
		// 已经不是持有者了，或者作业已经被别人停掉。不是错误。
		slog.Info("knowledge: final state not applied (no longer the owner)",
			"job_id", jobID, "state", state)
	}
	return nil
}

// markItemFailed 把 item 记成失败并累加 failed_items，同一个事务。
//
// ⚠️ 分两次提交的话，中间崩溃会留下一个 failed 的 item 而计数没加，
// succeeded+failed 永远凑不满 total——作业永远不会被判定为跑完。
func (r *Repository) markItemFailed(ctx context.Context, jobID, itemID string, epoch int, code string) error {
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		n, err := q.MarkItemFailed(ctx, gen.MarkItemFailedParams{
			LastErrorCode: sql.NullString{String: code, Valid: code != ""},
			ID:            itemID, JobID: jobID,
		})
		if err != nil {
			return fmt.Errorf("knowledge: mark item failed: %w", err)
		}
		if n == 0 {
			// 已经是终态了（另一个 worker 处理过）。不重复累加。
			return nil
		}
		if _, err := q.BumpJobItemOutcome(ctx, gen.BumpJobItemOutcomeParams{
			SucceededItems: 0, FailedItems: 1, ID: jobID, Epoch: int32(epoch),
		}); err != nil {
			return fmt.Errorf("knowledge: bump failed item count: %w", err)
		}
		return nil
	})
}
