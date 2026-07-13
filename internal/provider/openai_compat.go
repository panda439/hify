package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

// openAICompatClient implements Client against any OpenAI-compatible
// endpoint (OpenAI itself, DeepSeek, Moonshot, Qwen compatible-mode, Zhipu
// GLM, Ollama/vLLM) — see CLAUDE.md/plan for the supported-provider list.
type openAICompatClient struct {
	client     *openai.Client
	retryAfter *retryAfterHolder
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
	if len(extraHeaders) > 0 {
		transport = &headerRoundTripper{headers: extraHeaders, next: transport}
	}
	cfg.HTTPClient = &http.Client{Transport: transport}

	return &openAICompatClient{client: openai.NewClientWithConfig(cfg), retryAfter: holder}
}

// lastRetryAfter implements the optional retryAfterProvider interface
// resilience.go's backoff checks for.
func (c *openAICompatClient) lastRetryAfter() time.Duration {
	return c.retryAfter.getAndClear()
}

func (c *openAICompatClient) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	resp, err := c.client.CreateChatCompletion(ctx, toOpenAIRequest(req, false))
	if err != nil {
		return Message{}, classifyError(err)
	}
	if len(resp.Choices) == 0 {
		return Message{}, fmt.Errorf("provider: empty choices in response")
	}
	return fromOpenAIMessage(resp.Choices[0].Message), nil
}

func (c *openAICompatClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	stream, err := c.client.CreateChatCompletionStream(ctx, toOpenAIRequest(req, true))
	if err != nil {
		return nil, classifyError(err)
	}

	out := make(chan ChatChunk)
	go func() {
		defer close(out)
		defer stream.Close()
		for {
			resp, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				out <- ChatChunk{Err: classifyError(err)}
				return
			}
			if len(resp.Choices) == 0 {
				continue
			}
			choice := resp.Choices[0]
			out <- ChatChunk{
				DeltaContent:   choice.Delta.Content,
				DeltaToolCalls: fromOpenAIToolCalls(choice.Delta.ToolCalls),
				FinishReason:   string(choice.FinishReason),
			}
			if choice.FinishReason != "" {
				return
			}
		}
	}()
	return out, nil
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

	return openai.ChatCompletionRequest{
		Model:       req.Model,
		Messages:    messages,
		Temperature: float32(req.Temperature),
		MaxTokens:   req.MaxTokens,
		TopP:        float32(req.TopP),
		Tools:       tools,
		Stream:      stream,
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
			ID:   c.ID,
			Type: openai.ToolTypeFunction,
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
