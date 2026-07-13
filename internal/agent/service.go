package agent

import (
	"context"

	"hify/internal/platform"
	"hify/internal/provider"
	"hify/internal/user"
)

// Service is agent's public contract — the only thing higher-layer modules
// (conversation) are allowed to depend on.
type Service interface {
	CreateAgent(ctx context.Context, input CreateAgentInput) (Agent, error)
	ListAgents(ctx context.Context, limit, offset int) ([]Agent, int, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	UpdateAgent(ctx context.Context, id, userID, role string, input UpdateAgentInput) (Agent, error)
}

// service is constructed via NewService in wire.go. providerSvc is
// depended on only through its Service interface, per the layering rule —
// agent (layer 2) may call provider (layer 1) this way but never touch its
// repository.
type service struct {
	repo        *Repository
	providerSvc provider.Service
}

func (s *service) CreateAgent(ctx context.Context, input CreateAgentInput) (Agent, error) {
	if err := s.validateModel(ctx, input.ModelID); err != nil {
		return Agent{}, err
	}

	a := Agent{
		ID:           platform.NewID(),
		Name:         input.Name,
		Description:  input.Description,
		ModelID:      input.ModelID,
		SystemPrompt: input.SystemPrompt,
		Temperature:  resolveTemperature(input.Temperature),
		MaxTokens:    input.MaxTokens,
		TopP:         input.TopP,
		ExtraParams:  input.ExtraParams,
		IsActive:     true,
		CreatedBy:    input.CreatedBy,
	}
	if err := s.repo.createAgent(ctx, a); err != nil {
		return Agent{}, err
	}
	return s.repo.getAgent(ctx, a.ID)
}

func (s *service) ListAgents(ctx context.Context, limit, offset int) ([]Agent, int, error) {
	limit = platform.ClampLimit(limit)
	agents, err := s.repo.listAgents(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countAgents(ctx)
	if err != nil {
		return nil, 0, err
	}
	return agents, total, nil
}

func (s *service) GetAgent(ctx context.Context, id string) (Agent, error) {
	return s.repo.getAgent(ctx, id)
}

// UpdateAgent covers both edit and "delete" (is_active:false) — there is no
// separate DELETE endpoint, mirroring the provider module's pattern.
func (s *service) UpdateAgent(ctx context.Context, id, userID, role string, input UpdateAgentInput) (Agent, error) {
	existing, err := s.repo.getAgent(ctx, id)
	if err != nil {
		return Agent{}, err
	}
	if existing.CreatedBy != userID && role != user.RoleAdmin {
		return Agent{}, ErrForbidden
	}

	// Re-validate the model every time, not just when model_id changed —
	// this call is cheap (single indexed lookup) and avoids relying on the
	// assumption that "it was valid when first saved" still holds.
	if err := s.validateModel(ctx, input.ModelID); err != nil {
		return Agent{}, err
	}

	existing.Name = input.Name
	existing.Description = input.Description
	existing.ModelID = input.ModelID
	existing.SystemPrompt = input.SystemPrompt
	existing.Temperature = resolveTemperature(input.Temperature)
	existing.MaxTokens = input.MaxTokens
	existing.TopP = input.TopP
	existing.ExtraParams = input.ExtraParams
	existing.IsActive = input.IsActive

	if err := s.repo.updateAgent(ctx, existing); err != nil {
		return Agent{}, err
	}
	return s.repo.getAgent(ctx, id)
}

// validateModel enforces the agent-specific rule that a model must both
// exist and be a currently-active chat model — "only chat models may be
// attached to an Agent" is agent's business rule, not provider's, so it's
// checked here rather than inside provider.Service.GetModel.
func (s *service) validateModel(ctx context.Context, modelID string) error {
	m, err := s.providerSvc.GetModel(ctx, modelID)
	if err != nil {
		return err
	}
	if m.Capability != provider.CapabilityChat || !m.IsActive {
		return ErrInvalidModel
	}
	return nil
}

func resolveTemperature(p *float64) float64 {
	if p == nil {
		return defaultTemperature
	}
	return *p
}
