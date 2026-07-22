package eval

import (
	"context"
	"fmt"
	"strings"

	"hify/internal/conversation"
	"hify/internal/platform/trace"
	"hify/internal/provider"
)

// Run drives each test case through a fresh conversation, collects the
// resulting trace, and has judgeClient score it against the case's rubric.
// A case that errors out (conversation/judge failure) still produces a
// CaseResult — with Err set and Score left at 0 — rather than aborting the
// whole run, so one broken case doesn't hide the results of the others.
func Run(ctx context.Context, convSvc conversation.Service, traceStore *trace.Store, judgeClient provider.Client, judgeModel, userID string, cases []TestCase) RunReport {
	results := make([]CaseResult, 0, len(cases))
	for _, tc := range cases {
		results = append(results, runCase(ctx, convSvc, traceStore, judgeClient, judgeModel, userID, tc))
	}
	return RunReport{Results: results}
}

func runCase(ctx context.Context, convSvc conversation.Service, traceStore *trace.Store, judgeClient provider.Client, judgeModel, userID string, tc TestCase) CaseResult {
	result := CaseResult{Name: tc.Name}

	conv, err := convSvc.CreateConversation(ctx, userID, tc.AgentID)
	if err != nil {
		result.Err = fmt.Sprintf("create conversation: %v", err)
		return result
	}

	events, err := convSvc.StreamMessage(ctx, userID, conv.ID, tc.Prompt)
	if err != nil {
		result.Err = fmt.Sprintf("send message: %v", err)
		return result
	}

	var reply strings.Builder
	for evt := range events {
		if evt.TraceID != "" {
			result.TraceID = evt.TraceID
		}
		switch evt.Type {
		case conversation.EventDelta:
			reply.WriteString(evt.Content)
		case conversation.EventError:
			result.Err = evt.Error
		}
	}
	result.Reply = reply.String()
	if result.Err != "" {
		return result
	}

	spans, err := traceStore.ListByConversation(ctx, conv.ID)
	if err != nil {
		result.Err = fmt.Sprintf("list trace spans: %v", err)
		return result
	}
	for _, sp := range spans {
		if sp.TraceID == result.TraceID {
			result.Spans = append(result.Spans, sp)
		}
	}

	score, reasoning, err := Judge(ctx, judgeClient, judgeModel, tc, result.Reply, result.Spans)
	if err != nil {
		result.Err = fmt.Sprintf("judge: %v", err)
		return result
	}
	result.Score = score
	result.Reasoning = reasoning
	return result
}
