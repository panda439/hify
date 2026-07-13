package user

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"hify/internal/platform"
	"hify/internal/server/middleware"
)

// Handler is constructed via NewHandler in wire.go.
type Handler struct {
	service Service
}

// Me returns the profile of whoever RequireAuth identified this request as.
func (h *Handler) Me(c *gin.Context) error {
	u, err := h.service.GetByID(c.Request.Context(), middleware.UserIDFrom(c))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toResponse(u))
	return nil
}

// List is the admin user list, offset-paginated (small table, see CLAUDE.md
// pagination rules).
func (h *Handler) List(c *gin.Context) error {
	// A missing/invalid limit or offset parses to 0, which ClampLimit and
	// this handler both treat as "use the default" — not an error case
	// that needs surfacing to the client.
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	users, total, err := h.service.List(c.Request.Context(), limit, offset)
	if err != nil {
		return err
	}

	items := make([]userResponse, 0, len(users))
	for _, u := range users {
		items = append(items, toResponse(u))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[userResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}
