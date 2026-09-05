package provider

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"
)

// chatonce_test.go 是 ChatOnce 的**契约测试**（010 T014）。
//
// ⭐ 为什么整条链路上要多一个"只发一次"的入口：
// 既有的 Chat 会自动重试最多 3 次。对话场景里这是对的——用户只关心最后
// 拿没拿到答案。但关系抽取要给出一个**可复核的成本数字**，而自动重试意味着
// 一次 Chat 调用背后可能是 1 次、也可能是 3 次真实请求，账目对不上，
// 且偏差方向恒定：永远低估。
//
// 重试仍然要有，但必须由**我们自己那一层**负责（T018），因为只有那一层
// 能把每一次尝试都记进 relation_extraction_attempts。
//
// ⚠️ 这里最难也最要紧的一格是「发出去了没有」：
//   * 限流 / 熔断打开 / 拿不到并发槽 → **确定没发出去**，可以退还预算；
//   * 拿到了任何 HTTP 状态码 → **确定发出去了**；
//   * 传输层错误或超时 → **不知道**。
// 第三格不能被并进前两格。并进"没发出"会低估成本（钱可能已经花了），
// 并进"失败"会丢掉"服务端可能已经处理过一次"这个事实。

// countingClient 记录**真实网络请求次数**，用来证明 ChatOnce 恰好发一次。
type countingClient struct {
	fakeClient
	onceCalls atomic.Int64
	respond   func(n int64) (Message, error)
}

func (c *countingClient) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	n := c.onceCalls.Add(1)
	if c.respond != nil {
		return c.respond(n)
	}
	return Message{Role: RoleAssistant, Content: "ok"}, nil
}

func newOnceClient(inner Client) Client {
	return WithResilience(inner, ResilienceConfig{
		ProviderID: "p-once", MaxConcurrent: 4, MaxRetries: 2,
	})
}

func singleAttempt(t *testing.T, c Client) SingleAttemptChatter {
	t.Helper()
	sac, ok := c.(SingleAttemptChatter)
	if !ok {
		t.Fatal("弹性装饰器没有实现 SingleAttemptChatter")
	}
	return sac
}

// TestChatOnceMakesExactlyOneRequest——⭐ 本文件的立论。
// 同一个会失败的 inner，Chat 会打三次，ChatOnce 只能打一次。
func TestChatOnceMakesExactlyOneRequest(t *testing.T) {
	retryable := &adapterError{status: 503, cause: errors.New("boom")}

	// 对照：既有 Chat 确实会重试（不是在假设）。
	viaChat := &countingClient{respond: func(int64) (Message, error) { return Message{}, retryable }}
	if _, err := newOnceClient(viaChat).Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("Chat 本该失败")
	}
	if n := viaChat.onceCalls.Load(); n != 3 {
		t.Fatalf("对照组：Chat 打了 %d 次请求，want 3（MaxRetries=2）", n)
	}

	viaOnce := &countingClient{respond: func(int64) (Message, error) { return Message{}, retryable }}
	res, err := singleAttempt(t, newOnceClient(viaOnce)).
		ChatOnce(context.Background(), ChatRequest{}, time.Second)
	if err != nil {
		t.Fatalf("ChatOnce 不该把可重试错误当成调用失败返回：%v", err)
	}
	if n := viaOnce.onceCalls.Load(); n != 1 {
		t.Errorf("ChatOnce 打了 %d 次请求，want 1", n)
	}
	if res.Outcome != AttemptFailed {
		t.Errorf("outcome = %q, want %q", res.Outcome, AttemptFailed)
	}
	if !res.Dispatched {
		t.Error("拿到了 503 状态码，说明请求确实发出去了，Dispatched 应为 true")
	}
}

// TestChatOnceClassifiesDispatch——三格必须分得开。
func TestChatOnceClassifiesDispatch(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantOutcome   AttemptOutcome
		wantDispatch  bool
		wantErrorCode string
	}{
		{"成功", nil, AttemptCompleted, true, ""},
		{"服务端 500（确实发出去了）",
			&adapterError{status: 500, cause: errors.New("x")}, AttemptFailed, true, "http_500"},
		{"客户端 400（确实发出去了，且不该重试）",
			&adapterError{status: 400, cause: errors.New("x")}, AttemptFailed, true, "http_400"},
		{"传输层错误（不知道有没有发出去）",
			&adapterError{status: 0, cause: errors.New("connection reset")},
			AttemptUnknown, false, "transport"},
	}
	for _, tc := range cases {
		c := &countingClient{respond: func(int64) (Message, error) {
			if tc.err != nil {
				return Message{}, tc.err
			}
			return Message{Role: RoleAssistant, Content: "ok"}, nil
		}}
		res, err := singleAttempt(t, newOnceClient(c)).
			ChatOnce(context.Background(), ChatRequest{}, time.Second)
		if err != nil {
			t.Errorf("%s：ChatOnce 返回了调用级错误 %v", tc.name, err)
			continue
		}
		if res.Outcome != tc.wantOutcome {
			t.Errorf("%s：outcome = %q, want %q", tc.name, res.Outcome, tc.wantOutcome)
		}
		if res.Dispatched != tc.wantDispatch {
			t.Errorf("%s：Dispatched = %v, want %v", tc.name, res.Dispatched, tc.wantDispatch)
		}
		if res.ErrorCode != tc.wantErrorCode {
			t.Errorf("%s：ErrorCode = %q, want %q", tc.name, res.ErrorCode, tc.wantErrorCode)
		}
	}
}

