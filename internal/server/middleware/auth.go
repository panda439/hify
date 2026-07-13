package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/apperr"
	"hify/internal/platform/httperr"
	"hify/internal/platform/jwt"
)

const (
	userIDKey = "user_id"
	roleKey   = "role"
)

// RequireAuth parses the `Authorization: Bearer <token>` header and stores
// the verified claims in the gin context for handlers (and RequireRole) to
// read. It trusts the JWT claims as-is rather than re-querying the user on
// every request — per the project plan, only privileged role-escalation
// actions hit the DB for a fresh check, to keep the common request path
// fast.
func RequireAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			httperr.Write(c, apperr.Unauthorized("auth.missing_token", "未登录或登录已失效"))
			c.Abort()
			return
		}

		claims, err := jwt.Verify(secret, token)
		if err != nil {
			httperr.Write(c, apperr.Unauthorized("auth.invalid_token", "登录已过期，请重新登录"))
			c.Abort()
			return
		}

		c.Set(userIDKey, claims.UserID)
		c.Set(roleKey, claims.Role)
		c.Next()
	}
}

// UserIDFrom and RoleFrom read the claims RequireAuth stored in the
// context; every handler that needs "who is calling" uses these instead of
// re-parsing the token.
func UserIDFrom(c *gin.Context) string { return c.GetString(userIDKey) }
func RoleFrom(c *gin.Context) string   { return c.GetString(roleKey) }
