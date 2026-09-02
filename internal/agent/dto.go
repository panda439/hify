package agent

import "time"

// Temperature/MaxTokens/TopP are pointers so a client omitting the field is
// distinguishable from explicitly sending 0 — see model.go's note.
type createAgentRequest struct {
	Name             string         `json:"name" binding:"required"`
	Description      string         `json:"description"`
	ModelID          string         `json:"model_id" binding:"required"`
	SystemPrompt     string         `json:"system_prompt"`
	Temperature      *float64       `json:"temperature"`
	MaxTokens        *int           `json:"max_tokens"`
	TopP             *float64       `json:"top_p"`
	ExtraParams      map[string]any `json:"extra_params"`
	KnowledgeBaseIDs []string       `json:"knowledge_base_ids"`
	// DocumentIDs (004)：检索文档范围，空 = 不限定。
	DocumentIDs []string `json:"document_ids"`
	MCPToolIDs  []string `json:"mcp_tool_ids"`
}

type updateAgentRequest struct {
	Name             string         `json:"name" binding:"required"`
	Description      string         `json:"description"`
	ModelID          string         `json:"model_id" binding:"required"`
	SystemPrompt     string         `json:"system_prompt"`
	Temperature      *float64       `json:"temperature"`
	MaxTokens        *int           `json:"max_tokens"`
	TopP             *float64       `json:"top_p"`
	ExtraParams      map[string]any `json:"extra_params"`
	KnowledgeBaseIDs []string       `json:"knowledge_base_ids"`
	// DocumentIDs (004)：检索文档范围，空 = 不限定。
	DocumentIDs []string `json:"document_ids"`
	MCPToolIDs  []string `json:"mcp_tool_ids"`
	IsActive    bool     `json:"is_active"`
}

type agentResponse struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	ModelID          string         `json:"model_id"`
	SystemPrompt     string         `json:"system_prompt"`
	Temperature      float64        `json:"temperature"`
	MaxTokens        *int           `json:"max_tokens"`
	TopP             *float64       `json:"top_p"`
	ExtraParams      map[string]any `json:"extra_params"`
	KnowledgeBaseIDs []string       `json:"knowledge_base_ids"`
	// DocumentIDs (004)：检索文档范围，空 = 不限定。
	DocumentIDs []string  `json:"document_ids"`
	MCPToolIDs  []string  `json:"mcp_tool_ids"`
	IsActive    bool      `json:"is_active"`
	CreatedBy   string    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toAgentResponse(a Agent) agentResponse {
	return agentResponse{
		ID:               a.ID,
		Name:             a.Name,
		Description:      a.Description,
		ModelID:          a.ModelID,
		SystemPrompt:     a.SystemPrompt,
		Temperature:      a.Temperature,
		MaxTokens:        a.MaxTokens,
		TopP:             a.TopP,
		ExtraParams:      a.ExtraParams,
		KnowledgeBaseIDs: a.KnowledgeBaseIDs,
		DocumentIDs:      a.DocumentIDs,
		MCPToolIDs:       a.MCPToolIDs,
		IsActive:         a.IsActive,
		CreatedBy:        a.CreatedBy,
		CreatedAt:        a.CreatedAt,
		UpdatedAt:        a.UpdatedAt,
	}
}
