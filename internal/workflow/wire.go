package workflow

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"hify/internal/db/gen"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform/httperr"
	"hify/internal/provider"
	"hify/internal/server/middleware"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{queries: gen.New(db)}
}

func NewService(repo *Repository, providerSvc provider.Service, knowledgeSvc knowledge.Service, mcpSvc mcp.Service) Service {
	return &service{repo: repo, providerSvc: providerSvc, knowledgeSvc: knowledgeSvc, mcpSvc: mcpSvc}
}

func NewHandler(svc Service) *Handler {
	return &Handler{service: svc}
}

// RegisterRoutes wires workflow management onto "/api/v1/workflows" —
// same permission model as Agent (the sibling "compositional config"
// resource): every authenticated user can create/list/view/execute, no
// RequireRole gate; edit/disable ownership enforcement lives in
// service.go, not here, since it's a resource-level check the middleware
// layer can't express.
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	workflows := v1.Group("/workflows")
	workflows.Use(middleware.RequireAuth(jwtSecret))

	workflows.POST("", httperr.Wrap(h.Create))
	workflows.GET("", httperr.Wrap(h.List))
	workflows.GET("/:id", httperr.Wrap(h.Get))
	workflows.PUT("/:id", httperr.Wrap(h.Update))
	workflows.POST("/:id/executions", httperr.Wrap(h.Execute))
	workflows.GET("/:id/runs", httperr.Wrap(h.ListRuns))
	workflows.GET("/:id/runs/:runId", httperr.Wrap(h.GetRun))
	workflows.GET("/:id/runs/:runId/steps", httperr.Wrap(h.ListRunSteps))
}
