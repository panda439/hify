package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	gomcp "github.com/mark3labs/mcp-go/mcp"
)

// connect opens a fresh MCP client connection (stdio spawns a subprocess,
// sse dials the server) and does the protocol handshake. There is no
// persistent connection pool — Sync and CallTool are both infrequent,
// isolated operations (Sync is an explicit admin action; CallTool happens
// once per tool-call turn, not in a tight loop), so connect-do-close is
// simplest and matches the plan's Phase 4 scope. If tool-calling latency
// ever becomes a problem (stdio subprocess spawn is the expensive part),
// pooling per-server connections is the natural next step — not needed yet.
func connect(ctx context.Context, s Server) (*mcpclient.Client, error) {
	var c *mcpclient.Client
	var err error

	switch s.Transport {
	case TransportStdio:
		c, err = mcpclient.NewStdioMCPClient(s.Command, envSlice(s.Env), s.Args...)
	case TransportSSE:
		if len(s.Headers) > 0 {
			c, err = mcpclient.NewSSEMCPClient(s.URL, mcpclient.WithHeaders(s.Headers))
		} else {
			c, err = mcpclient.NewSSEMCPClient(s.URL)
		}
		if err == nil {
			err = c.Start(ctx)
		}
	default:
		return nil, ErrUnsupportedTransport
	}
	if err != nil {
		return nil, fmt.Errorf("mcp: connect: %w", err)
	}

	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := c.Initialize(initCtx, gomcp.InitializeRequest{
		Params: gomcp.InitializeParams{
			ProtocolVersion: gomcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      gomcp.Implementation{Name: "hify", Version: "0.1.0"},
		},
	}); err != nil {
		_ = c.Close() // best-effort cleanup; the Initialize error is what matters to the caller
		return nil, fmt.Errorf("mcp: initialize: %w", err)
	}
	return c, nil
}

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func listRemoteTools(ctx context.Context, s Server) ([]gomcp.Tool, error) {
	c, err := connect(ctx, s)
	if err != nil {
		return nil, err
	}
	defer c.Close()

	result, err := c.ListTools(ctx, gomcp.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("mcp: list tools: %w", err)
	}
	return result.Tools, nil
}

func callRemoteTool(ctx context.Context, s Server, toolName string, arguments json.RawMessage) (ToolCallResult, error) {
	c, err := connect(ctx, s)
	if err != nil {
		return ToolCallResult{}, err
	}
	defer c.Close()

	var args any
	if len(arguments) > 0 {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return ToolCallResult{}, fmt.Errorf("mcp: unmarshal tool arguments: %w", err)
		}
	}

	callCtx, cancel := context.WithTimeout(ctx, maxCallToolTimeout)
	defer cancel()
	result, err := c.CallTool(callCtx, gomcp.CallToolRequest{
		Params: gomcp.CallToolParams{Name: toolName, Arguments: args},
	})
	if err != nil {
		return ToolCallResult{}, fmt.Errorf("mcp: call tool: %w", err)
	}

	var sb strings.Builder
	for _, item := range result.Content {
		if text, ok := gomcp.AsTextContent(item); ok {
			sb.WriteString(text.Text)
		}
	}
	return ToolCallResult{Content: sb.String(), IsError: result.IsError}, nil
}

// toolInputSchemaJSON extracts the tool's JSON Schema for storage,
// reusing gomcp.Tool's own MarshalJSON (which already resolves the
// InputSchema-vs-RawInputSchema ambiguity) instead of re-implementing
// that logic here.
func toolInputSchemaJSON(t gomcp.Tool) (json.RawMessage, error) {
	full, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal tool: %w", err)
	}
	var wrapper struct {
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	if err := json.Unmarshal(full, &wrapper); err != nil {
		return nil, fmt.Errorf("mcp: unmarshal tool schema: %w", err)
	}
	return wrapper.InputSchema, nil
}
