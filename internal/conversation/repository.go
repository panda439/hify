package conversation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"hify/internal/db/gen"
	"hify/internal/platform"
)

// Repository is constructed via NewRepository in wire.go. db is kept
// alongside queries (rather than only ever going through queries) because
// createMessageWithCitations needs platform.WithTx's raw *sql.Tx to bind a
// fresh gen.Queries for the transaction — the same shape knowledge's
// Repository already uses for its own PG transactions.
type Repository struct {
	db      *sql.DB
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

// createMessageWithCitations is the one write path where an assistant
// message and its citations must succeed or fail together — see
// CLAUDE.md's Citation V1 spec section 4: a citation write failure must
// roll back the message too, never leave a message saved with its
// citations silently missing. Both statements run in the same MySQL
// transaction via platform.WithTx; citations is allowed to be empty (a
// normal answer with no [Sx] refs), in which case this is just the message
// insert.
func (r *Repository) createMessageWithCitations(ctx context.Context, m Message, citations []Citation) error {
	toolCalls := m.ToolCalls
	if len(toolCalls) == 0 {
		toolCalls = []byte("null")
	}

	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		if err := q.CreateMessage(ctx, gen.CreateMessageParams{
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
		for _, c := range citations {
			if err := q.CreateMessageCitation(ctx, gen.CreateMessageCitationParams{
				MessageID:       c.MessageID,
				Ref:             c.Ref,
				KnowledgeBaseID: c.KnowledgeBaseID,
				DocumentID:      c.DocumentID,
				DocumentName:    c.DocumentName,
				ChunkID:         c.ChunkID,
				ChunkIndex:      int32(c.ChunkIndex),
				Quote:           c.Quote,
				Score:           formatCitationScore(c.Score),
			}); err != nil {
				return fmt.Errorf("conversation: create message citation: %w", err)
			}
		}
		return nil
	})
}

// listCitationsByMessageIDs batch-loads every citation for a page of
// messages in one query — see CLAUDE.md's N+1 rule (spec section 11.1).
// The returned map only ever has entries for message IDs that actually
// have citations; callers must treat a missing key the same as an empty
// slice.
func (r *Repository) listCitationsByMessageIDs(ctx context.Context, messageIDs []string) (map[string][]Citation, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	rows, err := r.queries.ListCitationsByMessageIDs(ctx, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("conversation: list citations by message ids: %w", err)
	}
	out := make(map[string][]Citation, len(messageIDs))
	for _, row := range rows {
		c, err := toDomainCitation(row)
		if err != nil {
			return nil, err
		}
		out[c.MessageID] = append(out[c.MessageID], c)
	}
	// Sort each message's citations by ref number ascending (S1, S2, S10,
	// ...) — the SQL's ORDER BY message_id says nothing about intra-group
	// order, and a lexicographic ref sort would put "S10" before "S2".
	for id := range out {
		refs := out[id]
		sort.Slice(refs, func(i, j int) bool { return refNumber(refs[i].Ref) < refNumber(refs[j].Ref) })
	}
	return out, nil
}

// refNumber parses the numeric suffix of a "S<n>" ref for sorting — every
// ref in message_citations was produced by evidenceToCitations from a
// selectEvidence-assigned Ref, so the "S" prefix and valid integer suffix
// are guaranteed; a malformed value (which should be unreachable) sorts as
// 0 rather than panicking.
func refNumber(ref string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(ref, "S"))
	if err != nil {
		return 0
	}
	return n
}

// formatCitationScore/parseCitationScore bridge Citation.Score (float64,
// the shape every other layer works with) and message_citations.score's
// DECIMAL(5,4) column, which sqlc represents as a plain string — MySQL
// drivers don't have a native decimal Go type, and round-tripping through
// float64 via Sprintf/ParseFloat (rather than passing arbitrary-precision
// strings around) is fine here since a similarity score is never used for
// exact/financial comparisons.
func formatCitationScore(score float64) string {
	return strconv.FormatFloat(score, 'f', 4, 64)
}

func parseCitationScore(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

func toDomainCitation(row gen.MessageCitation) (Citation, error) {
	score, err := parseCitationScore(row.Score)
	if err != nil {
		return Citation{}, fmt.Errorf("conversation: parse citation score %q: %w", row.Score, err)
	}
	return Citation{
		MessageID:       row.MessageID,
		Ref:             row.Ref,
		KnowledgeBaseID: row.KnowledgeBaseID,
		DocumentID:      row.DocumentID,
		DocumentName:    row.DocumentName,
		ChunkID:         row.ChunkID,
		ChunkIndex:      int(row.ChunkIndex),
		Quote:           row.Quote,
		Score:           score,
		CreatedAt:       row.CreatedAt,
	}, nil
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
