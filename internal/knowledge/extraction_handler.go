package knowledge

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/apperr"
	"hify/internal/server/middleware"
)

// extraction_handler.go 是抽取操作的 HTTP 层（010 T029）。
// 路径 /knowledge-bases/:id/documents/:docId/extraction[/action]。

// extractionOperationRequest 是所有写操作的请求体。
//
// ⚠️ 额度字段是**追加量**不是新上限：写成上限的话，一个填小了的值会把
// 已经用掉的额度算成超支，作业立刻停在"预算耗尽"上。
type extractionOperationRequest struct {
	IdempotencyKey  string `json:"idempotency_key"`
	ModelID         string `json:"model_id"`
	AdditionalItems int    `json:"additional_chunks"`
	AdditionalCalls int    `json:"additional_calls"`
	AdditionalSecs  int    `json:"additional_active_seconds"`
	AdditionalRetry int    `json:"additional_retry_rounds"`
}

func (r extractionOperationRequest) toOperation() ExtractionOperation {
	return ExtractionOperation{
		IdempotencyKey: r.IdempotencyKey, ModelID: r.ModelID,
		AdditionalItems: r.AdditionalItems, AdditionalCalls: r.AdditionalCalls,
		AdditionalActiveSec: r.AdditionalSecs, AdditionalRetries: r.AdditionalRetry,
	}
}

// extractionStatusResponse 对外的状态。
//
// ⭐ 未知值序列化成 null，不填零。⚠️ 初始化还没完成时 total_items 填 0，
// 前端会显示"0/0 已完成"——一个看起来已经跑完的进度条，
// 而实际上一条都还没开始。
type extractionStatusResponse struct {
	Enabled         bool    `json:"enabled"`
	JobID           *string `json:"job_id"`
	State           *string `json:"state"`
	StopReason      *string `json:"stop_reason"`
	DocumentVersion *int64  `json:"document_version"`
	ModelID         *string `json:"model_id"`

	TotalItems     *int `json:"total_items"`
	SucceededItems *int `json:"succeeded_items"`
	FailedItems    *int `json:"failed_items"`

	// ⭐ confirmed_calls 与 possible_calls 分开：前者"确定发生过"，
	// 后者含那些不知道有没有发出去的。合并会让一个不确定的数字
	// 看起来像确定的。
	ConfirmedCalls      *int   `json:"confirmed_calls"`
	PossibleCalls       *int   `json:"possible_calls"`
	UnknownUsageAttempt *int   `json:"unknown_usage_attempts"`
	ActiveMs            *int64 `json:"active_ms"`

	RemainingCalls    *int   `json:"remaining_calls"`
	RemainingItems    *int   `json:"remaining_chunks"`
	RemainingActiveMs *int64 `json:"remaining_active_ms"`

	// ⚠️ cost_kind 恒为 not_applicable、cost_amount 恒为 null：
	// 本地模型没有金钱计费，写 0 等于说"花了零元"，而真实情况是
	// 这个口径不适用，硬件电力没有测量。
	CostKind   string  `json:"cost_kind"`
	CostAmount *string `json:"cost_amount"`
}

func toExtractionStatusResponse(st ExtractionStatus) extractionStatusResponse {
	return extractionStatusResponse{
		Enabled: st.Enabled, JobID: st.JobID, State: st.State, StopReason: st.StopReason,
		DocumentVersion: st.DocumentVersion, ModelID: st.ModelID,
		TotalItems: st.TotalItems, SucceededItems: st.SucceededItems, FailedItems: st.FailedItems,
		ConfirmedCalls: st.ConfirmedCalls, PossibleCalls: st.PossibleCalls,
		UnknownUsageAttempt: st.UnknownUsageAttempt, ActiveMs: st.ActiveMs,
		RemainingCalls: st.RemainingCalls, RemainingItems: st.RemainingItems,
		RemainingActiveMs: st.RemainingActiveMs,
		CostKind:          st.CostKind, CostAmount: st.CostAmount,
	}
}

func (h *Handler) GetExtractionStatus(c *gin.Context) error {
	st, err := h.service.GetExtractionStatus(c.Request.Context(),
		c.Param("id"), c.Param("docId"), middleware.UserIDFrom(c), middleware.RoleFrom(c))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toExtractionStatusResponse(st))
	return nil
}

func (h *Handler) EnableExtraction(c *gin.Context) error {
	return h.extractionSwitch(c, true, http.StatusAccepted)
}

func (h *Handler) DisableExtraction(c *gin.Context) error {
	return h.extractionSwitch(c, false, http.StatusOK)
}

func (h *Handler) extractionSwitch(c *gin.Context, enabled bool, okStatus int) error {
	var req extractionOperationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}
	st, err := h.service.SetExtractionEnabled(c.Request.Context(),
		c.Param("id"), c.Param("docId"), middleware.UserIDFrom(c), middleware.RoleFrom(c),
		enabled, req.toOperation())
	if err != nil {
		return err
	}
	c.JSON(okStatus, toExtractionStatusResponse(st))
	return nil
}

func (h *Handler) PauseExtraction(c *gin.Context) error {
	return h.extractionAction(c, h.service.PauseExtraction, http.StatusOK)
}

func (h *Handler) ResumeExtraction(c *gin.Context) error {
	return h.extractionAction(c, h.service.ResumeExtraction, http.StatusAccepted)
}

func (h *Handler) RestartExtraction(c *gin.Context) error {
	return h.extractionAction(c, h.service.RestartExtraction, http.StatusAccepted)
}

type extractionAction func(ctx context.Context, kbID, docID, userID, role string, op ExtractionOperation) (ExtractionStatus, error)

func (h *Handler) extractionAction(c *gin.Context, fn extractionAction, okStatus int) error {
	var req extractionOperationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}
	st, err := fn(c.Request.Context(), c.Param("id"), c.Param("docId"),
		middleware.UserIDFrom(c), middleware.RoleFrom(c), req.toOperation())
	if err != nil {
		return err
	}
	c.JSON(okStatus, toExtractionStatusResponse(st))
	return nil
}

var _ = apperr.InvalidInput
