package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/provider"
)

// extraction_runner.go 把已经建好的作业真正跑完（010 T021 的运行侧）。
//
// ⭐ 这个文件本身几乎不做决定：租约、账目、重试、预算、发布各自已经有主，
// 它只负责**按正确顺序**把它们串起来，并在每一个可能中断的地方停对地方。
// 顺序错了的表现全都不报错：
//   - 先调模型后占额度 → 预算超发;
//   - 先发布后结算 → 发布失败回滚时把一笔真花掉的钱抹掉;
//   - 不检查租约就接着跑 → 两个 worker 同时跑同一个作业，钱花两遍。
//
// ⚠️ 这里**没有**并发：一个作业同一时刻只有一个 worker，item 之间串行。
// 并行跑 item 能快几倍，但三重预算的闸门、连续失败的计数、租约的有效性
// 全都要跟着变成并发安全的判断，而它们每一个判断错了都只表现为数字不对。
// 本期不做，写在报告的"已知边界"里。

const (
	// extractionItemPageSize 是每次取多少个待处理 item。
	// ⚠️ 与 reconcileBatchSize 一样按 100 收口，别在这里放大。
	extractionItemPageSize = 100

	// maxAliasCandidatePool 是候选池的拉取上限，选取再按 32 收窄。
	// 拉一个比窗口大的池子，是为了让"精确名称命中优先"这条排序真的有得选。
	maxAliasCandidatePool = 200
)

// ErrExtractionJobNotClaimable：作业当前不能被接手（已结束、被暂停，
// 或租约还在别人手里）。⚠️ 这不是错误路径上的意外，是**正常**的并发结果，
// 调用方应当安静退出，不要重试。
var ErrExtractionJobNotClaimable = errors.New("knowledge: extraction job is not claimable")

// errModelGaveUp：一个阶段用完了它的尝试次数还是没拿到可用结果。
// ⚠️ 与"输出不合法"分开记：前者可能是模型一直在超时/限流，后者是协议对不上，
// 两者的下一步完全不同，而 last_error_code 是唯一能事后区分它们的地方。
var errModelGaveUp = errors.New("knowledge: model gave up after the attempt limit")

// singleAttemptModel 是运行器对模型的全部要求：发**恰好一次**请求。
//
// ⚠️ 故意不是整个 provider.Service：这里绝不能有机会调到带自动重试的
// Chat——重试一旦发生在这一层之下，账目就会静默失真且没有任何症状。
type singleAttemptModel interface {
	ChatOnce(ctx context.Context, modelID string, req provider.ChatRequest, timeout time.Duration) (provider.ChatAttemptResult, error)
}

type extractionRunner struct {
	repo   *Repository
	model  singleAttemptModel
	phases *phaseRunner
	// now 是时间的接缝，测试用。
	now func() time.Time
}

func newExtractionRunner(repo *Repository, model singleAttemptModel) *extractionRunner {
	return &extractionRunner{repo: repo, model: model, phases: newPhaseRunner(repo), now: time.Now}
}

// runJobResult 是一次运行的结局，供日志与运维观察。
type runJobResult struct {
	ItemsSucceeded     int
	ItemsFailed        int
	AmbiguousPositions int
	AmbiguousEvidence  int
	// StoppedEarly 说明这一轮没有把作业跑完：租约丢了、预算耗尽、
	// 连续失败过多，或者 ctx 被取消。⚠️ 它与"作业失败"不是一回事，
	// 前三种里有两种是可以从上次的位置接着跑的。
	StoppedEarly bool
	StopReason   string
}

