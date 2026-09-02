package agent

import "hify/internal/platform/apperr"

var (
	ErrNotFound             = apperr.NotFound("agent.not_found", "Agent 不存在")
	ErrForbidden            = apperr.Forbidden("agent.forbidden", "只有创建者本人或管理员可以修改该 Agent")
	ErrInvalidModel         = apperr.InvalidInput("agent.invalid_model", "所选模型不可用，请选择一个已启用的对话模型")
	ErrInvalidKnowledgeBase = apperr.InvalidInput("agent.invalid_knowledge_base", "所选知识库不可用，请选择一个已启用的知识库")
	ErrInvalidMCPTool       = apperr.InvalidInput("agent.invalid_mcp_tool", "所选工具不可用，请选择一个已启用的工具")
	ErrInvalidRequest       = apperr.InvalidInput("agent.invalid_request", "请求参数不正确")

	// 004-agent-document-scope：文档范围的两个校验错误。
	//
	// ErrInvalidScopedDocument：范围里的文档不属于该 Agent 绑定的任何知识库。
	// 这种配置本身不会让检索出错（002 的 FR-010：引用不存在实体的过滤条件
	// 产生"无匹配"而不是报错），但它一定是配错了——用户以为限定了范围，实际
	// 是限定到了一个永远匹配不到的集合。保存时就拒绝，比让它悄悄失效好。
	ErrInvalidScopedDocument = apperr.InvalidInput("agent.invalid_scoped_document", "所选文档不属于该 Agent 绑定的知识库，请重新选择")

	// ErrTooManyScopedDocuments：超过 002 的 maxFilterDocumentIDs(50)。
	// 拒绝而不截断——静默截断会悄悄改变用户指定的范围（002 的 FR-009）。
	ErrTooManyScopedDocuments = apperr.InvalidInput("agent.too_many_scoped_documents", "限定的文档数量超出上限（最多 50 份），请缩小范围")
)
