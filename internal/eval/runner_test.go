package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"hify/internal/conversation"
	"hify/internal/platform/trace"
	"hify/internal/provider"
)

// caseFixture is what one fake agent_id resolves to — CreateConversation
// only sees the agentID, so tests key fixtures by it and StreamMessage
// recovers the fixture from the conversation ID it handed back.
type caseFixture struct {
	createErr error
	streamErr error
	events    []conversation.StreamEvent
	// turnsEvents, when set, overrides events with one event sequence per
	// StreamMessage call on the same conversation (indexed by call order)
	// — this is what lets a test fixture answer differently per turn for
	// multi-turn (TestCase.Turns) cases. nil means "use events for every
	// call", matching the pre-multi-turn behavior.
	turnsEvents [][]conversation.StreamEvent
}

type fakeConvService struct {
	fixtures map[string]caseFixture
	// calls counts StreamMessage invocations per conversationID, used only
	// to index into turnsEvents. Lazily initialized — fine for
	// single-goroutine test use, not meant to be a general concurrency
	// pattern.
	calls map[string]int
}

func (f *fakeConvService) CreateConversation(_ context.Context, _, agentID string) (conversation.Conversation, error) {
	fx := f.fixtures[agentID]
	if fx.createErr != nil {
		return conversation.Conversation{}, fx.createErr
	}
	return conversation.Conversation{ID: "conv-" + agentID, AgentID: agentID}, nil
}

func (f *fakeConvService) ListConversations(context.Context, string, int, int) ([]conversation.Conversation, int, error) {
	return nil, 0, nil
}

func (f *fakeConvService) ListMessages(context.Context, string, string, *conversation.MessageCursor, int) ([]conversation.Message, map[string][]conversation.Citation, string, error) {
	return nil, nil, "", nil
}

