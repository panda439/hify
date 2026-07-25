package workflow

import (
	"context"

	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform"
	"hify/internal/provider"
	"hify/internal/user"
)

// Service is workflow's public contract. Workflow is the top (sixth)
// layer per CLAUDE.md, so nothing depends on it yet, but the same
// interface-based DI convention still applies for consistency.
type Service interface {
	CreateWorkflow(ctx context.Context, input CreateWorkflowInput) (Workflow, error)
	ListWorkflows(ctx context.Context, limit, offset int) ([]Workflow, int, error)
	GetWorkflow(ctx context.Context, id string) (Workflow, error)
	// UpdateWorkflow takes userID/role for the same creator-or-admin
	// ownership check agent/knowledge use — see CLAUDE.md's permission
	// model note.
	UpdateWorkflow(ctx context.Context, id, userID, role string, input UpdateWorkflowInput) (Workflow, error)

	// Execute starts a workflow run and streams its progress back over the
	// returned channel — see conversation's StreamMessage for the same
	// shape and reasoning. Any authenticated user may execute any active
	// workflow (a "use" operation, not a "modify" one — same reasoning as
	// any user being able to chat with any Agent regardless of who
	// created it).
	Execute(ctx context.Context, workflowID, userID, input string) (<-chan StepEvent, error)
	// ListRuns/GetRun/ListRunSteps take userID/role because run history is
	// NOT shared like the workflow definition itself — a run's input/output
	// and step trace can carry another user's prompts and retrieved
	// content, so non-admins only ever see their own runs (same
	// creator-or-admin check as UpdateWorkflow, applied per-run instead of
	// per-workflow).
	ListRuns(ctx context.Context, workflowID, userID, role string, limit, offset int) ([]WorkflowRun, int, error)
	GetRun(ctx context.Context, id, userID, role string) (WorkflowRun, error)
	ListRunSteps(ctx context.Context, runID, userID, role string) ([]WorkflowRunStep, error)
}

// service is constructed via NewService in wire.go.
type service struct {
	repo         *Repository
	providerSvc  provider.Service
	knowledgeSvc knowledge.Service
	mcpSvc       mcp.Service
}

func (s *service) CreateWorkflow(ctx context.Context, input CreateWorkflowInput) (Workflow, error) {
	if err := input.Definition.Validate(); err != nil {
		return Workflow{}, err
	}
	wf := Workflow{
		ID:          platform.NewID(),
		Name:        input.Name,
		Description: input.Description,
		Definition:  input.Definition,
		IsActive:    true,
		CreatedBy:   input.CreatedBy,
	}
	if err := s.repo.createWorkflow(ctx, wf); err != nil {
		return Workflow{}, err
	}
	return s.repo.getWorkflow(ctx, wf.ID)
}

func (s *service) ListWorkflows(ctx context.Context, limit, offset int) ([]Workflow, int, error) {
	limit = platform.ClampLimit(limit)
	workflows, err := s.repo.listWorkflows(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countWorkflows(ctx)
	if err != nil {
		return nil, 0, err
	}
	return workflows, total, nil
}

func (s *service) GetWorkflow(ctx context.Context, id string) (Workflow, error) {
	return s.repo.getWorkflow(ctx, id)
}

func (s *service) UpdateWorkflow(ctx context.Context, id, userID, role string, input UpdateWorkflowInput) (Workflow, error) {
	existing, err := s.repo.getWorkflow(ctx, id)
	if err != nil {
		return Workflow{}, err
	}
	if existing.CreatedBy != userID && role != user.RoleAdmin {
		return Workflow{}, ErrForbidden
	}
	if err := input.Definition.Validate(); err != nil {
		return Workflow{}, err
	}

	existing.Name = input.Name
	existing.Description = input.Description
	existing.Definition = input.Definition
	existing.IsActive = input.IsActive

	if err := s.repo.updateWorkflow(ctx, existing); err != nil {
		return Workflow{}, err
	}
	return s.repo.getWorkflow(ctx, id)
}

func (s *service) ListRuns(ctx context.Context, workflowID, userID, role string, limit, offset int) ([]WorkflowRun, int, error) {
	limit = platform.ClampLimit(limit)

	if role == user.RoleAdmin {
		runs, err := s.repo.listRuns(ctx, workflowID, limit, offset)
		if err != nil {
			return nil, 0, err
		}
		total, err := s.repo.countRuns(ctx, workflowID)
		if err != nil {
			return nil, 0, err
		}
		return runs, total, nil
	}

	runs, err := s.repo.listRunsByCreator(ctx, workflowID, userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countRunsByCreator(ctx, workflowID, userID)
	if err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

func (s *service) GetRun(ctx context.Context, id, userID, role string) (WorkflowRun, error) {
	run, err := s.repo.getRun(ctx, id)
	if err != nil {
		return WorkflowRun{}, err
	}
	if run.CreatedBy != userID && role != user.RoleAdmin {
		// Same not-found-collapsing as conversation's ownership check —
		// don't reveal that a run with this ID exists but belongs to
		// someone else.
		return WorkflowRun{}, ErrRunNotFound
	}
	return run, nil
}

func (s *service) ListRunSteps(ctx context.Context, runID, userID, role string) ([]WorkflowRunStep, error) {
	if _, err := s.GetRun(ctx, runID, userID, role); err != nil {
		return nil, err
	}
	return s.repo.listRunSteps(ctx, runID)
}
