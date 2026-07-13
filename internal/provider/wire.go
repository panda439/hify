package provider

import (
	"database/sql"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"hify/internal/db/gen"
	"hify/internal/platform/httperr"
	"hify/internal/server/middleware"
	"hify/internal/user"
)

func NewRepository(db *sql.DB) *Repository {
	return &Repository{queries: gen.New(db)}
}

// NewService needs the base64-encoded HIFY_ENCRYPTION_KEY (decoded once
// here, not per call) and a Redis client for the resilience decorator's
// rate limiter.
func NewService(repo *Repository, encryptionKeyBase64 string, rdb *redis.Client) (Service, error) {
	key, err := loadEncryptionKey(encryptionKeyBase64)
	if err != nil {
		return nil, err
	}
	return &service{
		repo: repo,
		reg:  newRegistry(repo, key, rdb),
	}, nil
}

func NewHandler(svc Service) *Handler {
	return &Handler{service: svc}
}

// RegisterRoutes wires provider management onto "/api/v1/providers" —
// admin-only, matching the plan's "CRUD + Test Connection 接口（admin only）".
func RegisterRoutes(v1 *gin.RouterGroup, h *Handler, jwtSecret string) {
	providers := v1.Group("/providers")
	providers.Use(middleware.RequireAuth(jwtSecret), middleware.RequireRole(user.RoleAdmin))

	providers.POST("", httperr.Wrap(h.Create))
	providers.GET("", httperr.Wrap(h.List))
	providers.GET("/:id", httperr.Wrap(h.Get))
	providers.PUT("/:id", httperr.Wrap(h.Update))
	providers.POST("/:id/test", httperr.Wrap(h.TestConnection))
	providers.POST("/:id/models", httperr.Wrap(h.AddModel))
	providers.GET("/:id/models", httperr.Wrap(h.ListModels))
	providers.PUT("/:id/models/:modelId", httperr.Wrap(h.UpdateModel))

	// Any authenticated user (not just admins) needs to browse chat models
	// to create an Agent — narrower than provider management, so it's
	// registered outside the admin-gated "providers" group above.
	v1.GET("/models", middleware.RequireAuth(jwtSecret), httperr.Wrap(h.ListModelsByCapability))
}
