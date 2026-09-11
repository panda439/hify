package retrievalbench

import (
	"encoding/json"
	"fmt"
	"testing"
)

func testReport() MetricReport {
	return MetricReport{Fingerprint: Fingerprint{Dataset: "miracl", Revision: "v1", QrelsSHA256: "q", CorpusSHA256: "c", EmbeddingModel: "bge", EmbeddingDigest: "d", EmbeddingDimension: 1024, K: []int{1, 3}}, Aggregates: []AggregateMetrics{{K: 1, Recall: .5, MRR: .5, NDCG: .5}}, Queries: []QueryMetrics{{QueryID: "q1", K: 1, Recall: .5}}}
}

func TestCompareReportsIdenticalAndDetectsRankingChange(t *testing.T) {
	a := testReport()
	b := testReport()
	got, err := CompareReports(a, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "IDENTICAL" {
		t.Fatalf("got %s", got.Status)
	}
	b.Aggregates[0].Recall = .25
	b.Queries[0].Recall = .25
	got, err = CompareReports(a, b, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "CHANGED" || len(got.RegressedQueries) != 1 {
		t.Fatalf("unexpected comparison: %+v", got)
	}
}

func TestCompareReportsRejectsIncompatibleUnlessDiagnosticOverride(t *testing.T) {
	a := testReport()
	b := testReport()
	b.Fingerprint.EmbeddingDigest = "other"
	if _, err := CompareReports(a, b, false); err == nil {
		t.Fatal("expected incompatibility")
	}
	got, err := CompareReports(a, b, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NonComparable {
		t.Fatalf("override must be marked non-comparable")
	}
}

func TestCompareReportsDiagnosticOverrideOmitsFormalDeltas(t *testing.T) {
	a := testReport()
	b := testReport()
	b.Fingerprint.EmbeddingDigest = "other"
	b.Aggregates[0].Recall = .25
	b.Queries[0].Recall = .25

	got, err := CompareReports(a, b, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NonComparable {
		t.Fatal("override must be marked non-comparable")
	}
	if got.Status != "NON_COMPARABLE" {
		t.Fatalf("status = %q, want NON_COMPARABLE", got.Status)
	}
	if len(got.Deltas) != 0 || len(got.ImprovedQueries) != 0 || len(got.RegressedQueries) != 0 || len(got.UnchangedQueries) != 0 {
		t.Fatalf("non-comparable comparison must not emit formal conclusions: %+v", got)
	}
}

func TestCompareReportsRejectsResultAffectingConfigurationChanges(t *testing.T) {
	for name, mutate := range map[string]func(*Fingerprint){
		"selection": func(fp *Fingerprint) { fp.SelectionConfigSHA256 = "other" },
		"chunk":     func(fp *Fingerprint) { fp.ChunkConfig = "{\"chunk_size\":1000}" },
		"retrieval": func(fp *Fingerprint) { fp.RetrievalConfig = "{\"top_k\":5}" },
		"service":   func(fp *Fingerprint) { fp.ServiceEndpointID = "sha256:other" },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := testReport(), testReport()
			mutate(&b.Fingerprint)
			if _, err := CompareReports(a, b, false); err == nil {
				t.Fatalf("expected %s configuration change to be incompatible", name)
			}
		})
	}
}

func TestCompareRerankExperimentAllowsOnlyRerankFieldsToDiffer(t *testing.T) {
	base := testReport()
	candidate := testReport()
	base.Fingerprint.RetrievalConfig = `{"top_k":10,"rerank_enabled":false}`
	candidate.Fingerprint.RetrievalConfig = `{"top_k":10,"rerank_enabled":true}`
	candidate.Fingerprint.RerankEnabled = true
	candidate.Fingerprint.RerankModelName = "BAAI/bge-reranker-v2-m3"
	candidate.Fingerprint.RerankModelDigest = "revision"
	candidate.Fingerprint.RerankCandidateLimit = 50
	candidate.Fingerprint.RerankTimeoutMS = 1500
	candidate.Aggregates[0].MRR = .75

	got, err := CompareRerankExperiment(base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExperimentVariable != "rerank" || got.Status != "CHANGED" || len(got.Deltas) != 1 {
		t.Fatalf("unexpected rerank comparison: %+v", got)
	}

	candidate.Fingerprint.RetrievalConfig = "changed"
	if _, err := CompareRerankExperiment(base, candidate); err == nil {
		t.Fatal("non-rerank fingerprint changes must be rejected")
	}
}

func TestDiagnosticOverrideWithRerankDifferenceEmitsNoFormalDelta(t *testing.T) {
	base, candidate := testReport(), testReport()
	candidate.Fingerprint.RerankEnabled = true
	candidate.Fingerprint.RerankModelName = "BAAI/bge-reranker-v2-m3"
	candidate.Aggregates[0].Recall = 0.1
	got, err := CompareReports(base, candidate, true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NonComparable || got.Status != "NON_COMPARABLE" || len(got.Deltas) != 0 {
		t.Fatalf("diagnostic override emitted formal result: %+v", got)
	}
}

func TestCompareQualityExperimentAllowsQualityRunModeOnly(t *testing.T) {
	base := testReport()
	candidate := testReport()
	candidate.Fingerprint.RerankEnabled = true
	candidate.Fingerprint.RerankModelName = "BAAI/bge-reranker-v2-m3"
	candidate.Fingerprint.RerankTimeoutMS = 30000
	candidate.Fingerprint.RunMode = "quality_diagnostic"
	comparison, err := CompareQualityExperiment(base, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if comparison.ExperimentVariable != "rerank_quality_diagnostic" {
		t.Fatalf("experiment variable = %q", comparison.ExperimentVariable)
	}
}

func TestCompareReportsClassifiesQueriesMutuallyExclusivelyAtK10(t *testing.T) {
	base := testReport()
	base.Queries = []QueryMetrics{
		{QueryID: "q1", K: 1, Recall: 0.1, MRR: 0.1, NDCG: 0.1},
		{QueryID: "q1", K: 10, Recall: 0.5, MRR: 0.5, NDCG: 0.5},
		{QueryID: "q2", K: 10, Recall: 0.5, MRR: 0.5, NDCG: 0.5},
		{QueryID: "q3", K: 10, Recall: 0.5, MRR: 0.5, NDCG: 0.5},
	}
	candidate := base
	candidate.Queries = []QueryMetrics{
		{QueryID: "q1", K: 1, Recall: 0.9, MRR: 0.9, NDCG: 0.9},
		{QueryID: "q1", K: 10, Recall: 0.4, MRR: 0.4, NDCG: 0.4},
		{QueryID: "q2", K: 10, Recall: 0.5, MRR: 0.5, NDCG: 0.5},
		{QueryID: "q3", K: 10, Recall: 0.6, MRR: 0.6, NDCG: 0.6},
	}
	got, err := CompareReports(base, candidate, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ImprovedQueries) != 1 || got.ImprovedQueries[0] != "q3" || len(got.RegressedQueries) != 1 || got.RegressedQueries[0] != "q1" || len(got.UnchangedQueries) != 1 || got.UnchangedQueries[0] != "q2" {
		t.Fatalf("K10 classification is not mutually exclusive: %+v", got)
	}
	if len(got.ImprovedQueries)+len(got.RegressedQueries)+len(got.UnchangedQueries) != 3 {
		t.Fatalf("classification count does not equal query count: %+v", got)
	}
}

func TestCompareReportsClassificationSerializationIsStable(t *testing.T) {
	base := testReport()
	candidate := base
	base.Queries = make([]QueryMetrics, 0, 30)
	candidate.Queries = make([]QueryMetrics, 0, 30)
	for i := 0; i < 30; i++ {
		queryID := fmt.Sprintf("q%02d", i)
		base.Queries = append(base.Queries, QueryMetrics{QueryID: queryID, K: 10, Recall: 0.5, MRR: 0.5, NDCG: 0.5})
		candidateMetric := QueryMetrics{QueryID: queryID, K: 10, Recall: 0.5, MRR: 0.5, NDCG: 0.5}
		if i%3 == 0 {
			candidateMetric.Recall += 0.1
		} else if i%3 == 1 {
			candidateMetric.Recall -= 0.1
		}
		candidate.Queries = append(candidate.Queries, candidateMetric)
	}
	var first []byte
	for i := 0; i < 100; i++ {
		got, err := CompareReports(base, candidate, false)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = encoded
			continue
		}
		if string(encoded) != string(first) {
			t.Fatalf("comparison serialization changed between runs: first=%s current=%s", first, encoded)
		}
	}
}
