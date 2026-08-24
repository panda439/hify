package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// headerRoundTripper injects extra_headers (the escape hatch for
// providers/gateways that need a custom header alongside or instead of the
// standard Authorization bearer token) into every outgoing request.
type headerRoundTripper struct {
	headers map[string]string
	next    http.RoundTripper
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return h.next.RoundTrip(req)
}

// retryAfterHolder captures the most recent 429 response's Retry-After
// header so resilience.go's backoff can honor the provider's own guidance
// instead of guessing. Scoped one-per-client (one-per-provider), so a race
// between two concurrent requests to the same provider can at worst pick a
// slightly stale delay — not a correctness issue, just an imprecise backoff.
type retryAfterHolder struct {
	mu    sync.Mutex
	value time.Duration
}

func (h *retryAfterHolder) set(d time.Duration) {
	h.mu.Lock()
	h.value = d
	h.mu.Unlock()
}

func (h *retryAfterHolder) getAndClear() time.Duration {
	h.mu.Lock()
	d := h.value
	h.value = 0
	h.mu.Unlock()
	return d
}

type retryAfterRoundTripper struct {
	holder *retryAfterHolder
	next   http.RoundTripper
}

func (t *retryAfterRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, perr := strconv.Atoi(ra); perr == nil {
				t.holder.set(time.Duration(secs) * time.Second)
			}
		}
	}
	return resp, err
}

// zeroTemperatureCtxKey/withZeroTemperatureMarker/zeroTemperatureRoundTripper
// are T023a's fix for the go-openai omitempty problem documented on
// ChatRequest.Temperature's doc comment. The SDK's CreateChatCompletion
// takes a typed openai.ChatCompletionRequest and marshals it internally —
// there is no exported hook to override that marshaling, and no field type
// change on OUR side can make json.Marshal keep a `float32`/`omitempty`
// field that equals its zero value (0 IS the zero value, pointer or not,
// once it's copied into out.Temperature — see toOpenAIRequest). The only
// place left to fix this is the actual bytes going out over the wire, which
// is exactly what an http.RoundTripper sees.
//
// The approach: Chat/ChatStream tag ctx with a marker whenever the caller's
// Temperature is explicitly *0 (never for nil — "unset" must still omit the
// field, that's correct behavior, not the bug). This RoundTripper reads that
// marker off the outgoing *http.Request's own context (http.NewRequestWithContext
// inside go-openai's request builder carries it through) and, only then,
// patches the JSON body to guarantee `"temperature":0` is present. Every
// other request (marker absent) passes through completely untouched.
type zeroTemperatureCtxKey struct{}

func withZeroTemperatureMarker(ctx context.Context, req ChatRequest) context.Context {
	if req.Temperature != nil && *req.Temperature == 0 {
		return context.WithValue(ctx, zeroTemperatureCtxKey{}, true)
	}
	return ctx
}

type zeroTemperatureRoundTripper struct {
	next http.RoundTripper
}

func (t *zeroTemperatureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	marked, _ := req.Context().Value(zeroTemperatureCtxKey{}).(bool)
	if !marked || req.Body == nil {
		return t.next.RoundTrip(req)
	}

	raw, err := io.ReadAll(req.Body)
	req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("provider: read chat request body for temperature patch: %w", err)
	}
	patched, perr := injectZeroTemperature(raw)
	if perr != nil {
		// 补丁失败就退回原始字节——温度字段回到 SDK 的默认行为（省略，
		// 供应商用自己的默认温度），比让这次请求直接失败更安全（FR-014
		// 一以贯之的"任何失败都降级，不让本轮失败"精神，虽然这里不是 RAG
		// 链路，但同一条原则同样适用：一个字段修不好不该拖垮整次调用）。
		patched = raw
	}
	req.Body = io.NopCloser(bytes.NewReader(patched))
	req.ContentLength = int64(len(patched))
	req.Header.Set("Content-Length", strconv.Itoa(len(patched)))
	// GetBody 也要换成补丁后的字节：net/http 在重定向、以及 HTTP/2 收到
	// GOAWAY 需要重发请求时会调用它重建 body。不换的话那条重试路径会把
	// 未打补丁的原始字节发出去，温度字段又被吞掉——一个只在偶发重试时
	// 才出现、极难复现的不一致。
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(patched)), nil
	}
	return t.next.RoundTrip(req)
}

