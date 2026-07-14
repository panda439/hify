package mcp

import (
	"encoding/json"
	"time"
)

type createServerRequest struct {
	Name      string            `json:"name" binding:"required"`
	Transport string            `json:"transport" binding:"required,oneof=stdio sse"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
}

type updateServerRequest struct {
	Name     string            `json:"name" binding:"required"`
	Command  string            `json:"command"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	IsActive bool              `json:"is_active"`
}

// serverResponse deliberately returns Env/Headers as key lists, not the
// full maps — values may hold secrets (an SSE Authorization header, an API
// key passed as a stdio env var), same asymmetry as provider's api_key
// (write full value, read back only a masked indicator).
type serverResponse struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Transport    string     `json:"transport"`
	Command      string     `json:"command,omitempty"`
	Args         []string   `json:"args,omitempty"`
	EnvKeys      []string   `json:"env_keys,omitempty"`
	URL          string     `json:"url,omitempty"`
	HeaderKeys   []string   `json:"header_keys,omitempty"`
	Status       string     `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	LastError    string     `json:"last_error"`
	IsActive     bool       `json:"is_active"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func toServerResponse(s Server) serverResponse {
	return serverResponse{
		ID:           s.ID,
		Name:         s.Name,
		Transport:    s.Transport,
		Command:      s.Command,
		Args:         s.Args,
		EnvKeys:      mapKeys(s.Env),
		URL:          s.URL,
		HeaderKeys:   mapKeys(s.Headers),
		Status:       s.Status,
		LastSyncedAt: s.LastSyncedAt,
		LastError:    s.LastError,
		IsActive:     s.IsActive,
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

func mapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type toolResponse struct {
	ID          string          `json:"id"`
	ServerID    string          `json:"mcp_server_id"`
	ToolName    string          `json:"tool_name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	IsActive    bool            `json:"is_active"`
}

func toToolResponse(t Tool) toolResponse {
	return toolResponse{
		ID:          t.ID,
		ServerID:    t.ServerID,
		ToolName:    t.ToolName,
		Description: t.Description,
		InputSchema: t.InputSchema,
		IsActive:    t.IsActive,
	}
}