// runJob 接手一个作业并尽量把它跑完。
//
// ⭐ 每个 item 之前都要重新确认两件事：租约还在自己手上，作业还是文档的
// 当前作业。少了任何一个，一个被取代的 worker 会继续花钱，并把结果发布到
// 一个已经没人看的 run 上。
func (r *extractionRunner) runJob(ctx context.Context, jobID string) (runJobResult, error) {
	var res runJobResult

	epoch, ok, err := r.repo.claimExtractionJob(ctx, jobID, extractionLeaseTTL)
	if err != nil {
		return res, err
	}
	if !ok {
		return res, ErrExtractionJobNotClaimable
	}
	keeper := startLeaseKeeper(ctx, r.repo, jobID, epoch, extractionLeaseTTL, extractionHeartbeatInterval)
	defer keeper.Stop()
	defer func() {
		// ⚠️ 释放租约用的是 context.WithoutCancel：ctx 已经被取消时
		// （关机、超时）仍然要把租约放掉，否则这个作业要等整个 TTL
		// 过去才能被别人接手。
		_ = r.repo.releaseExtractionLease(context.WithoutCancel(ctx), jobID, epoch)
	}()

	job, err := r.loadJob(ctx, jobID)
	if err != nil {
		return res, err
	}

	consecutiveFailures := 0
	afterIndex := int32(-1)
	for {
		if err := ctx.Err(); err != nil {
			res.StoppedEarly, res.StopReason = true, "canceled"
			return res, err
		}
		if !keeper.Valid() {
			res.StoppedEarly, res.StopReason = true, "lease_lost"
			return res, nil
		}

		items, err := r.repo.queries.ListOpenExtractionItems(ctx, gen.ListOpenExtractionItemsParams{
			JobID: jobID, ChunkIndex: afterIndex, Limit: extractionItemPageSize,
		})
		if err != nil {
			return res, fmt.Errorf("knowledge: list open items: %w", err)
		}
		if len(items) == 0 {
			// 没有待处理的 item 了：这一轮把作业跑完了。
			// ⭐ 全部 item 都失败时标 failed 而不是 succeeded：一个
			// "成功完成、产出为零"的作业在列表里和真正抽完的作业长得
			// 一模一样，而它其实什么都没抽到。
			state, reason := jobStateSucceeded, "completed"
			if res.ItemsSucceeded == 0 && res.ItemsFailed > 0 {
				state, reason = jobStateFailed, "all_items_failed"
			}
			if err := r.repo.stopExtractionJob(ctx, jobID, epoch, state, reason, true); err != nil {
				return res, err
			}
			return res, nil
		}

		chunks, err := r.loadChunks(ctx, job, items)
		if err != nil {
			return res, err
		}

		for _, item := range items {
			if err := ctx.Err(); err != nil {
				res.StoppedEarly, res.StopReason = true, "canceled"
				return res, err
			}
			if !keeper.Valid() {
				// ⭐ 在**开始下一次调用之前**检查，不是调用之后：
				// 租约已经丢了还发出去的那次调用，钱一样要花。
				res.StoppedEarly, res.StopReason = true, "lease_lost"
				return res, nil
			}
			current, err := r.repo.jobIsStillCurrent(ctx, jobID)
			if err != nil {
				return res, err
			}
			if !current {
				res.StoppedEarly, res.StopReason = true, "superseded"
				return res, nil
			}

			chunk, ok := chunks[item.ChunkID]
			if !ok {
				// 片段不见了或已经不是当前发布版本：这个 item 失败，
				// ⚠️ 但**不停整个作业**——单个片段消失是数据问题，
				// 其余片段仍然有效。
				if err := r.failItem(ctx, jobID, item.ID, epoch, "chunk_missing"); err != nil {
					return res, err
				}
				res.ItemsFailed++
				consecutiveFailures++
			} else {
				itemResult, itemErr := r.runItem(ctx, job, epoch, item, chunk)
				res.AmbiguousPositions += itemResult.AmbiguousPositions
				res.AmbiguousEvidence += itemResult.AmbiguousEvidence
				err = itemErr
				switch {
				case err == nil:
					res.ItemsSucceeded++
					consecutiveFailures = 0
				case errors.Is(err, ErrExtractionCallBudgetExhausted),
					errors.Is(err, ErrExtractionActiveTimeExhausted):
					// ⚠️ 预算耗尽不是这个 item 的失败：item 保持原状，
					// 追加额度之后从这里接着跑。把它记成失败会让"因为没钱
					// 停下"看起来像"这段书抽不出东西"。
					// ⭐ 停成 paused 而不是留在 running：留着的话恢复扫描
					// 每分钟都会把它捡回来，而每次都在同一处因为同样的
					// 理由停下——一个不花钱但永不停歇的循环。
					// paused 需要用户显式追加额度后 resume（T030）。
					res.StoppedEarly, res.StopReason = true, "budget_exhausted"
					if serr := r.repo.stopExtractionJob(ctx, jobID, epoch,
						jobStatePaused, res.StopReason, false); serr != nil {
						return res, serr
					}
					return res, err
				case errors.Is(err, ErrExtractionEpochLost):
					res.StoppedEarly, res.StopReason = true, "lease_lost"
					return res, nil
				case errors.Is(err, ErrItemAlreadyPublished):
					// 重复投递、提交后丢 ACK：这是**正常**的重放，当成功处理。
					res.ItemsSucceeded++
					consecutiveFailures = 0
				case ctx.Err() != nil:
					res.StoppedEarly, res.StopReason = true, "canceled"
					return res, err
				default:
					code, classified := itemFailureCode(err)
					if !classified {
						// ⚠️ 认不出来的错误几乎都是基础设施问题（数据库、
						// 编码），不是这段书的问题。把它记成 item 失败会
						// 让整整 500 个 item 一个接一个"失败"，最后得到一份
						// 看起来像"模型抽不出东西"的报告。停下来让人看。
						return res, err
					}
					slog.Warn("knowledge: extraction item failed",
						"job_id", jobID, "item_id", item.ID, "code", code, "err", err)
					if ferr := r.failItem(ctx, jobID, item.ID, epoch, code); ferr != nil {
						return res, ferr
					}
					res.ItemsFailed++
					consecutiveFailures++
				}
			}

			if shouldStopAfterConsecutiveFailures(consecutiveFailures) {
				// ⭐ 连续失败几乎一定是系统性问题（prompt 与 schema 对不上、
				// 模型被换掉、语料格式不对）。继续跑只是拿剩下几百个 item
				// 把预算烧完，换回一堆同样的失败。
				res.StoppedEarly, res.StopReason = true, "consecutive_failures"
				// 同样停成 paused：系统性问题要人来看，不该被恢复扫描
				// 每分钟自动重启一次。
				if serr := r.repo.stopExtractionJob(ctx, jobID, epoch,
					jobStatePaused, res.StopReason, false); serr != nil {
					return res, serr
				}
				return res, nil
			}
			afterIndex = item.ChunkIndex
		}
	}
}

