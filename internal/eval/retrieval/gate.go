// Package retrieval is Phase 6's "Deterministic Retrieval Eval Gate": pure,
// dependency-free metric and pass/fail-decision logic for a fixed
// retrieval-only regression dataset. It knows nothing about how a
// RetrievalCaseOutcome was produced — no *sql.DB, no knowledge.Service, no
// HTTP, no LLM — so both "does a healthy run pass" and "does a broken run
// fail" have fast, deterministic unit tests that never touch a real
// database (see gate_test.go). The real database-driving code lives in
// internal/knowledge/eval_gate_test.go, which builds this package's types
// from a real knowledge.Service.Retrieve() call and calls into the pure
// logic here to score it — see that file's doc comment for the full
// picture, including why this needs to be its own leaf package.
//
// This is a sibling of internal/eval, not a subpackage that reuses it:
// internal/eval (runner.go) imports internal/conversation, which imports
// internal/knowledge — so internal/knowledge/eval_gate_test.go importing
// internal/eval directly would form an import cycle
// (knowledge -> eval -> conversation -> knowledge). What this package DOES
// keep is the same conceptual shape internal/eval's RAGMetrics uses in
// model.go: an Evaluated/Value pair (here BoolMetric/FloatMetric,
// deliberately identically named and shaped) so "not configured" can never
// be confused with "evaluated and false/zero", and the same "Hit@K against
// an acceptable set, not multi-relevant-document Recall@K" definition
// documented on RAGMetrics.RecallAt1/RecallAt3.
package retrieval

import "fmt"

// BoolMetric/FloatMetric mirror internal/eval's identically-named types
// (model.go) — see the package doc for why this is a parallel type rather
// than an import.
type BoolMetric struct {
	Evaluated bool
	Value     bool
}

type FloatMetric struct {
	Evaluated bool
	Value     float64
}

// CaseOutcome is the deterministic, privacy-safe outcome of one Phase 6
// retrieval-gate case. It is built by the caller from a real
// knowledge.Service.Retrieve() call and deliberately cannot hold chunk
// content, the query text, an embedding vector, or a content fingerprint —
// there is no field here to accidentally populate with any of those.
// ResultCount and DuplicateContentCount are computed by comparing every
// returned chunk's normalized content in memory and discarding it
// immediately afterward; these two integers are the only trace that
// comparison ever leaves.
type CaseOutcome struct {
	Name string

	// ExpectedConfigured distinguishes "this case intentionally has no
	// expected document" (e.g. a negative/no-hit case) from "this case
	// expected a hit but the retrieval missed" — HitRank == 0 only means a
	// miss when this is true. A case with ExpectedConfigured:false is
	// excluded from HitAtK/ReciprocalRank (both return Evaluated:false for
	// it) and from the aggregate Hit@K/MRR rates below — it must never
	// silently count as a miss and drag those rates down.
	ExpectedConfigured bool
	// HitRank is the 1-based rank (matching Retrieve's return order) of
	// the first result whose document ID is in this case's accepted set;
	// 0 means none of the returned results matched. Only meaningful when
	// ExpectedConfigured is true.
	HitRank int

	// ResultCount is len(results) for this case's Retrieve() call.
	ResultCount int
	// DuplicateContentCount is how many of those results were dropped
	// from "content is normalized-identical to an earlier, higher-ranked
	// result in the same set" — always 0 for a correctly functioning
	// Phase 5 dedup; a nonzero value here (recomputed independently of
	// dedupExactContentChunks/expandWithNeighbors, on the values Retrieve
	// actually returned) is exactly the "duplicate content escaped RRF
	// fusion or neighbor expansion" regression this metric exists to
	// catch — see ContentUniqueRate.
	DuplicateContentCount int
}

// HitAtK reports whether the case's first matching result landed within
// the top k ranks (1-based, inclusive) — Hit@K against the case's accepted
// document set (deliberately not named Recall@K: that would imply
// "fraction of all relevant documents retrieved", which is not what a
// single accepted-set membership check measures — see the package doc).
// Evaluated is false whenever ExpectedConfigured is false, regardless of
// what HitRank happens to hold — a case with no expected document
// configured must never be silently scored as either a hit or a miss.
func HitAtK(o CaseOutcome, k int) BoolMetric {
	if !o.ExpectedConfigured {
		return BoolMetric{}
	}
	return BoolMetric{Evaluated: true, Value: o.HitRank >= 1 && o.HitRank <= k}
}

// ReciprocalRank is 1/HitRank for a hit, 0 for a configured-but-missed
// case, and Evaluated:false when ExpectedConfigured is false.
func ReciprocalRank(o CaseOutcome) FloatMetric {
	if !o.ExpectedConfigured {
		return FloatMetric{}
	}
	if o.HitRank < 1 {
		return FloatMetric{Evaluated: true, Value: 0}
	}
	return FloatMetric{Evaluated: true, Value: 1 / float64(o.HitRank)}
}

