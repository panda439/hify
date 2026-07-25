package conversation

import (
	"encoding/json"
	"fmt"
	"io"
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

func (h *Handler) Create(c *gin.Context) error {
	var req createConversationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	conv, err := h.service.CreateConversation(c.Request.Context(), middleware.UserIDFrom(c), req.AgentID)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toConversationResponse(conv))
	return nil
}

func (h *Handler) List(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	convs, total, err := h.service.ListConversations(c.Request.Context(), middleware.UserIDFrom(c), limit, offset)
	if err != nil {
		return err
	}

	items := make([]conversationResponse, 0, len(convs))
	for _, conv := range convs {
		items = append(items, toConversationResponse(conv))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[conversationResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) ListMessages(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	limit = platform.ClampLimit(limit)

	var cursor *MessageCursor
	if raw := c.Query("cursor"); raw != "" {
		decoded, err := DecodeCursor(raw)
		if err != nil {
			return ErrInvalidRequest
		}
		cursor = &decoded
	}

	messages, citations, nextCursor, err := h.service.ListMessages(c.Request.Context(), middleware.UserIDFrom(c), c.Param("id"), cursor, limit)
	if err != nil {
		return err
	}

	items := make([]messageResponse, 0, len(messages))
	for _, m := range messages {
		items = append(items, toMessageResponse(m, citations[m.ID]))
	}
	c.JSON(http.StatusOK, platform.CursorPage[messageResponse]{Items: items, NextCursor: nextCursor})
	return nil
}

// SendMessage is the one handler in Hify that can't fully delegate error
// handling to httperr.Wrap: everything up through Service.StreamMessage's
// synchronous pre-flight (conversation ownership, agent/model/client
// resolution) is a normal `return err` that Wrap turns into a JSON error
// response. Once StreamMessage hands back a channel, response headers are
// about to be committed to SSE, so from that point on we must always
// return nil — any failure is written as an in-band "error" event instead.
func (h *Handler) SendMessage(c *gin.Context) error {
	var req sendMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	events, err := h.service.StreamMessage(c.Request.Context(), middleware.UserIDFrom(c), c.Param("id"), req.Content)
	if err != nil {
		return err
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no") // disable reverse-proxy buffering if one is later added in front of this

	c.Stream(func(w io.Writer) bool {
		event, ok := <-events
		if !ok {
			return false
		}
		payload, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			return false
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload)
		// Keep reading until a terminal event — done/error are the only
		// ones that end the stream; retrieval (RAG debug info) and delta
		// are both non-terminal. A stray "== EventDelta" check here would
		// stop the loop dead on the very first non-delta event, which is
		// exactly what happened when EventRetrieval was added: retrieval
		// always arrives before any delta, so the stream silently ended
		// after just that one frame.
		return event.Type != EventDone && event.Type != EventError
	})
	return nil
}
