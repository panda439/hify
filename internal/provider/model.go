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