// ContentUniqueRate is the fraction of this case's results whose content
// was not a normalized-duplicate of an earlier, higher-ranked result.
// Unlike HitAtK/ReciprocalRank this is always Evaluated, independent of
// ExpectedConfigured — content duplication is a property of what came
// back, not of whether the case configured an expected document (the
// negative/no-hit case still must not return duplicate content, even
// though it has nothing to Hit@K against). An empty result set
// (ResultCount == 0) is defined as fully unique (rate 1.0), not a
// divide-by-zero or a misleading 0.0 — there is nothing duplicated in an
// empty set.
func ContentUniqueRate(o CaseOutcome) FloatMetric {
	if o.ResultCount == 0 {
		return FloatMetric{Evaluated: true, Value: 1}
	}
	unique := o.ResultCount - o.DuplicateContentCount
	return FloatMetric{Evaluated: true, Value: float64(unique) / float64(o.ResultCount)}
}

// GateMetrics aggregates a whole Phase 6 run's outcomes into the numbers
// EvaluateGate checks against a threshold.
type GateMetrics struct {
	HitAt1            FloatMetric
	HitAt3            FloatMetric
	MRR               FloatMetric
	ContentUniqueRate FloatMetric
}

// AggregateMetrics reduces every case's outcome into the four
// dataset-wide numbers a Phase 6 gate run reports and gates on. Hit@1/
// Hit@3/MRR average only over cases with ExpectedConfigured:true (a
// negative/no-hit case contributes to ContentUniqueRate but is excluded
// from these three) — a dataset with zero such cases reports all three as
// Evaluated:false rather than dividing by zero. ContentUniqueRate averages
// over every case, since content duplication is checked regardless of
// whether a case configured an expected document.
func AggregateMetrics(outcomes []CaseOutcome) GateMetrics {
	var uniqueSum float64
	for _, o := range outcomes {
		uniqueSum += ContentUniqueRate(o).Value // always Evaluated, see above
	}
	var contentUnique FloatMetric
	if len(outcomes) > 0 {
		contentUnique = FloatMetric{Evaluated: true, Value: uniqueSum / float64(len(outcomes))}
	}

	return GateMetrics{
		HitAt1:            hitRate(outcomes, 1),
		HitAt3:            hitRate(outcomes, 3),
		MRR:               meanReciprocalRank(outcomes),
		ContentUniqueRate: contentUnique,
	}
}

func hitRate(outcomes []CaseOutcome, k int) FloatMetric {
	var hits, n int
	for _, o := range outcomes {
		m := HitAtK(o, k)
		if !m.Evaluated {
			continue
		}
		n++
		if m.Value {
			hits++
		}
	}
	if n == 0 {
		return FloatMetric{}
	}
	return FloatMetric{Evaluated: true, Value: float64(hits) / float64(n)}
}

func meanReciprocalRank(outcomes []CaseOutcome) FloatMetric {
	var sum float64
	var n int
	for _, o := range outcomes {
		m := ReciprocalRank(o)
		if !m.Evaluated {
			continue
		}
		sum += m.Value
		n++
	}
	if n == 0 {
		return FloatMetric{}
	}
	return FloatMetric{Evaluated: true, Value: sum / float64(n)}
}

// GateThresholds are the Phase 6 regression-gate pass criteria — see
// EvaluateGate. All four are minimums: a run passes only when every metric
// is both Evaluated and at or above its threshold.
type GateThresholds struct {
	MinHitAt1            float64
	MinHitAt3            float64
	MinMRR               float64
	MinContentUniqueRate float64
}

// EvaluateGate is the pure pass/fail decision at the heart of the Phase 6
// regression gate. It is intentionally independent of any database or
// Service.Retrieve call, so both "a healthy run passes" and "a broken run
// fails" have dedicated, fast, deterministic unit tests (gate_test.go)
// proving the gate actually can return ok:false — the acceptance
// requirement that a wrong result must produce a real failure, not a
// report nobody reads. A metric that was never Evaluated (e.g. HitAt1 when
// the dataset configured zero cases with an expected document) is treated
// as a threshold failure, not silently skipped: an empty or misconfigured
// dataset must never look like a passing gate. reasons is nil when ok is
// true.
func EvaluateGate(m GateMetrics, th GateThresholds) (ok bool, reasons []string) {
	ok = true
	check := func(name string, fm FloatMetric, min float64) {
		if !fm.Evaluated {
			ok = false
			reasons = append(reasons, fmt.Sprintf("%s: not evaluated (no case in the dataset produced this metric)", name))
			return
		}
		if fm.Value < min {
			ok = false
			reasons = append(reasons, fmt.Sprintf("%s = %.4f, want >= %.4f", name, fm.Value, min))
		}
	}
	check("Hit@1", m.HitAt1, th.MinHitAt1)
	check("Hit@3", m.HitAt3, th.MinHitAt3)
	check("MRR", m.MRR, th.MinMRR)
	check("ContentUniqueRate", m.ContentUniqueRate, th.MinContentUniqueRate)
	return ok, reasons
}