// runItem 处理一个片段：抽取 → （必要时）归一 → 同一事务发布。
type runItemResult struct {
	AmbiguousPositions int
	AmbiguousEvidence  int
}

func (r *extractionRunner) runItem(ctx context.Context, job RelationExtractionJob, epoch int, item gen.ListOpenExtractionItemsRow, chunk extractionChunkView) (runItemResult, error) {
	if _, err := r.repo.queries.MarkItemRunning(ctx, gen.MarkItemRunningParams{
		ID: item.ID, JobID: job.ID,
	}); err != nil {
		return runItemResult{}, fmt.Errorf("knowledge: mark item running: %w", err)
	}

	instruction := buildExtractInstruction()
	rendered, err := fitExtractionInput(instruction, chunk.Content)
	if err != nil {
		return runItemResult{}, err // 输入超限：这个 item 失败，绝不截正文。
	}

	extractRaw, err := r.runPhaseWithValidation(ctx, job, epoch, item.ID, phaseExtract, rendered,
		func(raw []byte) error {
			_, perr := parseExtractionResponse(raw)
			return perr
		})
	if err != nil {
		return runItemResult{}, err
	}
	resp, err := parseExtractionResponse(extractRaw)
	if err != nil {
		return runItemResult{}, err
	}
	resolved, err := resolveExtraction(chunk, resp)
	if err != nil {
		return runItemResult{}, err
	}

	in := aliasInput{Chunk: chunk, Mentions: resolved.Mentions, Proposals: resolved.AliasProposals}
	surfaces := make([]string, 0, len(resolved.Mentions))
	for _, m := range resolved.Mentions {
		surfaces = append(surfaces, m.Surface)
	}
	pool, err := r.loadCandidates(ctx, job.ID)
	if err != nil {
		return runItemResult{}, err
	}
	in.Candidates, in.CandidateTruncated = selectAliasCandidates(pool, surfaces)

	var aliasRaw []byte
	var assign identityAssignment
	if !needsAliasPhase(in) {
		// 零调用路径：没有候选也没有提案，各 mention 独立成身份。
		assign = independentIdentities(in)
	} else {
		instruction := buildAliasInstruction(in)
		candidates := renderAliasCandidates(in.Candidates)
		renderedAlias, dropped, err := fitAliasInput(instruction, chunk.Content, candidates)
		if err != nil {
			return runItemResult{}, err
		}
		in.CandidateTruncated += dropped
		if dropped > 0 {
			// ⚠️ 必须留痕：一次因为候选被删而没能归一的结果，
			// 不标出来就会被读成模型判断失误。
			slog.Info("knowledge: alias candidates truncated",
				"job_id", job.ID, "item_id", item.ID, "dropped", in.CandidateTruncated)
			in.Candidates = in.Candidates[:len(in.Candidates)-dropped]
		}
		aliasRaw, err = r.runPhaseWithValidation(ctx, job, epoch, item.ID, phaseAlias, renderedAlias,
			func(raw []byte) error {
				parsed, perr := parseAliasResponse(raw)
				if perr != nil {
					return perr
				}
				_, perr = resolveAliasDecisions(in, parsed)
				return perr
			})
		if err != nil {
			return runItemResult{}, err
		}
		parsed, err := parseAliasResponse(aliasRaw)
		if err != nil {
			return runItemResult{}, err
		}
		assign, err = resolveAliasDecisions(in, parsed)
		if err != nil {
			return runItemResult{}, err
		}
	}

	outcome, err := buildExtractionOutcome(chunk, resolved.Relations, assign)
	if err != nil {
		return runItemResult{}, err
	}
	err = r.repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item.ID, Epoch: epoch, Outcome: outcome,
		ExtractResponse: extractRaw, AliasResponse: aliasRaw,
	})
	return runItemResult{AmbiguousPositions: resolved.AmbiguousPositions, AmbiguousEvidence: resolved.AmbiguousEvidence}, err
}

