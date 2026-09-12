package retrievalbench

import "testing"

func TestDiagnoseLowScoreQueriesSeparatesUnjudgedAndTitleSignals(t *testing.T) {
	ds := PreparedDataset{
		Queries:   []BenchmarkQuery{{ID: "q1"}},
		Documents: []BenchmarkDocument{{SourceDocumentID: "d1", Title: "目标标题"}, {SourceDocumentID: "d2", Title: "目标标题"}, {SourceDocumentID: "d3", Title: "其他"}},
		Qrels:     []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}},
	}
	run := RetrievalRun{Queries: ds.Queries, Qrels: ds.Qrels, Results: []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d2", "d3"}}}}
	got := DiagnoseLowScoreQueries(ds, run, 1)
	if len(got) != 1 || got[0].ReturnedUnjudgedCount != 2 || got[0].ReturnedTitleMatchCount != 1 || got[0].RelevantReturnedCount != 0 {
		t.Fatalf("unexpected diagnostic: %+v", got)
	}
	if len(run.Qrels) != 1 || run.Qrels[0].SourceDocumentID != "d1" {
		t.Fatal("diagnostic must not modify qrels")
	}
}
