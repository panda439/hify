package knowledge

import "time"

const (
	FileTypeTxt = "txt"
	FileTypeMD  = "md"
	FileTypePDF = "pdf"
)

const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

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
)

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
// pending -> processing -> ready|failed.
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
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Chunk is one embedded slice of a Document. It's immutable derived data —
// content changes mean delete-and-regenerate, never update-in-place, same
// as conversation.Message.
type Chunk struct {
	ID                 string
	KnowledgeBaseID    string
	DocumentID         string
	ChunkIndex         int
	Content            string
	ContentLength      int
	Embedding          []float32
	EmbeddingDimension int
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
