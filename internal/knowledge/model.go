package knowledge

import "time"

const (
	FileTypeTxt = "txt"
	FileTypeMD  = "md"
	FileTypePDF = "pdf"
)

// StatusPublishing sits between processing and ready: Embedding is done
// and the new version's chunks are already written to PG (unpublished),
// but the PG publish step (delete obsolete versions + mark this version
// published, see repository.go's publishDocumentVersion) hasn't been
// confirmed to have succeeded yet. Splitting this out from processing
// means ReconcileStuckDocuments never has to redo the (expensive) embed
// work to recover a document stuck here — see ProcessDocument and
// publishAndComplete.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusPublishing = "publishing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// leaseDuration bounds how long a claimed document (processing or
// publishing) can go without a heartbeat before ReconcileStuckDocuments is
// allowed to reclaim it — see ProcessDocument's renewal calls after every
// embed batch and before each state transition. It's sized to comfortably
// cover ONE external call, not the whole document (a document can take
// ~63 batches at maxChunksPerDocument/embedBatchSize, which is exactly
// why a single flat threshold covering the whole run doesn't work).
//
// provider.Client's resilience layer (provider/resilience.go) retries a
// single Embed call up to MaxRetries+1 = 3 times (default MaxRetries=2,
// see registry.go's resilienceConfigFrom), with retry-go backoff starting
// at 500ms and capped at 15s per gap (retry.Delay/retry.MaxDelay) — worst
// case backoff overhead across the two gaps between three attempts is
// well under a minute. The underlying HTTP client has no fixed timeout,
// so in theory one attempt could hang, but for a real (even
// rate-limited/degraded) embedding API, latency for a 32-item batch is
// seconds, not minutes. 3 minutes leaves generous headroom over that
// realistic worst case without approaching anything like "cover the
// entire document".
//
// This is a var, not a const, purely so tests can shrink it to exercise
// real lease expiry with a real (short) sleep instead of mocking time;
// production code never assigns to it.
var leaseDuration = 3 * time.Minute

// stalePendingThreshold is unrelated to leaseDuration — a pending
// document has never been claimed, so it never holds a lease. In normal
// operation a worker picks a pending document up within seconds of
// enqueue; anything stuck longer likely means the enqueue call itself
// never reached Redis (see UploadDocument's enqueueProcessDocument error
// comment), which only a plain elapsed-time scan can catch.
const stalePendingThreshold = 2 * time.Minute

const (
	defaultChunkSize    = 500
	defaultChunkOverlap = 50

	// maxFileSizeBytes is a hard cap on a single upload (10MB) — not the
	// same thing as maxChunksPerKnowledgeBase, which is a soft, whole-KB
	// limit checked at upload time (see service.go).
	maxFileSizeBytes = 10 << 20

	// maxChunksPerKnowledgeBase is the soft cap from the plan's Phase 3
	// design — exceeding it doesn't block the upload, the handler just
	// returns a warning the frontend surfaces to the user.
	maxChunksPerKnowledgeBase = 5000

	// maxChunksPerDocument is a hard cap, unlike maxChunksPerKnowledgeBase
	// above: a misconfigured chunk_size (e.g. a KB created with a very
	// small size) on a large file could otherwise blow up into hundreds of
	// thousands of chunks, each needing its own embedding call and PG row.
	// ProcessDocument rejects the document outright before ever calling
	// Embed if chunking produces more than this many pieces.
	maxChunksPerDocument = 2000

	// embedBatchSize bounds how many chunks go into a single
	// provider.Client.Embed call — see ProcessDocument's batching loop.
	// The provider layer already retries/rate-limits/circuit-breaks a
	// single Embed call (see provider/resilience.go); this cap exists so
	// one call's payload can't grow unbounded with maxChunksPerDocument,
	// not to duplicate that resilience.
	embedBatchSize = 32

	// maxTopK bounds Retrieve's topK — conversation hardcodes a small
	// topK (see conversation/context.go's retrievalTopK), but
	// workflow/executor.go's TopK is workflow-author-configurable JSON and
	// was previously unbounded. Retrieve silently clamps to this, the same
	// "soft usability cap, not a validation error" convention
	// platform.ClampLimit already uses for pagination.
	maxTopK = 50

	// defaultTopK is what an unset/non-positive topK clamps to — mirrors
	// conversation's own retrievalTopK default so Retrieve's fallback
	// behavior doesn't surprise either caller.
	defaultTopK = 5
)

