package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"hify/internal/db/gen"
	"hify/internal/platform"
)

// Repository is constructed via NewRepository in wire.go.
type Repository struct {
	db      *sql.DB
	queries *gen.Queries
}

func (r *Repository) createKnowledgeBase(ctx context.Context, kb KnowledgeBase) error {
	if err := r.queries.CreateKnowledgeBase(ctx, gen.CreateKnowledgeBaseParams{
		ID:               kb.ID,
		Name:             kb.Name,
		Description:      kb.Description,
		EmbeddingModelID: kb.EmbeddingModelID,
		ChunkSize:        int32(kb.ChunkSize),
		ChunkOverlap:     int32(kb.ChunkOverlap),
		CreatedBy:        kb.CreatedBy,
	}); err != nil {
		return fmt.Errorf("knowledge: create knowledge base: %w", err)
	}
	return nil
}

func (r *Repository) getKnowledgeBase(ctx context.Context, id string) (KnowledgeBase, error) {
	row, err := r.queries.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return KnowledgeBase{}, ErrNotFound
		}
		return KnowledgeBase{}, fmt.Errorf("knowledge: get knowledge base: %w", err)
	}
	return toDomainKnowledgeBase(row), nil
}

func (r *Repository) listKnowledgeBases(ctx context.Context, limit, offset int) ([]KnowledgeBase, error) {
	rows, err := r.queries.ListKnowledgeBases(ctx, gen.ListKnowledgeBasesParams{
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: list knowledge bases: %w", err)
	}
	out := make([]KnowledgeBase, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainKnowledgeBase(row))
	}
	return out, nil
}

func (r *Repository) countKnowledgeBases(ctx context.Context) (int, error) {
	n, err := r.queries.CountKnowledgeBases(ctx)
	if err != nil {
		return 0, fmt.Errorf("knowledge: count knowledge bases: %w", err)
	}
	return int(n), nil
}

func (r *Repository) updateKnowledgeBase(ctx context.Context, kb KnowledgeBase) error {
	if err := r.queries.UpdateKnowledgeBase(ctx, gen.UpdateKnowledgeBaseParams{
		Name:        kb.Name,
		Description: kb.Description,
		IsActive:    kb.IsActive,
		ID:          kb.ID,
	}); err != nil {
		return fmt.Errorf("knowledge: update knowledge base: %w", err)
	}
	return nil
}

func (r *Repository) createDocument(ctx context.Context, d Document) error {
	if err := r.queries.CreateDocument(ctx, gen.CreateDocumentParams{
		ID:              d.ID,
		KnowledgeBaseID: d.KnowledgeBaseID,
		FileName:        d.FileName,
		FileType:        d.FileType,
		FileSize:        int32(d.FileSize),
		StoragePath:     d.StoragePath,
		CreatedBy:       d.CreatedBy,
	}); err != nil {
		return fmt.Errorf("knowledge: create document: %w", err)
	}
	return nil
}

func (r *Repository) getDocument(ctx context.Context, id string) (Document, error) {
	row, err := r.queries.GetDocumentByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Document{}, ErrDocumentNotFound
		}
		return Document{}, fmt.Errorf("knowledge: get document: %w", err)
	}
	return toDomainDocument(row), nil
}

func (r *Repository) listDocumentsByKnowledgeBase(ctx context.Context, kbID string, limit, offset int) ([]Document, error) {
	rows, err := r.queries.ListDocumentsByKnowledgeBase(ctx, gen.ListDocumentsByKnowledgeBaseParams{
		KnowledgeBaseID: kbID,
		Limit:           int32(limit),
		Offset:          int32(offset),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: list documents: %w", err)
	}
	out := make([]Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainDocument(row))
	}
	return out, nil
}

func (r *Repository) countDocumentsByKnowledgeBase(ctx context.Context, kbID string) (int, error) {
	n, err := r.queries.CountDocumentsByKnowledgeBase(ctx, kbID)
	if err != nil {
		return 0, fmt.Errorf("knowledge: count documents: %w", err)
	}
	return int(n), nil
}

func (r *Repository) updateDocumentStatus(ctx context.Context, id, status, errMsg string, chunkCount int) error {
	if err := r.queries.UpdateDocumentStatus(ctx, gen.UpdateDocumentStatusParams{
		Status:       status,
		ErrorMessage: stringToNullString(errMsg),
		ChunkCount:   int32(chunkCount),
		ID:           id,
	}); err != nil {
		return fmt.Errorf("knowledge: update document status: %w", err)
	}
	return nil
}

