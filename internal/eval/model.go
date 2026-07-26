// Package eval is Hify's agent regression harness: it replays a fixed set
// of prompts through conversation.Service, has a separate model judge each
// reply, and compares the scores against a previous run. It's a developer
// tool driven by cmd/evalrunner, not a business module — no handler/dto,
// results are written to local files rather than a DB table (see the plan
// this shipped with for why).
package eval

import "hify/internal/platform/trace"

// TestCase is one row of the developer-maintained test set (see
// eval/testset.yaml). AgentID must reference an existing agent (already
// configured with a model/knowledge bases/tools via the normal UI/API) —
// eval harness doesn't invent its own agent-configuration format.
//
// The RAG-specific fields (ExpectedDocumentIDs/RequireCitation/
// ExpectedFacts/ForbiddenFacts) are all optional — a case that leaves them
// unset (like the "tool_call" example in testset.yaml, which has nothing to
// do with retrieval) simply skips the metrics that need them; see
// computeRAGMetrics' Evaluated flag.
type TestCase struct {
	Name    string `yaml:"name"`
	AgentID string `yaml:"agent_id"`
	Prompt  string `yaml:"prompt"`
	Rubric  string `yaml:"rubric"`

	// ExpectedDocumentIDs is a set of acceptable documents — order carries
	// no meaning. RetrievalHit/MRR/ExpectedDocumentCited all match against
	// the whole set: RetrievalHit/ExpectedDocumentCited if any of them
	// appears, MRR from the lowest-rank retrieval that matches any of them
	// (standard MRR-against-a-relevant-set semantics, not "the first ID in
	// this list is more important than the others").
	ExpectedDocumentIDs []string `yaml:"expected_document_ids,omitempty"`
	// ExpectedFacts/ForbiddenFacts are natural-language statements handed to
	// the judge model — matching them requires semantic understanding, so
	// this stays LLM-judged rather than a keyword/DSL check (see judge.go).
	ExpectedFacts   []string `yaml:"expected_facts,omitempty"`
	ForbiddenFacts  []string `yaml:"forbidden_facts,omitempty"`
	RequireCitation bool     `yaml:"require_citation,omitempty"`
}

// TestSet is the top-level shape of eval/testset.yaml.
type TestSet struct {
	Cases []TestCase `yaml:"cases"`
}

// RetrievalResult is one hit from EventRetrieval, stripped of its Content —
// see the package doc's privacy section: the eval report must never carry
// retrieved chunk text, so this type structurally can't hold it (there's no
// field to accidentally populate). Rank is 1-based, assigned from the
// EventRetrieval payload's order (i.e. the order the model actually saw the
// evidence in), not recomputed from Score.
type RetrievalResult struct {
	Rank            int     `json:"rank"`
	Ref             string  `json:"ref"`
	KnowledgeBaseID string  `json:"knowledge_base_id"`
	DocumentID      string  `json:"document_id"`
	DocumentName    string  `json:"document_name"`
	Score           float64 `json:"score"`
}

// CitationResult is one entry from EventFinal.Citations, stripped of its
// Quote for the same reason RetrievalResult drops Content — see the
// package doc's privacy section.
type CitationResult struct {
	Ref             string  `json:"ref"`
	KnowledgeBaseID string  `json:"knowledge_base_id"`
	DocumentID      string  `json:"document_id"`
	DocumentName    string  `json:"document_name"`
	ChunkID         string  `json:"chunk_id"`
	Score           float64 `json:"score"`
}

// BoolMetric/FloatMetric wrap a metric value with whether it was even
// applicable to this case — plain bool/float64 can't tell "not configured"
// apart from "evaluated and false/zero", and conflating the two would make
// a case with no ExpectedDocumentIDs silently look like a retrieval miss.
type BoolMetric struct {
	Evaluated bool `json:"evaluated"`
	Value     bool `json:"value"`
}

type FloatMetric struct {
	Evaluated bool    `json:"evaluated"`
	Value     float64 `json:"value"`
}

// RAGMetrics holds the deterministic (non-judge) retrieval/citation
// metrics for one case — see computeRAGMetrics for the exact rules
// governing when each field is Evaluated.
type RAGMetrics struct {
	RetrievalHit           BoolMetric  `json:"retrieval_hit"`
	MRR                    FloatMetric `json:"mrr"`
	CitationRequirementMet BoolMetric  `json:"citation_requirement_met"`
	ExpectedDocumentCited  BoolMetric  `json:"expected_document_cited"`
}

// CaseResult is one test case's outcome for one run. Reasoning != "" only
// when Err == "" (a case that failed to run or whose judge output didn't
// parse gets Err set and Score left at 0, never a fabricated default score
// standing in for "we don't actually know"). Retrievals/Citations are
// always non-nil (never null in the JSON report, even when empty — see
// runCase) and never carry retrieved content or citation quotes.
type CaseResult struct {
	Name      string       `json:"name"`
	Score     int          `json:"score"`
	Reasoning string       `json:"reasoning"`
	Reply     string       `json:"reply"`
	TraceID   string       `json:"trace_id"`
	Spans     []trace.Span `json:"spans"`
	Err       string       `json:"err,omitempty"`

	Retrievals []RetrievalResult `json:"retrievals"`
	Citations  []CitationResult  `json:"citations"`
	Metrics    RAGMetrics        `json:"metrics"`
}

// RunReport is what Save/Load persist to eval/runs/<timestamp>.json and
// eval/baseline.json.
type RunReport struct {
	RanAt   string       `json:"ran_at"`
	Results []CaseResult `json:"results"`
}
