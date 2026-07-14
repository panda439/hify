package agent

import "hify/internal/platform/apperr"

var (
	ErrNotFound             = apperr.NotFound("agent.not_found", "Agent 不存在")
	ErrForbidden            = apperr.Forbidden("agent.forbidden", "只有创建者本人或管理员可以修改该 Agent")
	ErrInvalidModel         = apperr.InvalidInput("agent.invalid_model", "所选模型不可用，请选择一个已启用的对话模型")
	ErrInvalidKnowledgeBase = apperr.InvalidInput("agent.invalid_knowledge_base", "所选知识库不可用，请选择一个已启用的知识库")
	ErrInvalidRequest       = apperr.InvalidInput("agent.invalid_request", "请求参数不正确")
)
