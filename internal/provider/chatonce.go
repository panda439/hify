package provider

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sony/gobreaker"
)

// chatonce.go 是"只发一次、并如实报告发生了什么"的调用入口
// （010-narrative-scene-chunking-and-relation-extraction，T014）。
//
// ⭐ 为什么不能复用既有的 Chat：
// Chat 自动重试最多 3 次。对话场景里这是对的——用户只关心最后拿没拿到答案。
// 但关系抽取要给出一个**可复核的成本数字**，而自动重试意味着一次 Chat 背后
// 可能是 1 次也可能是 3 次真实请求；账目对不上，且偏差方向恒定：永远低估。
// 重试仍然要有，只是必须由调用方那一层负责，因为只有那一层能把每一次尝试
// 都写进 relation_extraction_attempts。
//
// ⚠️ 限流、并发槽、熔断**照常复用**——只摘掉重试。摘掉限流等于让抽取作业
// 绕过整个供应商保护层去打几千次请求。

// AttemptOutcome 是一次尝试的结局。⭐ 四个取值不可合并，尤其是
// AttemptUnknown 与 AttemptNotDispatched：
//   - NotDispatched：**确定**没有任何字节离开本进程（限流/熔断/拿不到并发槽），
//     调用预留可以退还；
//   - Unknown：请求可能已经到达服务端并被处理过一次（传输层错误、超时），
//     **不能声称没发出**——那笔钱可能已经花了。
//
// 把 Unknown 并进 NotDispatched 会系统性低估成本；并进 Failed 会丢掉
// "服务端可能已经处理过一次"这个事实，而那正是幂等设计要依据的前提。
type AttemptOutcome string

const (
	AttemptCompleted     AttemptOutcome = "completed"
	AttemptFailed        AttemptOutcome = "failed"
	AttemptUnknown       AttemptOutcome = "unknown"
	AttemptNotDispatched AttemptOutcome = "not_dispatched"
)

// ChatAttemptResult 是一次尝试的全部事实。
//
// ⚠️ 它**不返回业务错误**：调用失败也是一个如实的结果，而不是异常。
// ChatOnce 只在"根本没能开始一次尝试"（比如上下文已取消）时返回 error。
// 这样调用方无法忘记记账——拿不到 result 的路径只有一条，且极窄。
type ChatAttemptResult struct {
	Outcome AttemptOutcome
	Message Message

	// Dispatched 为 true 表示**确定**请求已经发出（拿到了任何 HTTP 状态码）。
	// 为 false 只表示"不确定或确定没发"，具体看 Outcome。
	Dispatched bool
	ElapsedMs  int64

	// UsageKnown 为 false 时 InputTokens/OutputTokens 无意义。
	// ⚠️ 零值不是"花了 0 个 token"，是"没测到"。000017 的
	// chk_rea_usage_consistent 在数据库层守着同一件事。
	UsageKnown   bool
	InputTokens  int
	OutputTokens int

	FinishReason string
	ErrorCode    string
	// Cause 保留原始错误供调用方 errors.Is/As，不让它去 parse ErrorCode。
	Cause error
}

// SingleAttemptChatter 是可选接口，只有弹性装饰器实现。
//
// ⚠️ 做成可选接口而不是加进 Client：Client 每加一个方法，仓库里
// knowledge/conversation/workflow/eval 下每一个测试 fake 都要跟着改
// （llm.go 里 Rerank 的注释记着上一次的代价）。而 ChatOnce 只有一个调用方。
//
// ⚠️ 拿不到这个接口的调用方**必须报错**，绝不能退回去调 Chat——
// 那会带上自动重试，账目静默失真，而且没有任何症状。
type SingleAttemptChatter interface {
	ChatOnce(ctx context.Context, req ChatRequest, timeout time.Duration) (ChatAttemptResult, error)
}

// maxChatOnceTimeout 是硬上限，调用方给再大的值也会被压到这里。
// 宪法第 V 条要求「引入 LLM 参与链路时必须定义超时上限」——上限写在被调用
// 方而不是调用方，是因为调用方传错一个值不该能让一次调用无限期挂住。
const maxChatOnceTimeout = 120 * time.Second

