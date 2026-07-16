// Package server owns the Gin engine, route registration and cross-cutting
// middleware (auth/role/recover/cors). Route handlers for business modules
// live in each module's own handler.go; this package only wires them in.
package server

import (
	"database/sql"
	"log/slog"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"hify/internal/platform/httperr"
	"hify/internal/server/middleware"
)

type Config struct {
	Logger         *slog.Logger
	AllowedOrigins []string
	DB             *sql.DB
	// PG is the pgvector chunk store — retrieval depends on it, so
	// readiness must cover it alongside MySQL and Redis.
	PG    *sql.DB
	Redis *redis.Client
}

// New builds the engine with framework-level middleware and the
// liveness/readiness routes, and returns the "/api/v1" group so
// cmd/hify/main.go's buildApp can register each module's routes onto it
// once those modules exist.
func New(cfg Config) (*gin.Engine, *gin.RouterGroup) {
	r := gin.New()
	r.Use(middleware.RequestID())
	r.Use(middleware.Recover(cfg.Logger))
	r.Use(middleware.RequestLogger(cfg.Logger))
	r.Use(middleware.CORS(cfg.AllowedOrigins))

	v1 := r.Group("/api/v1")
	v1.GET("/health", httperr.Wrap(HealthCheck))
	v1.GET("/ready", httperr.Wrap(Readiness(cfg.DB, cfg.PG, cfg.Redis)))

	return r, v1
}
