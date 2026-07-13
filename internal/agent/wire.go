package agent

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"hify/internal/db/gen"
	"hify/internal/platform/httperr"
	"hify/internal/provider"
	"hify/internal/server/middleware"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{queries: gen.New(db)}
}

func NewService(repo *Repository, providerSvc provider.Service) Service {
	return &service{repo: repo, providerSvc: providerSvc}
}

func NewHandler(svc Service) *Handler {
	return &Handler{service: svc}
}

// RegisterRoutes wires Agent management onto "/api/v1/agents" — every
// authenticated user can create/list/view (no RequireRole gate); ownership
// enforcement for edits happens inside service.go, not here, since it's a
// resource-level check the middleware layer can't express.
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	agents := v1.Group("/agents")
	agents.Use(middleware.RequireAuth(jwtSecret))

	agents.POST("", httperr.Wrap(h.Create))
	agents.GET("", httperr.Wrap(h.List))
	agents.GET("/:id", httperr.Wrap(h.Get))
	agents.PUT("/:id", httperr.Wrap(h.Update))
}
