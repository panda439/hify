package mcp

import (
	"encoding/json"
	"time"
)

const (
	TransportStdio = "stdio"
	TransportSSE   = "sse"
)

const (
	StatusUnknown   = "unknown"
	StatusConnected = "connected"
	StatusFailed    = "failed"
)

// maxCallToolTimeout bounds a single tool invocation — a hung MCP server
// (stdio subprocess that never responds, or an unreachable SSE endpoint)
// must not block a conversation turn forever.
const maxCallToolTimeout = 30 * time.Second

// Server is a configured MCP connection. Command/Args/Env only apply to
// stdio transport; URL/Headers only apply to sse — deliberately not split
// into two types, since which fields are meaningful is a single `Transport`
// switch away, and both share every other column.
type Server struct {
	ID           string
	Name         string
	Transport    string
	Command      string
	Args         []string
	Env          map[string]string
	URL          string
	Headers      map[string]string
	Status       string
	LastSyncedAt *time.Time
	LastError    string
	IsActive     bool
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Tool is one tool discovered from a Server's tools/list response, cached
// in MySQL so the Agent form and the conversation engine don't need to
// hit the MCP server on every read — only Sync (an explicit admin action)
// talks to the server.
type Tool struct {
	ID          string
	ServerID    string
	ToolName    string
	Description string
	InputSchema json.RawMessage
	IsActive    bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type CreateServerInput struct {
	Name      string
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	CreatedBy string
}

// UpdateServerInput has no ownership-style immutable fields (unlike
// knowledge_bases' embedding model) — re-pointing a server at a different
// command/URL just means the next Sync reconciles against a different
// tool list, there's no stored derived data that becomes incompatible.
type UpdateServerInput struct {
	Name     string
	Command  string
	Args     []string
	Env      map[string]string
	URL      string
	Headers  map[string]string
	IsActive bool
}

// ToolCallResult is what CallTool returns to the conversation engine's
// tool-calling loop — just enough to build a `role: tool` message.
type ToolCallResult struct {
	Content string
	IsError bool
}
