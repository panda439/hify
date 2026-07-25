package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"hify/internal/db/gen"
)

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	queries *gen.Queries
}

func (r *Repository) createWorkflow(ctx context.Context, wf Workflow) error {
	defJSON, err := json.Marshal(wf.Definition)
	if err != nil {
		return fmt.Errorf("workflow: marshal definition: %w", err)
	}
	if err := r.queries.CreateWorkflow(ctx, gen.CreateWorkflowParams{
		ID:          wf.ID,
		Name:        wf.Name,
		Description: wf.Description,
		Definition:  defJSON,
		CreatedBy:   wf.CreatedBy,
	}); err != nil {
		return fmt.Errorf("workflow: create: %w", err)
	}
	return nil
}

func (r *Repository) getWorkflow(ctx context.Context, id string) (Workflow, error) {
	row, err := r.queries.GetWorkflowByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Workflow{}, ErrNotFound
		}
		return Workflow{}, fmt.Errorf("workflow: get: %w", err)
	}
	return toDomainWorkflow(row)
}

func (r *Repository) listWorkflows(ctx context.Context, limit, offset int) ([]Workflow, error) {
	rows, err := r.queries.ListWorkflows(ctx, gen.ListWorkflowsParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("workflow: list: %w", err)
	}
	out := make([]Workflow, 0, len(rows))
	for _, row := range rows {
		wf, err := toDomainWorkflow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, wf)
	}
	return out, nil
}

func (r *Repository) countWorkflows(ctx context.Context) (int, error) {
	n, err := r.queries.CountWorkflows(ctx)
	if err != nil {
		return 0, fmt.Errorf("workflow: count: %w", err)
	}
	return int(n), nil
}

func (r *Repository) updateWorkflow(ctx context.Context, wf Workflow) error {
	defJSON, err := json.Marshal(wf.Definition)
	if err != nil {
		return fmt.Errorf("workflow: marshal definition: %w", err)
	}
	if err := r.queries.UpdateWorkflow(ctx, gen.UpdateWorkflowParams{
		Name:        wf.Name,
		Description: wf.Description,
		Definition:  defJSON,
		IsActive:    wf.IsActive,
		ID:          wf.ID,
	}); err != nil {
		return fmt.Errorf("workflow: update: %w", err)
	}
	return nil
}

func (r *Repository) createRun(ctx context.Context, run WorkflowRun) error {
	if err := r.queries.CreateWorkflowRun(ctx, gen.CreateWorkflowRunParams{
		ID:         run.ID,
		WorkflowID: run.WorkflowID,
		Status:     run.Status,
		Input:      run.Input,
		CreatedBy:  run.CreatedBy,
	}); err != nil {
		return fmt.Errorf("workflow: create run: %w", err)
	}
	return nil
}

func (r *Repository) finishRun(ctx context.Context, run WorkflowRun) error {
	if err := r.queries.FinishWorkflowRun(ctx, gen.FinishWorkflowRunParams{
		Status:       run.Status,
		Output:       run.Output,
		ErrorMessage: run.ErrorMessage,
		FinishedAt:   timeToNullTime(run.FinishedAt),
		ID:           run.ID,
	}); err != nil {
		return fmt.Errorf("workflow: finish run: %w", err)
	}
	return nil
}

func (r *Repository) getRun(ctx context.Context, id string) (WorkflowRun, error) {
	row, err := r.queries.GetWorkflowRunByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return WorkflowRun{}, ErrRunNotFound
		}
		return WorkflowRun{}, fmt.Errorf("workflow: get run: %w", err)
	}
	return toDomainRun(row), nil
}

func (r *Repository) listRuns(ctx context.Context, workflowID string, limit, offset int) ([]WorkflowRun, error) {
	rows, err := r.queries.ListWorkflowRuns(ctx, gen.ListWorkflowRunsParams{
		WorkflowID: workflowID,
		Limit:      int32(limit),
		Offset:     int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("workflow: list runs: %w", err)
	}
	out := make([]WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainRun(row))
	}
	return out, nil
}

