// Package httperr is Hify's global exception handler. It has two halves:
// Write maps an apperr.AppError's Kind to the right HTTP status and a
// {"error":{"code","message"}} body; Wrap adapts an (c *gin.Context) error
// handler into a plain gin.HandlerFunc that calls Write automatically, so
// individual handlers just `return err` instead of repeating
// `if err != nil { httperr.Write(c, err); return }` everywhere. Panics are
// handled separately by the recover middleware in internal/server/middleware.
package httperr

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/apperr"
)

// HandlerFunc is the signature every handler.go function in every module
// implements: business logic in, error out, no manual response writing on
// the error path.
type HandlerFunc func(c *gin.Context) error

// Wrap adapts a HandlerFunc into a gin.HandlerFunc for route registration.
func Wrap(h HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := h(c); err != nil {
			Write(c, err)
		}
	}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Write classifies err and writes the corresponding HTTP status + body.
// Errors that aren't an *apperr.AppError are treated as unexpected internal
// failures: logged with full detail server-side, returned to the client as
// a generic 500 with no internal detail leaked.
func Write(c *gin.Context, err error) {
	var ae *apperr.AppError
	if errors.As(err, &ae) {
		c.JSON(statusFor(ae.Kind), errorBody{Error: errorDetail{Code: ae.Code, Message: ae.Message}})
		return
	}

	WriteInternal(c, err)
}

// WriteInternal logs cause and writes a generic 500 body. It's the shared
// tail end of Write's default branch and of the panic-recovery middleware
// (internal/server/middleware.Recover), so a handler error and an actual
// panic both produce byte-for-byte the same response shape.
func WriteInternal(c *gin.Context, cause any) {
	slog.Error("unhandled request error", "err", cause, "path", c.FullPath(), "method", c.Request.Method)
	c.JSON(http.StatusInternalServerError, errorBody{
		Error: errorDetail{Code: "internal_error", Message: "服务器内部错误，请稍后重试"},
	})
}

func statusFor(kind apperr.Kind) int {
	switch kind {
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindInvalidInput:
		return http.StatusBadRequest
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}