// TestChatOnceReportsNotDispatchedWhenBreakerOpen——⭐ 熔断打开时
// **一个字节都没发出去**，这笔调用预留必须能退还。
// 把它记成 failed 会让账目凭空多出一次从未发生的调用。
func TestChatOnceReportsNotDispatchedWhenBreakerOpen(t *testing.T) {
	c := &countingClient{respond: func(int64) (Message, error) {
		return Message{}, &adapterError{status: 500, cause: errors.New("x")}
	}}
	client := newOnceClient(c)
	sac := singleAttempt(t, client)
	// 连续失败把熔断器打开（ReadyToTrip: ConsecutiveFailures >= 5）。
	for i := 0; i < 6; i++ {
		if _, err := sac.ChatOnce(context.Background(), ChatRequest{}, time.Second); err != nil {
			t.Fatalf("第 %d 次：%v", i, err)
		}
	}
	before := c.onceCalls.Load()
	res, err := sac.ChatOnce(context.Background(), ChatRequest{}, time.Second)
	if err != nil {
		t.Fatalf("熔断打开不该返回调用级错误：%v", err)
	}
	if res.Outcome != AttemptNotDispatched {
		t.Errorf("outcome = %q, want %q", res.Outcome, AttemptNotDispatched)
	}
	if res.Dispatched {
		t.Error("熔断打开时 Dispatched 必须为 false")
	}
	if got := c.onceCalls.Load(); got != before {
		t.Errorf("熔断打开后仍然打出了 %d 次请求", got-before)
	}
	if !errors.Is(res.Cause, gobreaker.ErrOpenState) {
		t.Errorf("Cause 应能识别为熔断打开，得到 %v", res.Cause)
	}
}

// TestChatOnceEnforcesItsOwnTimeout——宪法第 V 条要求「超时上限」。
// ⚠️ 超时归 unknown 而不是 failed：请求可能已经到了服务端并被处理，
// 说"没发生过"就是在少记一笔真实成本。
func TestChatOnceEnforcesItsOwnTimeout(t *testing.T) {
	c := &countingClient{respond: func(int64) (Message, error) {
		time.Sleep(300 * time.Millisecond)
		return Message{Role: RoleAssistant, Content: "late"}, nil
	}}
	start := time.Now()
	res, err := singleAttempt(t, newOnceClient(c)).
		ChatOnce(context.Background(), ChatRequest{}, 50*time.Millisecond)
	if err != nil {
		t.Fatalf("超时不该返回调用级错误：%v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("超时上限没有生效，耗时 %v", elapsed)
	}
	if res.Outcome != AttemptUnknown {
		t.Errorf("outcome = %q, want %q——超时时请求可能已经被服务端处理",
			res.Outcome, AttemptUnknown)
	}
	if res.ElapsedMs <= 0 {
		t.Error("ElapsedMs 必须记录，哪怕这次尝试超时了")
	}
}

// TestChatOnceReportsUsageOnlyWhenProviderDoes——⚠️ 不是所有兼容供应商都返回
// token 用量。UsageKnown=false 时数字必须为零值且**调用方有义务当成未知**，
// 这正是 000017 那条 CHECK 约束在数据库里守的东西。
func TestChatOnceReportsUsageOnlyWhenProviderDoes(t *testing.T) {
	withUsage := &countingClient{respond: func(int64) (Message, error) {
		return Message{Role: RoleAssistant, Content: "ok",
			Usage: Usage{PromptTokens: 12, CompletionTokens: 3, TotalTokens: 15}}, nil
	}}
	res, err := singleAttempt(t, newOnceClient(withUsage)).
		ChatOnce(context.Background(), ChatRequest{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !res.UsageKnown || res.InputTokens != 12 || res.OutputTokens != 3 {
		t.Errorf("有用量时没有如实记录：%+v", res)
	}

	noUsage := &countingClient{}
	res, err = singleAttempt(t, newOnceClient(noUsage)).
		ChatOnce(context.Background(), ChatRequest{}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.UsageKnown {
		t.Error("供应商没返回用量，UsageKnown 必须为 false——零值不是「花了 0 个 token」")
	}
}
