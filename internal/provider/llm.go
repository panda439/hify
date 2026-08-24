package provider

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is the adapter-agnostic canonical shape every Client
// implementation translates to/from its own wire format. ToolCalls and
// ToolCallID are unused until Phase 4's tool-calling loop but defined now
// so the wire format/DB schema (messages.tool_calls) don't need to change
// later.
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Usage      Usage
}

// Usage is best-effort: not every OpenAI-compatible provider returns token
// counts (self-hosted Ollama/vLLM often don't), so a zero value here just
// means "unavailable", not an error — callers skip the corresponding
// gen_ai.usage.* trace attribute rather than treating it as a failure.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// ToolCall represents one function call, either fully resolved (a
// finished assistant message) or one fragment of a call still streaming
// in. Index is only meaningful in the latter case — the OpenAI streaming
// protocol splits a single tool call's arguments across many chunks, and
// with multiple tool calls in flight at once, Index (not ID, which is
// only present on a call's first chunk) is the only thing identifying
// which call a given fragment belongs to. See
// conversation/service.go's mergeToolCallDeltas for the accumulation
// logic this field exists to support.
type ToolCall struct {
	Index     *int
	ID        string
	Name      string
	Arguments json.RawMessage
}

type ToolDefinition struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type ChatRequest struct {
	Model    string
	Messages []Message
	// Temperature 是 *float64 而不是 float64（T023a，001-rag-query-rerank
	// US1 review 追加）：go-openai v1.41.2 的
	// ChatCompletionRequest.Temperature 标签是
	// `json:"temperature,omitempty"`，float32 类型的零值和"没设置"在
	// encoding/json 眼里没有区别——调用方显式传 0（比如查询改写要求的
	// "确定性输出"、Agent 配置里显式选择的 0）会被整个从请求体里省略掉，
	// 供应商按自己的默认温度（通常 1.0）执行，且没有任何报错，是个纯粹的
	// 静默行为偏差。改成指针后 nil 才代表"未设置、用供应商默认"，
	// 非 nil（包括 *0.0）代表"调用方明确要这个值"——但这只解决了 Hify
	// 自己内部"能不能区分"的问题，真正让 0 值发到线上还需要
	// openai_compat.go 的 zeroTemperatureRoundTripper（见该文件），因为
	// go-openai 的 CreateChatCompletion 自己对请求体的序列化不受调用方
	// 控制，光改这里的类型改变不了它序列化出的 JSON 字节。
	Temperature *float64
	MaxTokens   int
	TopP        float64
	Tools       []ToolDefinition
}

// ChatChunk is one increment of a streamed response. Err terminates the
// stream when non-nil; FinishReason is set on the final content chunk
// ("stop" | "tool_calls" | "length" | "error"). Usage is only ever
// populated on that same final chunk (see openai_compat.go's
// StreamOptions.IncludeUsage) — best-effort, same caveat as Message.Usage.
type ChatChunk struct {
	DeltaContent   string
	DeltaToolCalls []ToolCall
	FinishReason   string
	Usage          Usage
	Err            error
}

type EmbedRequest struct {
	Model string
	Input []string
}

type EmbedResult struct {
	Embeddings [][]float32
	Dimension  int
}

// RerankRequest is 001-rag-query-rerank's third model capability — see
// contracts/rerank-http-api.md. Documents is index-addressed: the request's
// slice position IS the candidate's identity as far as the rerank service
// is concerned, and RerankScore.Index refers back to that same position.
// TopN is always sent as len(Documents) (contract §「请求」) — Hify wants
// every candidate scored, never a server-side pre-filter.
type RerankRequest struct {
	Model     string
	Query     string
	Documents []string
	TopN      int
}

// RerankResult.Scores' order is NOT meaningful — see RerankScore's doc
// comment. Consumers (knowledge.applyRerank) must index by Score.Index, never
// assume Scores[i] corresponds to Documents[i].
type RerankResult struct {
	Scores []RerankScore
}

// RerankScore.Index addresses RerankRequest.Documents (0-based). Score's
// scale is whatever the serving model defines — Hify only ever compares
// scores against each other within the same response, never against a fixed
// threshold or across two different rerank calls.
type RerankScore struct {
	Index int
	Score float64
}

// Client is the single seam every model provider adapter implements.
// Business code (agent/conversation/knowledge, in later phases) only ever
// depends on this interface, never on a concrete adapter.
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (Message, error)
	ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error)
	Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)
	// Rerank is 001-rag-query-rerank's addition — a breaking change to this
	// interface (contracts/internal-contracts.md §2). Every existing
	// implementation (openai_compat.go's real adapter, resilience.go's
	// decorator, and every test-only fake across workflow/knowledge/eval/
	// conversation) must grow this method for the package to compile.
	Rerank(ctx context.Context, req RerankRequest) (RerankResult, error)
	TestConnection(ctx context.Context) error
}
