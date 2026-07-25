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