// clampTopK enforces maxTopK and a sane default, the same "clamp, don't
// reject" convention as platform.ClampLimit — callers (workflow node
// config in particular) must not be trusted to supply a bounded topK
// directly.
func clampTopK(topK int) int {
	if topK <= 0 {
		return defaultTopK
	}
	if topK > maxTopK {
		return maxTopK
	}
	return topK
}

// KnowledgeBase is the domain type for a RAG knowledge base. EmbeddingModelID
// /ChunkSize/ChunkOverlap are immutable after creation — see the "创建后不可
// 修改" note in the plan: every chunk under a KB is embedded with the same
// model at the same chunking granularity, and changing either mid-flight
// would make old and new chunks incomparable (different vector space,
// inconsistent retrieval granularity) without anyone getting an error.
type KnowledgeBase struct {
	ID               string
	Name             string
	Description      string
	EmbeddingModelID string
	ChunkSize        int
	ChunkOverlap     int
	IsActive         bool
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	// TotalChunks is computed at read time (not stored), backing the
	// frontend's "X / 5000 分片" soft-cap indicator — see
	// maxChunksPerKnowledgeBase.
	TotalChunks int
}

// Document is one uploaded file working its way through
// pending -> processing -> publishing -> ready, or processing -> failed.
// Version identifies the current processing attempt (not a retry
// counter) — every status transition in ProcessDocument is a
// compare-and-swap keyed on (id, version, expected old status). Version
// only advances when a new attempt is deliberately started (RetryDocument,
// or ReconcileStuckDocuments reclaiming a document whose processing lease
// expired); a normal pending->processing->publishing->ready run never
// changes it. This is what lets a late/duplicate task instance recognize
// it has been superseded and exit without republishing stale chunks — see
// service.go's ProcessDocument. LeaseExpiresAt is non-nil only while
// Status is processing or publishing — see leaseDuration.
type Document struct {
	ID              string
	KnowledgeBaseID string
	FileName        string
	FileType        string
	FileSize        int
	StoragePath     string
	Status          string
	ErrorMessage    string
	ChunkCount      int
	Version         int64
	LeaseExpiresAt  *time.Time
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Chunk is one embedded slice of a Document. It's immutable derived data —
// content changes mean delete-and-regenerate, never update-in-place, same
// as conversation.Message.
//
// DocumentName is a snapshot of Document.FileName taken at processing time
// (see service.go's ProcessDocument) — a source-attribution label, not a
// live join back to MySQL's documents table, so it survives the document
// being renamed, reprocessed, or later deleted. Chunks written before this
// field existed carry DocumentName == "" (see pgmigrations 000003); callers
// needing a display name must fall back to DocumentID themselves rather
// than treating "" as an error.
//
// PageNumber/SectionTitle are nil unless the parser can honestly produce
// them (see chunk.go's chunkDocument and its per-file-type chunkers):
// txt chunks never set either, md chunks may set SectionTitle but never
// PageNumber, pdf chunks may set PageNumber but never SectionTitle (the
// reconstructed-from-glyph-positions text has no reliable heading signal).
// Never fabricate a value here.
type Chunk struct {
	ID                 string
	KnowledgeBaseID    string
	DocumentID         string
	DocumentName       string
	ChunkIndex         int
	Content            string
	ContentLength      int
	Embedding          []float32
	EmbeddingDimension int
	PageNumber         *int
	SectionTitle       *string
	CreatedAt          time.Time
}

// RetrievedChunk is a Chunk annotated with its similarity score against a
// query — what conversation's context assembly and the debug panel consume.
type RetrievedChunk struct {
	Chunk
	Score float64
}

type CreateKnowledgeBaseInput struct {
	Name             string
	Description      string
	EmbeddingModelID string
	ChunkSize        int
	ChunkOverlap     int
	CreatedBy        string
}

// UpdateKnowledgeBaseInput deliberately excludes EmbeddingModelID/ChunkSize
// /ChunkOverlap — see the KnowledgeBase doc comment.
type UpdateKnowledgeBaseInput struct {
	Name        string
	Description string
	IsActive    bool
}
