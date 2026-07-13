package agent

import "time"

// defaultTemperature mirrors the agents.temperature column's DB default —
// applied here too so Create/Update behave the same whether or not the
// caller explicitly passed a value.
const defaultTemperature = 0.70

// Agent is the domain type for a configured chat assistant: a model
// selection plus its system prompt and sampling parameters.
type Agent struct {
	ID           string
	Name         string
	Description  string
	ModelID      string
	SystemPrompt string
	Temperature  float64
	MaxTokens    *int
	TopP         *float64
	ExtraParams  map[string]any
	IsActive     bool
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// CreateAgentInput and UpdateAgentInput share the same optional-field
// shape: Temperature/MaxTokens/TopP are pointers so the service can tell
// "not provided" (defaulted) apart from an explicit zero value — see
// CLAUDE.md's note on this exact bug with float/int JSON fields.
type CreateAgentInput struct {
	Name         string
	Description  string
	ModelID      string
	SystemPrompt string
	Temperature  *float64
	MaxTokens    *int
	TopP         *float64
	ExtraParams  map[string]any
	CreatedBy    string
}

type UpdateAgentInput struct {
	Name         string
	Description  string
	ModelID      string
	SystemPrompt string
	Temperature  *float64
	MaxTokens    *int
	TopP         *float64
	ExtraParams  map[string]any
	IsActive     bool
}
