package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/platform"
	"hify/internal/platform/trace"
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
	// ListMessages' citations map is keyed by message ID and batch-loaded
	// in one query (see repository.go's listCitationsByMessageIDs) — never
	// per-message, per CLAUDE.md's N+1 rule (Citation V1 spec section 11).
	// A message ID absent from the map has no citations (never a nil vs.
	// empty-slice distinction the caller needs to worry about).
	ListMessages(ctx context.Context, userID, conversationID string, cursor *MessageCursor, limit int) ([]Message, map[string][]Citation, string, error)

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
	traceStore   *trace.Store

	// rewriteEnabled/rewriteModelID/rewriteTimeout are
	// 001-rag-query-rerank's query-rewrite configuration, threaded
	// through from cmd/hify's buildApp (see
	// config.Config.RAGQueryRewriteEnabled et al.). rewriteModelID empty
	// means "use the current Agent's own chat model" — see
	// queryrewrite.go's rewriteQuery.
	rewriteEnabled bool
	rewriteModelID string
	rewriteTimeout time.Duration
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

func (s *service) ListMessages(ctx context.Context, userID, conversationID string, cursor *MessageCursor, limit int) ([]Message, map[string][]Citation, string, error) {
	if _, err := s.repo.getConversationForUser(ctx, conversationID, userID); err != nil {
		return nil, nil, "", err
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
		return nil, nil, "", err
	}

	var nextCursor string
	if len(rows) == limit {
		oldest := rows[len(rows)-1]
		nextCursor = EncodeCursor(MessageCursor{CreatedAt: oldest.CreatedAt, ID: oldest.ID})
	}

	reverseMessages(rows) // DB gives newest-first; the page itself reads chronologically

	ids := make([]string, 0, len(rows))
	for _, m := range rows {
		if m.Role == string(provider.RoleAssistant) {
			ids = append(ids, m.ID)
		}
	}
	citations, err := s.repo.listCitationsByMessageIDs(ctx, ids)
	if err != nil {
		return nil, nil, "", err
	}
	return rows, citations, nextCursor, nil
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

	// One trace per StreamMessage call — the root span (kind=turn) reuses
	// traceID as its own ID, so child spans (retrieval/llm_call/tool_call)
	// just need this one value as their parent_span_id. See
	// internal/platform/trace's package doc. turnStart is captured here,
	// before assembleContext's retrieval span, not inside runStream's
	// goroutine — a parent span's StartedAt must precede every child's, and
	// retrieval already happens synchronously before runStream is spawned.
	traceID := platform.NewID()
	turnStart := time.Now()

	assembled, err := s.assembleContext(ctx, conversationID, ag, model, content, traceID)
	if err != nil {
		return nil, err
	}

	req := provider.ChatRequest{
		Model:    model.ModelName,
		Messages: assembled.Messages,
		// ag.Temperature 是 resolveTemperature 已经落好默认值的具体
		// float64（agent/service.go），不是"可能未设置"——取地址传给
		// *float64 就是把 Agent 的显式配置如实带到线上，T023a 修好之后
		// Agent 显式配 0 才第一次真的生效。
		Temperature: &ag.Temperature,
		TopP:        derefFloat(ag.TopP),
		MaxTokens:   derefInt(ag.MaxTokens),
		Tools:       assembled.Tools,
	}

	events := make(chan StreamEvent)
	go s.runStream(ctx, client, req, conversationID, traceID, turnStart, assembled.Evidence, assembled.ToolNameToID, events)
	return events, nil
}

