package workflow

import "time"

// Definition is reused directly from model.go rather than duplicated into
// a parallel DTO-only shape — its JSON tags already match what the
// frontend's canvas editor sends/receives verbatim, and dag.go's Validate
// is what actually enforces correctness, not a binding tag here.
type createWorkflowRequest struct {
	Name        string     `json:"name" binding:"required"`
	Description string     `json:"description"`
	Definition  Definition `json:"definition" binding:"required"`
}

type updateWorkflowRequest struct {
	Name        string     `json:"name" binding:"required"`
	Description string     `json:"description"`
	Definition  Definition `json:"definition" binding:"required"`
	IsActive    bool       `json:"is_active"`
}

type executeWorkflowRequest struct {
	Input string `json:"input"`
}

type workflowResponse struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Definition  Definition `json:"definition"`
	IsActive    bool       `json:"is_active"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func toWorkflowResponse(wf Workflow) workflowResponse {
	return workflowResponse{
		ID:          wf.ID,
		Name:        wf.Name,
		Description: wf.Description,
		Definition:  wf.Definition,
		IsActive:    wf.IsActive,
		CreatedBy:   wf.CreatedBy,
		CreatedAt:   wf.CreatedAt,
		UpdatedAt:   wf.UpdatedAt,
	}
}

type workflowRunResponse struct {
	ID           string     `json:"id"`
	WorkflowID   string     `json:"workflow_id"`
	Status       string     `json:"status"`
	Input        string     `json:"input"`
	Output       string     `json:"output"`
	ErrorMessage string     `json:"error_message"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	CreatedBy    string     `json:"created_by"`
}

func toWorkflowRunResponse(run WorkflowRun) workflowRunResponse {
	return workflowRunResponse{
		ID:           run.ID,
		WorkflowID:   run.WorkflowID,
		Status:       run.Status,
		Input:        run.Input,
		Output:       run.Output,
		ErrorMessage: run.ErrorMessage,
		StartedAt:    run.StartedAt,
		FinishedAt:   run.FinishedAt,
		CreatedBy:    run.CreatedBy,
	}
}

type workflowRunStepResponse struct {
	ID            string     `json:"id"`
	WorkflowRunID string     `json:"workflow_run_id"`
	StepID        string     `json:"step_id"`
	StepType      string     `json:"step_type"`
	Status        string     `json:"status"`
	Input         string     `json:"input"`
	Output        string     `json:"output"`
	ErrorMessage  string     `json:"error_message"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at"`
}

func toWorkflowRunStepResponse(step WorkflowRunStep) workflowRunStepResponse {
	return workflowRunStepResponse{
		ID:            step.ID,
		WorkflowRunID: step.WorkflowRunID,
		StepID:        step.StepID,
		StepType:      step.StepType,
		Status:        step.Status,
		Input:         step.Input,
		Output:        step.Output,
		ErrorMessage:  step.ErrorMessage,
		StartedAt:     step.StartedAt,
		FinishedAt:    step.FinishedAt,
	}
}

// streamEventResponse is the JSON shape written to the SSE stream —
// separate from StepEvent (model.go) because the wire format uses
// snake_case and omits Go-side-only fields, same reasoning as
// conversation's StreamEvent already being its own JSON-tagged type.
type streamEventResponse struct {
	Type     string `json:"type"`
	RunID    string `json:"run_id,omitempty"`
	StepID   string `json:"step_id,omitempty"`
	StepType string `json:"step_type,omitempty"`
	Status   string `json:"status,omitempty"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

func toStreamEventResponse(e StepEvent) streamEventResponse {
	return streamEventResponse{
		Type:     e.Type,
		RunID:    e.RunID,
		StepID:   e.StepID,
		StepType: e.StepType,
		Status:   e.Status,
		Output:   e.Output,
		Error:    e.Error,
	}
}
