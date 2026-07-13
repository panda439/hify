package user

import (
	"database/sql"

	"github.com/gin-gonic/gin"

	"hify/internal/db/gen"
	"hify/internal/platform/httperr"
	"hify/internal/server/middleware"
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

// RegisterRoutes wires this module's endpoints onto the shared "/api/v1"
// group. jwtSecret is needed here (rather than threading a pre-built
// middleware through main.go) because every module registers its own
// routes with whatever auth requirements it needs.
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	users := v1.Group("/users")
	users.Use(middleware.RequireAuth(jwtSecret))
	users.GET("/me", httperr.Wrap(h.Me))
	users.GET("", middleware.RequireRole(RoleAdmin), httperr.Wrap(h.List))
}
