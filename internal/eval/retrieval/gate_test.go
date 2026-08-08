package retrieval

import "testing"

// --- HitAtK / ReciprocalRank / ContentUniqueRate: pure, no DB — see
// gate.go's package doc for why these are DB-free by design. Covers every
// scenario the Phase 6 task's minimum acceptance list names explicitly:
// rank 1, rank 3, miss, empty result, duplicate content, unconfigured
// expected value.

func TestHitAtKRank1(t *testing.T) {
	o := CaseOutcome{Name: "rank1", ExpectedConfigured: true, HitRank: 1, ResultCount: 3}
	if m := HitAtK(o, 1); !m.Evaluated || !m.Value {
		t.Fatalf("Hit@1 = %+v, want evaluated true", m)
	}
	if m := HitAtK(o, 3); !m.Evaluated || !m.Value {
		t.Fatalf("Hit@3 = %+v, want evaluated true (rank 1 is within top 3)", m)
	}
	if m := ReciprocalRank(o); !m.Evaluated || m.Value != 1.0 {
		t.Fatalf("ReciprocalRank = %+v, want evaluated 1.0", m)
	}
}

func TestHitAtKRank3(t *testing.T) {
	o := CaseOutcome{Name: "rank3", ExpectedConfigured: true, HitRank: 3, ResultCount: 3}
	if m := HitAtK(o, 1); !m.Evaluated || m.Value {
		t.Fatalf("Hit@1 = %+v, want evaluated false (hit is at rank 3, not within top 1)", m)
	}
	if m := HitAtK(o, 3); !m.Evaluated || !m.Value {
		t.Fatalf("Hit@3 = %+v, want evaluated true (rank 3 is within top 3)", m)
	}
	if m := ReciprocalRank(o); !m.Evaluated || m.Value != 1.0/3.0 {
		t.Fatalf("ReciprocalRank = %+v, want evaluated 1/3", m)
	}
}

func TestHitAtKMiss(t *testing.T) {
	// Configured (a document was expected) but HitRank == 0: the retrieval
	// genuinely missed. Must be a real evaluated false/zero, not confused
	// with "not configured" — see TestHitAtKUnconfiguredExpectedValue
	// below for the contrast this test exists to draw.
	o := CaseOutcome{Name: "miss", ExpectedConfigured: true, HitRank: 0, ResultCount: 2}
	if m := HitAtK(o, 1); !m.Evaluated || m.Value {
		t.Fatalf("Hit@1 = %+v, want evaluated false", m)
	}
	if m := HitAtK(o, 3); !m.Evaluated || m.Value {
		t.Fatalf("Hit@3 = %+v, want evaluated false", m)
	}
	if m := ReciprocalRank(o); !m.Evaluated || m.Value != 0 {
		t.Fatalf("ReciprocalRank = %+v, want evaluated 0", m)
	}
}

func TestHitAtKUnconfiguredExpectedValue(t *testing.T) {
	// ExpectedConfigured:false — e.g. a negative/no-hit case. HitRank is
	// deliberately left nonzero here to prove the Evaluated:false path
	// really is gated on ExpectedConfigured and not accidentally derived
	// from HitRank — a bug that swapped the condition would still pass
	// TestHitAtKMiss but fail here.
	o := CaseOutcome{Name: "no-expectation", ExpectedConfigured: false, HitRank: 7, ResultCount: 0}
	if m := HitAtK(o, 1); m.Evaluated {
		t.Fatalf("Hit@1 = %+v, want Evaluated:false regardless of HitRank", m)
	}
	if m := HitAtK(o, 3); m.Evaluated {
		t.Fatalf("Hit@3 = %+v, want Evaluated:false regardless of HitRank", m)
	}
	if m := ReciprocalRank(o); m.Evaluated {
		t.Fatalf("ReciprocalRank = %+v, want Evaluated:false regardless of HitRank", m)
	}
}

func TestContentUniqueRateEmptyResult(t *testing.T) {
	o := CaseOutcome{Name: "empty", ExpectedConfigured: false, ResultCount: 0, DuplicateContentCount: 0}
	m := ContentUniqueRate(o)
	if !m.Evaluated || m.Value != 1 {
		t.Fatalf("ContentUniqueRate(empty) = %+v, want evaluated 1.0 (nothing to duplicate in an empty set)", m)
	}
}

func TestContentUniqueRateDuplicateContent(t *testing.T) {
	o := CaseOutcome{Name: "dup", ExpectedConfigured: true, HitRank: 1, ResultCount: 4, DuplicateContentCount: 1}
	m := ContentUniqueRate(o)
	if !m.Evaluated || m.Value != 0.75 {
		t.Fatalf("ContentUniqueRate = %+v, want evaluated 0.75 (3 of 4 results unique)", m)
	}
}

func TestContentUniqueRateAllUnique(t *testing.T) {
	o := CaseOutcome{Name: "all-unique", ExpectedConfigured: true, HitRank: 1, ResultCount: 3, DuplicateContentCount: 0}
	if m := ContentUniqueRate(o); !m.Evaluated || m.Value != 1 {
		t.Fatalf("ContentUniqueRate = %+v, want evaluated 1.0", m)
	}
}

// --- AggregateMetrics: negative cases must not drag Hit@K/MRR down, but
// must still count toward ContentUniqueRate.

