package mcp

import "hify/internal/platform/apperr"

var (
	ErrNotFound             = apperr.NotFound("mcp.not_found", "MCP 服务器不存在")
	ErrToolNotFound         = apperr.NotFound("mcp.tool_not_found", "工具不存在")
	ErrToolInactive         = apperr.InvalidInput("mcp.tool_inactive", "该工具已被禁用或已从服务器移除")
	ErrUnsupportedTransport = apperr.InvalidInput("mcp.unsupported_transport", "不支持的传输方式，仅支持 stdio/sse")
	ErrConnectFailed        = apperr.InvalidInput("mcp.connect_failed", "连接 MCP 服务器失败，请检查配置")
	ErrInvalidRequest       = apperr.InvalidInput("mcp.invalid_request", "请求参数不正确")
)