func (f *fakeConvService) StreamMessage(_ context.Context, _, conversationID, _ string) (<-chan conversation.StreamEvent, error) {
	agentID := strings.TrimPrefix(conversationID, "conv-")
	fx := f.fixtures[agentID]
	if fx.streamErr != nil {
		return nil, fx.streamErr
	}

	events := fx.events
	if fx.turnsEvents != nil {
		if f.calls == nil {
			f.calls = make(map[string]int)
		}
		idx := f.calls[conversationID]
		f.calls[conversationID] = idx + 1
		if idx < len(fx.turnsEvents) {
			events = fx.turnsEvents[idx]
		} else {
			events = nil
		}
	}

	ch := make(chan conversation.StreamEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

var _ conversation.Service = (*fakeConvService)(nil)

type fakeTraceLister struct {
	spans []trace.Span
	err   error
}

func (f *fakeTraceLister) ListByConversation(context.Context, string) ([]trace.Span, error) {
	return f.spans, f.err
}

type fakeJudgeClient struct {
	chatFn func(ctx context.Context, req provider.ChatRequest) (provider.Message, error)
	called bool
}

func (f *fakeJudgeClient) Chat(ctx context.Context, req provider.ChatRequest) (provider.Message, error) {
	f.called = true
	return f.chatFn(ctx, req)
}
func (f *fakeJudgeClient) ChatStream(context.Context, provider.ChatRequest) (<-chan provider.ChatChunk, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeJudgeClient) Embed(context.Context, provider.EmbedRequest) (provider.EmbedResult, error) {
	return provider.EmbedResult{}, errors.New("not implemented")
}
func (f *fakeJudgeClient) TestConnection(context.Context) error { return nil }

var _ provider.Client = (*fakeJudgeClient)(nil)

func okJudge(score int) *fakeJudgeClient {
	return &fakeJudgeClient{chatFn: func(context.Context, provider.ChatRequest) (provider.Message, error) {
		return provider.Message{Content: fmt.Sprintf(`{"score":%d,"reasoning":"ok"}`, score)}, nil
	}}
}

func failingJudge(errMsg string) *fakeJudgeClient {
	return &fakeJudgeClient{chatFn: func(context.Context, provider.ChatRequest) (provider.Message, error) {
		return provider.Message{}, errors.New(errMsg)
	}}
}

func TestRunCase_UsesFinalContentNotDeltaConcatenation(t *testing.T) {
	fx := caseFixture{events: []conversation.StreamEvent{
		{Type: conversation.EventDelta, Content: "Hel"},
		{Type: conversation.EventDelta, Content: "lo [S99]"},
		{Type: conversation.EventFinal, Content: "Hello", Citations: []conversation.CitationResponse{}},
		{Type: conversation.EventDone},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	tc := TestCase{Name: "final-vs-delta", AgentID: "agent-1", Rubric: "r"}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, okJudge(4), "judge-model", "user-1", tc)

	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Reply != "Hello" {
		t.Fatalf("Reply = %q, want %q (must be EventFinal.Content, not delta concatenation)", result.Reply, "Hello")
	}
}

func TestRunCase_MissingFinalFails(t *testing.T) {
	fx := caseFixture{events: []conversation.StreamEvent{
		{Type: conversation.EventDelta, Content: "partial"},
		// stream closes without EventFinal or EventError
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	judge := okJudge(5)
	tc := TestCase{Name: "no-final", AgentID: "agent-1", Rubric: "r"}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, judge, "judge-model", "user-1", tc)

	if result.Err == "" {
		t.Fatalf("expected an error when stream ends without EventFinal, got none")
	}
	if judge.called {
		t.Fatalf("judge must not be called when the case never produced a final answer")
	}
	if result.Score != 0 {
		t.Fatalf("Score = %d, want 0 on a failed case", result.Score)
	}
}

func TestRunCase_StreamErrorFails(t *testing.T) {
	fx := caseFixture{events: []conversation.StreamEvent{
		{Type: conversation.EventError, Error: "上游模型调用失败"},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	judge := okJudge(5)
	tc := TestCase{Name: "stream-error", AgentID: "agent-1", Rubric: "r"}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, judge, "judge-model", "user-1", tc)

	if result.Err != "上游模型调用失败" {
		t.Fatalf("Err = %q, want the event's error message", result.Err)
	}
	if judge.called {
		t.Fatalf("judge must not be called on a stream error")
	}
}

func TestRun_OneCaseFailingDoesNotStopOthers(t *testing.T) {
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{
		"agent-fail": {streamErr: errors.New("boom")},
		"agent-ok": {events: []conversation.StreamEvent{
			{Type: conversation.EventFinal, Content: "ok reply", Citations: []conversation.CitationResponse{}},
			{Type: conversation.EventDone},
		}},
	}}
	cases := []TestCase{
		{Name: "fails", AgentID: "agent-fail", Rubric: "r"},
		{Name: "succeeds", AgentID: "agent-ok", Rubric: "r"},
	}

	report := Run(context.Background(), convSvc, &fakeTraceLister{}, okJudge(3), "judge-model", "user-1", cases)

	if len(report.Results) != 2 {
		t.Fatalf("len(Results) = %d, want 2", len(report.Results))
	}
	if report.Results[0].Err == "" {
		t.Fatalf("expected first case to have failed")
	}
	if report.Results[1].Err != "" || report.Results[1].Score != 3 {
		t.Fatalf("second case should have run to completion: %+v", report.Results[1])
	}
}

func TestRunCase_JudgeFailureRecordsError(t *testing.T) {
	fx := caseFixture{events: []conversation.StreamEvent{
		{Type: conversation.EventFinal, Content: "answer", Citations: []conversation.CitationResponse{}},
		{Type: conversation.EventDone},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	tc := TestCase{Name: "judge-fails", AgentID: "agent-1", Rubric: "r", ExpectedDocumentIDs: []string{"doc-1"}}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, failingJudge("judge unreachable"), "judge-model", "user-1", tc)

	if result.Err == "" {
		t.Fatalf("expected an error when the judge call fails")
	}
	if result.Score != 0 {
		t.Fatalf("Score = %d, want 0 when judge fails", result.Score)
	}
	// Deterministic metrics don't depend on the judge succeeding — they're
	// computed before Judge is even called.
	if !result.Metrics.RetrievalHit.Evaluated {
		t.Fatalf("Metrics should still be computed when only the judge call fails")
	}
}

func TestRunCase_RetrievalEmptyDoesNotError(t *testing.T) {
	fx := caseFixture{events: []conversation.StreamEvent{
		{Type: conversation.EventFinal, Content: "no retrieval happened", Citations: []conversation.CitationResponse{}},
		{Type: conversation.EventDone},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	tc := TestCase{Name: "no-retrieval", AgentID: "agent-1", Rubric: "r"}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, okJudge(4), "judge-model", "user-1", tc)

	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Retrievals == nil || len(result.Retrievals) != 0 {
		t.Fatalf("Retrievals = %#v, want non-nil empty slice", result.Retrievals)
	}
}

func TestRunCase_RetrievalCollectedWithoutContent(t *testing.T) {
	const secretContent = "SECRET_CHUNK_CONTENT_MUST_NOT_LEAK"
	fx := caseFixture{events: []conversation.StreamEvent{
		{Type: conversation.EventRetrieval, Retrieved: []conversation.RetrievedChunkInfo{
			{Ref: "S1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "手册.pdf", Content: secretContent, Score: 0.9},
		}},
		{Type: conversation.EventFinal, Content: "answer [S1]", Citations: []conversation.CitationResponse{
			{Ref: "S1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "手册.pdf", ChunkID: "chunk-1", Quote: secretContent, Score: 0.9},
		}},
		{Type: conversation.EventDone},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	tc := TestCase{Name: "with-retrieval", AgentID: "agent-1", Rubric: "r", ExpectedDocumentIDs: []string{"doc-1"}}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, okJudge(5), "judge-model", "user-1", tc)

	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if len(result.Retrievals) != 1 || result.Retrievals[0].Rank != 1 || result.Retrievals[0].DocumentID != "doc-1" {
		t.Fatalf("Retrievals = %+v, want one rank-1 hit for doc-1", result.Retrievals)
	}
	if len(result.Citations) != 1 || result.Citations[0].DocumentID != "doc-1" {
		t.Fatalf("Citations = %+v, want one entry for doc-1", result.Citations)
	}
	if !result.Metrics.RetrievalHit.Value || !result.Metrics.ExpectedDocumentCited.Value {
		t.Fatalf("Metrics = %+v, want RetrievalHit and ExpectedDocumentCited both true", result.Metrics)
	}
}

func TestRunCase_MultiTurnJudgesOnlyFinalTurn(t *testing.T) {
	fx := caseFixture{turnsEvents: [][]conversation.StreamEvent{
		{
			{Type: conversation.EventRetrieval, Retrieved: []conversation.RetrievedChunkInfo{
				{Ref: "S1", DocumentID: "doc-first-turn", Score: 0.9},
			}},
			{Type: conversation.EventFinal, Content: "第一轮回复", Citations: []conversation.CitationResponse{
				{Ref: "S1", DocumentID: "doc-first-turn", ChunkID: "c1", Score: 0.9},
			}},
			{Type: conversation.EventDone},
		},
		{
			{Type: conversation.EventRetrieval, Retrieved: []conversation.RetrievedChunkInfo{
				{Ref: "S1", DocumentID: "doc-final-turn", Score: 0.8},
			}},
			{Type: conversation.EventFinal, Content: "第二轮回复（指代第一轮）", Citations: []conversation.CitationResponse{
				{Ref: "S1", DocumentID: "doc-final-turn", ChunkID: "c2", Score: 0.8},
			}},
			{Type: conversation.EventDone},
		},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	tc := TestCase{
		Name:                "multi-turn",
		AgentID:             "agent-1",
		Rubric:              "r",
		Turns:               []string{"第一轮问题", "那第二个呢？"},
		ExpectedDocumentIDs: []string{"doc-final-turn"},
	}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, okJudge(5), "judge-model", "user-1", tc)

	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if result.Reply != "第二轮回复（指代第一轮）" {
		t.Fatalf("Reply = %q, want the final turn's reply", result.Reply)
	}
	if len(result.Retrievals) != 1 || result.Retrievals[0].DocumentID != "doc-final-turn" {
		t.Fatalf("Retrievals = %+v, want only the final turn's retrieval (doc-first-turn must not leak in)", result.Retrievals)
	}
	if len(result.Citations) != 1 || result.Citations[0].DocumentID != "doc-final-turn" {
		t.Fatalf("Citations = %+v, want only the final turn's citation", result.Citations)
	}
	if !result.Metrics.RetrievalHit.Value {
		t.Fatalf("expected RetrievalHit true from the final turn's retrieval matching ExpectedDocumentIDs")
	}
}

func TestRunCase_MultiTurnFailsIfAnyTurnErrors(t *testing.T) {
	fx := caseFixture{turnsEvents: [][]conversation.StreamEvent{
		{
			{Type: conversation.EventFinal, Content: "第一轮回复", Citations: []conversation.CitationResponse{}},
			{Type: conversation.EventDone},
		},
		{
			{Type: conversation.EventError, Error: "第二轮失败"},
		},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}
	judge := okJudge(5)
	tc := TestCase{Name: "multi-turn-fail", AgentID: "agent-1", Rubric: "r", Turns: []string{"第一轮", "第二轮"}}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, judge, "judge-model", "user-1", tc)

	if result.Err != "第二轮失败" {
		t.Fatalf("Err = %q, want the second turn's error message", result.Err)
	}
	if judge.called {
		t.Fatalf("judge must not be called when a turn errors")
	}
}

func TestCaseTurns_FallsBackToPromptWhenTurnsUnset(t *testing.T) {
	got := caseTurns(TestCase{Prompt: "单轮问题"})
	if len(got) != 1 || got[0] != "单轮问题" {
		t.Fatalf("caseTurns = %v, want [\"单轮问题\"]", got)
	}
}

// TestRunCase_MultiTurnJudgeRequestIncludesFirstAndLastTurn is the
// end-to-end regression test (on top of judge_test.go's direct
// buildJudgePrompt tests) that the actual request sent to the judge model
// via provider.Client.Chat — not just some intermediate string — carries
// both the first turn (coreference context) and the last turn (the one
// being scored). Before this fix, runCase/Judge built the prompt from
// tc.Prompt, which is empty for a Turns-based case, so the judge received
// no user question at all.
func TestRunCase_MultiTurnJudgeRequestIncludesFirstAndLastTurn(t *testing.T) {
	fx := caseFixture{turnsEvents: [][]conversation.StreamEvent{
		{
			{Type: conversation.EventFinal, Content: "第一轮回复", Citations: []conversation.CitationResponse{}},
			{Type: conversation.EventDone},
		},
		{
			{Type: conversation.EventFinal, Content: "第二轮回复", Citations: []conversation.CitationResponse{}},
			{Type: conversation.EventDone},
		},
	}}
	convSvc := &fakeConvService{fixtures: map[string]caseFixture{"agent-1": fx}}

	var capturedPrompt string
	judge := &fakeJudgeClient{chatFn: func(_ context.Context, req provider.ChatRequest) (provider.Message, error) {
		capturedPrompt = req.Messages[0].Content
		return provider.Message{Content: `{"score":4,"reasoning":"ok"}`}, nil
	}}

	tc := TestCase{
		Name:    "multi-turn-judge-request",
		AgentID: "agent-1",
		Rubric:  "r",
		Turns:   []string{"知识库创建以后还能修改用的向量模型吗？", "那分块大小呢，也不能改吗？"},
	}

	result := runCase(context.Background(), convSvc, &fakeTraceLister{}, judge, "judge-model", "user-1", tc)

	if result.Err != "" {
		t.Fatalf("unexpected error: %s", result.Err)
	}
	if !strings.Contains(capturedPrompt, "知识库创建以后还能修改用的向量模型吗？") {
		t.Fatalf("judge request must include the first turn, got:\n%s", capturedPrompt)
	}
	if !strings.Contains(capturedPrompt, "那分块大小呢，也不能改吗？") {
		t.Fatalf("judge request must include the last (scored) turn, got:\n%s", capturedPrompt)
	}
}

func TestCaseTurns_UsesTurnsWhenSet(t *testing.T) {
	got := caseTurns(TestCase{Prompt: "被忽略", Turns: []string{"第一轮", "第二轮"}})
	if len(got) != 2 || got[0] != "第一轮" || got[1] != "第二轮" {
		t.Fatalf("caseTurns = %v, want [\"第一轮\" \"第二轮\"] (Turns must take priority over Prompt)", got)
	}
}
