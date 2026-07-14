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
	Model       string
	Messages    []Message
	Temperature float64
	MaxTokens   int
	TopP        float64
	Tools       []ToolDefinition
}

// ChatChunk is one increment of a streamed response. Err terminates the
// stream when non-nil; FinishReason is set on the final content chunk
// ("stop" | "tool_calls" | "length" | "error").
type ChatChunk struct {
	DeltaContent   string
	DeltaToolCalls []ToolCall
	FinishReason   string
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

// Client is the single seam every model provider adapter implements.
// Business code (agent/conversation/knowledge, in later phases) only ever
// depends on this interface, never on a concrete adapter.
type Client interface {
	Chat(ctx context.Context, req ChatRequest) (Message, error)
	ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error)
	Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error)
	TestConnection(ctx context.Context) error
}
