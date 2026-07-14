package mcp

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
	var req createServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	srv, err := h.service.CreateServer(c.Request.Context(), CreateServerInput{
		Name:      req.Name,
		Transport: req.Transport,
		Command:   req.Command,
		Args:      req.Args,
		Env:       req.Env,
		URL:       req.URL,
		Headers:   req.Headers,
		CreatedBy: middleware.UserIDFrom(c),
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toServerResponse(srv))
	return nil
}

func (h *Handler) List(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	servers, total, err := h.service.ListServers(c.Request.Context(), limit, offset)
	if err != nil {
		return err
	}
	items := make([]serverResponse, 0, len(servers))
	for _, s := range servers {
		items = append(items, toServerResponse(s))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[serverResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) Get(c *gin.Context) error {
	srv, err := h.service.GetServer(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toServerResponse(srv))
	return nil
}

func (h *Handler) Update(c *gin.Context) error {
	var req updateServerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	srv, err := h.service.UpdateServer(c.Request.Context(), c.Param("id"), UpdateServerInput{
		Name:     req.Name,
		Command:  req.Command,
		Args:     req.Args,
		Env:      req.Env,
		URL:      req.URL,
		Headers:  req.Headers,
		IsActive: req.IsActive,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toServerResponse(srv))
	return nil
}

func (h *Handler) Sync(c *gin.Context) error {
	tools, err := h.service.SyncTools(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	items := make([]toolResponse, 0, len(tools))
	for _, t := range tools {
		items = append(items, toToolResponse(t))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

func (h *Handler) ListTools(c *gin.Context) error {
	tools, err := h.service.ListTools(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	items := make([]toolResponse, 0, len(tools))
	for _, t := range tools {
		items = append(items, toToolResponse(t))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}

// ListActiveTools backs the Agent form's tool picker — see Service's doc
// comment on why this is open to any authenticated user.
func (h *Handler) ListActiveTools(c *gin.Context) error {
	tools, err := h.service.ListActiveTools(c.Request.Context())
	if err != nil {
		return err
	}
	items := make([]toolResponse, 0, len(tools))
	for _, t := range tools {
		items = append(items, toToolResponse(t))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}
