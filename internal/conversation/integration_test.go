package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"hify/internal/agent"
	"hify/internal/knowledge"
	"hify/internal/mcp"
	"hify/internal/provider"
	"hify/internal/testutil"
)

// 链路 1（对话流式问答主链路）的集成测试：真实 MySQL 存会话/消息，
// agent/provider/knowledge/mcp 走各自 Service 接口的 fake——这正是模块
// 边界，测的是 conversation 自己的编排逻辑：上下文组装、流式转发、
// tool_calls 分片合并后的工具循环、以及每一步的落库。

// --- fakes ---

type fakeAgentSvc struct {
	agent.Service
	ag agent.Agent
}

func (f *fakeAgentSvc) GetAgent(ctx context.Context, id string) (agent.Agent, error) {
	if id != f.ag.ID {
		return agent.Agent{}, errors.New("agent not found")
	}
	return f.ag, nil
}

type fakeProviderSvc struct {
	provider.Service
	client provider.Client
}

func (f *fakeProviderSvc) GetModel(ctx context.Context, id string) (provider.Model, error) {
	return provider.Model{ID: id, ProviderID: "p1", ModelName: "chat-model", Capability: provider.CapabilityChat}, nil
}

func (f *fakeProviderSvc) ResolveClient(ctx context.Context, providerID string) (provider.Client, error) {
	return f.client, nil
}

// scriptedChatClient 每次 ChatStream 按脚本吐一组 chunk，并记录收到的请求，
// 供断言"第二轮请求里带上了 assistant tool_calls 和 tool 结果消息"。
type scriptedChatClient struct {
	provider.Client
	scripts  [][]provider.ChatChunk
	requests []provider.ChatRequest
}

func (f *scriptedChatClient) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatChunk, error) {
	f.requests = append(f.requests, req)
	call := len(f.requests) - 1
	if call >= len(f.scripts) {
		return nil, errors.New("scripted client: unexpected extra ChatStream call")
	}
	ch := make(chan provider.ChatChunk, len(f.scripts[call]))
	for _, c := range f.scripts[call] {
		ch <- c
	}
	close(ch)
	return ch, nil
}

type fakeKnowledgeSvc struct {
	knowledge.Service
	chunks []knowledge.RetrievedChunk
}

func (f *fakeKnowledgeSvc) Retrieve(ctx context.Context, kbIDs []string, query string, topK int) ([]knowledge.RetrievedChunk, error) {
	return f.chunks, nil
}

type fakeMCPSvc struct {
	mcp.Service
	tool    mcp.Tool
	calls   []json.RawMessage
	result  mcp.ToolCallResult
	callErr error
}

func (f *fakeMCPSvc) GetTool(ctx context.Context, id string) (mcp.Tool, error) {
	if id != f.tool.ID {
		return mcp.Tool{}, errors.New("tool not found")
	}
	return f.tool, nil
}

func (f *fakeMCPSvc) CallTool(ctx context.Context, toolID string, args json.RawMessage) (mcp.ToolCallResult, error) {
	f.calls = append(f.calls, args)
	return f.result, f.callErr
}

// --- helpers ---

func intp(i int) *int { return &i }

func seedConversation(t *testing.T, repo *Repository, convID, agentID, userID string) {
	t.Helper()
	err := repo.createConversation(context.Background(), Conversation{
		ID: convID, AgentID: agentID, UserID: userID, Title: "测试会话",
	})
	if err != nil {
		t.Fatalf("seed conversation: %v", err)
	}
}

func drainEvents(t *testing.T, events <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var out []StreamEvent
	for e := range events {
		out = append(out, e)
	}
	return out
}

