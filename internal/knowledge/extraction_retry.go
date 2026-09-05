package knowledge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"hify/internal/db/gen"
	"hify/internal/provider"
)

// extraction_retry.go 是**唯一**的重试层（010 T018）。
//
// ⭐ "唯一"是这一段最重要的性质。provider.ChatOnce 不重试，这里重试。
// 两层都重试的话，一个 item 最坏会打出 3×3 = 9 次真实请求，而账目只会
// 显示 3 次——成本数字直接错三倍，且没有任何症状。
//
// ⚠️ 计数落在数据库（items.extract_attempt_count / alias_attempt_count），
// 不在内存。放内存的表现是：worker 崩溃重启后计数归零，一个永远会失败的
// item 被无限重试，把预算烧光——而每一轮看起来都正常。

const (
	// maxAttemptsPerPhase 是单个阶段（抽取 / 归一）的尝试上限。
	maxAttemptsPerPhase = 3

	// maxConsecutiveItemFailures：连续这么多个 item 最终失败就停整个作业。
	//
	// ⭐ 连续失败几乎一定是**系统性**问题（prompt 与 schema 对不上、模型被
	// 换掉、语料格式不对），不是运气差。继续跑只是拿剩下几百个 item 把预算
	// 烧完，换回一堆同样的失败。而"跑完了但全是失败"看起来比"提前停下"
	// 更像正常结束——这正是要主动停的理由。
	maxConsecutiveItemFailures = 5
)

// retryDecision 是一次尝试之后的处置。
type retryDecision int

const (
	decisionAccept retryDecision = iota // 用这个结果
	decisionRetry                       // 值得再试
	decisionGiveUp                      // 最终失败，不再试
)

func (d retryDecision) String() string {
	switch d {
	case decisionAccept:
		return "accept"
	case decisionRetry:
		return "retry"
	default:
		return "give_up"
	}
}

// decideRetry 是纯函数：给定一次尝试的结局和它是第几次，决定下一步。
//
// ⭐ 配置类错误**永不重试**。模型不存在、密钥错、请求体不合法——再打两次
// 只是把同一个错误重复三遍，浪费预算，并把真正的原因埋在一堆重试日志里。
//
// ⚠️ 输出被截断（finish_reason == "length"）也不重试：同一个请求再打一次
// 会撞上同一堵墙，只是把预算再花一遍。要解决只能缩输入，那是上层的事。
func decideRetry(res provider.ChatAttemptResult, attemptNumber int) retryDecision {
	if res.Outcome == provider.AttemptCompleted {
		if res.FinishReason == finishReasonLength {
			return decisionGiveUp
		}
		return decisionAccept
	}
	if isConfigurationError(res.ErrorCode) {
		return decisionGiveUp
	}
	if attemptNumber >= maxAttemptsPerPhase {
		return decisionGiveUp
	}
	return decisionRetry
}

const finishReasonLength = "length"

// isConfigurationError 判断这个错误码是不是"再试也一样"。
//
// ⚠️ 429 是限流，**不**算配置错误——它恰恰是最值得退避后重试的一种。
func isConfigurationError(code string) bool {
	switch code {
	case "http_400", "http_401", "http_403", "http_404", "http_422":
		return true
	default:
		return false
	}
}

// retryBackoff 返回第 attemptNumber 次尝试**之后**的等待时长。
// 3 次尝试之间有 2 个间隔：1s、2s。
func retryBackoff(attemptNumber int) time.Duration {
	switch attemptNumber {
	case 1:
		return time.Second
	case 2:
		return 2 * time.Second
	default:
		return 0
	}
}

func shouldStopAfterConsecutiveFailures(n int) bool {
	return n >= maxConsecutiveItemFailures
}

// phaseInput 是跑一个阶段需要的全部输入。
type phaseInput struct {
	JobID           string
	ItemID          string
	Epoch           int
	Phase           string
	RequestHash     []byte
	MaxOutputTokens int
}

// attemptCaller 发出第 attemptNumber 次调用。
// ⚠️ 它**不重试**——重试是本文件的职责，实现方只负责发一次。
type attemptCaller func(ctx context.Context, attemptNumber int) (provider.ChatAttemptResult, error)

