package middleware

import (
	"github.com/gin-gonic/gin"

	"hify/internal/platform/apperr"
	"hify/internal/platform/httperr"
)

// RequireRole must be chained after RequireAuth. It's a coarse global check
// (admin vs member), not per-resource ownership — Hify's single shared
// workspace doesn't need finer-grained authorization at this scale (see
// project plan's auth design).
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if RoleFrom(c) != role {
			httperr.Write(c, apperr.Forbidden("auth.forbidden", "没有权限执行此操作"))
			c.Abort()
			return
		}
		c.Next()
	}
}