func eventTypes(events []StreamEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// --- tests ---

func TestIntegrationStreamMessageToolCallLoop(t *testing.T) {
	repo := NewRepository(testutil.MySQL(t, "conversation"))
	ctx := context.Background()

	chat := &scriptedChatClient{scripts: [][]provider.ChatChunk{
		// 第一轮：文字 + 跨 chunk 分片的 tool call，finish=tool_calls。
		{
			{DeltaContent: "让我查一下。"},
			{DeltaToolCalls: []provider.ToolCall{{Index: intp(0), ID: "call_1", Name: "get_answer", Arguments: json.RawMessage(`{"q":`)}}},
			{DeltaToolCalls: []provider.ToolCall{{Index: intp(0), Arguments: json.RawMessage(`"x"}`)}}},
			{FinishReason: "tool_calls"},
		},
		// 第二轮（携带工具结果后）：正常回答，finish=stop。
		{
			{DeltaContent: "答案是"},
			{DeltaContent: "42。"},
			{FinishReason: "stop"},
		},
	}}
	mcpSvc := &fakeMCPSvc{
		tool:   mcp.Tool{ID: "tool-1", ToolName: "get_answer", Description: "查答案", InputSchema: json.RawMessage(`{}`), IsActive: true},
		result: mcp.ToolCallResult{Content: "42"},
	}
	svc := NewService(repo,
		&fakeAgentSvc{ag: agent.Agent{ID: "ag-1", ModelID: "m1", SystemPrompt: "你是助手",
			KnowledgeBaseIDs: []string{"kb-1"}, MCPToolIDs: []string{"tool-1"},
			Temperature: 0.7, MaxTokens: intp(1000)}},
		&fakeProviderSvc{client: chat},
		&fakeKnowledgeSvc{chunks: []knowledge.RetrievedChunk{{
			Chunk: knowledge.Chunk{KnowledgeBaseID: "kb-1", DocumentID: "doc-1", Content: "参考资料内容"},
			Score: 0.9,
		}}},
		mcpSvc,
	)

	seedConversation(t, repo, "conv-1", "ag-1", "u1")
	events, err := svc.StreamMessage(ctx, "u1", "conv-1", "问题是什么")
	if err != nil {
		t.Fatalf("StreamMessage: %v", err)
	}
	got := drainEvents(t, events)

	// 事件序列：retrieval → delta → tool_call(running) → tool_call(done) →
	// delta ×2 → done。
	want := []string{EventRetrieval, EventDelta, EventToolCall, EventToolCall, EventDelta, EventDelta, EventDone}
	if strings.Join(eventTypes(got), ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence = %v, want %v", eventTypes(got), want)
	}
	if got[2].ToolCall.Status != "running" || got[3].ToolCall.Status != "done" || got[3].ToolCall.Result != "42" {
		t.Fatalf("tool call trace wrong: %+v, %+v", got[2].ToolCall, got[3].ToolCall)
	}

	// 工具收到的是分片合并后的完整 JSON。
	if len(mcpSvc.calls) != 1 || string(mcpSvc.calls[0]) != `{"q":"x"}` {
		t.Fatalf("CallTool args = %v, want spliced {\"q\":\"x\"}", mcpSvc.calls)
	}

	// 第一轮请求：system prompt + RAG 参考资料 + 用户消息 + 工具定义。
	first := chat.requests[0]
	if len(first.Tools) != 1 || first.Tools[0].Name != "get_answer" {
		t.Fatalf("first request tools = %+v", first.Tools)
	}
	if first.Messages[0].Role != provider.RoleSystem || first.Messages[0].Content != "你是助手" {
		t.Fatalf("first message should be system prompt: %+v", first.Messages[0])
	}
	if !strings.Contains(first.Messages[1].Content, "参考资料内容") {
		t.Fatalf("second message should carry retrieved context: %+v", first.Messages[1])
	}
	// 第二轮请求：追加了 assistant(tool_calls) 和 role=tool 的结果消息。
	second := chat.requests[1]
	n := len(second.Messages)
	if second.Messages[n-2].Role != provider.RoleAssistant || len(second.Messages[n-2].ToolCalls) != 1 {
		t.Fatalf("2nd round missing assistant tool_calls message: %+v", second.Messages[n-2])
	}
	if second.Messages[n-1].Role != provider.RoleTool || second.Messages[n-1].Content != "42" || second.Messages[n-1].ToolCallID != "call_1" {
		t.Fatalf("2nd round missing tool result message: %+v", second.Messages[n-1])
	}

	// 落库检查：user → assistant(带 tool_calls JSON) → tool → assistant。
	rows, err := repo.listRecentMessages(ctx, "conv-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	reverseMessages(rows) // 转成时间正序
	roles := make([]string, len(rows))
	for i, r := range rows {
		roles[i] = r.Role
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant" {
		t.Fatalf("persisted roles = %v", roles)
	}
	if !strings.Contains(string(rows[1].ToolCalls), `"get_answer"`) {
		t.Fatalf("assistant turn lost tool_calls JSON: %s", rows[1].ToolCalls)
	}
	if rows[2].Content != "42" || rows[2].ToolCallID != "call_1" {
		t.Fatalf("tool message row wrong: %+v", rows[2])
	}
	if rows[3].Content != "答案是42。" {
		t.Fatalf("final assistant content = %q", rows[3].Content)
	}
}

func TestIntegrationStreamMessageOwnership(t *testing.T) {
	repo := NewRepository(testutil.MySQL(t, "conversation"))
	svc := NewService(repo,
		&fakeAgentSvc{ag: agent.Agent{ID: "ag-own", ModelID: "m1"}},
		&fakeProviderSvc{client: &scriptedChatClient{}},
		&fakeKnowledgeSvc{}, &fakeMCPSvc{})

	seedConversation(t, repo, "conv-own", "ag-own", "owner-user")
	// 别人的会话：必须拒绝（防越权是链路 8 在 service 层的延伸）。
	if _, err := svc.StreamMessage(context.Background(), "other-user", "conv-own", "hi"); err == nil {
		t.Fatal("StreamMessage on another user's conversation must fail")
	}
}

func TestIntegrationStreamMessageMidStreamErrorPersistsPartial(t *testing.T) {
	repo := NewRepository(testutil.MySQL(t, "conversation"))
	ctx := context.Background()

	chat := &scriptedChatClient{scripts: [][]provider.ChatChunk{{
		{DeltaContent: "已经生成的部分"},
		{Err: errors.New("upstream died")},
	}}}
	svc := NewService(repo,
		&fakeAgentSvc{ag: agent.Agent{ID: "ag-err", ModelID: "m1"}},
		&fakeProviderSvc{client: chat},
		&fakeKnowledgeSvc{}, &fakeMCPSvc{})

	seedConversation(t, repo, "conv-err", "ag-err", "u1")
	events, err := svc.StreamMessage(ctx, "u1", "conv-err", "hi")
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)

	last := got[len(got)-1]
	if last.Type != EventError {
		t.Fatalf("stream must end with error event, got %v", eventTypes(got))
	}
	// 断流前已生成的内容必须落库（链路 1 的终点标准之一）。
	rows, err := repo.listRecentMessages(ctx, "conv-err", 10)
	if err != nil {
		t.Fatal(err)
	}
	reverseMessages(rows)
	if len(rows) != 2 || rows[1].Role != "assistant" || rows[1].Content != "已经生成的部分" {
		t.Fatalf("partial assistant content not persisted: %+v", rows)
	}
}

func TestIntegrationStreamMessageUnknownToolFedBackAsError(t *testing.T) {
	// 模型幻觉出不存在的工具名：不 panic、给模型一条"不可用"的 tool 消息、
	// 对话还能走到 done——这是工具循环最容易改坏的容错分支。
	repo := NewRepository(testutil.MySQL(t, "conversation"))
	ctx := context.Background()

	chat := &scriptedChatClient{scripts: [][]provider.ChatChunk{
		{
			{DeltaToolCalls: []provider.ToolCall{{Index: intp(0), ID: "call_x", Name: "no_such_tool", Arguments: json.RawMessage(`{}`)}}},
			{FinishReason: "tool_calls"},
		},
		{
			{DeltaContent: "好的，不用工具直接回答。"},
			{FinishReason: "stop"},
		},
	}}
	mcpSvc := &fakeMCPSvc{tool: mcp.Tool{ID: "tool-1", ToolName: "real_tool", IsActive: true}}
	svc := NewService(repo,
		&fakeAgentSvc{ag: agent.Agent{ID: "ag-hal", ModelID: "m1", MCPToolIDs: []string{"tool-1"}}},
		&fakeProviderSvc{client: chat},
		&fakeKnowledgeSvc{}, mcpSvc)

	seedConversation(t, repo, "conv-hal", "ag-hal", "u1")
	events, err := svc.StreamMessage(ctx, "u1", "conv-hal", "hi")
	if err != nil {
		t.Fatal(err)
	}
	got := drainEvents(t, events)
	if got[len(got)-1].Type != EventDone {
		t.Fatalf("hallucinated tool must not kill the turn: %v", eventTypes(got))
	}
	var errorTool *ToolCallInfo
	for _, e := range got {
		if e.Type == EventToolCall && e.ToolCall.Status == "error" {
			errorTool = e.ToolCall
		}
	}
	if errorTool == nil || errorTool.Name != "no_such_tool" {
		t.Fatalf("expected error-status tool_call event for unknown tool, events: %v", eventTypes(got))
	}
	if len(mcpSvc.calls) != 0 {
		t.Fatalf("unknown tool must not reach mcp.CallTool, calls: %v", mcpSvc.calls)
	}
}