func (r *resilientClient) ChatOnce(ctx context.Context, req ChatRequest, timeout time.Duration) (ChatAttemptResult, error) {
	if timeout <= 0 || timeout > maxChatOnceTimeout {
		timeout = maxChatOnceTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()

	// 并发槽：拿不到（上下文取消）说明这次尝试根本没开始。
	if err := r.acquire(callCtx); err != nil {
		return ChatAttemptResult{
			Outcome: AttemptNotDispatched, ErrorCode: "concurrency", Cause: err,
			ElapsedMs: time.Since(start).Milliseconds(),
		}, nil
	}
	defer r.sem.Release(1)

	if err := r.checkRateLimit(callCtx); err != nil {
		return ChatAttemptResult{
			Outcome: AttemptNotDispatched, ErrorCode: "rate_limited", Cause: err,
			ElapsedMs: time.Since(start).Milliseconds(),
		}, nil
	}
	msg, err := r.callWithinDeadline(callCtx, req)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return classifyAttemptError(err, elapsed), nil
	}

	m, ok := msg.(Message)
	if !ok {
		// 不可能发生；真发生了说明装饰器被接错了，宁可炸也不要静默产出空结果。
		return ChatAttemptResult{}, fmt.Errorf("provider: chat once: unexpected result type %T", msg)
	}
	return ChatAttemptResult{
		Outcome: AttemptCompleted, Message: m, Dispatched: true, ElapsedMs: elapsed,
		// ⚠️ 判据是"有没有任何一个计数非零"，不是"两个都非零"：
		// 有的供应商只回 prompt_tokens。全零才算未知。
		UsageKnown:   m.Usage.PromptTokens > 0 || m.Usage.CompletionTokens > 0 || m.Usage.TotalTokens > 0,
		InputTokens:  m.Usage.PromptTokens,
		OutputTokens: m.Usage.CompletionTokens,
		FinishReason: m.FinishReason,
	}, nil
}

// callWithinDeadline 强制墙钟上限，**不依赖被调方尊重 ctx**。
//
// ⭐ 光传 ctx 是不够的：一个忽略 ctx 的适配器能让这次调用无限期挂住，
// 而抽取作业有活跃时间预算（7200s）和租约（180s）两个东西挂在它上面——
// 一次挂死的调用会让整个作业停在那里，租约过期后被别的 worker 抢走，
// 于是同一个 chunk 被重复送进模型，钱花两遍。这不是理论风险，
// 是"上限写在被调用方"这条设计的全部理由。
//
// ⚠️ 超时后被放弃的那个 goroutine 会继续跑到 inner.Chat 自己返回。
// 结果写进**带缓冲**的 channel，所以它永远不会因为没人收而阻塞，
// 返回后即可被回收。真实适配器（go-openai）是尊重 ctx 的，
// 这条路径只在适配器行为异常时才走到；代价是那种情况下一次调用
// 多挂一个 goroutine，换来的是调用方的延迟有确定上界。
func (r *resilientClient) callWithinDeadline(ctx context.Context, req ChatRequest) (interface{}, error) {
	type outcome struct {
		msg interface{}
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := r.breaker.Execute(func() (interface{}, error) {
			return r.inner.Chat(ctx, req)
		})
		done <- outcome{msg, err}
	}()

	select {
	case o := <-done:
		return o.msg, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// classifyAttemptError 是"发出去了没有"这个判断的唯一实现处。
//
// ⚠️ 判据只有一条：**我们有没有拿到 HTTP 状态码**。拿到了就是发出去了
// （哪怕是 500）；没拿到就是不知道——传输层错误、超时、连接被重置，
// 三者都可能发生在服务端已经收到并处理之后。系统无从区分，所以不装作知道。
func classifyAttemptError(err error, elapsed int64) ChatAttemptResult {
	res := ChatAttemptResult{ElapsedMs: elapsed, Cause: err}

	// 熔断打开 / 半开限流：装饰器直接拒绝，inner 一次都没被调用。
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		res.Outcome, res.ErrorCode = AttemptNotDispatched, "circuit_open"
		return res
	}

	var ae *adapterError
	if errors.As(err, &ae) && ae.status != 0 {
		res.Outcome, res.Dispatched = AttemptFailed, true
		res.ErrorCode = "http_" + strconv.Itoa(ae.status)
		return res
	}

	// 超时与传输层错误都落到这里：不知道。
	res.Outcome = AttemptUnknown
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		res.ErrorCode = "timeout"
	case errors.Is(err, context.Canceled):
		res.ErrorCode = "canceled"
	default:
		res.ErrorCode = "transport"
	}
	return res
}
