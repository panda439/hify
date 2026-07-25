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
	// ErrContextTooLarge is computeFixedBudget's hard-limit failure — the
	// Agent's system prompt, the latest user message, and the tool
	// definition estimate are the only content assembleContext can never
	// drop (RAG evidence and older history are trimmed first, down to
	// nothing, before this can ever fire — see computeFixedBudget's doc
	// comment), so if even that mandatory core doesn't fit inside
	// totalBudgetChars(model), there is nothing left to trim and the turn
	// must fail outright rather than silently truncate the user's actual
	// question or blow past the model's real ContextWindow.
	ErrContextTooLarge = apperr.InvalidInput("conversation.context_too_large", "当前消息超过模型上下文限制，请缩短内容后重试")
)
