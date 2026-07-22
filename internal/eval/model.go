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
type TestCase struct {
	Name    string `yaml:"name"`
	AgentID string `yaml:"agent_id"`
	Prompt  string `yaml:"prompt"`
	Rubric  string `yaml:"rubric"`
}

// TestSet is the top-level shape of eval/testset.yaml.
type TestSet struct {
	Cases []TestCase `yaml:"cases"`
}

// CaseResult is one test case's outcome for one run. Reasoning != "" only
// when Err == "" (a case that failed to run or whose judge output didn't
// parse gets Err set and Score left at 0, never a fabricated default score
// standing in for "we don't actually know").
type CaseResult struct {
	Name      string       `json:"name"`
	Score     int          `json:"score"`
	Reasoning string       `json:"reasoning"`
	Reply     string       `json:"reply"`
	TraceID   string       `json:"trace_id"`
	Spans     []trace.Span `json:"spans"`
	Err       string       `json:"err,omitempty"`
}

// RunReport is what Save/Load persist to eval/runs/<timestamp>.json and
// eval/baseline.json.
type RunReport struct {
	RanAt   string       `json:"ran_at"`
	Results []CaseResult `json:"results"`
}
