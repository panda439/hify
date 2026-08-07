package eval

import (
	"context"
	"fmt"

	"hify/internal/conversation"
	"hify/internal/platform/trace"
	"hify/internal/provider"
)

// traceLister is the subset of *trace.Store the runner needs. Declared as
// an interface (not the concrete *trace.Store parameter type the runner
// used before this change) purely so runCase is unit-testable with a fake
// in-memory implementation instead of a real MySQL connection — *trace.Store
// itself is untouched and still satisfies this interface, so cmd/evalrunner
// needs no changes.
type traceLister interface {
	ListByConversation(ctx context.Context, conversationID string) ([]trace.Span, error)
}

// Run drives each test case through a fresh conversation, collects the
// resulting trace, and has judgeClient score it against the case's rubric.
// A case that errors out (conversation/judge failure) still produces a
// CaseResult — with Err set and Score left at 0 — rather than aborting the
// whole run, so one broken case doesn't hide the results of the others.
func Run(ctx context.Context, convSvc conversation.Service, traceStore traceLister, judgeClient provider.Client, judgeModel, userID string, cases []TestCase) RunReport {
	results := make([]CaseResult, 0, len(cases))
	for _, tc := range cases {
		results = append(results, runCase(ctx, convSvc, traceStore, judgeClient, judgeModel, userID, tc))
	}
	return RunReport{Results: results}
}

// streamCollection is the deterministic, DB- and judge-free product of
// draining one case's StreamEvent channel — split out from runCase so it's
// unit testable without a real conversation.Service (see runner_test.go).
// Retrievals/Citations are always non-nil so CaseResult never serializes
// them as JSON null (see CaseResult's doc comment).
type streamCollection struct {
	TraceID    string
	Reply      string
	GotFinal   bool
	Retrievals []RetrievalResult
	Citations  []CitationResult
	Err        string
}

// collectStream drains events to completion. EventFinal.Content is the
// only source of Reply — EventDelta frames are ignored here (the model's
// answer isn't final/citation-normalized until EventFinal, see
// conversation/model.go's EventFinal doc comment), and a stream that
// closes without ever sending EventFinal leaves GotFinal false so the
// caller can fail the case rather than judge a reply that was never
// actually finalized.
func collectStream(events <-chan conversation.StreamEvent) streamCollection {
	sc := streamCollection{
		Retrievals: make([]RetrievalResult, 0),
		Citations:  make([]CitationResult, 0),
	}
	for evt := range events {
		if evt.TraceID != "" {
			sc.TraceID = evt.TraceID
		}
		switch evt.Type {
		case conversation.EventRetrieval:
			for i, r := range evt.Retrieved {
				sc.Retrievals = append(sc.Retrievals, RetrievalResult{
					Rank:            i + 1,
					Ref:             r.Ref,
					KnowledgeBaseID: r.KnowledgeBaseID,
					DocumentID:      r.DocumentID,
					DocumentName:    r.DocumentName,
					Score:           r.Score,
				})
			}
		case conversation.EventFinal:
			sc.GotFinal = true
			sc.Reply = evt.Content
			for _, c := range evt.Citations {
				sc.Citations = append(sc.Citations, CitationResult{
					Ref:             c.Ref,
					KnowledgeBaseID: c.KnowledgeBaseID,
					DocumentID:      c.DocumentID,
					DocumentName:    c.DocumentName,
					ChunkID:         c.ChunkID,
					Score:           c.Score,
				})
			}
		case conversation.EventDelta:
			// Ignored — EventFinal.Content is the authoritative reply, not
			// the concatenation of deltas (they can differ: citation
			// normalization strips invalid [Sx] refs from the final text).
		case conversation.EventError:
			sc.Err = evt.Error
		}
	}
	return sc
}

// caseTurns returns the sequence of user prompts to send for tc — Turns
// when set (multi-turn case), otherwise the single legacy Prompt. See
// TestCase.Turns' doc comment.
func caseTurns(tc TestCase) []string {
	if len(tc.Turns) > 0 {
		return tc.Turns
	}
	return []string{tc.Prompt}
}

func runCase(ctx context.Context, convSvc conversation.Service, traceStore traceLister, judgeClient provider.Client, judgeModel, userID string, tc TestCase) CaseResult {
	result := CaseResult{
		Name:       tc.Name,
		Retrievals: make([]RetrievalResult, 0),
		Citations:  make([]CitationResult, 0),
	}

	conv, err := convSvc.CreateConversation(ctx, userID, tc.AgentID)
	if err != nil {
		result.Err = fmt.Sprintf("create conversation: %v", err)
		return result
	}

	// Every turn is sent in the same conversation so later turns see
	// earlier ones as history (that's what makes multi-turn coreference
	// cases meaningful) — only the final turn's outcome is kept for
	// scoring, see TestCase.Turns' doc comment. A single-Prompt case is
	// just the len(turns)==1 case of this same loop.
	var sc streamCollection
	turns := caseTurns(tc)
	for i, turn := range turns {
		events, err := convSvc.StreamMessage(ctx, userID, conv.ID, turn)
		if err != nil {
			result.Err = fmt.Sprintf("send message (turn %d/%d): %v", i+1, len(turns), err)
			return result
		}

		sc = collectStream(events)
		if sc.Err != "" {
			result.Err = sc.Err
			return result
		}
		if !sc.GotFinal {
			// The stream closed (no error event) without ever sending
			// EventFinal — there is no authoritative reply to judge, so
			// this case fails rather than silently scoring an empty/
			// partial answer.
			result.Err = fmt.Sprintf("turn %d/%d: stream ended without a final event", i+1, len(turns))
			return result
		}
	}

	result.TraceID = sc.TraceID
	result.Reply = sc.Reply
	result.Retrievals = sc.Retrievals
	result.Citations = sc.Citations

	result.Metrics = computeRAGMetrics(tc, result.Retrievals, result.Citations)

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
