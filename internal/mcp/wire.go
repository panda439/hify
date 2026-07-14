package mcp

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"hify/internal/db/gen"
	"hify/internal/platform/httperr"
	"hify/internal/server/middleware"
	"hify/internal/user"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{queries: gen.New(db)}
}

func NewService(repo *Repository) Service {
	return &service{repo: repo}
}

func NewHandler(svc Service) *Handler {
	return &Handler{service: svc}
}

// RegisterRoutes wires MCP server management onto "/api/v1/mcp-servers" —
// admin-only, same reasoning as provider (stdio transport is effectively
// "configure a command to run on the server", a materially different risk
// level than Agent/knowledge-base management). "/api/v1/mcp-tools" is the
// one exception: any authenticated user needs to browse available tools to
// attach them to an Agent, mirroring provider's "/models" carve-out.
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	servers := v1.Group("/mcp-servers")
	servers.Use(middleware.RequireAuth(jwtSecret), middleware.RequireRole(user.RoleAdmin))

	servers.POST("", httperr.Wrap(h.Create))
	servers.GET("", httperr.Wrap(h.List))
	servers.GET("/:id", httperr.Wrap(h.Get))
	servers.PUT("/:id", httperr.Wrap(h.Update))
	servers.POST("/:id/sync", httperr.Wrap(h.Sync))
	servers.GET("/:id/tools", httperr.Wrap(h.ListTools))

	v1.GET("/mcp-tools", middleware.RequireAuth(jwtSecret), httperr.Wrap(h.ListActiveTools))
}
