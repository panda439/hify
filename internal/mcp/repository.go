package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hify/internal/db/gen"
)

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	queries *gen.Queries
}

func (r *Repository) createServer(ctx context.Context, s Server) error {
	argsJSON, envJSON, headersJSON, err := marshalServerJSON(s)
	if err != nil {
		return err
	}
	if err := r.queries.CreateMCPServer(ctx, gen.CreateMCPServerParams{
		ID:        s.ID,
		Name:      s.Name,
		Transport: s.Transport,
		Command:   stringToNullString(s.Command),
		Args:      argsJSON,
		Env:       envJSON,
		Url:       stringToNullString(s.URL),
		Headers:   headersJSON,
		CreatedBy: s.CreatedBy,
	}); err != nil {
		return fmt.Errorf("mcp: create server: %w", err)
	}
	return nil
}

func (r *Repository) getServer(ctx context.Context, id string) (Server, error) {
	row, err := r.queries.GetMCPServerByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Server{}, ErrNotFound
		}
		return Server{}, fmt.Errorf("mcp: get server: %w", err)
	}
	return toDomainServer(row)
}

func (r *Repository) listServers(ctx context.Context, limit, offset int) ([]Server, error) {
	rows, err := r.queries.ListMCPServers(ctx, gen.ListMCPServersParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("mcp: list servers: %w", err)
	}
	out := make([]Server, 0, len(rows))
	for _, row := range rows {
		s, err := toDomainServer(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *Repository) countServers(ctx context.Context) (int, error) {
	n, err := r.queries.CountMCPServers(ctx)
	if err != nil {
		return 0, fmt.Errorf("mcp: count servers: %w", err)
	}
	return int(n), nil
}

func (r *Repository) updateServer(ctx context.Context, s Server) error {
	argsJSON, envJSON, headersJSON, err := marshalServerJSON(s)
	if err != nil {
		return err
	}
	if err := r.queries.UpdateMCPServer(ctx, gen.UpdateMCPServerParams{
		Name:     s.Name,
		Command:  stringToNullString(s.Command),
		Args:     argsJSON,
		Env:      envJSON,
		Url:      stringToNullString(s.URL),
		Headers:  headersJSON,
		IsActive: s.IsActive,
		ID:       s.ID,
	}); err != nil {
		return fmt.Errorf("mcp: update server: %w", err)
	}
	return nil
}

func (r *Repository) updateServerSyncResult(ctx context.Context, id, status, lastError string) error {
	if err := r.queries.UpdateMCPServerSyncResult(ctx, gen.UpdateMCPServerSyncResultParams{
		Status:       status,
		LastSyncedAt: sql.NullTime{Time: time.Now(), Valid: true},
		LastError:    stringToNullString(lastError),
		ID:           id,
	}); err != nil {
		return fmt.Errorf("mcp: update server sync result: %w", err)
	}
	return nil
}

func (r *Repository) upsertTool(ctx context.Context, t Tool) error {
	if err := r.queries.UpsertMCPTool(ctx, gen.UpsertMCPToolParams{
		ID:          t.ID,
		McpServerID: t.ServerID,
		ToolName:    t.ToolName,
		Description: t.Description,
		InputSchema: t.InputSchema,
	}); err != nil {
		return fmt.Errorf("mcp: upsert tool: %w", err)
	}
	return nil
}

func (r *Repository) listToolsByServer(ctx context.Context, serverID string) ([]Tool, error) {
	rows, err := r.queries.ListMCPToolsByServer(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}
	out := make([]Tool, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTool(row))
	}
	return out, nil
}

func (r *Repository) getTool(ctx context.Context, id string) (Tool, error) {
	row, err := r.queries.GetMCPToolByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Tool{}, ErrToolNotFound
		}
		return Tool{}, fmt.Errorf("mcp: get tool: %w", err)
	}
	return toDomainTool(row), nil
}

func (r *Repository) deactivateTool(ctx context.Context, id string) error {
	if err := r.queries.UpdateMCPToolActive(ctx, gen.UpdateMCPToolActiveParams{
		IsActive: false,
		ID:       id,
	}); err != nil {
		return fmt.Errorf("mcp: deactivate tool: %w", err)
	}
	return nil
}

func marshalServerJSON(s Server) (args, env, headers []byte, err error) {
	args, err = json.Marshal(s.Args)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp: marshal args: %w", err)
	}
	env, err = json.Marshal(s.Env)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp: marshal env: %w", err)
	}
	headers, err = json.Marshal(s.Headers)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("mcp: marshal headers: %w", err)
	}
	return args, env, headers, nil
}

func toDomainServer(row gen.McpServer) (Server, error) {
	var args []string
	if len(row.Args) > 0 {
		if err := json.Unmarshal(row.Args, &args); err != nil {
			return Server{}, fmt.Errorf("mcp: unmarshal args: %w", err)
		}
	}
	var env map[string]string
	if len(row.Env) > 0 {
		if err := json.Unmarshal(row.Env, &env); err != nil {
			return Server{}, fmt.Errorf("mcp: unmarshal env: %w", err)
		}
	}
	var headers map[string]string
	if len(row.Headers) > 0 {
		if err := json.Unmarshal(row.Headers, &headers); err != nil {
			return Server{}, fmt.Errorf("mcp: unmarshal headers: %w", err)
		}
	}
	var lastSyncedAt *time.Time
	if row.LastSyncedAt.Valid {
		t := row.LastSyncedAt.Time
		lastSyncedAt = &t
	}
	return Server{
		ID:           row.ID,
		Name:         row.Name,
		Transport:    row.Transport,
		Command:      row.Command.String,
		Args:         args,
		Env:          env,
		URL:          row.Url.String,
		Headers:      headers,
		Status:       row.Status,
		LastSyncedAt: lastSyncedAt,
		LastError:    row.LastError.String,
		IsActive:     row.IsActive,
		CreatedBy:    row.CreatedBy,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}, nil
}

func toDomainTool(row gen.McpTool) Tool {
	return Tool{
		ID:          row.ID,
		ServerID:    row.McpServerID,
		ToolName:    row.ToolName,
		Description: row.Description,
		InputSchema: row.InputSchema,
		IsActive:    row.IsActive,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}
}

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
