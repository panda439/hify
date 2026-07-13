package conversation

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"hify/internal/agent"
	"hify/internal/platform"
	"hify/internal/provider"
)

// Service is conversation's public contract.
type Service interface {
	CreateConversation(ctx context.Context, userID, agentID string) (Conversation, error)
	ListConversations(ctx context.Context, userID string, limit, offset int) ([]Conversation, int, error)
	ListMessages(ctx context.Context, userID, conversationID string, cursor *MessageCursor, limit int) ([]Message, string, error)

	// StreamMessage does all pre-flight validation synchronously (so
	// failures surface as normal HTTP errors) and only returns the event
	// channel once the upstream call is actually about to start — from
	// that point on, any failure becomes an in-band StreamEvent, never a
	// second HTTP response, since the handler will already have committed
	// SSE response headers by then.
	StreamMessage(ctx context.Context, userID, conversationID, content string) (<-chan StreamEvent, error)
}

// service is constructed via NewService in wire.go. agentSvc/providerSvc
// are depended on only through their Service interfaces, per the layering
// rule — conversation (layer 3) may call agent/provider (layers 1-2) this
// way but never touch their repositories.
type service struct {
	repo        *Repository
	agentSvc    agent.Service
	providerSvc provider.Service
}

func (s *service) CreateConversation(ctx context.Context, userID, agentID string) (Conversation, error) {
	ag, err := s.agentSvc.GetAgent(ctx, agentID)
	if err != nil {
		return Conversation{}, err
	}
	if !ag.IsActive {
		return Conversation{}, ErrAgentInactive
	}

	conv := Conversation{
		ID:      platform.NewID(),
		AgentID: agentID,
		UserID:  userID,
	}
	if err := s.repo.createConversation(ctx, conv); err != nil {
		return Conversation{}, err
	}
	return s.repo.getConversationForUser(ctx, conv.ID, userID)
}

func (s *service) ListConversations(ctx context.Context, userID string, limit, offset int) ([]Conversation, int, error) {
	limit = platform.ClampLimit(limit)
	convs, err := s.repo.listConversationsByUser(ctx, userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countConversationsByUser(ctx, userID)
	if err != nil {
		return nil, 0, err
	}
	return convs, total, nil
}

func (s *service) ListMessages(ctx context.Context, userID, conversationID string, cursor *MessageCursor, limit int) ([]Message, string, error) {
	if _, err := s.repo.getConversationForUser(ctx, conversationID, userID); err != nil {
		return nil, "", err
	}
	limit = platform.ClampLimit(limit)

	var rows []Message
	var err error
	if cursor == nil {
		rows, err = s.repo.listRecentMessages(ctx, conversationID, limit)
	} else {
		rows, err = s.repo.listMessagesBeforeCursor(ctx, conversationID, *cursor, limit)
	}
	if err != nil {
		return nil, "", err
	}

	var nextCursor string
	if len(rows) == limit {
		oldest := rows[len(rows)-1]
		nextCursor = EncodeCursor(MessageCursor{CreatedAt: oldest.CreatedAt, ID: oldest.ID})
	}

	reverseMessages(rows) // DB gives newest-first; the page itself reads chronologically
	return rows, nextCursor, nil
}

func (s *service) StreamMessage(ctx context.Context, userID, conversationID, content string) (<-chan StreamEvent, error) {
	conv, err := s.repo.getConversationForUser(ctx, conversationID, userID)
	if err != nil {
		return nil, err
	}

	ag, err := s.agentSvc.GetAgent(ctx, conv.AgentID)
	if err != nil {
		return nil, err
	}

	model, err := s.providerSvc.GetModel(ctx, ag.ModelID)
	if err != nil {
		return nil, err
	}

	client, err := s.providerSvc.ResolveClient(ctx, model.ProviderID)
	if err != nil {
		return nil, err
	}

	userMsg := Message{ID: platform.NewID(), ConversationID: conversationID, Role: string(provider.RoleUser), Content: content}
	if err := s.repo.createMessage(ctx, userMsg); err != nil {
		return nil, err
	}
	if err := s.repo.touchConversation(ctx, conversationID, time.Now()); err != nil {
		slog.Warn("conversation: touch after user message failed", "err", err, "conversation_id", conversationID)
	}

	chatMessages, err := s.assembleContext(ctx, conversationID, ag, model)
	if err != nil {
		return nil, err
	}

	req := provider.ChatRequest{
		Model:       model.ModelName,
		Messages:    chatMessages,
		Temperature: ag.Temperature,
		TopP:        derefFloat(ag.TopP),
		MaxTokens:   derefInt(ag.MaxTokens),
	}

	events := make(chan StreamEvent)
	go s.runStream(ctx, client, req, conversationID, events)
	return events, nil
}

// runStream owns the whole lifetime of one streamed reply: it forwards
// upstream chunks as StreamEvents and, regardless of how the stream ends
// (finished cleanly, upstream error, or client disconnect), persists
// whatever content was generated using a fresh background context — the
// request ctx passed to ChatStream may already be canceled by the time we
// get here, but a disconnect must not lose the partial reply.
func (s *service) runStream(ctx context.Context, client provider.Client, req provider.ChatRequest, conversationID string, events chan<- StreamEvent) {
	defer close(events)

	chunks, err := client.ChatStream(ctx, req)
	if err != nil {
		events <- StreamEvent{Type: EventError, Error: provider.WrapClientError(err).Error()}
		return
	}

	var buf strings.Builder
	var streamErr error
	for chunk := range chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
		if chunk.DeltaContent != "" {
			buf.WriteString(chunk.DeltaContent)
			events <- StreamEvent{Type: EventDelta, Content: chunk.DeltaContent}
		}
	}

	if buf.Len() > 0 {
		persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		msg := Message{ID: platform.NewID(), ConversationID: conversationID, Role: string(provider.RoleAssistant), Content: buf.String()}
		if err := s.repo.createMessage(persistCtx, msg); err != nil {
			slog.Error("conversation: persist assistant message failed", "err", err, "conversation_id", conversationID)
		}
		if err := s.repo.touchConversation(persistCtx, conversationID, time.Now()); err != nil {
			slog.Warn("conversation: touch after assistant message failed", "err", err, "conversation_id", conversationID)
		}
		cancel()
	}

	if streamErr != nil {
		events <- StreamEvent{Type: EventError, Error: provider.WrapClientError(streamErr).Error()}
		return
	}
	events <- StreamEvent{Type: EventDone}
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
