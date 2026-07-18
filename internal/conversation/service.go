package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform"
	"hify/internal/provider"
)

// maxToolCallIterations bounds the tool-calling loop — a model that keeps
// requesting tools (a bad prompt, a confused model, or a tool that always
// looks "incomplete" to it) must not turn one conversation turn into an
// unbounded loop of upstream calls.
const maxToolCallIterations = 5

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

// service is constructed via NewService in wire.go. agentSvc/providerSvc/
// knowledgeSvc/mcpSvc are depended on only through their Service
// interfaces, per the layering rule — conversation (layer 4) may call
// agent/provider/mcp (layer 1/3) and knowledge (layer 2) this way but
// never touch their repositories.
type service struct {
	repo         *Repository
	agentSvc     agent.Service
	providerSvc  provider.Service
	knowledgeSvc knowledge.Service
	mcpSvc       mcp.Service
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

	assembled, err := s.assembleContext(ctx, conversationID, ag, model, content)
	if err != nil {
		return nil, err
	}

	req := provider.ChatRequest{
		Model:       model.ModelName,
		Messages:    assembled.Messages,
		Temperature: ag.Temperature,
		TopP:        derefFloat(ag.TopP),
		MaxTokens:   derefInt(ag.MaxTokens),
		Tools:       assembled.Tools,
	}

	events := make(chan StreamEvent)
	go s.runStream(ctx, client, req, conversationID, assembled.Retrieved, assembled.ToolNameToID, events)
	return events, nil
}

// runStream owns the whole lifetime of one streamed reply, including the
// tool-calling loop: each iteration streams one ChatStream response; if it
// ends with finish_reason=="tool_calls", the accumulated calls are
// dispatched to mcp.Service, the results are persisted as `role: tool`
// messages and fed back into the next iteration's request, and the loop
// continues — until a normal "stop" finish, an error, or
// maxToolCallIterations is hit. Regardless of how any iteration ends,
// whatever content/tool-calls were generated get persisted using a fresh
// background context — the request ctx passed to ChatStream may already
// be canceled by the time we get here, but a disconnect must not lose the
// partial reply.
func (s *service) runStream(ctx context.Context, client provider.Client, req provider.ChatRequest, conversationID string, retrieved []knowledge.RetrievedChunk, toolNameToID map[string]string, events chan<- StreamEvent) {
	defer close(events)
	// Must run after (i.e. be deferred before, since defers are LIFO) the
	// close(events) above so a recovered panic can still get an error event
	// out on the still-open channel — see CLAUDE.md's goroutine recover
	// rule; every chat message goes through this goroutine, so an
	// unrecovered panic here would take the whole process down for every
	// concurrent user, not just this one request.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("conversation: panic in runStream", "panic", r, "conversation_id", conversationID, "stack", string(debug.Stack()))
			trySend(ctx, events, StreamEvent{Type: EventError, Error: "服务器内部错误，请稍后重试"})
		}
	}()

	if len(retrieved) > 0 {
		if !trySend(ctx, events, StreamEvent{Type: EventRetrieval, Retrieved: toRetrievedChunkInfo(retrieved)}) {
			return
		}
	}

	messages := req.Messages

	for iteration := 0; iteration < maxToolCallIterations; iteration++ {
		req.Messages = messages

		chunks, err := client.ChatStream(ctx, req)
		if err != nil {
			trySend(ctx, events, StreamEvent{Type: EventError, Error: provider.WrapClientError(err).Error()})
			return
		}

		var buf strings.Builder
		var toolCalls []provider.ToolCall
		var finishReason string
		var streamErr error
		for chunk := range chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
				break
			}
			if chunk.DeltaContent != "" {
				buf.WriteString(chunk.DeltaContent)
				if !trySend(ctx, events, StreamEvent{Type: EventDelta, Content: chunk.DeltaContent}) {
					// Client disconnected (or the request context was
					// otherwise canceled) and nobody downstream is reading
					// events anymore — a plain send here would block this
					// goroutine forever (see resilience.go's matching fix).
					// Bail, but don't lose whatever was already generated.
					s.persistAssistantTurn(conversationID, buf.String(), nil)
					return
				}
			}
			if len(chunk.DeltaToolCalls) > 0 {
				toolCalls = mergeToolCallDeltas(toolCalls, chunk.DeltaToolCalls)
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
			}
		}

		if streamErr != nil {
			s.persistAssistantTurn(conversationID, buf.String(), nil)
			trySend(ctx, events, StreamEvent{Type: EventError, Error: provider.WrapClientError(streamErr).Error()})
			return
		}

		if finishReason == "tool_calls" && len(toolCalls) > 0 && len(toolNameToID) > 0 {
			s.persistAssistantTurn(conversationID, buf.String(), toolCalls)
			messages = append(messages, provider.Message{Role: provider.RoleAssistant, Content: buf.String(), ToolCalls: toolCalls})

			for _, tc := range toolCalls {
				result := s.runToolCall(ctx, tc, toolNameToID, conversationID, events)
				messages = append(messages, provider.Message{Role: provider.RoleTool, Content: result, ToolCallID: tc.ID})
			}
			continue
		}

		s.persistAssistantTurn(conversationID, buf.String(), nil)
		trySend(ctx, events, StreamEvent{Type: EventDone})
		return
	}

	trySend(ctx, events, StreamEvent{Type: EventError, Error: "工具调用次数过多，已终止本轮对话"})
}

