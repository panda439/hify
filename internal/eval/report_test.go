package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oldFormatBaselineJSON is what eval/baseline.json looked like before this
// change — no retrievals/citations/metrics keys at all.
const oldFormatBaselineJSON = `{
  "ran_at": "2026-01-01T00:00:00Z",
  "results": [
    {"name": "basic_qa", "score": 4, "reasoning": "ok", "reply": "old reply", "trace_id": "t1", "spans": []}
  ]
}`

func TestLoad_OldBaselineMissingNewFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(oldFormatBaselineJSON), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	report, err := Load(path)
	if err != nil {
		t.Fatalf("Load old-format baseline: %v", err)
	}
	if len(report.Results) != 1 {
		t.Fatalf("len(Results) = %d, want 1", len(report.Results))
	}
	r := report.Results[0]
	if r.Metrics.RetrievalHit.Evaluated {
		t.Fatalf("expected zero-value Metrics for a baseline predating RAGMetrics")
	}
	if r.Retrievals != nil || r.Citations != nil {
		t.Fatalf("Retrievals/Citations should decode to nil (absent keys), got %#v / %#v", r.Retrievals, r.Citations)
	}
}

func TestCompare_OldBaselineRendersDashForNewMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(oldFormatBaselineJSON), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	baseline, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	current := RunReport{RanAt: "2026-02-01T00:00:00Z", Results: []CaseResult{
		{
			Name: "basic_qa", Score: 5, Reasoning: "better",
			Retrievals: []RetrievalResult{}, Citations: []CitationResult{},
			Metrics: RAGMetrics{
				RetrievalHit: BoolMetric{Evaluated: true, Value: true},
				MRR:          FloatMetric{Evaluated: true, Value: 1},
			},
		},
	}}

	out := Compare(current, baseline)
	if !strings.Contains(out, "— / — / —") {
		t.Fatalf("expected old baseline's missing metrics to render as dashes, got:\n%s", out)
	}
}

func TestRunReport_JSONHasNoNullRetrievalsOrCitations(t *testing.T) {
	report := RunReport{Results: []CaseResult{
		{Name: "empty-case", Retrievals: make([]RetrievalResult, 0), Citations: make([]CitationResult, 0)},
	}}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, `"retrievals":null`) || strings.Contains(s, `"citations":null`) {
		t.Fatalf("expected [] not null for empty Retrievals/Citations, got: %s", s)
	}
	if !strings.Contains(s, `"retrievals":[]`) || !strings.Contains(s, `"citations":[]`) {
		t.Fatalf("expected explicit [] for empty Retrievals/Citations, got: %s", s)
	}
}

func TestRunReport_JSONNeverCarriesRetrievedContentOrQuote(t *testing.T) {
	const secretContent = "SECRET_CHUNK_CONTENT_MUST_NOT_LEAK"
	const secretQuote = "SECRET_QUOTE_MUST_NOT_LEAK"

	report := RunReport{Results: []CaseResult{
		{
			Name: "case-with-retrieval",
			Retrievals: []RetrievalResult{
				{Rank: 1, Ref: "S1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "手册.pdf", Score: 0.9},
			},
			Citations: []CitationResult{
				{Ref: "S1", KnowledgeBaseID: "kb-1", DocumentID: "doc-1", DocumentName: "手册.pdf", ChunkID: "chunk-1", Score: 0.9},
			},
		},
	}}

	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if strings.Contains(s, secretContent) {
		t.Fatalf("report JSON must never contain retrieved chunk content, found %q in: %s", secretContent, s)
	}
	if strings.Contains(s, secretQuote) {
		t.Fatalf("report JSON must never contain citation quotes, found %q in: %s", secretQuote, s)
	}
	if strings.Contains(s, `"content"`) || strings.Contains(s, `"quote"`) {
		t.Fatalf("RetrievalResult/CitationResult must not have content/quote fields at all, got: %s", s)
	}
}
