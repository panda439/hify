package trace

import (
	"context"
	"database/sql"
	"fmt"

	"hify/internal/db/gen"
)

// Store persists spans directly via sqlc-generated queries — the same
// repository.go shape every business module uses, just living in platform
// since trace recording is a cross-cutting concern, not a business entity
// with its own CRUD/handler surface.
type Store struct {
	queries *gen.Queries
}

func NewStore(db *sql.DB) *Store {
	return &Store{queries: gen.New(db)}
}

// Record writes one finished span. Callers must not let a Record failure
// abort the conversation/LLM call it's describing — trace recording is a
// side channel, and a broken audit trail is a much smaller problem than a
// broken chat reply. Record itself just reports the error; it's on the
// caller to log-and-continue rather than propagate it.
func (s *Store) Record(ctx context.Context, span Span) error {
	if span.Attrs == nil {
		// attrs is NOT NULL — a span constructed without any attributes
		// (e.g. the root turn span) would otherwise fail the insert.
		span.Attrs = Attrs(nil)
	}
	if err := s.queries.CreateTraceSpan(ctx, gen.CreateTraceSpanParams{
		ID:             span.ID,
		TraceID:        span.TraceID,
		ParentSpanID:   span.ParentSpanID,
		ConversationID: span.ConversationID,
		Kind:           span.Kind,
		Name:           span.Name,
		Status:         span.Status,
		Input:          span.Input,
		Output:         span.Output,
		ErrorMessage:   span.ErrorMessage,
		Attrs:          span.Attrs,
		StartedAt:      span.StartedAt,
		FinishedAt:     span.FinishedAt,
	}); err != nil {
		return fmt.Errorf("trace: record span: %w", err)
	}
	return nil
}

// ListByConversation returns every span recorded for conversationID, across
// all of its turns, oldest first — callers that only want one turn's spans
// (e.g. eval harness) filter client-side by TraceID.
func (s *Store) ListByConversation(ctx context.Context, conversationID string) ([]Span, error) {
	rows, err := s.queries.ListTraceSpansByConversation(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("trace: list spans by conversation: %w", err)
	}
	out := make([]Span, 0, len(rows))
	for _, row := range rows {
		out = append(out, Span{
			ID:             row.ID,
			TraceID:        row.TraceID,
			ParentSpanID:   row.ParentSpanID,
			ConversationID: row.ConversationID,
			Kind:           row.Kind,
			Name:           row.Name,
			Status:         row.Status,
			Input:          row.Input,
			Output:         row.Output,
			ErrorMessage:   row.ErrorMessage,
			Attrs:          row.Attrs,
			StartedAt:      row.StartedAt,
			FinishedAt:     row.FinishedAt,
		})
	}
	return out, nil
}
