package conversation

import (
	"fmt"
	"strings"
	"time"

	"hify/internal/knowledge"
)

// Conversation is scoped to a single owning user — unlike Agent, it is
// never shared, so any cross-user access must read as "not found" (see
// errors.go), never "forbidden".
type Conversation struct {
	ID        string
	AgentID   string
	UserID    string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// LastMessage is the sidebar preview snippet — only populated by
	// ListConversations (a per-row subquery), empty on every other path
	// (Create/Get) since those don't need it and it'd cost an extra query.
	LastMessage string
}

// Message is one turn in a Conversation. ToolCalls/ToolCallID are wired
// through the schema now (Phase 4 will populate them) but stay unused/empty
// in Phase 2.
type Message struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	ToolCalls      []byte
	ToolCallID     string
	TokenCount     int
	CreatedAt      time.Time
}

// StreamEvent is one frame of an in-progress SendMessage SSE stream. Once
// the stream has started, even a failure surfaces as an Error-typed event,
// never a second HTTP response — see conversation/handler.go.
type StreamEvent struct {
	Type      string               `json:"type"`
	Content   string               `json:"content,omitempty"`
	Error     string               `json:"error,omitempty"`
	Retrieved []RetrievedChunkInfo `json:"retrieved,omitempty"`
	ToolCall  *ToolCallInfo        `json:"tool_call,omitempty"`
}

// ToolCallInfo backs the chat UI's "工具调用过程" display — a running/done/
// error trace of one MCP tool invocation within the current turn.
type ToolCallInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"` // running | done | error
	Result string `json:"result,omitempty"`
}

// RetrievedChunkInfo is what the debug panel actually needs from a
// knowledge.RetrievedChunk — deliberately not the domain type itself
// (wrong JSON shape: no tags, PascalCase; and it carries the full
// embedding vector, which has zero UI value and would bloat every
// retrieval event for nothing).
type RetrievedChunkInfo struct {
	KnowledgeBaseID string  `json:"knowledge_base_id"`
	DocumentID      string  `json:"document_id"`
	Content         string  `json:"content"`
	Score           float64 `json:"score"`
}

func toRetrievedChunkInfo(chunks []knowledge.RetrievedChunk) []RetrievedChunkInfo {
	out := make([]RetrievedChunkInfo, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, RetrievedChunkInfo{
			KnowledgeBaseID: c.KnowledgeBaseID,
			DocumentID:      c.DocumentID,
			Content:         c.Content,
			Score:           c.Score,
		})
	}
	return out
}

const (
	// EventRetrieval fires once, before the first delta, only when the
	// Agent has knowledge bases attached and retrieval found something —
	// backs the chat UI's debug panel (see the plan's RAG design).
	EventRetrieval = "retrieval"
	// EventToolCall fires twice per tool invocation (status=running, then
	// status=done|error) — backs the chat UI's tool-call trace.
	EventToolCall = "tool_call"
	EventDelta    = "delta"
	EventDone     = "done"
	EventError    = "error"
)

// MessageCursor is the keyset cursor for paging a conversation's message
// history — see CLAUDE.md's "大表一律用游标分页" rule (messages is one of
// the tables that rule targets).
type MessageCursor struct {
	CreatedAt time.Time
	ID        string
}

// EncodeCursor/DecodeCursor keep the wire representation opaque to the
// frontend (it just echoes whatever string it was given back as a query
// param) — "_" is safe as a separator since it appears in neither
// RFC3339Nano timestamps nor UUIDv7 strings.
func EncodeCursor(c MessageCursor) string {
	return c.CreatedAt.Format(time.RFC3339Nano) + "_" + c.ID
}

func DecodeCursor(s string) (MessageCursor, error) {
	parts := strings.SplitN(s, "_", 2)
	if len(parts) != 2 {
		return MessageCursor{}, fmt.Errorf("conversation: malformed cursor")
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return MessageCursor{}, fmt.Errorf("conversation: malformed cursor timestamp: %w", err)
	}
	return MessageCursor{CreatedAt: t, ID: parts[1]}, nil
}