// recordSpan writes span via traceStore using a fresh background context
// (the request ctx may already be canceled by the time a span is ready to
// record — e.g. after a client disconnect) and only logs on failure: a
// broken trace must never take down the conversation it's describing.
func (s *service) recordSpan(span trace.Span) {
	recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.traceStore.Record(recordCtx, span); err != nil {
		slog.Error("conversation: record trace span failed", "err", err, "kind", span.Kind, "trace_id", span.TraceID)
	}
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
//
// evidence is this turn's final, ref-numbered RAG evidence (see
// context.go's selectEvidence) — the only place [Sx] citations in the
// model's answer can legitimately point to. Citation parsing/validation
// only happens once, against the fully accumulated answer on the normal
// completion path (see persistFinalAssistantTurn) — never per SSE delta,
// since a [S1] marker can span multiple deltas.
func (s *service) runStream(ctx context.Context, client provider.Client, req provider.ChatRequest, conversationID, traceID string, turnStart time.Time, evidence []Evidence, toolNameToID map[string]string, events chan<- StreamEvent) {
	defer close(events)
	allowedRefs := make(map[string]struct{}, len(evidence))
	for _, e := range evidence {
		allowedRefs[e.Ref] = struct{}{}
	}

	// turnErr backs the root (kind=turn) span recorded by the deferred
	// block below — every abnormal return path in this function sets
	// turnErr right before returning; the normal EventDone path leaves it
	// nil. validCitationCount/invalidCitationCount are populated only on
	// the normal completion path (persistFinalAssistantTurn), left at 0
	// otherwise — a turn that errors or gets tool-called never reaches
	// citation parsing. turnStart is a parameter (captured by the caller
	// before assembleContext's retrieval span, not here) so the root
	// span's StartedAt precedes every child span's, retrieval included.
	// This defer is registered before the recover defer (LIFO: recover
	// runs first, so it can still set turnErr on a panic) but after
	// close(events) (so the span write happens before the channel closes,
	// same reasoning as the recover defer's own comment below).
	var turnErr error
	var validCitationCount, invalidCitationCount int
	// 005-tool-loop-guard：本轮因重复调用被拦截的次数，进 trace（只是计数，不含参数）。
	var loopBlockedCount int
	defer func() {
		status := trace.StatusOK
		errMsg := ""
		if turnErr != nil {
			status = trace.StatusError
			errMsg = turnErr.Error()
		}
		s.recordSpan(trace.Span{
			ID: traceID, TraceID: traceID, ParentSpanID: "",
			ConversationID: conversationID, Kind: trace.KindTurn, Name: "conversation.turn",
			Status: status, ErrorMessage: errMsg,
			Attrs: trace.Attrs(map[string]any{
				trace.AttrValidCitationCount:   validCitationCount,
				trace.AttrInvalidCitationCount: invalidCitationCount,
				trace.AttrToolLoopBlockedCount: loopBlockedCount,
			}),
			StartedAt: turnStart, FinishedAt: time.Now(),
		})
	}()
	// Must run after (i.e. be deferred before, since defers are LIFO) the
	// close(events) above so a recovered panic can still get an error event
	// out on the still-open channel — see CLAUDE.md's goroutine recover
	// rule; every chat message goes through this goroutine, so an
	// unrecovered panic here would take the whole process down for every
	// concurrent user, not just this one request.
	defer func() {
		if r := recover(); r != nil {
			slog.Error("conversation: panic in runStream", "panic", r, "conversation_id", conversationID, "stack", string(debug.Stack()))
			turnErr = fmt.Errorf("conversation: panic in runStream: %v", r)
			trySend(ctx, events, StreamEvent{Type: EventError, TraceID: traceID, Error: "服务器内部错误，请稍后重试"})
		}
	}()

	if len(evidence) > 0 {
		if !trySend(ctx, events, StreamEvent{Type: EventRetrieval, TraceID: traceID, Retrieved: toRetrievedChunkInfo(evidence)}) {
			return
		}
	}

	messages := req.Messages

	// 005-tool-loop-guard：第二层止损的状态机，只在这一轮内有效（见 toolloop.go）。
	loopGuard := newToolLoopDetector()

	for iteration := 0; iteration < maxToolCallIterations; iteration++ {
		req.Messages = messages
		// 被拦截的工具在本轮后续迭代里必须真的消失，而不只是"注入一条消息劝它别调"
		// ——已知失败模式是模型道歉之后再调一次同样的。
		req.Tools = availableTools(req.Tools, loopGuard)

		llmSpanStart := time.Now()
		chunks, err := client.ChatStream(ctx, req)
		if err != nil {
			turnErr = err
			s.recordLLMCallSpan(traceID, conversationID, req.Model, llmSpanStart, messages, "", "", trace.StatusError, err.Error(), provider.Usage{})
			trySend(ctx, events, StreamEvent{Type: EventError, TraceID: traceID, Error: provider.WrapClientError(err).Error()})
			return
		}

		var buf strings.Builder
		var toolCalls []provider.ToolCall
		var finishReason string
		var usage provider.Usage
		var streamErr error
		for chunk := range chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
				break
			}
			if chunk.DeltaContent != "" {
				buf.WriteString(chunk.DeltaContent)
				if !trySend(ctx, events, StreamEvent{Type: EventDelta, TraceID: traceID, Content: chunk.DeltaContent}) {
					// Client disconnected (or the request context was
					// otherwise canceled) and nobody downstream is reading
					// events anymore — a plain send here would block this
					// goroutine forever (see resilience.go's matching fix).
					// Bail, but don't lose whatever was already generated.
					turnErr = errClientDisconnected
					s.recordLLMCallSpan(traceID, conversationID, req.Model, llmSpanStart, messages, buf.String(), finishReason, trace.StatusError, turnErr.Error(), usage)
					s.persistAssistantTurn(conversationID, buf.String(), nil)
					return
				}
			}
			if len(chunk.DeltaToolCalls) > 0 {
				toolCalls = mergeToolCallDeltas(toolCalls, chunk.DeltaToolCalls)
			}
			if chunk.FinishReason != "" {
				finishReason = chunk.FinishReason
				usage = chunk.Usage
			}
		}

		if streamErr != nil {
			turnErr = streamErr
			s.recordLLMCallSpan(traceID, conversationID, req.Model, llmSpanStart, messages, buf.String(), finishReason, trace.StatusError, streamErr.Error(), usage)
			s.persistAssistantTurn(conversationID, buf.String(), nil)
			trySend(ctx, events, StreamEvent{Type: EventError, TraceID: traceID, Error: provider.WrapClientError(streamErr).Error()})
			return
		}

		if finishReason == "tool_calls" && len(toolCalls) > 0 && len(toolNameToID) > 0 {
			s.recordLLMCallSpan(traceID, conversationID, req.Model, llmSpanStart, messages, buf.String(), finishReason, trace.StatusOK, "", usage)
			s.persistAssistantTurn(conversationID, buf.String(), toolCalls)
			messages = append(messages, provider.Message{Role: provider.RoleAssistant, Content: buf.String(), ToolCalls: toolCalls})

			for _, tc := range toolCalls {
				// 005-tool-loop-guard：先看这次调用是不是同一调用的第
				// maxIdenticalToolCalls 次连续出现。是的话不执行它，回一条工具
				// 结果（协议要求每个 tool_call 都有配对的 tool 消息，缺了下一次
				// 请求就是畸形的），并注入一条给模型指出路的系统消息。
				if loopGuard.isBlocked(tc.Name) {
					blocked := blockedToolResultMessage(tc.Name)
					s.persistToolResult(conversationID, tc, blocked)
					messages = append(messages, provider.Message{Role: provider.RoleTool, Content: blocked, ToolCallID: tc.ID})
					continue
				}
				if loopGuard.observe(tc.Name, string(tc.Arguments)) {
					slog.Info("conversation: tool loop detected, blocking tool for this turn",
						"conversation_id", conversationID, "trace_id", traceID,
						"tool", tc.Name, "repeat_count", maxIdenticalToolCalls,
						"args_fingerprint", fingerprintPrefix(toolCallFingerprint(tc.Name, string(tc.Arguments))))
					loopBlockedCount++

					blocked := blockedToolResultMessage(tc.Name)
					s.persistToolResult(conversationID, tc, blocked)
					messages = append(messages, provider.Message{Role: provider.RoleTool, Content: blocked, ToolCallID: tc.ID})
					messages = append(messages, provider.Message{
						Role:    provider.RoleSystem,
						Content: loopInterventionMessage(tc.Name, maxIdenticalToolCalls),
					})
					continue
				}
				result := s.runToolCall(ctx, tc, toolNameToID, conversationID, traceID, events)
				messages = append(messages, provider.Message{Role: provider.RoleTool, Content: result, ToolCallID: tc.ID})
			}
			continue
		}

		s.recordLLMCallSpan(traceID, conversationID, req.Model, llmSpanStart, messages, buf.String(), finishReason, trace.StatusOK, "", usage)

		finalContent, citations, invalidCount, persistErr := s.persistFinalAssistantTurn(conversationID, buf.String(), evidence, allowedRefs)
		if persistErr != nil {
			// The transaction failed: neither the assistant message nor its
			// citations exist in MySQL, and persistFinalAssistantTurn never
			// called touchConversation on this path (see its doc comment) —
			// so nothing here may claim success. Sending final/done would
			// tell the client an answer was saved when it wasn't (content
			// would vanish on refresh, breaking both consistency equalities
			// from CLAUDE.md's Citation V1 spec). error is the only
			// terminal event on this path; the message is generic Chinese
			// text, never the raw DB error (see persistFinalAssistantTurn's
			// own log line for the real cause).
			turnErr = persistErr
			invalidCitationCount = invalidCount
			trySend(ctx, events, StreamEvent{Type: EventError, TraceID: traceID, Error: "保存回答失败，请稍后重试"})
			return
		}
		validCitationCount, invalidCitationCount = len(citations), invalidCount

		// final carries the authoritative, citation-normalized content and
		// structured citations — see model.go's EventFinal doc comment.
		// Sent even if the send below is dropped by a disconnected client:
		// the message is already durably saved by persistFinalAssistantTurn
		// regardless of who's still listening (CLAUDE.md spec section
		// 10.9's "final 发送失败不能回滚已保存内容" — there's simply nothing
		// to roll back here, the DB write already happened).
		if !trySend(ctx, events, StreamEvent{Type: EventFinal, TraceID: traceID, Content: finalContent, Citations: toCitationResponses(citations)}) {
			slog.Warn("conversation: final event not delivered (client disconnected), content already persisted", "conversation_id", conversationID)
			return
		}
		trySend(ctx, events, StreamEvent{Type: EventDone, TraceID: traceID})
		return
	}

	// 005-tool-loop-guard 第三层：触顶不再直接报错。
	//
	// 中间过程本来就是逐轮落库的（上面每次 tool_calls 分支都调了
	// persistAssistantTurn / persistToolResult），缺的是**一个收尾的答复**——
	// 在此之前用户看到的是一条错误，会话历史里一串工具调用然后戛然而止，
	// 不知道系统做了什么、为什么停了。
	//
	// 收尾文案由程序拼接、不再调模型：让模型基于不完整的中间结果作答会诱发
	// 填空式幻觉（它会把缺的那部分编出来），而"信息可能不完整"这句声明必须
	// 由程序保证，不能寄望于提示词。见 toolLoopExhaustedMessage 的注释。
	//
	// 不挂任何 Citation：这是一条程序文案，不是基于检索证据生成的回答，
	// 给它挂引用会让它看起来像有出处。
	turnErr = errTooManyToolIterations
	exhausted := toolLoopExhaustedMessage()
	s.persistAssistantTurn(conversationID, exhausted, nil)
	if !trySend(ctx, events, StreamEvent{Type: EventFinal, TraceID: traceID, Content: exhausted}) {
		slog.Warn("conversation: tool-loop exhaustion notice not delivered (client disconnected), content already persisted",
			"conversation_id", conversationID)
		return
	}
	trySend(ctx, events, StreamEvent{Type: EventDone, TraceID: traceID})
}

