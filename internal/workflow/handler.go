package workflow

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
	var req createWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	wf, err := h.service.CreateWorkflow(c.Request.Context(), CreateWorkflowInput{
		Name:        req.Name,
		Description: req.Description,
		Definition:  req.Definition,
		CreatedBy:   middleware.UserIDFrom(c),
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toWorkflowResponse(wf))
	return nil
}

func (h *Handler) List(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	workflows, total, err := h.service.ListWorkflows(c.Request.Context(), limit, offset)
	if err != nil {
		return err
	}
	items := make([]workflowResponse, 0, len(workflows))
	for _, wf := range workflows {
		items = append(items, toWorkflowResponse(wf))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[workflowResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) Get(c *gin.Context) error {
	wf, err := h.service.GetWorkflow(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toWorkflowResponse(wf))
	return nil
}

func (h *Handler) Update(c *gin.Context) error {
	var req updateWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	wf, err := h.service.UpdateWorkflow(c.Request.Context(), c.Param("id"), middleware.UserIDFrom(c), middleware.RoleFrom(c), UpdateWorkflowInput{
		Name:        req.Name,
		Description: req.Description,
		Definition:  req.Definition,
		IsActive:    req.IsActive,
	})
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toWorkflowResponse(wf))
	return nil
}

// Execute is the one handler in this module that can't fully delegate
// error handling to httperr.Wrap — same reasoning as
// conversation.Handler.SendMessage: everything through Service.Execute's
// synchronous pre-flight (workflow lookup, active check, run row insert)
// is a normal `return err`, but once Execute hands back a channel, headers
// are about to be committed to SSE, so from that point on we always return
// nil and surface failure as an in-band "error" event instead.
func (h *Handler) Execute(c *gin.Context) error {
	var req executeWorkflowRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	events, err := h.service.Execute(c.Request.Context(), c.Param("id"), middleware.UserIDFrom(c), req.Input)
	if err != nil {
		return err
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	c.Stream(func(w io.Writer) bool {
		event, ok := <-events
		if !ok {
			return false
		}
		payload, marshalErr := json.Marshal(toStreamEventResponse(event))
		if marshalErr != nil {
			return false
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload)
		// step events are non-terminal; done/error end the stream — same
		// pattern (and same past bug to avoid) as conversation's SendMessage.
		return event.Type != EventDone && event.Type != EventError
	})
	return nil
}

func (h *Handler) ListRuns(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	runs, total, err := h.service.ListRuns(c.Request.Context(), c.Param("id"), limit, offset)
	if err != nil {
		return err
	}
	items := make([]workflowRunResponse, 0, len(runs))
	for _, r := range runs {
		items = append(items, toWorkflowRunResponse(r))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[workflowRunResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) GetRun(c *gin.Context) error {
	run, err := h.service.GetRun(c.Request.Context(), c.Param("runId"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toWorkflowRunResponse(run))
	return nil
}

func (h *Handler) ListRunSteps(c *gin.Context) error {
	steps, err := h.service.ListRunSteps(c.Request.Context(), c.Param("runId"))
	if err != nil {
		return err
	}
	items := make([]workflowRunStepResponse, 0, len(steps))
	for _, s := range steps {
		items = append(items, toWorkflowRunStepResponse(s))
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
	return nil
}
