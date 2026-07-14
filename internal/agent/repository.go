package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"hify/internal/db/gen"
	"hify/internal/platform"
)

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	db      *sql.DB
	queries *gen.Queries
}

func (r *Repository) createAgent(ctx context.Context, a Agent) error {
	extraParamsJSON, err := json.Marshal(a.ExtraParams)
	if err != nil {
		return fmt.Errorf("agent: marshal extra_params: %w", err)
	}

	err = r.queries.CreateAgent(ctx, gen.CreateAgentParams{
		ID:           a.ID,
		Name:         a.Name,
		Description:  a.Description,
		ModelID:      a.ModelID,
		SystemPrompt: a.SystemPrompt,
		Temperature:  formatDecimal(a.Temperature),
		MaxTokens:    intPtrToNullInt32(a.MaxTokens),
		TopP:         float64PtrToNullString(a.TopP),
		ExtraParams:  extraParamsJSON,
		CreatedBy:    a.CreatedBy,
	})
	if err != nil {
		return fmt.Errorf("agent: create: %w", err)
	}
	return nil
}

func (r *Repository) getAgent(ctx context.Context, id string) (Agent, error) {
	row, err := r.queries.GetAgentByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Agent{}, ErrNotFound
		}
		return Agent{}, fmt.Errorf("agent: get by id: %w", err)
	}
	a, err := toDomainAgent(row)
	if err != nil {
		return Agent{}, err
	}
	kbIDs, err := r.queries.ListKnowledgeBaseIDsByAgent(ctx, id)
	if err != nil {
		return Agent{}, fmt.Errorf("agent: list knowledge base ids: %w", err)
	}
	a.KnowledgeBaseIDs = kbIDs
	return a, nil
}

// listAgents populates KnowledgeBaseIDs per row (one extra query each) —
// the frontend's edit form reuses list rows to prefill instead of a
// separate GET (see agents.tsx), so the list response needs the same
// fields getAgent returns. Acceptable N+1 on this small, offset-paginated
// table, same tradeoff as knowledge's TotalChunks and conversation's
// last_message preview.
func (r *Repository) listAgents(ctx context.Context, limit, offset int) ([]Agent, error) {
	rows, err := r.queries.ListAgents(ctx, gen.ListAgentsParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("agent: list: %w", err)
	}
	out := make([]Agent, 0, len(rows))
	for _, row := range rows {
		a, err := toDomainAgent(row)
		if err != nil {
			return nil, err
		}
		kbIDs, err := r.queries.ListKnowledgeBaseIDsByAgent(ctx, a.ID)
		if err != nil {
			return nil, fmt.Errorf("agent: list knowledge base ids: %w", err)
		}
		a.KnowledgeBaseIDs = kbIDs
		out = append(out, a)
	}
	return out, nil
}

// setKnowledgeBases replaces the full set of knowledge bases associated
// with agentID — delete-then-reinsert inside a transaction, matching the
// "replace-all semantics" the query file documents.
func (r *Repository) setKnowledgeBases(ctx context.Context, agentID string, knowledgeBaseIDs []string) error {
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		if err := q.DeleteAgentKnowledgeBases(ctx, agentID); err != nil {
			return fmt.Errorf("agent: clear knowledge base associations: %w", err)
		}
		for _, kbID := range knowledgeBaseIDs {
			if err := q.CreateAgentKnowledgeBase(ctx, gen.CreateAgentKnowledgeBaseParams{
				AgentID:         agentID,
				KnowledgeBaseID: kbID,
			}); err != nil {
				return fmt.Errorf("agent: associate knowledge base: %w", err)
			}
		}
		return nil
	})
}

func (r *Repository) countAgents(ctx context.Context) (int, error) {
	n, err := r.queries.CountAgents(ctx)
	if err != nil {
		return 0, fmt.Errorf("agent: count: %w", err)
	}
	return int(n), nil
}

func (r *Repository) updateAgent(ctx context.Context, a Agent) error {
	extraParamsJSON, err := json.Marshal(a.ExtraParams)
	if err != nil {
		return fmt.Errorf("agent: marshal extra_params: %w", err)
	}

	err = r.queries.UpdateAgent(ctx, gen.UpdateAgentParams{
		Name:         a.Name,
		Description:  a.Description,
		ModelID:      a.ModelID,
		SystemPrompt: a.SystemPrompt,
		Temperature:  formatDecimal(a.Temperature),
		MaxTokens:    intPtrToNullInt32(a.MaxTokens),
		TopP:         float64PtrToNullString(a.TopP),
		ExtraParams:  extraParamsJSON,
		IsActive:     a.IsActive,
		ID:           a.ID,
	})
	if err != nil {
		return fmt.Errorf("agent: update: %w", err)
	}
	return nil
}

func toDomainAgent(row gen.Agent) (Agent, error) {
	temperature, err := strconv.ParseFloat(row.Temperature, 64)
	if err != nil {
		return Agent{}, fmt.Errorf("agent: parse temperature: %w", err)
	}

	var extraParams map[string]any
	if len(row.ExtraParams) > 0 {
		if err := json.Unmarshal(row.ExtraParams, &extraParams); err != nil {
			return Agent{}, fmt.Errorf("agent: unmarshal extra_params: %w", err)
		}
	}

	return Agent{
		ID:           row.ID,
		Name:         row.Name,
		Description:  row.Description,
		ModelID:      row.ModelID,
		SystemPrompt: row.SystemPrompt,
		Temperature:  temperature,
		MaxTokens:    nullInt32ToPtr(row.MaxTokens),
		TopP:         nullStringToFloat64Ptr(row.TopP),
		ExtraParams:  extraParams,
		IsActive:     row.IsActive,
		CreatedBy:    row.CreatedBy,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}, nil
}

// formatDecimal renders a float64 to match the DECIMAL(3,2) column scale —
// consistent formatting matters here because the column is read back as a
// string, not because MySQL cares how the literal is spelled.
func formatDecimal(f float64) string {
	return strconv.FormatFloat(f, 'f', 2, 64)
}

func intPtrToNullInt32(p *int) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(*p), Valid: true}
}

func nullInt32ToPtr(n sql.NullInt32) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int32)
	return &v
}

func float64PtrToNullString(p *float64) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: formatDecimal(*p), Valid: true}
}

func nullStringToFloat64Ptr(n sql.NullString) *float64 {
	if !n.Valid {
		return nil
	}
	v, err := strconv.ParseFloat(n.String, 64)
	if err != nil {
		return nil
	}
	return &v
}