// injectZeroTemperature is the pure byte-patch: if the marshaled body
// already has a "temperature" key (a future go-openai version that fixes
// this, or a different field ordering), it's left alone; otherwise
// `"temperature":0` is spliced in right after the opening `{` — valid
// regardless of where it lands since JSON object key order is not
// meaningful. No full unmarshal/remarshal round trip, so it can't reorder
// or reformat anything else in the body (float precision, key order) that a
// decode-then-encode pass risks touching.
func injectZeroTemperature(body []byte) ([]byte, error) {
	// "已经有 temperature 了吗"必须只看**顶层**键，不能用
	// bytes.Contains 扫整个 body。原因：工具定义和聊天请求走同一个 JSON
	// ——一个参数名恰好叫 temperature 的 MCP 工具（比如查天气的），会让
	// body 里出现 `"temperature":{"type":"number"}`，整体扫描就会误判成
	// "顶层已经有了"，于是跳过补丁，悄悄退回 T023a 要修的那个 bug 本身。
	// 这种 bug 只在挂了特定工具的 Agent 上出现，最难查。
	//
	// 用 map[string]json.RawMessage 只解一层：RawMessage 不递归解析各字段
	// 的值，成本很低，而且拿到的是精确的顶层键集合。判断完仍然走下面的
	// 字节拼接，不做 remarshal——避免重排键序或改变浮点表示。
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("provider: chat request body is not a json object: %w", err)
	}
	if _, ok := top["temperature"]; ok {
		return body, nil
	}
	idx := bytes.IndexByte(body, '{')
	if idx < 0 {
		return nil, fmt.Errorf("provider: chat request body has no top-level json object")
	}
	patched := make([]byte, 0, len(body)+len(`"temperature":0,`))
	patched = append(patched, body[:idx+1]...)
	patched = append(patched, []byte(`"temperature":0,`)...)
	patched = append(patched, body[idx+1:]...)
	return patched, nil
}

// openAICompatClient implements Client against any OpenAI-compatible
// endpoint (OpenAI itself, DeepSeek, Moonshot, Qwen compatible-mode, Zhipu
// GLM, Ollama/vLLM) — see CLAUDE.md/plan for the supported-provider list.
type openAICompatClient struct {
	client     *openai.Client
	retryAfter *retryAfterHolder

	// baseURL/apiKey/httpClient back Rerank (001-rag-query-rerank) — go-openai
	// has no /rerank endpoint (it's not part of the OpenAI API surface, see
	// contracts/rerank-http-api.md), so Rerank goes straight over net/http
	// instead of through c.client. httpClient is the SAME wrapped client
	// passed to openai.ClientConfig.HTTPClient above (retryAfterRoundTripper
	// + optional headerRoundTripper already layered in) — reusing it means
	// Rerank gets the identical Retry-After capture and extra_headers
	// injection as Chat/Embed for free, per T021's "复用既有 classifyError
	// 与 retry-after 采集".
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func newOpenAICompatClient(baseURL, apiKey string, extraHeaders map[string]string, httpClient *http.Client) *openAICompatClient {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = baseURL

	transport := httpClient.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	holder := &retryAfterHolder{}
	transport = &retryAfterRoundTripper{holder: holder, next: transport}
	// zeroTemperatureRoundTripper only ever touches a request whose ctx
	// carries withZeroTemperatureMarker's marker (set by Chat/ChatStream) —
	// it's a no-op for every other call this client makes, including Embed
	// and Rerank.
	transport = &zeroTemperatureRoundTripper{next: transport}
	if len(extraHeaders) > 0 {
		transport = &headerRoundTripper{headers: extraHeaders, next: transport}
	}
	wrapped := &http.Client{Transport: transport}
	cfg.HTTPClient = wrapped

	return &openAICompatClient{
		client:     openai.NewClientWithConfig(cfg),
		retryAfter: holder,
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: wrapped,
	}
}

// lastRetryAfter implements the optional retryAfterProvider interface
// resilience.go's backoff checks for.
func (c *openAICompatClient) lastRetryAfter() time.Duration {
	return c.retryAfter.getAndClear()
}

func (c *openAICompatClient) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	ctx = withZeroTemperatureMarker(ctx, req)
	resp, err := c.client.CreateChatCompletion(ctx, toOpenAIRequest(req, false))
	if err != nil {
		return Message{}, classifyError(err)
	}
	if len(resp.Choices) == 0 {
		return Message{}, fmt.Errorf("provider: empty choices in response")
	}
	msg := fromOpenAIMessage(resp.Choices[0].Message)
	msg.Usage = fromOpenAIUsage(resp.Usage)
	return msg, nil
}

