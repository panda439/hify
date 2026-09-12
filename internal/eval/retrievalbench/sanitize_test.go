package retrievalbench

import (
	"encoding/json"
	"testing"
)

func TestSanitizeHostedRunClearsQueryTextWithoutChangingScoreEvidence(t *testing.T) {
	run := RetrievalRun{
		Fingerprint: Fingerprint{QueryIDs: []string{"q1"}, K: []int{10}},
		Queries:     []BenchmarkQuery{{ID: "q1", Text: "敏感查询正文"}},
		Qrels:       []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}},
		Results:     []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d1"}}},
		QueryCount:  1, Complete: true,
		RerankIdentity: &RerankModelIdentity{Source: RerankSourceHostedAPI},
	}
	before, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	sanitized, err := SanitizeHostedRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if sanitized.Queries[0].Text != "" {
		t.Fatalf("sanitized query text = %q, want empty", sanitized.Queries[0].Text)
	}
	if run.Queries[0].Text == "" {
		t.Fatal("sanitizing must not mutate the input run")
	}
	after, err := RescoreRun(sanitized)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	afterJSON, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("score evidence changed after sanitization:\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
	}
}

func TestSanitizeHostedRunRejectsNonHostedRun(t *testing.T) {
	run := RetrievalRun{Queries: []BenchmarkQuery{{ID: "q1", Text: "正文"}}}
	if _, err := SanitizeHostedRun(run); err == nil {
		t.Fatal("non-hosted run must not be sanitized as a hosted raw run")
	}
}
