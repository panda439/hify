package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"hify/internal/platform/httperr"
)

type healthResponse struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

// HealthCheck is a pure liveness probe: no DB/Redis dependency, just "is the
// process up and routing requests".
func HealthCheck(c *gin.Context) error {
	c.JSON(http.StatusOK, healthResponse{Status: "ok", Message: "Hify is running"})
	return nil
}

// Readiness actually pings MySQL, PostgreSQL, and Redis — "the process is
// up" (health) and "the process can do its job" (ready) are different
// questions, and only the latter tells you the stack as a whole is usable.
func Readiness(db, pgdb *sql.DB, rdb *redis.Client) httperr.HandlerFunc {
	return func(c *gin.Context) error {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		if err := db.PingContext(ctx); err != nil {
			return fmt.Errorf("readiness: mysql unreachable: %w", err)
		}
		if err := pgdb.PingContext(ctx); err != nil {
			return fmt.Errorf("readiness: postgres unreachable: %w", err)
		}
		if err := rdb.Ping(ctx).Err(); err != nil {
			return fmt.Errorf("readiness: redis unreachable: %w", err)
		}

		c.JSON(http.StatusOK, healthResponse{Status: "ok", Message: "Hify is ready"})
		return nil
	}
}
