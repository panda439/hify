package conversation

import (
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
	Type    string `json:"type"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	EventDelta = "delta"
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
