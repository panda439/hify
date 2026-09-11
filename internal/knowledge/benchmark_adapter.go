package knowledge

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"sync"
	"time"
)

// BenchmarkKnowledgeService is the small part of Service needed by the
// offline benchmark. Keeping this interface narrow makes the adapter unable
// to bypass normal upload, processing, and Retrieve paths.
type BenchmarkKnowledgeService interface {
	UploadDocument(context.Context, string, string, string, string, string, []byte) (Document, error)
	ProcessDocument(context.Context, string, int64) error
	GetDocument(context.Context, string) (Document, error)
	Retrieve(context.Context, []string, string, int, RetrieveOptions) ([]RetrievedChunk, error)
}

// benchmarkRetrieveObserverKey and benchmarkRetrieveObserver are deliberately
// package-private. BenchmarkAdapter can observe the concrete Service path
// without widening Service's public API or changing normal callers.
type benchmarkRetrieveObserverKey struct{}

type benchmarkRetrieveObserver struct {
	rerank rerankStats
}

func benchmarkObserverFromContext(ctx context.Context) *benchmarkRetrieveObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(benchmarkRetrieveObserverKey{}).(*benchmarkRetrieveObserver)
	return observer
}

func (o *benchmarkRetrieveObserver) record(stats rerankStats) {
	if o != nil {
		o.rerank = stats
	}
}

type BenchmarkDocumentInput struct {
	SourceDocumentID string
	FileName         string
	Content          string
	HifyDocumentID   string
}
type BenchmarkIngestReport struct {
	KnowledgeBaseID        string            `json:"knowledge_base_id"`
	Documents              map[string]string `json:"documents"`
	Ready                  int               `json:"ready"`
	Failed                 int               `json:"failed"`
	ChunkCount             int               `json:"chunk_count"`
	EmbeddingElapsedMS     int64             `json:"embedding_elapsed_ms"`
	EmbeddingDocumentCount int               `json:"embedding_document_count"`
	EmbeddingChunkCount    int               `json:"embedding_chunk_count"`
	Errors                 map[string]string `json:"errors,omitempty"`
	ElapsedMS              int64             `json:"elapsed_ms"`
	Complete               bool              `json:"complete"`
}
type BenchmarkRetrievalResult struct {
	QueryID          string
	Result           RawBenchmarkRetrieval
	Error            string
	ElapsedMS        int64
	RerankEnabled    bool
	RerankApplied    bool
	RerankDegraded   bool
	RerankInputCount int
	RerankDurationMS int64
}
type RawBenchmarkRetrieval struct {
	ChunkHits       []BenchmarkChunkHit
	DocumentRanking []string
}
type BenchmarkChunkHit struct {
	ChunkID          string
	SourceDocumentID string
	Rank             int
}

type BenchmarkAdapter struct {
	svc                BenchmarkKnowledgeService
	kbID, userID, role string
	mu                 sync.RWMutex
	sourceByHify       map[string]string
}

func NewBenchmarkAdapter(svc BenchmarkKnowledgeService, kbID, userID, role string) *BenchmarkAdapter {
	return &BenchmarkAdapter{svc: svc, kbID: kbID, userID: userID, role: role, sourceByHify: make(map[string]string)}
}