// availableTools 去掉本轮已被循环检测停用的工具。返回原切片（而不是拷贝）
// 当没有任何工具被停用时——绝大多数对话走这条路径，不该为此多分配一次。
func availableTools(tools []provider.ToolDefinition, guard *toolLoopDetector) []provider.ToolDefinition {
	if len(guard.blocked) == 0 {
		return tools
	}
	out := make([]provider.ToolDefinition, 0, len(tools))
	for _, t := range tools {
		if !guard.isBlocked(t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// errClientDisconnected/errTooManyToolIterations back runStream's turnErr —
// curated Chinese text isn't needed here (unlike workflow's equivalent)
// since these only ever populate trace_spans.error_message, an internal
// debugging field never shown to the end user (the StreamEvent sent to the
// client already carries its own Chinese message at each call site).
var (
	errClientDisconnected    = errors.New("client disconnected mid-stream")
	errTooManyToolIterations = errors.New("max tool-call iterations reached")
)

// recordLLMCallSpan records one ChatStream call (one loop iteration) as a
// kind=llm_call span. Input/Output deliberately stay empty — messages can
// carry the Agent's system prompt, the user's question, and (via the
// review fix in context.go) retrieved knowledge base content, none of
// which trace_spans may hold a full copy of by default (see CLAUDE.md's
// trace-privacy fix); AttrMessageCount/AttrInputLength/AttrOutputLength
// give a debugger enough signal (how big was this request/answer) without
// ever persisting what's actually in it. usage is best-effort (see
// provider.Usage's doc comment) and simply omitted from Attrs when zero.
func (s *service) recordLLMCallSpan(traceID, conversationID, model string, start time.Time, messages []provider.Message, output, finishReason, status, errMsg string, usage provider.Usage) {
	s.recordSpan(trace.Span{
		ID: platform.NewID(), TraceID: traceID, ParentSpanID: traceID,
		ConversationID: conversationID, Kind: trace.KindLLMCall, Name: "llm.chat_stream",
		Status: status, ErrorMessage: errMsg,
		Attrs: trace.Attrs(map[string]any{
			trace.AttrRequestModel:      model,
			trace.AttrFinishReasons:     finishReason,
			trace.AttrUsageInputTokens:  usage.PromptTokens,
			trace.AttrUsageOutputTokens: usage.CompletionTokens,
			trace.AttrMessageCount:      len(messages),
			trace.AttrInputLength:       messageContentRuneTotal(messages),
			trace.AttrOutputLength:      len([]rune(output)),
		}),
		StartedAt: start, FinishedAt: time.Now(),
	})
}

// messageContentRuneTotal sums every message's content length for
// AttrInputLength — a size signal for the llm_call span that doesn't
// require (and must not become) storing the messages themselves.
func messageContentRuneTotal(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		total += len([]rune(m.Content))
	}
	return total
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

// persistToolResult 落一条 role=tool 消息。005-tool-loop-guard 用它给**没有真正
// 执行**的调用（被循环检测拦截的）补一条配对结果——工具调用协议要求每个
// tool_call 都有配对的 tool 消息，缺了下一次请求就是畸形的；静默丢弃还会让模型
// 看不到任何反馈，更容易继续转圈。
func (s *service) persistToolResult(conversationID string, tc provider.ToolCall, content string) {
	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.repo.createMessage(persistCtx, Message{
		ID:             platform.NewID(),
		ConversationID: conversationID,
		Role:           string(provider.RoleTool),
		Content:        content,
		ToolCallID:     tc.ID,
	}); err != nil {
		slog.Error("conversation: persist blocked tool message failed", "err", err, "conversation_id", conversationID)
	}
}

// runToolCall dispatches one accumulated tool call to mcp.Service, emits
// the running/done|error StreamEvents the chat UI's tool-call trace needs,
// persists the resulting `role: tool` message, and returns the message
// content to feed back into the next ChatStream call — a Chinese
// explanation on failure, since every tool_call from an assistant message
// must get a matching tool response or the next API call is malformed.
func (s *service) runToolCall(ctx context.Context, tc provider.ToolCall, toolNameToID map[string]string, conversationID, traceID string, events chan<- StreamEvent) string {
	// Best-effort: whether or not anyone is still listening, the tool must
	// still run and its result must still be persisted below (every
	// tool_call needs a matching tool response for the next API call to be
	// well-formed) — so a failed send here doesn't abort the function.
	trySend(ctx, events, StreamEvent{Type: EventToolCall, TraceID: traceID, ToolCall: &ToolCallInfo{Name: tc.Name, Status: "running"}})

	spanStart := time.Now()
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

	// spanErrMsg/Input/Output deliberately never carry tc.Arguments or
	// result verbatim — a tool's return value is exactly the "工具返回的
	// 敏感原文" CLAUDE.md's trace-privacy fix forbids storing by default;
	// a generic marker plus AttrInputLength/AttrOutputLength below is
	// enough to see that (and how much) a tool call failed without ever
	// persisting what it actually returned.
	spanStatus := trace.StatusOK
	spanErrMsg := ""
	if status == "error" {
		spanStatus = trace.StatusError
		spanErrMsg = "tool call reported an error"
	}
	s.recordSpan(trace.Span{
		ID: platform.NewID(), TraceID: traceID, ParentSpanID: traceID,
		ConversationID: conversationID, Kind: trace.KindToolCall, Name: tc.Name,
		Status: spanStatus, ErrorMessage: spanErrMsg,
		Attrs: trace.Attrs(map[string]any{
			trace.AttrToolName:     tc.Name,
			trace.AttrInputLength:  len([]rune(string(tc.Arguments))),
			trace.AttrOutputLength: len([]rune(result)),
		}),
		StartedAt:  spanStart,
		FinishedAt: time.Now(),
	})

	trySend(ctx, events, StreamEvent{Type: EventToolCall, TraceID: traceID, ToolCall: &ToolCallInfo{Name: tc.Name, Status: status, Result: result}})

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

// persistFinalAssistantTurn is the terminal-answer counterpart to
// persistAssistantTurn — it owns the one place raw model output becomes
// the authoritative saved/returned content: normalizeCitations strips any
// [Sx] the model wasn't actually offered, evidenceToCitations turns the
// refs that survive into message_citations rows (quotes taken verbatim
// from evidence, never re-derived from the model's own text), and
// createMessageWithCitations saves message+citations as one MySQL
// transaction — a citation write failure rolls the message back too (see
// CLAUDE.md's Citation V1 spec section 4), rather than ever leaving a
// saved message with silently missing citations.
//
// On success, returns the normalized content (== what gets sent as
// EventFinal.Content and == what's now in messages.content) and the
// persisted citations (empty, never nil, when the model cited nothing).
// On a transaction failure, err is non-nil and content/citations are the
// zero value — the caller (runStream) MUST treat this as "nothing was
// saved": no touchConversation call happens on this path (below), and the
// caller must not send final/done, only an error event, or the client
// would be told an answer was saved when it wasn't (see the code-review
// fix this closes — final.content/citations must always equal what's
// actually in MySQL, never what merely rendered locally). invalidCount is
// still returned on failure since it comes from the (side-effect-free)
// normalizeCitations parse, not from the DB write, and remains useful for
// the caller's trace attrs regardless of persistence outcome.
func (s *service) persistFinalAssistantTurn(conversationID, rawContent string, evidence []Evidence, allowedRefs map[string]struct{}) (content string, citations []Citation, invalidCount int, err error) {
	content, validRefs, invalidCount := normalizeCitations(rawContent, allowedRefs)

	persistCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	msg := Message{ID: platform.NewID(), ConversationID: conversationID, Role: string(provider.RoleAssistant), Content: content}
	citations = evidenceToCitations(msg.ID, evidence, validRefs)

	if err := s.repo.createMessageWithCitations(persistCtx, msg, citations); err != nil {
		slog.Error("conversation: persist final assistant message with citations failed", "err", err, "conversation_id", conversationID)
		return "", nil, invalidCount, err
	}
	if err := s.repo.touchConversation(persistCtx, conversationID, time.Now()); err != nil {
		slog.Warn("conversation: touch after assistant message failed", "err", err, "conversation_id", conversationID)
	}
	return content, citations, invalidCount, nil
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