// runPhaseWithValidation 跑一个阶段，把**校验失败也当成这次尝试失败**。
//
// ⭐ 校验必须发生在重试循环**里面**。放在外面的话，一次结构不合法的输出
// 会直接判这个 item 失败，而契约给的是"至多 3 次"——模型偶尔漏个字段是
// 常态，第二次通常就对了。
//
// ⚠️ 同时，这次调用**确实发生了**，所以它照常预留、照常结算、照常记账：
// 一次输出不合法的调用和一次成功的调用花的钱一样多。
func (r *extractionRunner) runPhaseWithValidation(ctx context.Context, job RelationExtractionJob, epoch int, itemID, phase, rendered string, validate func([]byte) error) ([]byte, error) {
	if replay, ok, err := r.repo.findReplayableResponse(ctx, itemID, phase); err != nil {
		return nil, err
	} else if ok {
		// ⭐ 已经有一次成功且原始响应落盘的尝试：拿它接着算，绝不再打一次。
		// 再打一次的后果不是"结果不一致"，是那笔钱白花第二遍，
		// 而账目上看起来完全正常——两次都是真实发生的调用。
		raw := []byte(replay.Body)
		if err := validate(raw); err != nil {
			// 回放的响应过不了校验（协议改了、或者当初就没过）：
			// ⚠️ 不能拿它当成功，也不该悄悄重打——重打会绕过"一个阶段
			// 至多 3 次"的上限。让这个 item 失败，重启由用户显式发起。
			return nil, err
		}
		return raw, nil
	}

	requestHash := sha256.Sum256([]byte(rendered))
	var accepted []byte
	// ⚠️ 记住最后一次校验错误：三次输出都不合法时，"模型放弃了"这个说法
	// 掩盖了真正的原因（协议对不上）。两者的下一步完全不同——前者调模型
	// 或超时设置，后者改 prompt 或 schema——而 last_error_code 是事后
	// 唯一能区分它们的地方。
	var lastValidationErr error
	res, err := r.phases.runPhase(ctx, phaseInput{
		JobID: job.ID, ItemID: itemID, Epoch: epoch, Phase: phase,
		RequestHash: requestHash[:], MaxOutputTokens: maxOutputTokens,
	}, func(ctx context.Context, attemptNumber int) (provider.ChatAttemptResult, error) {
		out, err := r.model.ChatOnce(ctx, job.ModelID, provider.ChatRequest{
			Messages:  []provider.Message{{Role: "user", Content: rendered}},
			MaxTokens: maxOutputTokens,
		}, extractionCallTimeout)
		if err != nil {
			return out, err
		}
		if out.Outcome != provider.AttemptCompleted {
			return out, nil
		}
		if verr := validate([]byte(out.Message.Content)); verr != nil {
			lastValidationErr = verr
			// ⚠️ 结果本身留着（要落进 attempt 表的 raw_response），
			// 只把结局改成失败，让重试层按自己的规则决定要不要再试。
			out.Outcome = provider.AttemptFailed
			out.ErrorCode = "invalid_output"
			slog.Debug("knowledge: extraction output rejected",
				"item_id", itemID, "phase", phase, "attempt", attemptNumber, "err", verr)
			return out, nil
		}
		accepted = []byte(out.Message.Content)
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	if accepted == nil {
		if lastValidationErr != nil {
			return nil, fmt.Errorf("%s phase: %w", phase, lastValidationErr)
		}
		return nil, fmt.Errorf("%w: %s phase, outcome=%s finish=%s code=%s",
			errModelGaveUp, phase, res.Outcome, res.FinishReason, res.ErrorCode)
	}
	return accepted, nil
}

// loadJob 读一次作业的运行期字段。
func (r *extractionRunner) loadJob(ctx context.Context, jobID string) (RelationExtractionJob, error) {
	row, err := r.repo.queries.GetRelationExtractionJob(ctx, jobID)
	if err != nil {
		return RelationExtractionJob{}, fmt.Errorf("knowledge: load extraction job: %w", err)
	}
	return RelationExtractionJob{
		ID: row.ID, DocumentID: row.DocumentID, KnowledgeBaseID: row.KnowledgeBaseID,
		DocumentVersion: int64(row.DocumentVersion), RunNumber: int(row.RunNumber),
		ModelID: row.ModelID, State: row.State, Epoch: int(row.Epoch),
		InitializationComplete: row.InitializationComplete,
		TotalItems:             int(row.TotalItems),
	}, nil
}

// loadChunks 一次把这一页 item 的片段全取回来。
// ⚠️ 逐个 item 查一次是 Phase 7 邻接查询踩过的同一个 N+1。
func (r *extractionRunner) loadChunks(ctx context.Context, job RelationExtractionJob, items []gen.ListOpenExtractionItemsRow) (map[string]extractionChunkView, error) {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ChunkID)
	}
	rows, err := r.repo.pgQueries.GetPublishedNarrativeChunksByIDs(ctx,
		pggen.GetPublishedNarrativeChunksByIDsParams{
			DocumentID: job.DocumentID, DocumentVersion: job.DocumentVersion, Column3: ids,
		})
	if err != nil {
		return nil, fmt.Errorf("knowledge: load chunks for extraction: %w", err)
	}
	out := make(map[string]extractionChunkView, len(rows))
	for _, row := range rows {
		meta, err := decodeNarrativeMetadata(row.NarrativeMetadata)
		if err != nil {
			return nil, fmt.Errorf("knowledge: chunk %s: %w", row.ID, err)
		}
		if meta == nil {
			// 初始化时校验过每一个片段都有坐标，这里再遇到 nil 说明片段被
			// 重新处理过。当成"片段不见了"处理，由调用方判这个 item 失败。
			continue
		}
		out[row.ID] = extractionChunkView{
			ChunkID: row.ID, DocumentVersion: job.DocumentVersion,
			Content: row.Content, Meta: *meta,
		}
	}
	return out, nil
}