func (c *openAICompatClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	ctx = withZeroTemperatureMarker(ctx, req)
	stream, err := c.client.CreateChatCompletionStream(ctx, toOpenAIRequest(req, true))
	if err != nil {
		return nil, classifyError(err)
	}

	out := make(chan ChatChunk)
	go func() {
		defer close(out)
		defer stream.Close()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("provider: panic in chatstream reader", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				trySendChunk(ctx, out, ChatChunk{Err: classifyError(err)})
				return
			}
			if len(resp.Choices) == 0 {
				continue
			}
			choice := resp.Choices[0]
			chunk := ChatChunk{
				DeltaContent:   choice.Delta.Content,
				DeltaToolCalls: fromOpenAIToolCalls(choice.Delta.ToolCalls),
				FinishReason:   string(choice.FinishReason),
			}
			if choice.FinishReason != "" {
				// stream_options.include_usage (set below in
				// toOpenAIRequest) makes the API send token usage on a
				// separate trailing chunk with an empty choices array right
				// after this one — read it now so callers see usage on the
				// same chunk that carries FinishReason, rather than exposing
				// that two-chunk wire quirk. Best-effort: not every
				// OpenAI-compatible provider actually honors the option, and
				// a provider that emits FinishReason then goes silent
				// without closing the connection must not be able to block
				// this goroutine forever — recvTrailingUsage bounds the
				// extra read with its own timeout, independent of ctx and
				// of resilience.go's idle/duration timers (which only guard
				// the outer forwarder's channel read, not this underlying
				// network read).
				if resp.Usage != nil {
					chunk.Usage = fromOpenAIUsage(*resp.Usage)
				} else if trailing, ok := recvTrailingUsage(stream); ok {
					chunk.Usage = fromOpenAIUsage(*trailing)
				}
			}
			if !trySendChunk(ctx, out, chunk) {
				return
			}
			if choice.FinishReason != "" {
				return
			}
		}
	}()
	return out, nil
}

// trailingUsageTimeout bounds recvTrailingUsage's extra read — generous
// enough that any conformant provider's near-instant trailing chunk clears
// it easily, short enough that a non-conformant one can't hold this
// goroutine open indefinitely.
const trailingUsageTimeout = 5 * time.Second

