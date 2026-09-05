package provider

import (
	"context"
	"fmt"
	"time"

	"hify/internal/platform"
)

// Service is provider's public contract — the only thing higher-layer
// modules (agent, conversation, knowledge, in later phases) are allowed to
// depend on, via ResolveClient.
type Service interface {
	CreateProvider(ctx context.Context, input CreateProviderInput) (Provider, error)
	ListProviders(ctx context.Context, limit, offset int) ([]Provider, int, error)
	GetProvider(ctx context.Context, id string) (Provider, error)
	UpdateProvider(ctx context.Context, id string, input UpdateProviderInput) (Provider, error)
	TestConnection(ctx context.Context, id string) error

	AddModel(ctx context.Context, providerID string, input CreateModelInput) (Model, error)
	GetModel(ctx context.Context, id string) (Model, error)
	ListModels(ctx context.Context, providerID string) ([]Model, error)
	ListModelsByCapability(ctx context.Context, capability string) ([]Model, error)
	UpdateModel(ctx context.Context, id string, input UpdateModelInput) (Model, error)

	// ResolveClient is what agent/conversation (Phase 2+) call to get a
	// live, resilience-wrapped Client for a given provider.
	ResolveClient(ctx context.Context, providerID string) (Client, error)

	// ChatOnce 发出**恰好一次**推理请求并如实报告结果（010 T014）。
	//
	// ⭐ 它与 Chat 的唯一区别是不自动重试。关系抽取要给出一个可复核的成本
	// 数字，而 Chat 背后可能是 1 次也可能是 3 次真实请求；重试改由调用方
	// 负责，因为只有那一层能把每一次尝试都记进账。限流/并发/熔断照常生效。
	//
	// ⚠️ 返回的 error 只表示"根本没能开始一次尝试"（模型不存在、供应商
	// 解析不出、装饰器接错）。调用失败本身是 ChatAttemptResult 里的一个
	// 如实结局，不是 error——这样调用方无法忘记记账。
	ChatOnce(ctx context.Context, modelID string, req ChatRequest, timeout time.Duration) (ChatAttemptResult, error)
}

// service is constructed via NewService in wire.go.
type service struct {
	repo *Repository
	reg  *registry
}

func (s *service) CreateProvider(ctx context.Context, input CreateProviderInput) (Provider, error) {
	if input.AdapterType == "" {
		input.AdapterType = AdapterOpenAICompatible
	}
	if input.AdapterType != AdapterOpenAICompatible {
		return Provider{}, ErrUnsupportedType
	}
	if input.AuthType == AuthTypeAPIKey && input.APIKey == "" {
		return Provider{}, ErrAPIKeyRequired
	}

	var encrypted []byte
	if input.AuthType == AuthTypeAPIKey {
		var err error
		encrypted, err = encryptAPIKey(s.reg.encryptionKey, input.APIKey)
		if err != nil {
			return Provider{}, err
		}
	}

	p := Provider{
		ID:             platform.NewID(),
		Name:           input.Name,
		AdapterType:    input.AdapterType,
		BaseURL:        input.BaseURL,
		AuthType:       input.AuthType,
		ExtraHeaders:   input.ExtraHeaders,
		ExtraConfig:    input.ExtraConfig,
		LastTestStatus: TestStatusUnknown,
		IsActive:       true,
		CreatedBy:      input.CreatedBy,
	}
	if err := s.repo.createProvider(ctx, p, encrypted); err != nil {
		return Provider{}, err
	}
	return s.repo.getProvider(ctx, p.ID)
}

