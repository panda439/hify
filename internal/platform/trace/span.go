// Package trace records the execution trace of one agent turn — each user
// message sent to conversation.Service.StreamMessage produces one trace
// (root span kind=KindTurn), with child spans for retrieval, each LLM call,
// and each tool call. It is infrastructure, not a business module: no
// handler/dto, called directly by conversation/provider the same way they
// already call platform.NewID/platform.ClampLimit.
package trace

import (
	"encoding/json"
	"time"
)

const (
	KindTurn      = "turn"
	KindRetrieval = "retrieval"
	KindLLMCall   = "llm_call"
	KindToolCall  = "tool_call"

	StatusOK    = "ok"
	StatusError = "error"
)

// Span is one node of a trace. ParentSpanID is empty for the root (kind ==
// KindTurn) span; every other span's ParentSpanID points at that root — a
// flat two-level hierarchy is enough for one conversation turn, unlike a
// general-purpose tracer that nests arbitrarily deep.
type Span struct {
	ID             string
	TraceID        string
	ParentSpanID   string
	ConversationID string
	Kind           string
	Name           string
	Status         string
	Input          string
	Output         string
	ErrorMessage   string
	Attrs          json.RawMessage
	StartedAt      time.Time
	FinishedAt     time.Time
}