// deleteDocument removes a document and its chunks atomically — without a
// transaction, a crash between the two deletes could leave orphaned chunks
// (harmless but wasted space) or a document with no chunks pointing nowhere
// useful; neither is catastrophic, but there's no reason to allow either
// when WithTx makes it free to avoid.
func (r *Repository) deleteDocument(ctx context.Context, id string) error {
	return platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		if err := q.DeleteChunksByDocument(ctx, id); err != nil {
			return fmt.Errorf("knowledge: delete chunks for document: %w", err)
		}
		if err := q.DeleteDocument(ctx, id); err != nil {
			return fmt.Errorf("knowledge: delete document: %w", err)
		}
		return nil
	})
}

func (r *Repository) createChunk(ctx context.Context, c Chunk) error {
	embeddingJSON, err := json.Marshal(c.Embedding)
	if err != nil {
		return fmt.Errorf("knowledge: marshal embedding: %w", err)
	}
	if err := r.queries.CreateChunk(ctx, gen.CreateChunkParams{
		ID:                 c.ID,
		KnowledgeBaseID:    c.KnowledgeBaseID,
		DocumentID:         c.DocumentID,
		ChunkIndex:         int32(c.ChunkIndex),
		Content:            c.Content,
		ContentLength:      int32(c.ContentLength),
		Embedding:          embeddingJSON,
		EmbeddingDimension: int32(c.EmbeddingDimension),
	}); err != nil {
		return fmt.Errorf("knowledge: create chunk: %w", err)
	}
	return nil
}

// listChunksByKnowledgeBase returns every chunk for a knowledge base —
// intentionally unbounded (no LIMIT), since callers need the full set to
// build the embedding-matrix cache (see cache.go). Bounded instead by
// maxChunksPerKnowledgeBase at upload time.
func (r *Repository) listChunksByKnowledgeBase(ctx context.Context, kbID string) ([]Chunk, error) {
	rows, err := r.queries.ListChunksByKnowledgeBase(ctx, kbID)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list chunks: %w", err)
	}
	out := make([]Chunk, 0, len(rows))
	for _, row := range rows {
		c, err := toDomainChunk(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (r *Repository) countChunksByKnowledgeBase(ctx context.Context, kbID string) (int, error) {
	n, err := r.queries.CountChunksByKnowledgeBase(ctx, kbID)
	if err != nil {
		return 0, fmt.Errorf("knowledge: count chunks: %w", err)
	}
	return int(n), nil
}

func toDomainKnowledgeBase(row gen.KnowledgeBase) KnowledgeBase {
	return KnowledgeBase{
		ID:               row.ID,
		Name:             row.Name,
		Description:      row.Description,
		EmbeddingModelID: row.EmbeddingModelID,
		ChunkSize:        int(row.ChunkSize),
		ChunkOverlap:     int(row.ChunkOverlap),
		IsActive:         row.IsActive,
		CreatedBy:        row.CreatedBy,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

func toDomainDocument(row gen.Document) Document {
	return Document{
		ID:              row.ID,
		KnowledgeBaseID: row.KnowledgeBaseID,
		FileName:        row.FileName,
		FileType:        row.FileType,
		FileSize:        int(row.FileSize),
		StoragePath:     row.StoragePath,
		Status:          row.Status,
		ErrorMessage:    row.ErrorMessage.String,
		ChunkCount:      int(row.ChunkCount),
		CreatedBy:       row.CreatedBy,
		CreatedAt:       row.CreatedAt,
		UpdatedAt:       row.UpdatedAt,
	}
}

func toDomainChunk(row gen.Chunk) (Chunk, error) {
	var embedding []float32
	if err := json.Unmarshal(row.Embedding, &embedding); err != nil {
		return Chunk{}, fmt.Errorf("knowledge: unmarshal embedding: %w", err)
	}
	return Chunk{
		ID:                 row.ID,
		KnowledgeBaseID:    row.KnowledgeBaseID,
		DocumentID:         row.DocumentID,
		ChunkIndex:         int(row.ChunkIndex),
		Content:            row.Content,
		ContentLength:      int(row.ContentLength),
		Embedding:          embedding,
		EmbeddingDimension: int(row.EmbeddingDimension),
		CreatedAt:          row.CreatedAt,
	}, nil
}

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
