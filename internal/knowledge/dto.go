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
	ID           string `json:"id"`
	FileName     string `json:"file_name"`
	FileType     string `json:"file_type"`
	FileSize     int    `json:"file_size"`
	Status       string `json:"status"`
	ErrorMessage string `json:"error_message"`
	ChunkCount   int    `json:"chunk_count"`

	// UnextractedPages are the pages whose text could not be read
	// (007-document-processing-notice). Serialises as null when there is
	// nothing to report — never as an empty array, so a client has exactly
	// one shape to check for "no notice".
	//
	// ⚠️ It is NOT an error. A document carrying it has Status "ready" and
	// is fully searchable; the missing pages simply are not in it. Failure
	// reasons stay in ErrorMessage and the two never mix (FR-002).
	//
	// ⚠️ A client MUST gate display on Status == "ready", not on this field
	// being non-empty: the value can outlive the attempt that wrote it —
	// a later attempt that FAILED leaves the previous success's list in
	// place, and showing it then would describe a state the document is no
	// longer in (FR-005 / contract C5).
	UnextractedPages []int `json:"unextracted_pages"`

	// UnparseablePages are pages the parser could not read at all
	// (008-unparseable-page-notice). Same serialisation rules as above.
	//
	// ⚠️ Parallel to UnextractedPages but NOT interchangeable: the two exist
	// as separate fields because the user's next step differs — the former
	// says "run OCR on these", the latter says "OCR will not help, re-export
	// with another tool". A client that merges them into one "N pages
	// missing" line throws away the only part that tells the user what to do.
	UnparseablePages []int     `json:"unparseable_pages"`
	Version          int64     `json:"version"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
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
		// nil stays nil so it serialises as JSON null, not [].
		UnextractedPages: d.UnextractedPages,
		UnparseablePages: d.UnparseablePages,
		Version:          d.Version,
		CreatedAt:        d.CreatedAt,
		UpdatedAt:        d.UpdatedAt,
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
	// PageNumber/PageEnd are the closed page interval this chunk covers
	// (006-pdf-layout-chunking): PageNumber is the first page, PageEnd the
	// last. They are ALWAYS both null or both set — never one of each —
	// because a chunk either has an honest page range or has none. The
	// frontend relies on exactly that: equal ends render as "第 N 页",
	// different ends as "第 N-M 页", both null as "—". It must NOT paper
	// over a mismatch with `page_end ?? page_number`; that would turn a
	// backend bug into a UI that merely looks fine.
	//
	// Both stay pointers so a chunk with no page serializes as JSON null
	// rather than 0 — a txt/md chunk (or a row written before the 000003
	// migration) genuinely has no page, and 0 would be a fabricated one.
	PageNumber *int    `json:"page_number"`
	PageEnd    *int    `json:"page_end"`
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
			PageEnd:      c.PageEnd,
			Content:      c.Content,
			Score:        c.Score,
			IsNeighbor:   isNeighbor,
			NeighborOf:   c.NeighborOf,
		})
	}
	return out
}
