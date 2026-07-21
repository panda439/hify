package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sony/gobreaker"
)

// Characterization tests for the resilience decorator — 链路 7。这些行为
// 不影响编译、普通手测也测不出，只有故障注入才能验证：错误分类重试、
// 熔断快速失败、流式"仅首连重试、断流不重试"、空闲超时。

// fakeClient scripts per-call failures. retryAfter 返回极小值以同时覆盖
// retryAfterAwareDelay 的 Retry-After 优先路径并让重试用例保持毫秒级。
type fakeClient struct {
	chatCalls   atomic.Int64
	streamCalls atomic.Int64
	// failFirst: 前 N 次 Chat 返回 failErr，之后成功。
	failFirst int64
	failErr   error
	stream    func() <-chan ChatChunk
}

func (f *fakeClient) lastRetryAfter() time.Duration { return time.Millisecond }

func (f *fakeClient) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	n := f.chatCalls.Add(1)
	if n <= f.failFirst {
		return Message{}, f.failErr
	}
	return Message{Role: RoleAssistant, Content: "ok"}, nil
}

func (f *fakeClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	f.streamCalls.Add(1)
	if f.stream == nil {
		ch := make(chan ChatChunk)
		close(ch)
		return ch, nil
	}
	return f.stream(), nil
}

func (f *fakeClient) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	return EmbedResult{}, nil
}

func (f *fakeClient) TestConnection(ctx context.Context) error { return nil }

func wrap(f *fakeClient, maxRetries int) Client {
	return WithResilience(f, ResilienceConfig{
		ProviderID:        "test",
		MaxConcurrent:     4,
		MaxRetries:        maxRetries,
		IdleTimeout:       100 * time.Millisecond,
		MaxStreamDuration: 5 * time.Second,
		// Redis nil + RateLimitPerMinute 0 → 令牌桶跳过（其行为属集成测试）。
	})
}

func status(code int) error {
	return &adapterError{status: code, cause: fmt.Errorf("upstream said %d", code)}
}

func TestIsRetryableClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport failure (status 0)", status(0), true},
		{"429 rate limited", status(429), true},
		{"500 server error", status(500), true},
		{"503 unavailable", status(503), true},
		{"400 bad request", status(400), false},
		{"401 unauthorized", status(401), false},
		{"breaker open", gobreaker.ErrOpenState, false},
		{"plain non-adapter error", errors.New("boom"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Fatalf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestChatRetriesRetryableThenSucceeds(t *testing.T) {
	f := &fakeClient{failFirst: 2, failErr: status(500)}
	c := wrap(f, 3)

	msg, err := c.Chat(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("Chat should succeed after retries: %v", err)
	}
	if msg.Content != "ok" {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if got := f.chatCalls.Load(); got != 3 {
		t.Fatalf("inner Chat called %d times, want 3 (2 failures + 1 success)", got)
	}
}

func TestChatDoesNotRetryNonRetryable(t *testing.T) {
	f := &fakeClient{failFirst: 10, failErr: status(401)}
	c := wrap(f, 3)

	if _, err := c.Chat(context.Background(), ChatRequest{}); err == nil {
		t.Fatal("expected error")
	}
	if got := f.chatCalls.Load(); got != 1 {
		t.Fatalf("non-retryable 401 retried: inner called %d times, want 1", got)
	}
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	f := &fakeClient{failFirst: 100, failErr: status(400)} // 400: 不重试，每次调用恰好打到 inner 一次
	c := wrap(f, 0)

	for i := 0; i < 5; i++ {
		if _, err := c.Chat(context.Background(), ChatRequest{}); err == nil {
			t.Fatalf("call %d unexpectedly succeeded", i)
		}
	}
	// 第 6 次：熔断已开，必须快速失败且不再打到 inner。
	_, err := c.Chat(context.Background(), ChatRequest{})
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Fatalf("6th call err = %v, want gobreaker.ErrOpenState", err)
	}
	if got := f.chatCalls.Load(); got != 5 {
		t.Fatalf("inner called %d times, want 5 (breaker must shed the 6th)", got)
	}
}

func TestChatStreamForwardsChunksAndErrWithoutRetry(t *testing.T) {
	// 断流（已有内容流出后出错）绝不能重试——重试会向客户端重复推送。
	f := &fakeClient{}
	f.stream = func() <-chan ChatChunk {
		ch := make(chan ChatChunk, 2)
		ch <- ChatChunk{DeltaContent: "部分内容"}
		ch <- ChatChunk{Err: status(500)}
		close(ch)
		return ch
	}
	c := wrap(f, 3)

	out, err := c.ChatStream(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var chunks []ChatChunk
	for chunk := range out {
		chunks = append(chunks, chunk)
	}
	if len(chunks) != 2 || chunks[0].DeltaContent != "部分内容" || chunks[1].Err == nil {
		t.Fatalf("unexpected forwarded chunks: %+v", chunks)
	}
	if got := f.streamCalls.Load(); got != 1 {
		t.Fatalf("mid-stream failure retried: ChatStream called %d times, want 1", got)
	}
}

func TestChatStreamIdleTimeout(t *testing.T) {
	f := &fakeClient{}
	f.stream = func() <-chan ChatChunk {
		return make(chan ChatChunk) // 永不产出
	}
	c := wrap(f, 0)

	out, err := c.ChatStream(context.Background(), ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	select {
	case chunk := <-out:
		if chunk.Err == nil || !strings.Contains(chunk.Err.Error(), "idle timeout") {
			t.Fatalf("expected idle timeout error chunk, got %+v", chunk)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle timeout never fired")
	}
}

func TestChatStreamStopsAtFinishReason(t *testing.T) {
	f := &fakeClient{}
	f.stream = func() <-chan ChatChunk {
		ch := make(chan ChatChunk, 3)
		ch <- ChatChunk{DeltaContent: "a"}
		ch <- ChatChunk{DeltaContent: "b", FinishReason: "stop"}
		// 协议之外的多余 chunk：转发层看到 FinishReason 应停止。
		ch <- ChatChunk{DeltaContent: "泄漏"}
		close(ch)
		return ch
	}
	c := wrap(f, 0)

	out, _ := c.ChatStream(context.Background(), ChatRequest{})
	var got []ChatChunk
	for chunk := range out {
		got = append(got, chunk)
	}
	if len(got) != 2 || got[1].FinishReason != "stop" {
		t.Fatalf("forwarder must stop at FinishReason: %+v", got)
	}
}
