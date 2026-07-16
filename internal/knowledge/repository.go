package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	pgvector "github.com/pgvector/pgvector-go"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/platform"
)

// Repository is constructed via NewRepository in wire.go. Knowledge is the
// only module that spans two databases: knowledge_bases/documents live in
// MySQL (business data), chunks live in PostgreSQL+pgvector (content +
// embedding together, so retrieval is a single SQL query).
type Repository struct {
	db        *sql.DB
	queries   *gen.Queries
	pgdb      *sql.DB
	pgQueries *pggen.Queries
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

// deleteDocument removes a document's chunks (PostgreSQL) and the document
// row (MySQL). No cross-database transaction exists, so ordering is the
// consistency tool: chunks go first — a crash between the two steps leaves a
// document whose chunks are already gone (retryable, chunk delete is
// idempotent), never orphaned chunks that keep polluting retrieval results.
func (r *Repository) deleteDocument(ctx context.Context, id string) error {
	if err := r.pgQueries.DeleteChunksByDocument(ctx, id); err != nil {
		return fmt.Errorf("knowledge: delete chunks for document: %w", err)
	}
	if err := r.queries.DeleteDocument(ctx, id); err != nil {
		return fmt.Errorf("knowledge: delete document: %w", err)
	}
	return nil
}

// createChunks inserts a document's chunks in one PostgreSQL transaction —
// all-or-nothing replaces the old per-row loop that could leave a partial
// set behind on failure. A committed loop over ≤ maxChunksPerKnowledgeBase
// rows is fast enough; pgx-native CopyFrom would be quicker but requires
// abandoning database/sql, noted as a future optimization only.
func (r *Repository) createChunks(ctx context.Context, chunks []Chunk) error {
	return platform.WithTx(ctx, r.pgdb, func(tx *sql.Tx) error {
		q := r.pgQueries.WithTx(tx)
		for _, c := range chunks {
			if err := q.CreateChunk(ctx, pggen.CreateChunkParams{
				ID:                 c.ID,
				KnowledgeBaseID:    c.KnowledgeBaseID,
				DocumentID:         c.DocumentID,
				ChunkIndex:         int32(c.ChunkIndex),
				Content:            c.Content,
				ContentLength:      int32(c.ContentLength),
				Embedding:          pgvector.NewVector(c.Embedding),
				EmbeddingDimension: int32(c.EmbeddingDimension),
			}); err != nil {
				return fmt.Errorf("knowledge: create chunk: %w", err)
			}
		}
		return nil
	})
}

// searchChunks pushes similarity scoring and topK selection down to
// pgvector (`ORDER BY embedding <=> query LIMIT topK`), replacing the old
// load-everything-and-score-in-Go path. The dimension filter comes from the
// query vector itself: only chunks embedded by a same-dimension model are
// comparable, which is what keeps mixed-dimension knowledge bases in one
// table from ever colliding.
func (r *Repository) searchChunks(ctx context.Context, kbIDs []string, queryVec []float32, topK int) ([]RetrievedChunk, error) {
	rows, err := r.pgQueries.SearchChunks(ctx, pggen.SearchChunksParams{
		QueryEmbedding:     pgvector.NewVector(queryVec),
		KnowledgeBaseIds:   kbIDs,
		EmbeddingDimension: int32(len(queryVec)),
		TopK:               int32(topK),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: search chunks: %w", err)
	}
	out := make([]RetrievedChunk, 0, len(rows))
	for _, row := range rows {
		out = append(out, RetrievedChunk{
			Chunk: Chunk{
				ID:              row.ID,
				KnowledgeBaseID: row.KnowledgeBaseID,
				DocumentID:      row.DocumentID,
				ChunkIndex:      int(row.ChunkIndex),
				Content:         row.Content,
				ContentLength:   int(row.ContentLength),
				// Embedding stays nil on purpose: no consumer reads it
				// (conversation/workflow only use Content), and shipping
				// vectors back defeats the point of scoring in-database.
				EmbeddingDimension: int(row.EmbeddingDimension),
				CreatedAt:          row.CreatedAt,
			},
			Score: row.Score,
		})
	}
	return out, nil
}

func (r *Repository) countChunksByKnowledgeBase(ctx context.Context, kbID string) (int, error) {
	n, err := r.pgQueries.CountChunksByKnowledgeBase(ctx, kbID)
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

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
