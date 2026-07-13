package middleware

import (
	"fmt"
	"log/slog"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/httperr"
)

// Recover replaces gin.Recovery(): a panic anywhere downstream gets logged
// with its stack trace (via slog, matching the rest of the app's logging)
// and turned into the same {"error":{"code","message"}} shape httperr.Write
// produces for ordinary returned errors, instead of Gin's default
// plain-text/HTML panic response.
func Recover(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("panic recovered",
					"panic", fmt.Sprint(r),
					"path", c.Request.URL.Path,
					"request_id", RequestIDFrom(c),
					"stack", string(debug.Stack()),
				)
				httperr.WriteInternal(c, r)
				c.Abort()
			}
		}()
		c.Next()
	}
}
