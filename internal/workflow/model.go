package workflow

import (
	"encoding/json"
	"time"
)

// StepType enumerates the node kinds the executor understands — the plan's
// simplified workflow doesn't need a general node-plugin system, just this
// fixed set.
type StepType string

const (
	StepStart              StepType = "start"
	StepEnd                StepType = "end"
	StepLLMCall            StepType = "llm_call"
	StepKnowledgeRetrieval StepType = "knowledge_retrieval"
	StepConditional        StepType = "conditional"
	StepToolCall           StepType = "tool_call"
)

// Step is one node in a Definition's graph. Config is step-type-specific
// (see the *Config structs in executor.go) and stored as raw JSON so each
// step handler unmarshals only what it needs, rather than one giant struct
// carrying every type's fields. Next/NextIfTrue/NextIfFalse hold other
// Step.ID values, not indices — a plain adjacency-by-ID graph, validated
// once at save time (see dag.go) so the executor never has to re-check
// structural invariants at run time.
type Step struct {
	ID          string          `json:"id"`
	Type        StepType        `json:"type"`
	Config      json.RawMessage `json:"config,omitempty"`
	Next        string          `json:"next,omitempty"`
	NextIfTrue  string          `json:"next_if_true,omitempty"`
	NextIfFalse string          `json:"next_if_false,omitempty"`
}

// Definition is the whole DAG, stored verbatim in workflows.definition —
// see dag.go's Validate for the invariants this must satisfy before it's
// ever saved.
type Definition struct {
	Steps []Step `json:"steps"`
}

// StepByID is a lookup helper shared by dag.go and executor.go.
func (d Definition) StepByID(id string) (Step, bool) {
	for _, s := range d.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

// startStep returns the workflow's unique entry point. Validate guarantees
// exactly one exists before a Definition is ever persisted, so callers
// past that point (i.e. the executor) can treat "not found" as a bug, not
// a user-facing error.
func (d Definition) startStep() (Step, bool) {
	for _, s := range d.Steps {
		if s.Type == StepStart {
			return s, true
		}
	}
	return Step{}, false
}

type Workflow struct {
	ID          string
	Name        string
	Description string
	Definition  Definition
	IsActive    bool
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type CreateWorkflowInput struct {
	Name        string
	Description string
	Definition  Definition
	CreatedBy   string
}

// UpdateWorkflowInput has no immutable-after-creation fields (unlike
// knowledge_bases' embedding model) — editing the definition freely is the
// whole point of a workflow editor, there's no derived data downstream
// that would become inconsistent the way stored embeddings would.
type UpdateWorkflowInput struct {
	Name        string
	Description string
	Definition  Definition
	IsActive    bool
}

const (
	RunStatusRunning   = "running"
	RunStatusSucceeded = "succeeded"
	RunStatusFailed    = "failed"
)

type WorkflowRun struct {
	ID           string
	WorkflowID   string
	Status       string
	Input        string
	Output       string
	ErrorMessage string
	StartedAt    time.Time
	FinishedAt   *time.Time
	CreatedBy    string
}

const (
	StepStatusSucceeded = "succeeded"
	StepStatusFailed    = "failed"
)

// WorkflowRunStep is one row of the execution trace — see the migration's
// doc comment on why this table is append-only (one INSERT per finished
// step, never an UPDATE), unlike WorkflowRun.
type WorkflowRunStep struct {
	ID            string
	WorkflowRunID string
	StepID        string
	StepType      string
	Status        string
	Input         string
	Output        string
	ErrorMessage  string
	StartedAt     time.Time
	FinishedAt    *time.Time
}

// Event types for the SSE stream Execute produces — mirrors
// conversation.StreamEvent's discriminated-union shape (retrieval/tool_call
// as non-terminal, done/error as terminal). EventStep fires twice per
// step (running, then succeeded|failed), EventDone/EventError end the
// stream exactly once, matching the run's own final status.
const (
	EventStep  = "step"
	EventDone  = "done"
	EventError = "error"
)

// StepEvent is one frame of an in-progress Execute SSE stream. RunID is
// set on every frame so the frontend can navigate to the run's detail page
// without a separate round trip once the stream ends.
type StepEvent struct {
	Type     string
	RunID    string
	StepID   string
	StepType string
	Status   string // running | succeeded | failed (Type == EventStep only)
	Output   string
	Error    string
}
