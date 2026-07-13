package conversation

import "hify/internal/platform/apperr"

// ErrNotFound doubles as both "this conversation doesn't exist" and "it
// exists but you don't own it" — unlike agent's ownership check (403),
// conversations must not confirm existence to a non-owner, so both cases
// collapse to the same 404. See CLAUDE.md's conversation privacy note.
var (
	ErrNotFound       = apperr.NotFound("conversation.not_found", "会话不存在")
	ErrAgentInactive  = apperr.InvalidInput("conversation.agent_inactive", "该 Agent 已被禁用，无法创建新会话")
	ErrInvalidRequest = apperr.InvalidInput("conversation.invalid_request", "请求参数不正确")
)
