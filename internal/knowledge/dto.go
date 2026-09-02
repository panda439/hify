package knowledge

import "time"

type createKnowledgeBaseRequest struct {
	Name             string `json:"name" binding:"required"`
	Description      string `json:"description"`
	EmbeddingModelID string `json:"embedding_model_id" binding:"required"`
	ChunkSize        *int   `json:"chunk_size"`
	ChunkOverlap     *int   `json:"chunk_overlap"`
}

type updateKnowledgeBaseRequest struct {
	Name        string `json:"name" binding:"required"`
	Description string `json:"description"`
	IsActive    bool   `json:"is_active"`
}

type knowledgeBaseResponse struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Description      string    `json:"description"`
	EmbeddingModelID string    `json:"embedding_model_id"`
	ChunkSize        int       `json:"chunk_size"`
	ChunkOverlap     int       `json:"chunk_overlap"`
	IsActive         bool      `json:"is_active"`
	TotalChunks      int       `json:"total_chunks"`
	MaxChunks        int       `json:"max_chunks"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func toKnowledgeBaseResponse(kb KnowledgeBase) knowledgeBaseResponse {
	return knowledgeBaseResponse{
		ID:               kb.ID,
		Name:             kb.Name,
		Description:      kb.Description,
		EmbeddingModelID: kb.EmbeddingModelID,
		ChunkSize:        kb.ChunkSize,
		ChunkOverlap:     kb.ChunkOverlap,
		IsActive:         kb.IsActive,
		TotalChunks:      kb.TotalChunks,
		MaxChunks:        maxChunksPerKnowledgeBase,
		CreatedAt:        kb.CreatedAt,
		UpdatedAt:        kb.UpdatedAt,
	}
}

type documentResponse struct {
	ID           string    `json:"id"`
	FileName     string    `json:"file_name"`
	FileType     string    `json:"file_type"`
	FileSize     int       `json:"file_size"`
	Status       string    `json:"status"`
	ErrorMessage string    `json:"error_message"`
	ChunkCount   int       `json:"chunk_count"`
	Version      int64     `json:"version"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func toDocumentResponse(d Document) documentResponse {
	return documentResponse{
		ID:           d.ID,
		FileName:     d.FileName,
		FileType:     d.FileType,
		FileSize:     d.FileSize,
		Status:       d.Status,
		ErrorMessage: d.ErrorMessage,
		ChunkCount:   d.ChunkCount,
		Version:      d.Version,
		CreatedAt:    d.CreatedAt,
		UpdatedAt:    d.UpdatedAt,
	}
}

// --- 003-retrieval-playground: 试检索面板 ---
//
// retrieveRequest/retrieveResponse back POST /knowledge-bases/:id/retrieve,
// the first (and so far only) HTTP entry point into Service.Retrieve — see
// specs/003-retrieval-playground/contracts/retrieval-http-api.md. The
// endpoint is a QUERY, not a resource creation: POST is used only because
// the body carries a document-ID array and the raw question, and the raw
// question must not end up in a URL where gateway/proxy access logs would
// capture it (same privacy line 002-metadata-filter draws for its own
// diagnostics).
type retrieveRequest struct {
	Query string `json:"query" binding:"required"`
	TopK  int    `json:"top_k"`

	// 002-metadata-filter's filter, flattened into the request body. All
	// three omitted == an empty filter == pre-002 retrieval behavior.
	DocumentIDs []string `json:"document_ids"`
	PageMin     *int     `json:"page_min"`
	PageMax     *int     `json:"page_max"`
}

type chunkResult struct {
	ID           string `json:"id"`
	DocumentID   string `json:"document_id"`
	DocumentName string `json:"document_name"`
	// PageNumber stays a pointer so a chunk with no page serializes as
	// JSON null rather than 0 — a txt/md chunk (or a row written before
	// the 000003 migration) genuinely has no page, and 0 would be a
	// fabricated one. The frontend renders null as "—".
	PageNumber *int    `json:"page_number"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`

	// IsNeighbor marks a chunk pulled in by the Phase 4/7 neighbor window
	// rather than hit by retrieval itself. It matters to the caller
	// because neighbors are EXEMPT from page filtering (002's FR-011), so
	// a page-scoped probe can legitimately return chunks outside the
	// requested range — without this flag that looks like a bug.
	IsNeighbor bool   `json:"is_neighbor"`
	NeighborOf string `json:"neighbor_of"`
}

type retrieveResponse struct {
	Chunks []chunkResult `json:"chunks"`
	// HitCount/NeighborCount let the caller tell "the filter worked but
	// this scope has no answer" from "nothing matched at all" — the
	// distinction US3 of the spec requires.
	HitCount      int  `json:"hit_count"`
	NeighborCount int  `json:"neighbor_count"`
	FilterApplied bool `json:"filter_applied"`
}

func toRetrieveResponse(chunks []RetrievedChunk, filterApplied bool) retrieveResponse {
	// Always a non-nil slice: an empty result must serialize as [] rather
	// than null so the frontend can map over it unconditionally.
	out := retrieveResponse{Chunks: make([]chunkResult, 0, len(chunks)), FilterApplied: filterApplied}
	for _, c := range chunks {
		isNeighbor := c.NeighborOf != ""
		if isNeighbor {
			out.NeighborCount++
		} else {
			out.HitCount++
		}
		out.Chunks = append(out.Chunks, chunkResult{
			ID:           c.ID,
			DocumentID:   c.DocumentID,
			DocumentName: c.DocumentName,
			PageNumber:   c.PageNumber,
			Content:      c.Content,
			Score:        c.Score,
			IsNeighbor:   isNeighbor,
			NeighborOf:   c.NeighborOf,
		})
	}
	return out
}
