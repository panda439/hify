package provider

import "time"

const (
	AdapterOpenAICompatible = "openai_compatible"
)

const (
	AuthTypeAPIKey = "api_key"
	AuthTypeNone   = "none"
)

const (
	TestStatusUnknown = "unknown"
	TestStatusSuccess = "success"
	TestStatusFailed  = "failed"
)

const (
	CapabilityChat      = "chat"
	CapabilityEmbedding = "embedding"
	// CapabilityRerank 是 001-rag-query-rerank 新增的第三类模型能力——供应
	// 商体系原本只区分 chat/embedding，重排序需要接入交叉编码器式的
	// /rerank 端点（见 llm.go 的 RerankRequest），因此单独开一个能力枚举，
	// 而不是复用 embedding（量纲和调用协议完全不同）。migration 000012 把
	// provider_models.capability 的 CHECK 约束同步扩到三个合法值。
	CapabilityRerank = "rerank"
)

// ExtraConfig holds the resilience/adapter knobs stored in
// model_providers.extra_config — deliberately a flat struct of optional
// overrides, not a required config block: zero values mean "use the
// registry's sane defaults" (see registry.go).
type ExtraConfig struct {
	IdleTimeoutSeconds int `json:"idle_timeout_seconds,omitempty"`
	MaxConcurrent      int `json:"max_concurrent,omitempty"`
	RateLimitPerMinute int `json:"rate_limit_per_minute,omitempty"`
}

// Provider is the domain type for a configured LLM provider connection.
// It never carries the decrypted API key — HasAPIKey only tells callers
// whether one is configured, for UI display.
type Provider struct {
	ID             string
	Name           string
	AdapterType    string
	BaseURL        string
	AuthType       string
	HasAPIKey      bool
	ExtraHeaders   map[string]string
	ExtraConfig    ExtraConfig
	LastTestedAt   *time.Time
	LastTestStatus string
	LastTestError  string
	IsActive       bool
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Model is one model_name+capability offered by a Provider.
type Model struct {
	ID                 string
	ProviderID         string
	ModelName          string
	Capability         string
	ContextWindow      *int
	MaxOutputTokens    *int
	EmbeddingDimension *int
	IsDefault          bool
	IsActive           bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type CreateProviderInput struct {
	Name         string
	AdapterType  string
	BaseURL      string
	AuthType     string
	APIKey       string // plaintext; service encrypts before storing
	ExtraHeaders map[string]string
	ExtraConfig  ExtraConfig
	CreatedBy    string
}

// UpdateProviderInput.APIKey: nil means "leave unchanged", a non-nil value
// (including empty string, valid when AuthType=none) replaces it.
type UpdateProviderInput struct {
	Name         string
	BaseURL      string
	AuthType     string
	APIKey       *string
	ExtraHeaders map[string]string
	ExtraConfig  ExtraConfig
	IsActive     bool
}

type CreateModelInput struct {
	ModelName          string
	Capability         string
	ContextWindow      *int
	MaxOutputTokens    *int
	EmbeddingDimension *int
	IsDefault          bool
}

type UpdateModelInput struct {
	ContextWindow      *int
	MaxOutputTokens    *int
	EmbeddingDimension *int
	IsDefault          bool
	IsActive           bool
}
