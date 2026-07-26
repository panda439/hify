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
}

type fakeConvService struct {
	fixtures map[string]caseFixture
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
	ch := make(chan conversation.StreamEvent, len(fx.events))
	for _, e := range fx.events {
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
