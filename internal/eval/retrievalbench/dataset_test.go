package retrievalbench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareDatasetIsDeterministicAndPreservesJudgments(t *testing.T) {
	in := DatasetInput{
		Queries: []BenchmarkQuery{{ID: "q2", Text: "乙"}, {ID: "q1", Text: "甲"}, {ID: "q3", Text: "丙"}},
		Qrels:   []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}, {QueryID: "q1", SourceDocumentID: "d2", Relevance: 0}, {QueryID: "q2", SourceDocumentID: "d3", Relevance: 1}},
		Corpus:  []BenchmarkDocument{{SourceDocumentID: "d1", Text: "一"}, {SourceDocumentID: "d2", Text: "二"}, {SourceDocumentID: "d3", Text: "三"}, {SourceDocumentID: "u1", Text: "干扰"}},
	}
	opts := PrepareOptions{Seed: 7, QueryLimit: 2, TargetDocuments: 4, MinDocuments: 3, MaxDocuments: 5}
	a, err := PrepareDataset(in, opts)
	if err != nil {
		t.Fatal(err)
	}
	b, err := PrepareDataset(in, opts)
	if err != nil {
		t.Fatal(err)
	}
	if a.Manifest.ConfigSHA256 != b.Manifest.ConfigSHA256 || a.Manifest.QrelsSHA256 != b.Manifest.QrelsSHA256 || !equalStrings(a.Manifest.QueryIDs, b.Manifest.QueryIDs) || !equalStrings(a.Manifest.DocumentIDs, b.Manifest.DocumentIDs) {
		t.Fatalf("not deterministic: a=%+v b=%+v", a.Manifest, b.Manifest)
	}
	if len(a.Queries) != 2 || len(a.Documents) != 4 || len(a.Qrels) != 3 {
		t.Fatalf("unexpected sizes: q=%d d=%d r=%d", len(a.Queries), len(a.Documents), len(a.Qrels))
	}
	if a.Documents[3].JudgmentStatus != "unjudged" {
		t.Fatalf("supplement is not marked unjudged: %+v", a.Documents[3])
	}
}

func TestPrepareDatasetRejectsMissingReferencesAndConflicts(t *testing.T) {
	base := DatasetInput{Queries: []BenchmarkQuery{{ID: "q1", Text: "q"}}, Qrels: []Qrel{{QueryID: "q1", SourceDocumentID: "missing", Relevance: 1}}, Corpus: nil}
	if _, err := PrepareDataset(base, PrepareOptions{QueryLimit: 1, MinDocuments: 1, MaxDocuments: 2, TargetDocuments: 1}); err == nil {
		t.Fatal("expected missing document error")
	}
	conflict := DatasetInput{Queries: []BenchmarkQuery{{ID: "q1", Text: "q"}}, Qrels: []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}}, Corpus: []BenchmarkDocument{{SourceDocumentID: "d1", Text: "a"}, {SourceDocumentID: "d1", Text: "b"}}}
	if _, err := PrepareDataset(conflict, PrepareOptions{QueryLimit: 1, MinDocuments: 1, MaxDocuments: 2, TargetDocuments: 1}); err == nil {
		t.Fatal("expected duplicate conflict")
	}
}

func TestWriteDatasetPublishesAtomically(t *testing.T) {
	ds := PreparedDataset{Manifest: DatasetManifest{SchemaVersion: "v1", QueryIDs: []string{"q1"}, DocumentIDs: []string{"d1"}}, Queries: []BenchmarkQuery{{ID: "q1", Text: "q"}}, Documents: []BenchmarkDocument{{SourceDocumentID: "d1", Text: "doc"}}, Qrels: []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}}}
	dir := t.TempDir()
	target := filepath.Join(dir, "mini")
	if err := WriteDataset(target, ds); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