// phaseRunner 把预留、调用、结算、重试串起来。
type phaseRunner struct {
	repo *Repository
	// sleep 是退避的接缝：真实实现尊重 ctx 取消，测试里换成空实现，
	// 免得每个用例真的睡 3 秒。
	sleep func(ctx context.Context, d time.Duration) error
}

func newPhaseRunner(repo *Repository) *phaseRunner {
	return &phaseRunner{repo: repo, sleep: sleepWithContext}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runPhase 跑一个阶段直到成功或最终失败，返回**最后一次**尝试的结果。
//
// ⭐ 每一次尝试（包括被重试掉的那些）都单独预留、单独结算、单独落账。
// 只记最后一次会让成本少算三分之二。
//
// ⚠️ 额度耗尽的错误**不吞**：调用方要据此把作业停成 budget_exhausted，
// 而不是当成这个 item 失败了——两者的下一步完全不同（前者可以追加额度
// 后继续，后者是内容或配置的问题）。
func (p *phaseRunner) runPhase(ctx context.Context, in phaseInput, call attemptCaller) (provider.ChatAttemptResult, error) {
	counts, err := p.repo.queries.GetItemAttemptCounts(ctx, in.ItemID)
	if err != nil {
		return provider.ChatAttemptResult{}, fmt.Errorf("knowledge: read item attempt counts: %w", err)
	}
	// ⭐ 从**已经用掉的次数**接着算，不是从 0 开始。
	used := int(counts.ExtractAttemptCount)
	if in.Phase == phaseAlias {
		used = int(counts.AliasAttemptCount)
	}

	var last provider.ChatAttemptResult
	for used < maxAttemptsPerPhase {
		attemptNumber := used + 1

		att, err := p.repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch, Phase: in.Phase,
			AttemptNumber: attemptNumber, RequestHash: in.RequestHash,
			MaxOutputTokens: in.MaxOutputTokens,
		})
		if err != nil {
			// 额度耗尽 / epoch 已失效：立刻停手并原样上抛。
			return last, err
		}
		if err := p.bumpAttemptCount(ctx, in); err != nil {
			return last, err
		}
		used = attemptNumber

		res, callErr := call(ctx, attemptNumber)
		if callErr != nil {
			// 连一次尝试都没能开始（模型解析不出、装饰器接错）。
			// ⚠️ 仍然要把这次预留结算掉，否则它会停在 reserved 上，
			// 最后被恢复扫描改判 unknown——凭空多出一笔"可能花了的钱"。
			_ = p.repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
				Outcome: provider.AttemptNotDispatched, ErrorCode: "not_started",
			})
			return last, callErr
		}
		if err := p.repo.settleExtractionAttempt(ctx, att, res); err != nil {
			return res, err
		}
		last = res

		switch decideRetry(res, attemptNumber) {
		case decisionAccept, decisionGiveUp:
			return res, nil
		}
		if err := p.sleep(ctx, retryBackoff(attemptNumber)); err != nil {
			return res, err
		}
	}
	// 次数在进入本轮之前就已经用尽（恢复后接着跑的情形）。
	if last.Outcome == "" {
		last = provider.ChatAttemptResult{
			Outcome: provider.AttemptFailed, ErrorCode: "attempts_exhausted"}
	}
	return last, nil
}

func (p *phaseRunner) bumpAttemptCount(ctx context.Context, in phaseInput) error {
	var extract, alias int32
	if in.Phase == phaseAlias {
		alias = 1
	} else {
		extract = 1
	}
	if _, err := p.repo.queries.BumpItemAttemptCount(ctx, gen.BumpItemAttemptCountParams{
		ExtractAttemptCount: extract, AliasAttemptCount: alias, ID: in.ItemID,
	}); err != nil {
		return fmt.Errorf("knowledge: bump attempt count: %w", err)
	}
	return nil
}

// 编译期确认哨兵错误没有被改成不可比较的类型。
var _ = errors.Is
