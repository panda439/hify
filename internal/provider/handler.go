package provider

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
	var req createProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	p, err := h.service.CreateProvider(c.Request.Context(), CreateProviderInput{
		Name:         req.Name,
		BaseURL:      req.BaseURL,
		AuthType:     req.AuthType,
		APIKey:       req.APIKey,
		ExtraHeaders: req.ExtraHeaders,
		ExtraConfig:  req.ExtraConfig,
		CreatedBy:    middleware.UserIDFrom(c),
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toProviderResponse(p))
	return nil
}

func (h *Handler) List(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	providers, total, err := h.service.ListProviders(c.Request.Context(), limit, offset)
	if err != nil {
		return err
	}

	items := make([]providerResponse, 0, len(providers))
	for _, p := range providers {
		items = append(items, toProviderResponse(p))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[providerResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) Get(c *gin.Context) error {
	p, err := h.service.GetProvider(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toProviderResponse(p))
	return nil
}

func (h *Handler) Update(c *gin.Context) error {
	var req updateProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	p, err := h.service.UpdateProvider(c.Request.Context(), c.Param("id"), UpdateProviderInput{
		Name:         req.Name,
		BaseURL:      req.BaseURL,
		AuthType:     req.AuthType,
		APIKey:       req.APIKey,
		ExtraHeaders: req.ExtraHeaders,
		ExtraConfig:  req.ExtraConfig,
		IsActive:     req.IsActive,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toProviderResponse(p))
	return nil
}

func (h *Handler) TestConnection(c *gin.Context) error {
	if err := h.service.TestConnection(c.Request.Context(), c.Param("id")); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}

func (h *Handler) AddModel(c *gin.Context) error {
	var req addModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	m, err := h.service.AddModel(c.Request.Context(), c.Param("id"), CreateModelInput{
		ModelName:          req.ModelName,
		Capability:         req.Capability,
		ContextWindow:      req.ContextWindow,
		MaxOutputTokens:    req.MaxOutputTokens,
		EmbeddingDimension: req.EmbeddingDimension,
		IsDefault:          req.IsDefault,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toModelResponse(m))
	return nil
}

func (h *Handler) ListModels(c *gin.Context) error {
	models, err := h.service.ListModels(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	items := make([]modelResponse, 0, len(models))
	for _, m := range models {
		items = append(items, toModelResponse(m))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

// ListModelsByCapability backs the model picker in the Agent form — any
// authenticated user needs this (not just admins), unlike the rest of this
// module's routes, since creating an Agent requires choosing a chat model
// but doesn't require managing providers.
func (h *Handler) ListModelsByCapability(c *gin.Context) error {
	capability := c.Query("capability")
	if capability != CapabilityChat && capability != CapabilityEmbedding {
		return ErrInvalidRequest
	}
	models, err := h.service.ListModelsByCapability(c.Request.Context(), capability)
	if err != nil {
		return err
	}
	items := make([]modelResponse, 0, len(models))
	for _, m := range models {
		items = append(items, toModelResponse(m))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

func (h *Handler) UpdateModel(c *gin.Context) error {
	var req updateModelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	m, err := h.service.UpdateModel(c.Request.Context(), c.Param("modelId"), UpdateModelInput{
		ContextWindow:      req.ContextWindow,
		MaxOutputTokens:    req.MaxOutputTokens,
		EmbeddingDimension: req.EmbeddingDimension,
		IsDefault:          req.IsDefault,
		IsActive:           req.IsActive,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toModelResponse(m))
	return nil
}
