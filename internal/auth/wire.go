package auth

import (
	"database/sql"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"hify/internal/db/gen"
	"hify/internal/platform/httperr"
	"hify/internal/user"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db, queries: gen.New(db)}
}

func NewService(repo *Repository, users user.Service, redis *redis.Client, jwtSecret string, accessTTL, refreshTTL time.Duration) Service {
	return &service{
		repo:       repo,
		users:      users,
		redis:      redis,
		jwtSecret:  jwtSecret,
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

func NewHandler(svc Service, env string) *Handler {
	return &Handler{service: svc, secureCookie: env != "development"}
}

func RegisterRoutes(v1 *gin.RouterGroup, h *Handler) {
	g := v1.Group("/auth")
	g.POST("/login", httperr.Wrap(h.Login))
	g.POST("/refresh", httperr.Wrap(h.Refresh))
	g.POST("/logout", httperr.Wrap(h.Logout))
}
