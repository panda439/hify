package agent

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"hify/internal/platform"
	"hify/internal/server/middleware"
)

// Handler is constructed via NewHandler in wire.go.
type Handler struct {
	service Service
}

func (h *Handler) Create(c *gin.Context) error {
	var req createAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	a, err := h.service.CreateAgent(c.Request.Context(), CreateAgentInput{
		Name:             req.Name,
		Description:      req.Description,
		ModelID:          req.ModelID,
		SystemPrompt:     req.SystemPrompt,
		Temperature:      req.Temperature,
		MaxTokens:        req.MaxTokens,
		TopP:             req.TopP,
		ExtraParams:      req.ExtraParams,
		KnowledgeBaseIDs: req.KnowledgeBaseIDs,
		MCPToolIDs:       req.MCPToolIDs,
		CreatedBy:        middleware.UserIDFrom(c),
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toAgentResponse(a))
	return nil
}

// List returns every Agent regardless of is_active — Agents are a shared,
// all-visible directory (see CLAUDE.md's permission model), so disabled
// ones stay listed with is_active:false rather than disappearing.
func (h *Handler) List(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	agents, total, err := h.service.ListAgents(c.Request.Context(), limit, offset)
	if err != nil {
		return err
	}

	items := make([]agentResponse, 0, len(agents))
	for _, a := range agents {
		items = append(items, toAgentResponse(a))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[agentResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) Get(c *gin.Context) error {
	a, err := h.service.GetAgent(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toAgentResponse(a))
	return nil
}

// Update also covers "delete" via is_active:false — no separate DELETE
// route, matching the provider module's pattern.
func (h *Handler) Update(c *gin.Context) error {
	var req updateAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	a, err := h.service.UpdateAgent(
		c.Request.Context(),
		c.Param("id"),
		middleware.UserIDFrom(c),
		middleware.RoleFrom(c),
		UpdateAgentInput{
			Name:             req.Name,
			Description:      req.Description,
			ModelID:          req.ModelID,
			SystemPrompt:     req.SystemPrompt,
			Temperature:      req.Temperature,
			MaxTokens:        req.MaxTokens,
			TopP:             req.TopP,
			ExtraParams:      req.ExtraParams,
			KnowledgeBaseIDs: req.KnowledgeBaseIDs,
			MCPToolIDs:       req.MCPToolIDs,
			IsActive:         req.IsActive,
		},
	)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toAgentResponse(a))
	return nil
}
