package retrievalbench

import (
	"math"
	"testing"
)

func TestScoreQueryComputesStandardMetrics(t *testing.T) {
	qrels := []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 2}, {QueryID: "q1", SourceDocumentID: "d2", Relevance: 1}, {QueryID: "q1", SourceDocumentID: "d3", Relevance: 0}}
	report, err := ScoreQuery(BenchmarkQuery{ID: "q1", Text: "query"}, []string{"d3", "d2", "d1"}, qrels, false, []int{1, 3})
	if err != nil {
		t.Fatal(err)
	}
	m1 := report[0]
	if m1.TruePositives != 0 || m1.RelevantDocuments != 2 || m1.Recall != 0 || m1.Precision != 0 || m1.MRR != 0 || m1.AP != 0 || m1.NDCG != 0 {
		t.Fatalf("unexpected @1 metrics: %+v", m1)
	}
	m3 := report[1]
	want := map[string]float64{"recall": 1, "precision": 2.0 / 3, "mrr": 0.5, "ap": (0.5 + 2.0/3) / 2, "ndcg": (1/math.Log2(3) + 2/math.Log2(4)) / (2/math.Log2(2) + 1/math.Log2(3))}
	if m3.TruePositives != 2 || m3.RelevantDocuments != 2 || math.Abs(m3.Recall-want["recall"]) > 1e-9 || math.Abs(m3.Precision-want["precision"]) > 1e-9 || math.Abs(m3.MRR-want["mrr"]) > 1e-9 || math.Abs(m3.AP-want["ap"]) > 1e-9 || math.Abs(m3.NDCG-want["ndcg"]) > 1e-9 {
		t.Fatalf("unexpected @3 metrics: %+v want=%v", m3, want)
	}
}

func TestScoreQueryEmptyAndFailedAreZero(t *testing.T) {
	qrels := []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}}
	for _, tc := range []struct {
		name   string
		failed bool
	}{{"empty", false}, {"failed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ScoreQuery(BenchmarkQuery{ID: "q1"}, nil, qrels, tc.failed, []int{1, 3, 5, 10})
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range got {
				if m.Recall != 0 || m.Precision != 0 || m.MRR != 0 || m.AP != 0 || m.NDCG != 0 {
					t.Fatalf("non-zero empty/failed metrics: %+v", m)
				}
			}
		})
	}
}

func TestAggregateMetricsKeepsFailedQueriesInDenominator(t *testing.T) {
	queries := []BenchmarkQuery{{ID: "q1"}, {ID: "q2"}}
	results := []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d1"}}, {QueryID: "q2", Error: "embedding unavailable"}}
	qrels := []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}, {QueryID: "q2", SourceDocumentID: "d2", Relevance: 1}}
	report, err := ScoreRun(queries, results, qrels, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	if report.QueryCount != 2 || report.FailedQueryCount != 1 || report.Aggregates[0].Recall != 0.5 {
		t.Fatalf("failed query was excluded: %+v", report)
	}
}