func (r *extractionRunner) loadCandidates(ctx context.Context, jobID string) ([]aliasCandidate, error) {
	rows, err := r.repo.queries.ListJobCharacters(ctx, gen.ListJobCharactersParams{
		JobID: jobID, Limit: maxAliasCandidatePool,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: load alias candidates: %w", err)
	}
	out := make([]aliasCandidate, 0, len(rows))
	for _, row := range rows {
		cand := aliasCandidate{
			CharacterID: row.ID, DisplayName: row.DisplayName,
			FirstSourceOrder: row.FirstSourceOrder,
		}
		// ⚠️ CAST(... AS CHAR) 在 sqlc 里是 interface{}：MySQL 驱动给回
		// []byte 或者 nil。类型断言失败就当没有依据，不当错误。
		if raw, ok := row.IdentityEvidence.([]byte); ok && len(raw) > 0 {
			// 身份依据存的是归一时的 supports 数组；解析不出就当没有依据，
			// ⚠️ 不报错：一个候选少了依据只是它更难被链接（保守方向），
			// 而让整个作业因为一条历史数据格式不对而停下是过度反应。
			var supports []aliasSupport
			if err := json.Unmarshal(raw, &supports); err == nil {
				for i, s := range supports {
					if i >= maxAliasCandidateEvidence {
						break
					}
					cand.Evidence = append(cand.Evidence, aliasCandidateEvidence{
						Ref: fmt.Sprintf("%s#%d", row.ID, i), Quote: s.Quote,
					})
				}
			}
		}
		out = append(out, cand)
	}
	return out, nil
}

func (r *extractionRunner) failItem(ctx context.Context, jobID, itemID string, epoch int, code string) error {
	n, err := r.repo.queries.MarkItemFailed(ctx, gen.MarkItemFailedParams{
		LastErrorCode: nullString(code), ID: itemID, JobID: jobID,
	})
	if err != nil {
		return fmt.Errorf("knowledge: mark item failed: %w", err)
	}
	if n == 0 {
		// 已经有结局了（重放、或者被别人处理过）：不重复计数。
		return nil
	}
	if _, err := r.repo.queries.BumpJobItemOutcome(ctx, gen.BumpJobItemOutcomeParams{
		SucceededItems: 0, FailedItems: 1, ID: jobID, Epoch: int32(epoch),
	}); err != nil {
		return fmt.Errorf("knowledge: bump failed items: %w", err)
	}
	return nil
}

// itemFailureCode 把内部错误压成一个稳定的短码，存进 items.last_error_code。
// ⚠️ 不存错误全文：那里面会带上引文片段（也就是正文），而 last_error_code
// 会出现在列表接口里。
func itemFailureCode(err error) (string, bool) {
	switch {
	case errors.Is(err, errExtractionResponseInvalid):
		return "extract_invalid", true
	case errors.Is(err, errAliasResponseInvalid), errors.Is(err, errAliasMergeUnsupported):
		return "alias_invalid", true
	case errors.Is(err, errQuoteNotFound), errors.Is(err, errQuoteNotCitable):
		return "quote_unverifiable", true
	case errors.Is(err, ErrExtractionInputTooLarge):
		return "input_too_large", true
	case errors.Is(err, errModelGaveUp):
		return "model_gave_up", true
	default:
		return "", false
	}
}