func (r *Repository) countRuns(ctx context.Context, workflowID string) (int, error) {
	n, err := r.queries.CountWorkflowRuns(ctx, workflowID)
	if err != nil {
		return 0, fmt.Errorf("workflow: count runs: %w", err)
	}
	return int(n), nil
}

func (r *Repository) listRunsByCreator(ctx context.Context, workflowID, createdBy string, limit, offset int) ([]WorkflowRun, error) {
	rows, err := r.queries.ListWorkflowRunsByCreator(ctx, gen.ListWorkflowRunsByCreatorParams{
		WorkflowID: workflowID,
		CreatedBy:  createdBy,
		Limit:      int32(limit),
		Offset:     int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("workflow: list runs by creator: %w", err)
	}
	out := make([]WorkflowRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainRun(row))
	}
	return out, nil
}

func (r *Repository) countRunsByCreator(ctx context.Context, workflowID, createdBy string) (int, error) {
	n, err := r.queries.CountWorkflowRunsByCreator(ctx, gen.CountWorkflowRunsByCreatorParams{
		WorkflowID: workflowID,
		CreatedBy:  createdBy,
	})
	if err != nil {
		return 0, fmt.Errorf("workflow: count runs by creator: %w", err)
	}
	return int(n), nil
}

func (r *Repository) createRunStep(ctx context.Context, step WorkflowRunStep) error {
	if err := r.queries.CreateWorkflowRunStep(ctx, gen.CreateWorkflowRunStepParams{
		ID:            step.ID,
		WorkflowRunID: step.WorkflowRunID,
		StepID:        step.StepID,
		StepType:      step.StepType,
		Status:        step.Status,
		Input:         step.Input,
		Output:        step.Output,
		ErrorMessage:  step.ErrorMessage,
		StartedAt:     step.StartedAt,
		FinishedAt:    timeToNullTime(step.FinishedAt),
	}); err != nil {
		return fmt.Errorf("workflow: create run step: %w", err)
	}
	return nil
}

func (r *Repository) listRunSteps(ctx context.Context, workflowRunID string) ([]WorkflowRunStep, error) {
	rows, err := r.queries.ListWorkflowRunSteps(ctx, workflowRunID)
	if err != nil {
		return nil, fmt.Errorf("workflow: list run steps: %w", err)
	}
	out := make([]WorkflowRunStep, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainRunStep(row))
	}
	return out, nil
}

func toDomainWorkflow(row gen.Workflow) (Workflow, error) {
	var def Definition
	if err := json.Unmarshal(row.Definition, &def); err != nil {
		return Workflow{}, fmt.Errorf("workflow: unmarshal definition: %w", err)
	}
	return Workflow{
		ID:          row.ID,
		Name:        row.Name,
		Description: row.Description,
		Definition:  def,
		IsActive:    row.IsActive,
		CreatedBy:   row.CreatedBy,
		CreatedAt:   row.CreatedAt,
		UpdatedAt:   row.UpdatedAt,
	}, nil
}

func toDomainRun(row gen.WorkflowRun) WorkflowRun {
	return WorkflowRun{
		ID:           row.ID,
		WorkflowID:   row.WorkflowID,
		Status:       row.Status,
		Input:        row.Input,
		Output:       row.Output,
		ErrorMessage: row.ErrorMessage,
		StartedAt:    row.StartedAt,
		FinishedAt:   nullTimeToPtr(row.FinishedAt),
		CreatedBy:    row.CreatedBy,
	}
}

func toDomainRunStep(row gen.WorkflowRunStep) WorkflowRunStep {
	return WorkflowRunStep{
		ID:            row.ID,
		WorkflowRunID: row.WorkflowRunID,
		StepID:        row.StepID,
		StepType:      row.StepType,
		Status:        row.Status,
		Input:         row.Input,
		Output:        row.Output,
		ErrorMessage:  row.ErrorMessage,
		StartedAt:     row.StartedAt,
		FinishedAt:    nullTimeToPtr(row.FinishedAt),
	}
}

func timeToNullTime(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func nullTimeToPtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}
