package conversation

import "time"

type createConversationRequest struct {
	AgentID string `json:"agent_id" binding:"required"`
}

type conversationResponse struct {
	ID          string    `json:"id"`
	AgentID     string    `json:"agent_id"`
	Title       string    `json:"title"`
	LastMessage string    `json:"last_message"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func toConversationResponse(c Conversation) conversationResponse {
	return conversationResponse{
		ID:          c.ID,
		AgentID:     c.AgentID,
		Title:       c.Title,
		LastMessage: c.LastMessage,
		CreatedAt:   c.CreatedAt,
		UpdatedAt:   c.UpdatedAt,
	}
}

type messageResponse struct {
	ID         string    `json:"id"`
	Role       string    `json:"role"`
	Content    string    `json:"content"`
	TokenCount int       `json:"token_count"`
	CreatedAt  time.Time `json:"created_at"`
}

func toMessageResponse(m Message) messageResponse {
	return messageResponse{
		ID:         m.ID,
		Role:       m.Role,
		Content:    m.Content,
		TokenCount: m.TokenCount,
		CreatedAt:  m.CreatedAt,
	}
}

type sendMessageRequest struct {
	Content string `json:"content" binding:"required"`
}
