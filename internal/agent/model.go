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
	// KnowledgeBaseIDs backs RAG retrieval — conversation's context
	// assembly reads this off the Agent it already fetched instead of a
	// separate cross-module call. agent depends on knowledge (not the
	// reverse) specifically so this field can be validated here — see
	// CLAUDE.md's note on the agent/knowledge layer correction.
	KnowledgeBaseIDs []string
	// MCPToolIDs backs the conversation engine's tool-calling loop — same
	// reasoning as KnowledgeBaseIDs, agent depends on mcp (layer 1) so it
	// can validate tool IDs at create/update time.
	MCPToolIDs []string

	// DocumentIDs (004-agent-document-scope) narrows RAG retrieval to a
	// subset of the documents inside KnowledgeBaseIDs. EMPTY MEANS NO
	// RESTRICTION — retrieval then covers those knowledge bases in full,
	// exactly as it did before this field existed.
	//
	// It is a flat, agent-wide list rather than a per-knowledge-base
	// grouping: see the spec's Clarifications for why (grouping would
	// require teaching 002's RetrieveFilter about grouped document lists
	// and writing the recall SQL as several OR groups, and only pays off
	// for an agent that binds several knowledge bases while wanting to
	// restrict just some of them — a case that does not exist today).
	// The consequence to keep in mind: if an agent binds two knowledge
	// bases and this list only names documents from one, the other
	// contributes nothing. The Agent form makes that visible by listing
	// every selected document grouped by knowledge base.
	//
	// Feeds RetrieveOptions.Filter.DocumentIDs in conversation's context
	// assembly — it is never applied anywhere else, and never re-filtered
	// in Go after retrieval.
	DocumentIDs []string
}

// CreateAgentInput and UpdateAgentInput share the same optional-field
// shape: Temperature/MaxTokens/TopP are pointers so the service can tell
// "not provided" (defaulted) apart from an explicit zero value — see
// CLAUDE.md's note on this exact bug with float/int JSON fields.
type CreateAgentInput struct {
	Name             string
	Description      string
	ModelID          string
	SystemPrompt     string
	Temperature      *float64
	MaxTokens        *int
	TopP             *float64
	ExtraParams      map[string]any
	KnowledgeBaseIDs []string
	MCPToolIDs       []string
	DocumentIDs      []string
	CreatedBy        string
}

type UpdateAgentInput struct {
	Name             string
	Description      string
	ModelID          string
	SystemPrompt     string
	Temperature      *float64
	MaxTokens        *int
	TopP             *float64
	ExtraParams      map[string]any
	KnowledgeBaseIDs []string
	MCPToolIDs       []string
	DocumentIDs      []string
	IsActive         bool
}
