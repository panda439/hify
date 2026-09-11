package knowledge

import (
	"context"
	"errors"
	"testing"
)

type benchmarkFakeService struct {
	uploaded []string
	docs     map[string]Document
	fail     bool
	rerank   rerankStats
}

func (f *benchmarkFakeService) UploadDocument(ctx context.Context, kbID, userID, role, fileName, fileType string, content []byte) (Document, error) {
	if f.fail {
		return Document{}, errors.New("upload failed")
	}
	id := "hify-" + fileName
	d := Document{ID: id, KnowledgeBaseID: kbID, FileName: fileName, FileType: fileType, Status: StatusPending, Version: 1}
	f.docs[id] = d
	f.uploaded = append(f.uploaded, id)
	return d, nil
}
func (f *benchmarkFakeService) ProcessDocument(ctx context.Context, id string, version int64) error {
	d := f.docs[id]
	d.Status = StatusReady
	d.ChunkCount = 2
	f.docs[id] = d
	return nil
}
func (f *benchmarkFakeService) GetDocument(ctx context.Context, id string) (Document, error) {
	d, ok := f.docs[id]
	if !ok {
		return Document{}, errors.New("missing")
	}
	return d, nil
}
func (f *benchmarkFakeService) Retrieve(ctx context.Context, ids []string, q string, k int, opts RetrieveOptions) ([]RetrievedChunk, error) {
	benchmarkObserverFromContext(ctx).record(f.rerank)
	return []RetrievedChunk{{Chunk: Chunk{DocumentID: ids[0], ID: "chunk-1"}}}, nil
}

func TestBenchmarkAdapterIngestsIsolatedDocumentsAndResumesReady(t *testing.T) {
	f := &benchmarkFakeService{docs: map[string]Document{}}
	a := NewBenchmarkAdapter(f, "kb-bench", "user", "admin")
	report, err := a.Ingest(context.Background(), []BenchmarkDocumentInput{{SourceDocumentID: "miracl-1", FileName: "miracl-1.txt", Content: "内容"}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Failed != 0 || report.Ready != 1 || report.Documents["miracl-1"] != "hify-miracl-1.txt" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if err := a.ValidateComplete(report); err != nil {
		t.Fatal(err)
	}
	fail := &benchmarkFakeService{docs: map[string]Document{}, fail: true}
	a = NewBenchmarkAdapter(fail, "kb-bench", "user", "admin")
	report, err = a.Ingest(context.Background(), []BenchmarkDocumentInput{{SourceDocumentID: "miracl-2", FileName: "miracl-2.txt", Content: "内容"}})
	if err == nil || report.Complete {
		t.Fatalf("failed ingest must be incomplete: report=%+v err=%v", report, err)
	}
}

func TestBenchmarkAdapterRejectsCrossKnowledgeBaseResults(t *testing.T) {
	f := &benchmarkFakeService{docs: map[string]Document{}}
	a := NewBenchmarkAdapter(f, "", "user", "admin")
	if _, err := a.Retrieve(context.Background(), "q", 10); err == nil {
		t.Fatal("expected isolated KB validation")
	}
}

func TestBenchmarkAdapterCapturesAppliedAndDegradedRerankObservation(t *testing.T) {
	f := &benchmarkFakeService{
		docs:   map[string]Document{},
		rerank: rerankStats{Enabled: true, Applied: false, Degraded: true, InputCount: 50, DurationMs: 1501},
	}
	a := NewBenchmarkAdapter(f, "kb-bench", "user", "admin")
	got, err := a.Retrieve(context.Background(), "问题", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !got.RerankEnabled || got.RerankApplied || !got.RerankDegraded || got.RerankInputCount != 50 || got.RerankDurationMS != 1501 {
		t.Fatalf("adapter lost Hify rerank observation: %+v", got)
	}
}