func TestAggregateMetricsExcludesUnconfiguredFromHitAndMRR(t *testing.T) {
	outcomes := []CaseOutcome{
		{Name: "hit-rank1", ExpectedConfigured: true, HitRank: 1, ResultCount: 2},
		{Name: "hit-rank3", ExpectedConfigured: true, HitRank: 3, ResultCount: 3},
		{Name: "no-hit-expected", ExpectedConfigured: false, ResultCount: 0}, // must not count as a miss
	}
	m := AggregateMetrics(outcomes)
	if !m.HitAt1.Evaluated || m.HitAt1.Value != 0.5 {
		t.Fatalf("HitAt1 = %+v, want evaluated 0.5 (1 of 2 *configured* cases hit within rank 1; the unconfigured case must not count as a miss)", m.HitAt1)
	}
	if !m.HitAt3.Evaluated || m.HitAt3.Value != 1.0 {
		t.Fatalf("HitAt3 = %+v, want evaluated 1.0 (both configured cases hit within rank 3)", m.HitAt3)
	}
	wantMRR := (1.0 + 1.0/3.0) / 2
	if !m.MRR.Evaluated || m.MRR.Value != wantMRR {
		t.Fatalf("MRR = %+v, want evaluated %f (averaged over 2 configured cases only)", m.MRR, wantMRR)
	}
	// ContentUniqueRate DOES include the unconfigured case (content
	// duplication is independent of whether a document was expected).
	if !m.ContentUniqueRate.Evaluated || m.ContentUniqueRate.Value != 1.0 {
		t.Fatalf("ContentUniqueRate = %+v, want evaluated 1.0 (all 3 cases, including the unconfigured one, have no duplicates)", m.ContentUniqueRate)
	}
}

func TestAggregateMetricsAllUnconfiguredYieldsUnevaluatedHitAndMRR(t *testing.T) {
	outcomes := []CaseOutcome{
		{Name: "neg1", ExpectedConfigured: false, ResultCount: 0},
		{Name: "neg2", ExpectedConfigured: false, ResultCount: 0},
	}
	m := AggregateMetrics(outcomes)
	if m.HitAt1.Evaluated || m.HitAt3.Evaluated || m.MRR.Evaluated {
		t.Fatalf("HitAt1/HitAt3/MRR = %+v/%+v/%+v, want all Evaluated:false (zero configured cases, not a fabricated 0.0)", m.HitAt1, m.HitAt3, m.MRR)
	}
	if !m.ContentUniqueRate.Evaluated || m.ContentUniqueRate.Value != 1.0 {
		t.Fatalf("ContentUniqueRate = %+v, want evaluated 1.0", m.ContentUniqueRate)
	}
}

// --- EvaluateGate: the failure-path test the task's minimum acceptance
// explicitly calls out ("专门测试门禁失败路径，证明错误结果会导致非零退出/
// 测试失败，而不是永远绿"). Both directions are covered: a healthy run
// passes, and each of the four ways a run can go bad independently fails
// it.

func healthyMetrics() GateMetrics {
	return GateMetrics{
		HitAt1:            FloatMetric{Evaluated: true, Value: 1.0},
		HitAt3:            FloatMetric{Evaluated: true, Value: 1.0},
		MRR:               FloatMetric{Evaluated: true, Value: 0.9},
		ContentUniqueRate: FloatMetric{Evaluated: true, Value: 1.0},
	}
}

func standardThresholds() GateThresholds {
	return GateThresholds{MinHitAt1: 0.8, MinHitAt3: 1.0, MinMRR: 0.8, MinContentUniqueRate: 1.0}
}

func TestEvaluateGatePassesOnHealthyRun(t *testing.T) {
	ok, reasons := EvaluateGate(healthyMetrics(), standardThresholds())
	if !ok || len(reasons) != 0 {
		t.Fatalf("EvaluateGate(healthy) = ok=%v reasons=%v, want ok=true and no reasons", ok, reasons)
	}
}

func TestEvaluateGateFailsOnLowHitRate(t *testing.T) {
	m := healthyMetrics()
	m.HitAt1 = FloatMetric{Evaluated: true, Value: 0.5} // below the 0.8 threshold
	ok, reasons := EvaluateGate(m, standardThresholds())
	if ok {
		t.Fatalf("EvaluateGate must fail when Hit@1 drops below its threshold, got ok=true")
	}
	if len(reasons) == 0 {
		t.Fatalf("expected at least one reason explaining the failure, got none")
	}
}

func TestEvaluateGateFailsOnLowMRR(t *testing.T) {
	m := healthyMetrics()
	m.MRR = FloatMetric{Evaluated: true, Value: 0.3} // below the 0.8 threshold — an MRR regression
	ok, reasons := EvaluateGate(m, standardThresholds())
	if ok {
		t.Fatalf("EvaluateGate must fail when MRR drops below its threshold, got ok=true")
	}
	if len(reasons) == 0 {
		t.Fatalf("expected at least one reason explaining the MRR failure, got none")
	}
}

func TestEvaluateGateFailsOnDuplicateContent(t *testing.T) {
	m := healthyMetrics()
	m.ContentUniqueRate = FloatMetric{Evaluated: true, Value: 0.75} // duplicate content leaked into results
	ok, reasons := EvaluateGate(m, standardThresholds())
	if ok {
		t.Fatalf("EvaluateGate must fail when duplicate content is present, got ok=true")
	}
	if len(reasons) == 0 {
		t.Fatalf("expected at least one reason explaining the duplicate-content failure, got none")
	}
}

func TestEvaluateGateFailsOnUnevaluatedMetric(t *testing.T) {
	// An empty/misconfigured dataset (e.g. zero cases configured an
	// expected document) must not look like a passing gate just because
	// there was nothing to measure.
	m := healthyMetrics()
	m.HitAt1 = FloatMetric{} // Evaluated:false
	ok, reasons := EvaluateGate(m, standardThresholds())
	if ok {
		t.Fatalf("EvaluateGate must fail when a required metric was never evaluated, got ok=true")
	}
	if len(reasons) == 0 {
		t.Fatalf("expected at least one reason explaining the unevaluated-metric failure, got none")
	}
}