func (a *BenchmarkAdapter) Ingest(ctx context.Context, docs []BenchmarkDocumentInput) (BenchmarkIngestReport, error) {
	r := BenchmarkIngestReport{KnowledgeBaseID: a.kbID, Documents: make(map[string]string), Errors: make(map[string]string)}
	if a == nil || a.svc == nil {
		return r, fmt.Errorf("benchmark adapter service is required")
	}
	if a.kbID == "" {
		return r, fmt.Errorf("benchmark knowledge base id is required")
	}
	started := time.Now()
	for _, in := range docs {
		if in.SourceDocumentID == "" || in.FileName == "" {
			r.Failed++
			r.Errors[in.SourceDocumentID] = "source document id and filename are required"
			continue
		}
		d := Document{}
		var err error
		if in.HifyDocumentID != "" {
			d, err = a.svc.GetDocument(ctx, in.HifyDocumentID)
			if err == nil && d.Status == StatusReady && d.StoragePath != "" {
				stored, readErr := os.ReadFile(d.StoragePath)
				if readErr != nil {
					err = fmt.Errorf("read checkpoint document: %w", readErr)
				} else if fmt.Sprintf("%x", sha256.Sum256(stored)) != fmt.Sprintf("%x", sha256.Sum256([]byte(in.Content))) {
					err = fmt.Errorf("checkpoint content hash differs")
				}
			}
		} else {
			d, err = a.svc.UploadDocument(ctx, a.kbID, a.userID, a.role, in.FileName, FileTypeTxt, []byte(in.Content))
		}
		if err == nil && d.KnowledgeBaseID != a.kbID {
			err = fmt.Errorf("document belongs to another knowledge base")
		}
		if err == nil && d.Status != StatusReady {
			embeddingStarted := time.Now()
			if err = a.svc.ProcessDocument(ctx, d.ID, d.Version); err == nil {
				r.EmbeddingElapsedMS += time.Since(embeddingStarted).Milliseconds()
				d, err = a.svc.GetDocument(ctx, d.ID)
			}
			if err == nil {
				r.EmbeddingDocumentCount++
				r.EmbeddingChunkCount += d.ChunkCount
			}
		}
		if err != nil {
			r.Failed++
			r.Errors[in.SourceDocumentID] = err.Error()
			continue
		}
		if d.Status != StatusReady {
			r.Failed++
			r.Errors[in.SourceDocumentID] = fmt.Sprintf("document status %s", d.Status)
			continue
		}
		r.Ready++
		r.ChunkCount += d.ChunkCount
		r.Documents[in.SourceDocumentID] = d.ID
		a.mu.Lock()
		a.sourceByHify[d.ID] = in.SourceDocumentID
		a.mu.Unlock()
	}
	r.ElapsedMS = time.Since(started).Milliseconds()
	r.Complete = r.Failed == 0 && r.Ready == len(docs)
	if !r.Complete {
		return r, fmt.Errorf("benchmark ingest incomplete: %d failed documents", r.Failed)
	}
	return r, nil
}

func (a *BenchmarkAdapter) ValidateComplete(r BenchmarkIngestReport) error {
	if !r.Complete || r.Failed > 0 {
		return fmt.Errorf("benchmark ingest is incomplete")
	}
	return nil
}

func (a *BenchmarkAdapter) Retrieve(ctx context.Context, query string, topK int) (BenchmarkRetrievalResult, error) {
	out := BenchmarkRetrievalResult{QueryID: query}
	if a == nil || a.svc == nil {
		return out, fmt.Errorf("benchmark adapter service is required")
	}
	if a.kbID == "" {
		return out, fmt.Errorf("benchmark knowledge base id is required")
	}
	observer := &benchmarkRetrieveObserver{}
	ctx = context.WithValue(ctx, benchmarkRetrieveObserverKey{}, observer)
	started := time.Now()
	hits, err := a.svc.Retrieve(ctx, []string{a.kbID}, query, topK, RetrieveOptions{})
	out.ElapsedMS = time.Since(started).Milliseconds()
	out.RerankEnabled = observer.rerank.Enabled
	out.RerankApplied = observer.rerank.Applied
	out.RerankDegraded = observer.rerank.Degraded
	out.RerankInputCount = observer.rerank.InputCount
	out.RerankDurationMS = observer.rerank.DurationMs
	if err != nil {
		out.Error = err.Error()
		return out, err
	}
	seen := map[string]struct{}{}
	for i, h := range hits {
		source := h.DocumentID
		a.mu.RLock()
		if mapped, ok := a.sourceByHify[h.DocumentID]; ok {
			source = mapped
		}
		a.mu.RUnlock()
		out.Result.ChunkHits = append(out.Result.ChunkHits, BenchmarkChunkHit{ChunkID: h.ID, SourceDocumentID: source, Rank: i + 1})
		if source != "" {
			if _, ok := seen[source]; !ok {
				seen[source] = struct{}{}
				out.Result.DocumentRanking = append(out.Result.DocumentRanking, source)
			}
		}
	}
	return out, nil
}