func (s *service) ListProviders(ctx context.Context, limit, offset int) ([]Provider, int, error) {
	limit = platform.ClampLimit(limit)
	providers, err := s.repo.listProviders(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countProviders(ctx)
	if err != nil {
		return nil, 0, err
	}
	return providers, total, nil
}

func (s *service) GetProvider(ctx context.Context, id string) (Provider, error) {
	return s.repo.getProvider(ctx, id)
}

func (s *service) UpdateProvider(ctx context.Context, id string, input UpdateProviderInput) (Provider, error) {
	existing, err := s.repo.getProvider(ctx, id)
	if err != nil {
		return Provider{}, err
	}
	if input.AuthType == AuthTypeAPIKey && input.APIKey == nil && !existing.HasAPIKey {
		return Provider{}, ErrAPIKeyRequired
	}

	existing.Name = input.Name
	existing.BaseURL = input.BaseURL
	existing.AuthType = input.AuthType
	existing.ExtraHeaders = input.ExtraHeaders
	existing.ExtraConfig = input.ExtraConfig
	existing.IsActive = input.IsActive

	if err := s.repo.updateProvider(ctx, existing); err != nil {
		return Provider{}, err
	}

	if input.APIKey != nil {
		var encrypted []byte
		if *input.APIKey != "" {
			encrypted, err = encryptAPIKey(s.reg.encryptionKey, *input.APIKey)
			if err != nil {
				return Provider{}, err
			}
		}
		if err := s.repo.updateAPIKey(ctx, id, encrypted); err != nil {
			return Provider{}, err
		}
	}

	// Config (base_url/auth/key/extra_config/is_active) may have changed —
	// evict the cached client so the next resolve() rebuilds against it.
	s.reg.invalidate(id)

	return s.repo.getProvider(ctx, id)
}

// TestConnection goes through the same resilience-wrapped client a real
// call would use, so a successful test is a meaningful signal — not a
// separate, unrepresentative code path.
func (s *service) TestConnection(ctx context.Context, id string) error {
	client, err := s.reg.resolve(ctx, id)
	if err != nil {
		return err
	}

	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	testErr := client.TestConnection(testCtx)

	status, errMsg := TestStatusSuccess, ""
	var wrappedErr error
	if testErr != nil {
		status = TestStatusFailed
		wrappedErr = WrapClientError(testErr)
		errMsg = wrappedErr.Error() // apperr.AppError.Error() is the Chinese message, safe to store/display
	}

	if err := s.repo.updateTestResult(ctx, id, status, errMsg); err != nil {
		return err
	}
	return wrappedErr
}

// GetModel is what agent.Service uses to validate a model_id at
// create/update time (must resolve, and the caller checks
// Capability/IsActive itself — this method doesn't enforce a capability,
// callers with different needs use it differently).
func (s *service) GetModel(ctx context.Context, id string) (Model, error) {
	return s.repo.getModel(ctx, id)
}

func (s *service) AddModel(ctx context.Context, providerID string, input CreateModelInput) (Model, error) {
	if _, err := s.repo.getProvider(ctx, providerID); err != nil {
		return Model{}, err
	}
	// 001-rag-query-rerank：能力白名单放行 rerank（第三类模型能力，见
	// model.go 的 CapabilityRerank 文档注释）；非法值仍归为
	// ErrWrongCapability，映射到中文 invalid_input。
	if input.Capability != CapabilityChat && input.Capability != CapabilityEmbedding && input.Capability != CapabilityRerank {
		return Model{}, ErrWrongCapability
	}

	m := Model{
		ID:                 platform.NewID(),
		ProviderID:         providerID,
		ModelName:          input.ModelName,
		Capability:         input.Capability,
		ContextWindow:      input.ContextWindow,
		MaxOutputTokens:    input.MaxOutputTokens,
		EmbeddingDimension: input.EmbeddingDimension,
		IsDefault:          input.IsDefault,
		IsActive:           true,
	}
	if err := s.repo.createModel(ctx, m); err != nil {
		return Model{}, err
	}
	return s.repo.getModel(ctx, m.ID)
}

func (s *service) ListModels(ctx context.Context, providerID string) ([]Model, error) {
	return s.repo.listModelsByProvider(ctx, providerID)
}

func (s *service) ListModelsByCapability(ctx context.Context, capability string) ([]Model, error) {
	return s.repo.listActiveModelsByCapability(ctx, capability)
}

func (s *service) UpdateModel(ctx context.Context, id string, input UpdateModelInput) (Model, error) {
	existing, err := s.repo.getModel(ctx, id)
	if err != nil {
		return Model{}, err
	}
	existing.ContextWindow = input.ContextWindow
	existing.MaxOutputTokens = input.MaxOutputTokens
	existing.EmbeddingDimension = input.EmbeddingDimension
	existing.IsDefault = input.IsDefault
	existing.IsActive = input.IsActive

	if err := s.repo.updateModel(ctx, existing); err != nil {
		return Model{}, err
	}
	return s.repo.getModel(ctx, id)
}

func (s *service) ResolveClient(ctx context.Context, providerID string) (Client, error) {
	return s.reg.resolve(ctx, providerID)
}

func (s *service) ChatOnce(ctx context.Context, modelID string, req ChatRequest, timeout time.Duration) (ChatAttemptResult, error) {
	model, err := s.GetModel(ctx, modelID)
	if err != nil {
		return ChatAttemptResult{}, err
	}
	client, err := s.ResolveClient(ctx, model.ProviderID)
	if err != nil {
		return ChatAttemptResult{}, err
	}
	sac, ok := client.(SingleAttemptChatter)
	if !ok {
		// ⚠️ 绝不退回去调 client.Chat。那会带上自动重试，账目静默失真——
		// 一次调用记成一次、实际发了三次，而且没有任何症状。
		// 宁可这条路直接失败，让接线错误立刻暴露。
		return ChatAttemptResult{}, fmt.Errorf(
			"provider: client for %s does not support single-attempt chat", model.ProviderID)
	}
	return sac.ChatOnce(ctx, req, timeout)
}
