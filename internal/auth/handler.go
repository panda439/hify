package auth

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const refreshCookieName = "hify_refresh_token"

// Handler is constructed via NewHandler in wire.go. secureCookie is false
// only in local development (plain HTTP has no TLS for the Secure flag to
// require).
type Handler struct {
	service      Service
	secureCookie bool
}

func (h *Handler) Login(c *gin.Context) error {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	session, err := h.service.Login(c.Request.Context(), req.Email, req.Password, c.ClientIP())
	if err != nil {
		return err
	}

	h.setRefreshCookie(c, session.RefreshToken, session.RefreshTTL)
	c.JSON(http.StatusOK, toSessionResponse(session))
	return nil
}

func (h *Handler) Refresh(c *gin.Context) error {
	raw, err := c.Cookie(refreshCookieName)
	if err != nil || raw == "" {
		return ErrInvalidRefreshToken
	}

	session, err := h.service.Refresh(c.Request.Context(), raw)
	if err != nil {
		return err
	}

	h.setRefreshCookie(c, session.RefreshToken, session.RefreshTTL)
	c.JSON(http.StatusOK, toSessionResponse(session))
	return nil
}

func (h *Handler) Logout(c *gin.Context) error {
	raw, _ := c.Cookie(refreshCookieName)
	if raw != "" {
		if err := h.service.Logout(c.Request.Context(), raw); err != nil {
			return err
		}
	}
	h.clearRefreshCookie(c)
	c.Status(http.StatusNoContent)
	return nil
}

// The refresh cookie is scoped to /api/v1/auth so it isn't sent on every
// single API request — only the handful of endpoints that actually need it.
func (h *Handler) setRefreshCookie(c *gin.Context, value string, ttl time.Duration) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(refreshCookieName, value, int(ttl.Seconds()), "/api/v1/auth", "", h.secureCookie, true)
}

func (h *Handler) clearRefreshCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(refreshCookieName, "", -1, "/api/v1/auth", "", h.secureCookie, true)
}
