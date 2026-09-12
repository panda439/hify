package retrievalbench

import (
	"math"
	"reflect"
	"testing"
)

func TestMetricReportCarriesStageCountsAndLocalCostStatus(t *testing.T) {
	r := MetricReport{Stages: BenchmarkStages{
		Prepare:       PhaseStats{ElapsedMS: 1, DocumentCount: 800},
		Embedding:     PhaseStats{ElapsedMS: 2, DocumentCount: 800, ChunkCount: 822},
		Ingest:        PhaseStats{ElapsedMS: 3, DocumentCount: 800, ChunkCount: 822},
		Query:         PhaseStats{ElapsedMS: 4, QueryCount: 50},
		EmbeddingCost: "not_applicable",
	}}
	if r.Stages.EmbeddingCost != "not_applicable" || r.Stages.Query.QueryCount != 50 || r.Stages.Embedding.ChunkCount != 822 {
		t.Fatalf("stage evidence missing: %+v", r.Stages)
	}
}

func TestRescoreRunIsDeterministicAndRankingChangeProducesDelta(t *testing.T) {
	run := RetrievalRun{
		Fingerprint: Fingerprint{K: []int{1, 3}},
		Queries:     []BenchmarkQuery{{ID: "q1"}},
		Qrels:       []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}},
		Results:     []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d1", "d2"}}},
	}
	a, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("repeated rescore differs: a=%+v b=%+v", a, b)
	}
	run.Results[0].DocumentRanking = []string{"d2", "d1"}
	c, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	comparison, err := CompareReports(a, c, false)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.Status != "CHANGED" || len(comparison.Deltas) != 2 || len(comparison.UnchangedQueries) != 0 {
		t.Fatalf("unexpected ranking delta: %+v", comparison)
	}
}

func TestRescoreRunCarriesRerankIdentityAndStats(t *testing.T) {
	run := RetrievalRun{
		Fingerprint:    Fingerprint{K: []int{1}},
		Queries:        []BenchmarkQuery{{ID: "q1"}},
		Qrels:          []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}},
		Results:        []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d1"}}},
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", Ready: true},
		RerankStats:    &RerankPhaseStats{RequestCount: 1, SuccessCount: 1},
	}
	report, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if report.RerankIdentity == nil || report.RerankStats == nil {
		t.Fatalf("rescore dropped rerank evidence: %+v", report)
	}
}

func TestRerankEvidenceRejectsMissingIncompleteAndNonFiniteStats(t *testing.T) {
	valid := MetricReport{
		Complete:       true,
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
		RerankStats:    &RerankPhaseStats{RequestCount: 1, SuccessCount: 1, HifyEnabledCount: 1, HifyAppliedCount: 1, HifyOutcome: "all_applied", SteadyP50MS: 1, SteadyP95MS: 2},
	}
	for name, mutate := range map[string]func(*MetricReport){
		"missing identity": func(r *MetricReport) { r.RerankIdentity = nil },
		"missing stats":    func(r *MetricReport) { r.RerankStats = nil },
		"nonfinite latency": func(r *MetricReport) {
			r.RerankStats.SteadyP95MS = math.Inf(1)
		},
		"incomplete outcome": func(r *MetricReport) { r.RerankStats.HifyOutcome = "not_all_applied_or_degraded" },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			if err := ValidateRerankEvidence(r); err == nil {
				t.Fatal("expected invalid rerank evidence")
			}
		})
	}
}

func TestValidateRerankEvidenceRejectsAllAppliedWithInconsistentHifyCounts(t *testing.T) {
	report := MetricReport{
		Complete: true, QueryCount: 50,
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
		RerankStats: &RerankPhaseStats{
			RequestCount: 50, SuccessCount: 50,
			HifyEnabledCount: 50, HifyAppliedCount: 0, HifyDegradedCount: 50,
			HifyOutcome: "all_applied", SteadyP50MS: 1, SteadyP95MS: 2,
		},
	}
	if err := ValidateRerankEvidence(report); err == nil {
		t.Fatal("all_applied with inconsistent Hify counts must be rejected")
	}
}
