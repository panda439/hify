package conversation

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
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
	TraceID   string               `json:"trace_id,omitempty"`
	Content   string               `json:"content,omitempty"`
	Error     string               `json:"error,omitempty"`
	Retrieved []RetrievedChunkInfo `json:"retrieved,omitempty"`
	ToolCall  *ToolCallInfo        `json:"tool_call,omitempty"`
	// Citations is populated only on the EventFinal frame — see
	// runStream's doc comment for why citations can't be known until the
	// full streamed answer has been parsed. The `omitempty` tag here only
	// governs every OTHER event type (retrieval/delta/tool_call/done/error
	// never carry a citations field at all, which is what omitempty is
	// for) — MarshalJSON below overrides this for EventFinal specifically,
	// since a final event with no citations must still serialize the
	// field as `[]`, never be silently dropped by the same omitempty rule.
	Citations []CitationResponse `json:"citations,omitempty"`
}

// finalStreamEventPayload is EventFinal's actual wire shape — Citations
// has no omitempty, and MarshalJSON below always substitutes a non-nil
// slice, so `"citations":[]` renders even when the model cited nothing.
// This is what makes the SSE final contract stable for a frontend: it
// never needs to distinguish "citations absent" from "no citations", only
// ever the latter (see CLAUDE.md's Citation V1 review fix).
type finalStreamEventPayload struct {
	Type      string             `json:"type"`
	TraceID   string             `json:"trace_id,omitempty"`
	Content   string             `json:"content"`
	Citations []CitationResponse `json:"citations"`
}

// MarshalJSON gives EventFinal its own stable shape (see
// finalStreamEventPayload) while leaving every other event type's
// encoding exactly as its struct tags already describe — the type alias
// avoids infinite recursion into this same method.
func (e StreamEvent) MarshalJSON() ([]byte, error) {
	if e.Type == EventFinal {
		citations := e.Citations
		if citations == nil {
			citations = []CitationResponse{}
		}
		return json.Marshal(finalStreamEventPayload{
			Type: e.Type, TraceID: e.TraceID, Content: e.Content, Citations: citations,
		})
	}
	type alias StreamEvent
	return json.Marshal(alias(e))
}

// ToolCallInfo backs the chat UI's "工具调用过程" display — a running/done/
// error trace of one MCP tool invocation within the current turn.
type ToolCallInfo struct {
	Name   string `json:"name"`
	Status string `json:"status"` // running | done | error
	Result string `json:"result,omitempty"`
}

// RetrievedChunkInfo is what the debug panel actually needs from an
// Evidence — deliberately not the domain type itself (wrong JSON shape: no
// tags, PascalCase). Built from the final, post-filter Evidence set (see
// context.go's assembleContext), not the raw candidates knowledge.Retrieve
// returned — this is "what the model actually saw", the same set the
// citation ref numbers are drawn from.
type RetrievedChunkInfo struct {
	Ref             string  `json:"ref"`
	KnowledgeBaseID string  `json:"knowledge_base_id"`
	DocumentID      string  `json:"document_id"`
	DocumentName    string  `json:"document_name"`
	Content         string  `json:"content"`
	Score           float64 `json:"score"`
}

func toRetrievedChunkInfo(evidence []Evidence) []RetrievedChunkInfo {
	out := make([]RetrievedChunkInfo, 0, len(evidence))
	for _, e := range evidence {
		out = append(out, RetrievedChunkInfo{
			Ref:             e.Ref,
			KnowledgeBaseID: e.KnowledgeBaseID,
			DocumentID:      e.DocumentID,
			DocumentName:    e.DocumentName,
			Content:         e.Content,
			Score:           e.Score,
		})
	}
	return out
}

// Citation is one persisted message_citations row — a snapshot of the
// evidence a specific assistant message actually cited, independent of
// whatever the source document/chunk looks like now (see the migration
// comment: no cross-database FK, by design). PageNumber/SectionTitle are
// deliberately not columns here (see message_citations' schema) — Citation
// V1 never has real values for them (knowledge's parser doesn't produce
// them), so there's nothing to persist yet; ChunkIndex/Score/Quote already
// carry everything a citation needs to be independently useful once a
// document is gone.
type Citation struct {
	MessageID       string
	Ref             string
	KnowledgeBaseID string
	DocumentID      string
	DocumentName    string
	ChunkID         string
	ChunkIndex      int
	Quote           string
	Score           float64
	CreatedAt       time.Time
}

// evidenceToCitations converts the subset of this turn's Evidence that the
// model actually cited (validRefs, in normalizeCitations' first-occurrence
// order) into the rows createMessageWithCitations will persist — this is
// the one function responsible for the "quote == what the model actually
// saw" invariant: Quote comes from Evidence.Content, never re-fetched from
// knowledge or re-derived from the model's own answer text.
func evidenceToCitations(messageID string, evidence []Evidence, validRefs []string) []Citation {
	byRef := make(map[string]Evidence, len(evidence))
	for _, e := range evidence {
		byRef[e.Ref] = e
	}
	out := make([]Citation, 0, len(validRefs))
	for _, ref := range validRefs {
		e, ok := byRef[ref]
		if !ok {
			// normalizeCitations is only ever called with allowedRefs built
			// from this same evidence slice, so this can't happen in
			// practice — guarded rather than trusted, since a silent
			// mismatch here would mean a citation "cites" evidence that was
			// never actually sent to the model.
			continue
		}
		out = append(out, Citation{
			MessageID:       messageID,
			Ref:             e.Ref,
			KnowledgeBaseID: e.KnowledgeBaseID,
			DocumentID:      e.DocumentID,
			DocumentName:    e.DocumentName,
			ChunkID:         e.ChunkID,
			ChunkIndex:      e.ChunkIndex,
			Quote:           e.Content,
			Score:           e.Score,
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
	// EventFinal is the authoritative post-processing frame: normalized
	// content (invalid [Sx] refs stripped) and the structured citations
	// that were actually persisted — sent once, always immediately before
	// EventDone, on every path that reaches a real answer (not sent on the
	// mid-stream-error/disconnect paths, where there's no "final" content
	// to speak of). See runStream's doc comment for the full ordering
	// contract.
	EventFinal = "final"
	EventDone  = "done"
	EventError = "error"
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
