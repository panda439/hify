package conversation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"hify/internal/db/gen"
)

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	queries *gen.Queries
}

func (r *Repository) createConversation(ctx context.Context, conv Conversation) error {
	if err := r.queries.CreateConversation(ctx, gen.CreateConversationParams{
		ID:      conv.ID,
		AgentID: conv.AgentID,
		UserID:  conv.UserID,
		Title:   conv.Title,
	}); err != nil {
		return fmt.Errorf("conversation: create: %w", err)
	}
	return nil
}

// getConversationForUser is the single privacy gate: a row that doesn't
// exist and a row owned by someone else both come back as ErrNotFound, so
// callers can't distinguish "wrong id" from "not yours" (see errors.go).
func (r *Repository) getConversationForUser(ctx context.Context, id, userID string) (Conversation, error) {
	row, err := r.queries.GetConversationByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Conversation{}, ErrNotFound
		}
		return Conversation{}, fmt.Errorf("conversation: get by id: %w", err)
	}
	if row.UserID != userID {
		return Conversation{}, ErrNotFound
	}
	return toDomainConversation(row), nil
}

func (r *Repository) listConversationsByUser(ctx context.Context, userID string, limit, offset int) ([]Conversation, error) {
	rows, err := r.queries.ListConversationsByUser(ctx, gen.ListConversationsByUserParams{
		UserID: userID,
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("conversation: list: %w", err)
	}
	out := make([]Conversation, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainConversationWithPreview(row))
	}
	return out, nil
}

func (r *Repository) countConversationsByUser(ctx context.Context, userID string) (int, error) {
	n, err := r.queries.CountConversationsByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("conversation: count: %w", err)
	}
	return int(n), nil
}

func (r *Repository) touchConversation(ctx context.Context, id string, at time.Time) error {
	if err := r.queries.TouchConversation(ctx, gen.TouchConversationParams{
		UpdatedAt: at,
		ID:        id,
	}); err != nil {
		return fmt.Errorf("conversation: touch: %w", err)
	}
	return nil
}

func (r *Repository) createMessage(ctx context.Context, m Message) error {
	// json.RawMessage doesn't implement sql.Scanner, so a column holding a
	// true SQL NULL fails to scan back out ("unsupported Scan ... driver.Value
	// type <nil>"). Store the JSON literal null instead of a bare nil slice
	// whenever there are no tool calls yet — same convention provider.go
	// uses for extra_headers/extra_config.
	toolCalls := m.ToolCalls
	if len(toolCalls) == 0 {
		toolCalls = []byte("null")
	}

	if err := r.queries.CreateMessage(ctx, gen.CreateMessageParams{
		ID:             m.ID,
		ConversationID: m.ConversationID,
		Role:           m.Role,
		Content:        m.Content,
		ToolCalls:      toolCalls,
		ToolCallID:     stringToNullString(m.ToolCallID),
		TokenCount:     int32(m.TokenCount),
	}); err != nil {
		return fmt.Errorf("conversation: create message: %w", err)
	}
	return nil
}

// listRecentMessages returns up to limit messages, newest first — always
// filtered by conversation_id per CLAUDE.md's large-table rule. Used both
// for context assembly (see context.go) and the first page of history.
func (r *Repository) listRecentMessages(ctx context.Context, conversationID string, limit int) ([]Message, error) {
	rows, err := r.queries.ListRecentMessagesByConversation(ctx, gen.ListRecentMessagesByConversationParams{
		ConversationID: conversationID,
		Limit:          int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("conversation: list recent messages: %w", err)
	}
	out := make([]Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainMessage(row))
	}
	return out, nil
}

func (r *Repository) listMessagesBeforeCursor(ctx context.Context, conversationID string, cursor MessageCursor, limit int) ([]Message, error) {
	rows, err := r.queries.ListMessagesByConversationBeforeCursor(ctx, gen.ListMessagesByConversationBeforeCursorParams{
		ConversationID: conversationID,
		CreatedAt:      cursor.CreatedAt,
		CreatedAt_2:    cursor.CreatedAt,
		ID:             cursor.ID,
		Limit:          int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("conversation: list messages before cursor: %w", err)
	}
	out := make([]Message, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainMessage(row))
	}
	return out, nil
}

func toDomainConversation(row gen.Conversation) Conversation {
	return Conversation{
		ID:        row.ID,
		AgentID:   row.AgentID,
		UserID:    row.UserID,
		Title:     row.Title,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// toDomainConversationWithPreview handles ListConversationsByUserRow, whose
// last_message column sqlc can't type as a plain string (the COALESCE'd
// correlated subquery defeats its MySQL type inference) — it comes back as
// any, holding a []byte from the driver in practice.
func toDomainConversationWithPreview(row gen.ListConversationsByUserRow) Conversation {
	return Conversation{
		ID:          row.ID,
		AgentID:     row.AgentID,
		UserID:      row.UserID,
		Title:       row.Title,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
		LastMessage: anyToString(row.LastMessage),
	}
}

func anyToString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

func toDomainMessage(row gen.Message) Message {
	return Message{
		ID:             row.ID,
		ConversationID: row.ConversationID,
		Role:           row.Role,
		Content:        row.Content,
		ToolCalls:      row.ToolCalls,
		ToolCallID:     row.ToolCallID.String,
		TokenCount:     int(row.TokenCount),
		CreatedAt:      row.CreatedAt,
	}
}

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
