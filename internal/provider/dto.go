package provider

import "time"

type createProviderRequest struct {
	Name         string            `json:"name" binding:"required"`
	BaseURL      string            `json:"base_url" binding:"required"`
	AuthType     string            `json:"auth_type" binding:"required,oneof=api_key none"`
	APIKey       string            `json:"api_key"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	ExtraConfig  ExtraConfig       `json:"extra_config"`
}

type updateProviderRequest struct {
	Name         string            `json:"name" binding:"required"`
	BaseURL      string            `json:"base_url" binding:"required"`
	AuthType     string            `json:"auth_type" binding:"required,oneof=api_key none"`
	APIKey       *string           `json:"api_key"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	ExtraConfig  ExtraConfig       `json:"extra_config"`
	IsActive     bool              `json:"is_active"`
}

type providerResponse struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	AdapterType     string      `json:"adapter_type"`
	BaseURL         string      `json:"base_url"`
	AuthType        string      `json:"auth_type"`
	HasAPIKey       bool        `json:"has_api_key"`
	ExtraHeaderKeys []string    `json:"extra_header_keys,omitempty"`
	ExtraConfig     ExtraConfig `json:"extra_config"`
	LastTestedAt    *time.Time  `json:"last_tested_at"`
	LastTestStatus  string      `json:"last_test_status"`
	LastTestError   string      `json:"last_test_error"`
	IsActive        bool        `json:"is_active"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}

func toProviderResponse(p Provider) providerResponse {
	return providerResponse{
		ID:              p.ID,
		Name:            p.Name,
		AdapterType:     p.AdapterType,
		BaseURL:         p.BaseURL,
		AuthType:        p.AuthType,
		HasAPIKey:       p.HasAPIKey,
		ExtraHeaderKeys: extraHeaderKeys(p.ExtraHeaders),
		ExtraConfig:     p.ExtraConfig,
		LastTestedAt:    p.LastTestedAt,
		LastTestStatus:  p.LastTestStatus,
		LastTestError:   p.LastTestError,
		IsActive:        p.IsActive,
		CreatedAt:       p.CreatedAt,
		UpdatedAt:       p.UpdatedAt,
	}
}

// extraHeaderKeys returns only the header names, never their values — a
// header may carry a secret (bearer token, API key passed as a custom
// header), same asymmetry as HasAPIKey and mcp.serverResponse's
// EnvKeys/HeaderKeys.
func extraHeaderKeys(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	out := make([]string, 0, len(headers))
	for k := range headers {
		out = append(out, k)
	}
	return out
}

type addModelRequest struct {
	ModelName          string `json:"model_name" binding:"required"`
	Capability         string `json:"capability" binding:"required,oneof=chat embedding"`
	ContextWindow      *int   `json:"context_window"`
	MaxOutputTokens    *int   `json:"max_output_tokens"`
	EmbeddingDimension *int   `json:"embedding_dimension"`
	IsDefault          bool   `json:"is_default"`
}

type updateModelRequest struct {
	ContextWindow      *int `json:"context_window"`
	MaxOutputTokens    *int `json:"max_output_tokens"`
	EmbeddingDimension *int `json:"embedding_dimension"`
	IsDefault          bool `json:"is_default"`
	IsActive           bool `json:"is_active"`
}

type modelResponse struct {
	ID                 string `json:"id"`
	ProviderID         string `json:"provider_id"`
	ModelName          string `json:"model_name"`
	Capability         string `json:"capability"`
	ContextWindow      *int   `json:"context_window"`
	MaxOutputTokens    *int   `json:"max_output_tokens"`
	EmbeddingDimension *int   `json:"embedding_dimension"`
	IsDefault          bool   `json:"is_default"`
	IsActive           bool   `json:"is_active"`
}

func toModelResponse(m Model) modelResponse {
	return modelResponse{
		ID:                 m.ID,
		ProviderID:         m.ProviderID,
		ModelName:          m.ModelName,
		Capability:         m.Capability,
		ContextWindow:      m.ContextWindow,
		MaxOutputTokens:    m.MaxOutputTokens,
		EmbeddingDimension: m.EmbeddingDimension,
		IsDefault:          m.IsDefault,
		IsActive:           m.IsActive,
	}
}
