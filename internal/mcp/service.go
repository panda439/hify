package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"hify/internal/platform"
)

// Service is mcp's public contract — the only thing higher-layer modules
// (agent, conversation) are allowed to depend on.
type Service interface {
	CreateServer(ctx context.Context, input CreateServerInput) (Server, error)
	ListServers(ctx context.Context, limit, offset int) ([]Server, int, error)
	GetServer(ctx context.Context, id string) (Server, error)
	UpdateServer(ctx context.Context, id string, input UpdateServerInput) (Server, error)

	// SyncTools connects to the server, lists its tools, upserts them
	// (reactivating any that reappeared), and deactivates any previously
	// known tool that's no longer offered — see the doc comment on the
	// implementation for why deactivate, not delete.
	SyncTools(ctx context.Context, serverID string) ([]Tool, error)
	ListTools(ctx context.Context, serverID string) ([]Tool, error)
	// ListActiveTools backs the Agent form's tool picker — any
	// authenticated user needs this (not just admins), same reasoning as
	// provider.Service.ListModelsByCapability.
	ListActiveTools(ctx context.Context) ([]Tool, error)
	GetTool(ctx context.Context, toolID string) (Tool, error)

	// CallTool is what conversation's tool-calling loop invokes — not
	// exposed over HTTP, only through this interface.
	CallTool(ctx context.Context, toolID string, arguments json.RawMessage) (ToolCallResult, error)
}

// service is constructed via NewService in wire.go.
type service struct {
	repo *Repository
}

func (s *service) CreateServer(ctx context.Context, input CreateServerInput) (Server, error) {
	if err := validateTransportConfig(input.Transport, input.Command, input.URL); err != nil {
		return Server{}, err
	}

	srv := Server{
		ID:        platform.NewID(),
		Name:      input.Name,
		Transport: input.Transport,
		Command:   input.Command,
		Args:      input.Args,
		Env:       input.Env,
		URL:       input.URL,
		Headers:   input.Headers,
		Status:    StatusUnknown,
		IsActive:  true,
		CreatedBy: input.CreatedBy,
	}
	if err := s.repo.createServer(ctx, srv); err != nil {
		return Server{}, err
	}
	return s.repo.getServer(ctx, srv.ID)
}

func validateTransportConfig(transport, command, url string) error {
	switch transport {
	case TransportStdio:
		if command == "" {
			return ErrInvalidRequest
		}
	case TransportSSE:
		if url == "" {
			return ErrInvalidRequest
		}
	default:
		return ErrUnsupportedTransport
	}
	return nil
}

func (s *service) ListServers(ctx context.Context, limit, offset int) ([]Server, int, error) {
	limit = platform.ClampLimit(limit)
	servers, err := s.repo.listServers(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countServers(ctx)
	if err != nil {
		return nil, 0, err
	}
	return servers, total, nil
}

func (s *service) GetServer(ctx context.Context, id string) (Server, error) {
	return s.repo.getServer(ctx, id)
}

func (s *service) UpdateServer(ctx context.Context, id string, input UpdateServerInput) (Server, error) {
	existing, err := s.repo.getServer(ctx, id)
	if err != nil {
		return Server{}, err
	}
	if err := validateTransportConfig(existing.Transport, input.Command, input.URL); err != nil {
		return Server{}, err
	}

	existing.Name = input.Name
	existing.Command = input.Command
	existing.Args = input.Args
	existing.URL = input.URL
	existing.IsActive = input.IsActive
	// Env/Headers may hold secrets (an SSE Authorization header, an API key
	// passed as a stdio env var) and the API only ever returns their key
	// names back, never values (see serverResponse) — so unlike the other
	// fields, a nil map here means "the client didn't touch this section",
	// not "clear it". An explicit empty map (present in the JSON body as
	// `{}`, not omitted) still clears it, same write-full/read-masked
	// asymmetry as provider's api_key.
	if input.Env != nil {
		existing.Env = input.Env
	}
	if input.Headers != nil {
		existing.Headers = input.Headers
	}

	if err := s.repo.updateServer(ctx, existing); err != nil {
		return Server{}, err
	}
	return s.repo.getServer(ctx, id)
}

// SyncTools is a synchronous, admin-triggered action (not a background
// job) — connecting to an MCP server and listing its tools is a single
// request/response round trip, nothing like the multi-minute document
// processing pipeline knowledge base uploads go through.
func (s *service) SyncTools(ctx context.Context, serverID string) ([]Tool, error) {
	srv, err := s.repo.getServer(ctx, serverID)
	if err != nil {
		return nil, err
	}

	remoteTools, err := listRemoteTools(ctx, srv)
	if err != nil {
		syncErr := fmt.Errorf("%w: %v", ErrConnectFailed, err)
		_ = s.repo.updateServerSyncResult(ctx, serverID, StatusFailed, syncErr.Error())
		return nil, syncErr
	}

	existing, err := s.repo.listToolsByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(remoteTools))
	for _, rt := range remoteTools {
		schema, err := toolInputSchemaJSON(rt)
		if err != nil {
			return nil, err
		}
		if err := s.repo.upsertTool(ctx, Tool{
			ID:          platform.NewID(), // discarded by ON DUPLICATE KEY UPDATE when the (server_id, tool_name) pair already exists
			ServerID:    serverID,
			ToolName:    rt.Name,
			Description: rt.Description,
			InputSchema: schema,
		}); err != nil {
			return nil, err
		}
		seen[rt.Name] = true
	}

	// A tool that existed before but isn't in this sync's results has
	// disappeared from the server — deactivate rather than delete, same
	// reasoning as provider_models: agent_mcp_tools may still reference
	// it, and hard-deleting would leave that association pointing at
	// nothing instead of a visibly-disabled row.
	for _, t := range existing {
		if !seen[t.ToolName] {
			if err := s.repo.deactivateTool(ctx, t.ID); err != nil {
				return nil, err
			}
		}
	}

	if err := s.repo.updateServerSyncResult(ctx, serverID, StatusConnected, ""); err != nil {
		return nil, err
	}
	return s.repo.listToolsByServer(ctx, serverID)
}

func (s *service) ListTools(ctx context.Context, serverID string) ([]Tool, error) {
	return s.repo.listToolsByServer(ctx, serverID)
}

func (s *service) ListActiveTools(ctx context.Context) ([]Tool, error) {
	servers, err := s.repo.listServers(ctx, platform.MaxPageLimit, 0)
	if err != nil {
		return nil, err
	}
	var out []Tool
	for _, srv := range servers {
		if !srv.IsActive {
			continue
		}
		tools, err := s.repo.listToolsByServer(ctx, srv.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range tools {
			if t.IsActive {
				out = append(out, t)
			}
		}
	}
	return out, nil
}

func (s *service) GetTool(ctx context.Context, toolID string) (Tool, error) {
	return s.repo.getTool(ctx, toolID)
}

func (s *service) CallTool(ctx context.Context, toolID string, arguments json.RawMessage) (ToolCallResult, error) {
	tool, err := s.repo.getTool(ctx, toolID)
	if err != nil {
		return ToolCallResult{}, err
	}
	if !tool.IsActive {
		return ToolCallResult{}, ErrToolInactive
	}

	srv, err := s.repo.getServer(ctx, tool.ServerID)
	if err != nil {
		return ToolCallResult{}, err
	}
	if !srv.IsActive {
		return ToolCallResult{}, ErrToolInactive
	}

	return callRemoteTool(ctx, srv, tool.ToolName, arguments)
}
