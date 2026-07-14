package workflow

import "hify/internal/platform/apperr"

var (
	ErrNotFound    = apperr.NotFound("workflow.not_found", "工作流不存在")
	ErrRunNotFound = apperr.NotFound("workflow.run_not_found", "执行记录不存在")
	ErrForbidden   = apperr.Forbidden("workflow.forbidden", "只有创建者本人或管理员可以修改该工作流")

	// Definition validation — each is a distinct sentinel (rather than one
	// generic ErrInvalidDefinition) so the client gets a specific, fixed
	// Chinese message per failure mode instead of a vague "定义不合法"; see
	// CLAUDE.md's note that AppError.Message can't carry dynamic detail
	// (only Code/Kind do), so granularity has to live in which sentinel
	// gets returned, not in interpolated text.
	ErrEmptyDefinition          = apperr.InvalidInput("workflow.empty_definition", "工作流至少需要一个节点")
	ErrDuplicateStepID          = apperr.InvalidInput("workflow.duplicate_step_id", "节点 ID 重复")
	ErrMissingStartStep         = apperr.InvalidInput("workflow.missing_start_step", "工作流必须有且仅有一个开始节点")
	ErrDanglingEdge             = apperr.InvalidInput("workflow.dangling_edge", "节点连线指向了不存在的节点")
	ErrConditionalMissingBranch = apperr.InvalidInput("workflow.conditional_missing_branch", "条件节点必须同时配置「条件为真」和「条件为假」两条分支")
	ErrDeadEnd                  = apperr.InvalidInput("workflow.dead_end", "非结束节点必须连接后续节点")
	ErrEndHasOutgoing           = apperr.InvalidInput("workflow.end_has_outgoing", "结束节点不能再连接后续节点")
	ErrCycleDetected            = apperr.InvalidInput("workflow.cycle_detected", "工作流中存在循环，无法执行")
	ErrUnreachableStep          = apperr.InvalidInput("workflow.unreachable_step", "存在从开始节点无法到达的节点")
	ErrInvalidStepConfig        = apperr.InvalidInput("workflow.invalid_step_config", "节点配置不合法")

	// Execution-time errors.
	ErrTooManySteps      = apperr.InvalidInput("workflow.too_many_steps", "工作流执行步数超过上限，可能存在异常")
	ErrToolCallFailed    = apperr.InvalidInput("workflow.tool_call_failed", "工具调用失败")
	ErrConditionNotBool  = apperr.InvalidInput("workflow.condition_not_bool", "条件表达式的求值结果不是布尔值")
	ErrWorkflowNotActive = apperr.InvalidInput("workflow.not_active", "工作流已被禁用，无法执行")
	ErrInvalidRequest    = apperr.InvalidInput("workflow.invalid_request", "请求参数不正确")
)
