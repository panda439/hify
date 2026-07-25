package knowledge

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"hify/internal/platform"
	"hify/internal/server/middleware"
)

// Handler is constructed via NewHandler in wire.go.
type Handler struct {
	service Service
}

// maxUploadBodyBytes bounds the whole multipart request body, not just the
// file part — some headroom over maxFileSizeBytes for the multipart
// boundary and other form fields/headers.
const maxUploadBodyBytes = maxFileSizeBytes + 1<<20

func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func (h *Handler) Create(c *gin.Context) error {
	var req createKnowledgeBaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	// ChunkSize/ChunkOverlap default here, not in service.go, specifically
	// so "omitted" (nil) and "explicitly 0" (a legitimate no-overlap
	// choice) stay distinguishable — CreateKnowledgeBaseInput's fields are
	// plain ints, so this pointer check has to happen while req still has
	// the pointer.
	input := CreateKnowledgeBaseInput{
		Name:             req.Name,
		Description:      req.Description,
		EmbeddingModelID: req.EmbeddingModelID,
		ChunkSize:        defaultChunkSize,
		ChunkOverlap:     defaultChunkOverlap,
		CreatedBy:        middleware.UserIDFrom(c),
	}
	if req.ChunkSize != nil {
		input.ChunkSize = *req.ChunkSize
	}
	if req.ChunkOverlap != nil {
		input.ChunkOverlap = *req.ChunkOverlap
	}

	kb, err := h.service.CreateKnowledgeBase(c.Request.Context(), input)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toKnowledgeBaseResponse(kb))
	return nil
}

func (h *Handler) List(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	kbs, total, err := h.service.ListKnowledgeBases(c.Request.Context(), limit, offset)
	if err != nil {
		return err
	}
	items := make([]knowledgeBaseResponse, 0, len(kbs))
	for _, kb := range kbs {
		items = append(items, toKnowledgeBaseResponse(kb))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[knowledgeBaseResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) Get(c *gin.Context) error {
	kb, err := h.service.GetKnowledgeBase(c.Request.Context(), c.Param("id"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toKnowledgeBaseResponse(kb))
	return nil
}

func (h *Handler) Update(c *gin.Context) error {
	var req updateKnowledgeBaseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		return ErrInvalidRequest
	}

	kb, err := h.service.UpdateKnowledgeBase(
		c.Request.Context(),
		c.Param("id"),
		middleware.UserIDFrom(c),
		middleware.RoleFrom(c),
		UpdateKnowledgeBaseInput{
			Name:        req.Name,
			Description: req.Description,
			IsActive:    req.IsActive,
		},
	)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toKnowledgeBaseResponse(kb))
	return nil
}

// UploadDocument takes a multipart file upload (field name "file") rather
// than JSON — the only handler in this module that isn't a plain JSON
// body, since it's carrying binary/text file content, not a structured
// request.
func (h *Handler) UploadDocument(c *gin.Context) error {
	// Cap the request body before Gin's multipart parser buffers it, so an
	// oversized upload is rejected without first reading the whole thing
	// into memory — maxUploadBodyBytes allows some headroom over
	// maxFileSizeBytes for multipart framing (boundary, part headers).
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadBodyBytes)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		if isBodyTooLarge(err) {
			return ErrFileTooLarge
		}
		return ErrInvalidRequest
	}
	fileType := fileTypeFromName(fileHeader.Filename)
	if fileType == "" {
		return ErrUnsupportedFileType
	}

	f, err := fileHeader.Open()
	if err != nil {
		return ErrInvalidRequest
	}
	defer f.Close()

	content, err := io.ReadAll(f)
	if err != nil {
		if isBodyTooLarge(err) {
			return ErrFileTooLarge
		}
		return ErrInvalidRequest
	}

	doc, err := h.service.UploadDocument(
		c.Request.Context(),
		c.Param("id"),
		middleware.UserIDFrom(c),
		middleware.RoleFrom(c),
		fileHeader.Filename,
		fileType,
		content,
	)
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toDocumentResponse(doc))
	return nil
}

func fileTypeFromName(name string) string {
	switch {
	case strings.HasSuffix(name, ".txt"):
		return FileTypeTxt
	case strings.HasSuffix(name, ".md"):
		return FileTypeMD
	case strings.HasSuffix(name, ".pdf"):
		return FileTypePDF
	default:
		return ""
	}
}

func (h *Handler) ListDocuments(c *gin.Context) error {
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	limit = platform.ClampLimit(limit)

	docs, total, err := h.service.ListDocuments(c.Request.Context(), c.Param("id"), limit, offset)
	if err != nil {
		return err
	}
	items := make([]documentResponse, 0, len(docs))
	for _, d := range docs {
		items = append(items, toDocumentResponse(d))
	}

	page := offset/limit + 1
	c.JSON(http.StatusOK, platform.OffsetPage[documentResponse]{
		Items:    items,
		Total:    total,
		Page:     page,
		PageSize: limit,
	})
	return nil
}

func (h *Handler) GetDocument(c *gin.Context) error {
	doc, err := h.service.GetDocument(c.Request.Context(), c.Param("docId"))
	if err != nil {
		return err
	}
	c.JSON(http.StatusOK, toDocumentResponse(doc))
	return nil
}

func (h *Handler) DeleteDocument(c *gin.Context) error {
	if err := h.service.DeleteDocument(c.Request.Context(), c.Param("docId"), middleware.UserIDFrom(c), middleware.RoleFrom(c)); err != nil {
		return err
	}
	c.Status(http.StatusNoContent)
	return nil
}
