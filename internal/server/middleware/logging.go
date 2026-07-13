package middleware

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
)

// RequestLogger logs one structured line per completed request. This is the
// baseline observability every request gets for free — no metrics system,
// just slog lines that are enough to answer "why was this slow" / "why did
// this 401" at Hify's current scale.
func RequestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path

		c.Next()

		logger.Info("http_request",
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFrom(c),
			"client_ip", c.ClientIP(),
		)
	}
}
