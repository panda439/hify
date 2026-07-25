package conversation

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"hify/internal/agent"
	"hify/internal/db/gen"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform/httperr"
	"hify/internal/platform/trace"
	"hify/internal/provider"
	"hify/internal/server/middleware"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db, queries: gen.New(db)}
}

// traceStore is a concrete *trace.Store, not a Service interface — trace
// recording is cross-cutting infrastructure (see internal/platform/trace's
// package doc), not a business module, so it doesn't fall under the
// "cross-module calls only through Service interfaces" rule that governs
// agent/provider/knowledge/mcp below.
func NewService(repo *Repository, agentSvc agent.Service, providerSvc provider.Service, knowledgeSvc knowledge.Service, mcpSvc mcp.Service, traceStore *trace.Store) Service {
	return &service{repo: repo, agentSvc: agentSvc, providerSvc: providerSvc, knowledgeSvc: knowledgeSvc, mcpSvc: mcpSvc, traceStore: traceStore}
}

func NewHandler(svc Service) *Handler {
	return &Handler{service: svc}
}

// RegisterRoutes wires conversations onto "/api/v1/conversations" — every
// authenticated user manages only their own conversations; ownership is
// enforced inside service.go (see the privacy note in errors.go), not by a
// route-level gate, since it's per-row not per-role.
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	conversations := v1.Group("/conversations")
	conversations.Use(middleware.RequireAuth(jwtSecret))

	conversations.POST("", httperr.Wrap(h.Create))
	conversations.GET("", httperr.Wrap(h.List))
	conversations.GET("/:id/messages", httperr.Wrap(h.ListMessages))
	conversations.POST("/:id/messages", httperr.Wrap(h.SendMessage))
}