// recvTrailingUsage reads one more frame off stream, bounded by
// trailingUsageTimeout instead of ctx — see ChatStream's call site comment
// for why ctx alone isn't a sufficient guard here. On timeout it gives up
// and returns ok=false; the underlying stream.Recv() goroutine is left
// running, but stream.Close() (deferred in ChatStream) unblocks it as soon
// as this function's caller returns, so it doesn't run forever.
func recvTrailingUsage(stream *openai.ChatCompletionStream) (*openai.Usage, bool) {
	type result struct {
		resp openai.ChatCompletionStreamResponse
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("provider: panic in trailing usage reader", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		resp, err := stream.Recv()
		ch <- result{resp, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil || r.resp.Usage == nil {
			return nil, false
		}
		return r.resp.Usage, true
	case <-time.After(trailingUsageTimeout):
		return nil, false
	}
}

// trySendChunk delivers chunk on out but never blocks past ctx's
// cancellation. Without this, a client disconnect propagates up through
// resilientClient's forwarder (see resilience.go) and conversation/
// workflow's SSE loop — once nobody downstream is reading anymore, a plain
// `out <- chunk` would block this goroutine (and its held stream/socket)
// forever. false means the caller should stop generating further chunks.
func trySendChunk(ctx context.Context, out chan<- ChatChunk, chunk ChatChunk) bool {
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *openAICompatClient) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	resp, err := c.client.CreateEmbeddings(ctx, openai.EmbeddingRequest{
		Model: openai.EmbeddingModel(req.Model),
		Input: req.Input,
	})
	if err != nil {
		return EmbedResult{}, classifyError(err)
	}

	embeddings := make([][]float32, len(resp.Data))
	dimension := 0
	for i, d := range resp.Data {
		embeddings[i] = d.Embedding
		if len(d.Embedding) > dimension {
			dimension = len(d.Embedding)
		}
	}
	return EmbedResult{Embeddings: embeddings, Dimension: dimension}, nil
}

// rerankWireRequest/rerankWireResponse mirror contracts/rerank-http-api.md's
// wire shapes exactly — kept private to this file, never exposed as the
// provider.RerankRequest/Result domain types (same "adapter-only wire
// struct" pattern as fromOpenAIMessage etc. for chat).
type rerankWireRequest struct {
	Model           string   `json:"model"`
	Query           string   `json:"query"`
	Documents       []string `json:"documents"`
	TopN            int      `json:"top_n"`
	ReturnDocuments bool     `json:"return_documents"`
}

type rerankWireResponse struct {
	Results []struct {
		Index int             `json:"index"`
		Score json.RawMessage `json:"relevance_score"`
	} `json:"results"`
}

// Rerank implements Client.Rerank against contracts/rerank-http-api.md's
// POST {base_url}/rerank — a net/http direct call (plan.md's "rerank 端点不
// 经过它，用 net/http 直连": go-openai has no rerank method at all, so there
// is nothing in the SDK to route through). return_documents is always false
// (§「请求」table — avoid echoing chunk content back over the wire, both for
// bandwidth and because FR-017 forbids letting it leak into anything Hify
// might log downstream) and top_n is always len(req.Documents) — Hify wants
// every candidate scored, never a server-side pre-filter that could silently
// drop some.
func (c *openAICompatClient) Rerank(ctx context.Context, req RerankRequest) (RerankResult, error) {
	body, err := json.Marshal(rerankWireRequest{
		Model:           req.Model,
		Query:           req.Query,
		Documents:       req.Documents,
		TopN:            len(req.Documents),
		ReturnDocuments: false,
	})
	if err != nil {
		return RerankResult{}, fmt.Errorf("provider: encode rerank request: %w", err)
	}

	url := strings.TrimSuffix(c.baseURL, "/") + "/rerank"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return RerankResult{}, fmt.Errorf("provider: build rerank request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		// Transport-level failure (timeout/DNS/connection refused) — same
		// "no HTTP status, treat as retryable" classification classifyError
		// gives go-openai's own network errors.
		return RerankResult{}, &adapterError{status: 0, cause: fmt.Errorf("provider: rerank request failed: %w", err)}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return RerankResult{}, fmt.Errorf("provider: read rerank response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Reuses the same adapterError shape classifyError produces for
		// go-openai's own errors, so resilience.go's isRetryable/backoff
		// treats a rerank 429/5xx exactly like a chat/embed one — 429's
		// Retry-After was already captured by retryAfterRoundTripper above,
		// c.httpClient is the same wrapped client.
		return RerankResult{}, &adapterError{status: resp.StatusCode, cause: fmt.Errorf("provider: rerank request failed with status %d: %s", resp.StatusCode, truncateForError(respBody))}
	}

	var wire rerankWireResponse
	if err := json.Unmarshal(respBody, &wire); err != nil {
		return RerankResult{}, fmt.Errorf("provider: parse rerank response: %w", err)
	}
	return validateRerankResponse(wire, len(req.Documents))
}

// truncateForError caps how much of a non-2xx rerank response body ends up
// in an error string — an oversized or malformed error page from a
// misconfigured base_url must not blow up log lines.
func truncateForError(body []byte) string {
	const maxLen = 200
	if len(body) > maxLen {
		return string(body[:maxLen]) + "..."
	}
	return string(body)
}

// validateRerankResponse is contracts/rerank-http-api.md's "Hify 侧强制校
// 验" (FR-011) — implemented once, here, at the adapter boundary, so a
// caller (knowledge.applyRerank) can trust that any RerankResult it receives
// already satisfies "one score per document, no gaps, no duplicates,
// no out-of-range index". Any of the 5 conditions makes the WHOLE response
// untrusted — Hify returns an error and the caller degrades to the
// pre-rerank fused order (FR-011's "整体丢弃，禁止部分采用"), never accepts
// a partial result.
func validateRerankResponse(wire rerankWireResponse, documentCount int) (RerankResult, error) {
	if len(wire.Results) == 0 || len(wire.Results) != documentCount {
		return RerankResult{}, fmt.Errorf("provider: rerank response has %d results, want %d", len(wire.Results), documentCount)
	}

	seen := make([]bool, documentCount)
	scores := make([]RerankScore, documentCount)
	for _, r := range wire.Results {
		if r.Index < 0 || r.Index >= documentCount {
			return RerankResult{}, fmt.Errorf("provider: rerank response index %d out of range [0, %d)", r.Index, documentCount)
		}
		if seen[r.Index] {
			return RerankResult{}, fmt.Errorf("provider: rerank response has duplicate index %d", r.Index)
		}
		var score float64
		if err := json.Unmarshal(r.Score, &score); err != nil {
			return RerankResult{}, fmt.Errorf("provider: rerank response relevance_score at index %d is not numeric: %w", r.Index, err)
		}
		seen[r.Index] = true
		scores[r.Index] = RerankScore{Index: r.Index, Score: score}
	}
	for i, ok := range seen {
		if !ok {
			return RerankResult{}, fmt.Errorf("provider: rerank response missing index %d", i)
		}
	}
	return RerankResult{Scores: scores}, nil
}

// TestConnection hits the provider's model-listing endpoint — cheap (no
// tokens consumed) and supported by every OpenAI-compatible provider we
// target, including Ollama/vLLM's compatibility layer.
func (c *openAICompatClient) TestConnection(ctx context.Context) error {
	_, err := c.client.ListModels(ctx)
	if err != nil {
		return classifyError(err)
	}
	return nil
}

func toOpenAIRequest(req ChatRequest, stream bool) openai.ChatCompletionRequest {
	messages := make([]openai.ChatCompletionMessage, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = openai.ChatCompletionMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			ToolCalls:  toOpenAIToolCalls(m.ToolCalls),
		}
	}

	var tools []openai.Tool
	for _, t := range req.Tools {
		tools = append(tools, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}

	out := openai.ChatCompletionRequest{
		Model:     req.Model,
		Messages:  messages,
		MaxTokens: req.MaxTokens,
		TopP:      float32(req.TopP),
		Tools:     tools,
		Stream:    stream,
	}
	// req.Temperature==nil 代表调用方没有意见，让 out.Temperature 保持零值
	// ——因为它本来就有 `json:"temperature,omitempty"` 标签，零值本来就会
	// 被省略，这正是"未设置"该有的效果。req.Temperature!=nil 时才显式赋
	// 值；但即便这里赋成 0，go-openai 序列化这个结构体时同样会把它省略掉
	// （omitempty 认的是"marshal 出来的值是不是零值"，不是"调用方有没有主
	// 动赋值"）——这正是 T023a 要修的坑。真正让"显式 0"发到线上的是
	// Chat/ChatStream 里的 zeroTemperatureRoundTripper 标记 + 请求体补丁，
	// 不是这里。
	if req.Temperature != nil {
		out.Temperature = float32(*req.Temperature)
	}
	if stream {
		// Requests the trailing usage-only chunk ChatStream reads above.
		// Providers that don't understand the option just ignore it.
		out.StreamOptions = &openai.StreamOptions{IncludeUsage: true}
	}
	return out
}

func fromOpenAIUsage(u openai.Usage) Usage {
	return Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
}

func fromOpenAIMessage(m openai.ChatCompletionMessage) Message {
	return Message{
		Role:       Role(m.Role),
		Content:    m.Content,
		ToolCalls:  fromOpenAIToolCalls(m.ToolCalls),
		ToolCallID: m.ToolCallID,
	}
}

func toOpenAIToolCalls(calls []ToolCall) []openai.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]openai.ToolCall, len(calls))
	for i, c := range calls {
		out[i] = openai.ToolCall{
			Index: c.Index,
			ID:    c.ID,
			Type:  openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      c.Name,
				Arguments: string(c.Arguments),
			},
		}
	}
	return out
}

func fromOpenAIToolCalls(calls []openai.ToolCall) []ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ToolCall, len(calls))
	for i, c := range calls {
		out[i] = ToolCall{
			Index:     c.Index,
			ID:        c.ID,
			Name:      c.Function.Name,
			Arguments: []byte(c.Function.Arguments),
		}
	}
	return out
}

// classifyError normalizes go-openai's error shapes so resilience.go can
// tell retryable failures (timeouts, 429, 5xx) from permanent ones
// (401/400/404) without depending on this adapter's internals.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return &adapterError{status: apiErr.HTTPStatusCode, cause: err}
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return &adapterError{status: reqErr.HTTPStatusCode, cause: err}
	}
	// Network-level errors (connection refused, DNS, timeout) carry no
	// HTTP status — treat as retryable (status 0).
	return &adapterError{status: 0, cause: err}
}

// adapterError carries the HTTP status (0 for transport-level failures)
// that resilience.go's isRetryable classifies on.
type adapterError struct {
	status int
	cause  error
}

func (e *adapterError) Error() string { return e.cause.Error() }
func (e *adapterError) Unwrap() error { return e.cause }
