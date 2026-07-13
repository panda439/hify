package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	RequestIDHeader = "X-Request-ID"
	requestIDKey    = "request_id"
)

// RequestID assigns a correlation ID to every request (reusing one supplied
// by an upstream proxy if present) so RequestLogger's log lines for a given
// request can be tied together, and clients can quote it back when
// reporting an issue.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		c.Set(requestIDKey, id)
		c.Writer.Header().Set(RequestIDHeader, id)
		c.Next()
	}
}

// RequestIDFrom reads the ID set by RequestID out of the gin context.
func RequestIDFrom(c *gin.Context) string {
	return c.GetString(requestIDKey)
}
