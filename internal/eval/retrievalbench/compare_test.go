package retrievalbench

import "testing"

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
