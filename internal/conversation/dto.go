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
	// Citations is always present (empty slice, never omitted/null) so the
	// frontend contract doesn't have to special-case "no citations" versus
	// "field absent" — see CLAUDE.md's Citation V1 spec section 11.2: every
	// message carries the same shape regardless of role or whether it has
	// any citations.
	Citations []CitationResponse `json:"citations"`
}

// CitationResponse is message_citations' HTTP-facing shape — PageNumber/
// SectionTitle are always null in Citation V1 (see Citation's doc comment)
// but kept on the wire contract now so the frontend doesn't need a
// breaking change once knowledge gains real page/section extraction.
type CitationResponse struct {
	Ref             string  `json:"ref"`
	KnowledgeBaseID string  `json:"knowledge_base_id"`
	DocumentID      string  `json:"document_id"`
	DocumentName    string  `json:"document_name"`
	ChunkID         string  `json:"chunk_id"`
	ChunkIndex      int     `json:"chunk_index"`
	PageNumber      *int    `json:"page_number"`
	SectionTitle    *string `json:"section_title"`
	Quote           string  `json:"quote"`
	Score           float64 `json:"score"`
}

func toCitationResponse(c Citation) CitationResponse {
	return CitationResponse{
		Ref:             c.Ref,
		KnowledgeBaseID: c.KnowledgeBaseID,
		DocumentID:      c.DocumentID,
		DocumentName:    c.DocumentName,
		ChunkID:         c.ChunkID,
		ChunkIndex:      c.ChunkIndex,
		PageNumber:      nil,
		SectionTitle:    nil,
		Quote:           c.Quote,
		Score:           c.Score,
	}
}

func toCitationResponses(citations []Citation) []CitationResponse {
	out := make([]CitationResponse, 0, len(citations))
	for _, c := range citations {
		out = append(out, toCitationResponse(c))
	}
	return out
}

func toMessageResponse(m Message, citations []Citation) messageResponse {
	return messageResponse{
		ID:         m.ID,
		Role:       m.Role,
		Content:    m.Content,
		TokenCount: m.TokenCount,
		CreatedAt:  m.CreatedAt,
		Citations:  toCitationResponses(citations),
	}
}

type sendMessageRequest struct {
	Content string `json:"content" binding:"required"`

	// Relation 是「这一轮问的是人物关系」的显式选项（010 T035）。
	//
	// ⭐ 是**显式选项**而不是从提问里猜意图。⚠️ 猜错的两个方向后果都不小：
	// 该走关系分支却走了普通 RAG，用户得到一个基于零散片段的含糊回答；
	// 不该走却走了，一个普通问题被答成"没有找到这两个人的关系记录"。
	// 而无论哪一种，用户都看不出系统做过一次判断。
	Relation *relationQueryRequest `json:"relation"`
}

type relationQueryRequest struct {
	DocumentID string `json:"document_id" binding:"required"`
	Subject    string `json:"subject" binding:"required"`
	Object     string `json:"object" binding:"required"`
	SubjectID  string `json:"subject_character_id"`
	ObjectID   string `json:"object_character_id"`
}
