package mcp

import (
	"errors"
	"testing"
)

// Characterization tests for transport 配置校验 — 链路 9（MCP 工具同步）
// 的入口护栏：stdio 必须有 command，sse 必须有 url，其他 transport 拒绝。

func TestValidateTransportConfig(t *testing.T) {
	cases := []struct {
		name                    string
		transport, command, url string
		want                    error
	}{
		{"stdio with command ok", TransportStdio, "npx some-server", "", nil},
		{"stdio missing command", TransportStdio, "", "", ErrInvalidRequest},
		{"sse with url ok", TransportSSE, "", "https://mcp.example.com/sse", nil},
		{"sse missing url", TransportSSE, "", "", ErrInvalidRequest},
		{"unknown transport", "websocket", "cmd", "url", ErrUnsupportedTransport},
		{"empty transport", "", "", "", ErrUnsupportedTransport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTransportConfig(tc.transport, tc.command, tc.url)
			if !errors.Is(err, tc.want) {
				t.Fatalf("validateTransportConfig(%q) = %v, want %v", tc.transport, err, tc.want)
			}
		})
	}
}
