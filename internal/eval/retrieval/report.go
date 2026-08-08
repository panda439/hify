package retrieval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// GateHit is one privacy-safe row of a real Service.Retrieve result, kept
// for human inspection alongside a case's outcome. Like CaseOutcome, it
// structurally cannot hold chunk content, the query text, an embedding, or
// a relevance score — there is no field here to put any of those in.
// Rank/ChunkID/DocumentID/KnowledgeBaseID/NeighborOf are exactly the
// fields the Phase 6 task's privacy requirement names as allowed ("case
// 名、chunk/document ID、rank、NeighborOf、计数与聚合指标") — Score is
// deliberately NOT on that list and must never be added back here (see
// Codex's first-round Phase 6 review, 待修复项 1: an earlier version of
// this struct carried Score, which is not one of the allowed fields).
type GateHit struct {
	Rank            int    `json:"rank"`
	ChunkID         string `json:"chunk_id"`
	DocumentID      string `json:"document_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
	NeighborOf      string `json:"neighbor_of,omitempty"`
}

// GateCaseReport is one case's full record in a saved Phase 6 run report —
// the case name, its privacy-safe hit list, and the outcome derived from
// it.
type GateCaseReport struct {
	Name    string      `json:"name"`
	Hits    []GateHit   `json:"hits"`
	Outcome CaseOutcome `json:"outcome"`
}

// GateReport is what SaveReport persists — the whole Phase 6 run: every
// case's privacy-safe result, the aggregate metrics, and the pass/fail
// decision EvaluateGate produced from them. This is a side artifact for
// human inspection (see docs/eval-phase6-retrieval-gate-report.md); the
// actual regression gate is the calling test's own assertion on Pass, not
// this file — see EvaluateGate's doc comment for why a report nobody reads
// must never be the only enforcement mechanism.
type GateReport struct {
	RanAt   string           `json:"ran_at"`
	Cases   []GateCaseReport `json:"cases"`
	Metrics GateMetrics      `json:"metrics"`
	Pass    bool             `json:"pass"`
	Reasons []string         `json:"reasons,omitempty"`
}

// SaveReport writes report as JSON to path, creating parent directories as
// needed — mirrors internal/eval's Save for the agent-level harness.
func SaveReport(report GateReport, path string) error {
	if report.RanAt == "" {
		report.RanAt = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("retrieval: marshal gate report: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("retrieval: create gate report dir: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("retrieval: write gate report: %w", err)
	}
	return nil
}