// trySend delivers evt on events but never blocks past ctx's cancellation.
// Once the client disconnects, gin's SSE writer (handler.go's c.Stream)
// stops reading from events entirely, and a plain `events <- evt` would
// then block this goroutine forever — which in turn stops it draining
// chunks, which in turn blocks resilience.go's forwarder goroutine on its
// own send, pinning that provider's concurrency semaphore indefinitely.
// false means the caller should treat delivery as abandoned.
func trySend(ctx context.Context, events chan<- StreamEvent, evt StreamEvent) bool {
	select {
	case events <- evt:
		return true
	case <-ctx.Done():
		return false
	}
}

// runToolCall dispatches one accumulated tool call to mcp.Service, emits
// the running/done|error StreamEvents the chat UI's tool-call trace needs,
// persists the resulting `role: tool` message, and returns the message
// content to feed back into the next ChatStream call — a Chinese
// explanation on failure, since every tool_call from an assistant message
// must get a matching tool response or the next API call is malformed.
func (s *service) runToolCall(ctx context.Context, tc provider.ToolCall, toolNameToID map[string]string, conversationID string, events chan<- StreamEvent) string {
	// Best-effort: whether or not anyone is still listening, the tool must
	// still run and its result must still be persisted below (every
	// tool_call needs a matching tool response for the next API call to be
	// well-formed) — so a failed send here doesn't abort the function.
	trySend(ctx, events, StreamEvent{Type: EventToolCall, ToolCall: &ToolCallInfo{Name: tc.Name, Status: "running"}})

	var result string
	status := "done"

	toolID, ok := toolNameToID[tc.Name]
	if !ok {
		result = "该工具当前不可用"
		status = "error"
	} else {
		callResult, err := s.mcpSvc.CallTool(ctx, toolID, tc.Arguments)
		switch {
		case err != nil:
			result = fmt.Sprintf("工具调用失败：%v", err)
			status = "error"
		case callResult.IsError:
			result = "工具执行出错：" + callResult.Content
			status = "error"
		default:
			result = callResult.Content
		}
	}

	trySend(ctx, events, StreamEvent{Type: EventToolCall, ToolCall: &ToolCallInfo{Name: tc.Name, Status: status, Result: result}})

	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	toolMsg := Message{
		ID:             platform.NewID(),
		ConversationID: conversationID,
		Role:           string(provider.RoleTool),
		Content:        result,
		ToolCallID:     tc.ID,
	}
	if err := s.repo.createMessage(persistCtx, toolMsg); err != nil {
		slog.Error("conversation: persist tool message failed", "err", err, "conversation_id", conversationID)
	}

	return result
}

// persistAssistantTurn saves whatever content/tool_calls a ChatStream
// iteration produced, using a fresh background context (see runStream's
// doc comment) — a no-op if there's nothing to save (content empty and no
// tool calls), which happens when a stream errors before any content
// arrives.
func (s *service) persistAssistantTurn(conversationID, content string, toolCalls []provider.ToolCall) {
	if content == "" && len(toolCalls) == 0 {
		return
	}

	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	msg := Message{ID: platform.NewID(), ConversationID: conversationID, Role: string(provider.RoleAssistant), Content: content}
	if len(toolCalls) > 0 {
		toolCallsJSON, err := marshalToolCallsForStorage(toolCalls)
		if err != nil {
			slog.Error("conversation: marshal tool_calls for storage failed", "err", err, "conversation_id", conversationID)
		} else {
			msg.ToolCalls = toolCallsJSON
		}
	}
	if err := s.repo.createMessage(persistCtx, msg); err != nil {
		slog.Error("conversation: persist assistant message failed", "err", err, "conversation_id", conversationID)
	}
	if err := s.repo.touchConversation(persistCtx, conversationID, time.Now()); err != nil {
		slog.Warn("conversation: touch after assistant message failed", "err", err, "conversation_id", conversationID)
	}
}

// storedToolCall is the messages.tool_calls JSON shape — a small,
// independent representation rather than reusing provider.ToolCall
// directly, so this storage format doesn't shift underneath us if that
// type's fields (e.g. the streaming-only Index) ever change.
type storedToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func marshalToolCallsForStorage(calls []provider.ToolCall) ([]byte, error) {
	stored := make([]storedToolCall, 0, len(calls))
	for _, c := range calls {
		stored = append(stored, storedToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	return json.Marshal(stored)
}

// mergeToolCallDeltas accumulates streamed tool-call fragments by Index —
// the OpenAI streaming protocol splits one tool call's arguments across
// many chunks (and can interleave multiple tool calls), with only the
// first chunk of a given call carrying its ID/Name. See provider.ToolCall's
// doc comment for why Index (not ID) is the merge key.
func mergeToolCallDeltas(existing []provider.ToolCall, deltas []provider.ToolCall) []provider.ToolCall {
	for _, d := range deltas {
		idx := 0
		if d.Index != nil {
			idx = *d.Index
		}
		for len(existing) <= idx {
			existing = append(existing, provider.ToolCall{})
		}
		if d.ID != "" {
			existing[idx].ID = d.ID
		}
		if d.Name != "" {
			existing[idx].Name = d.Name
		}
		existing[idx].Arguments = append(existing[idx].Arguments, d.Arguments...)
	}
	return existing
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
