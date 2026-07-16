package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/hibiken/asynq"

	"hify/internal/platform"
	"hify/internal/provider"
	"hify/internal/user"
)

// Service is knowledge's public contract — the only thing higher-layer
// modules (agent, conversation) are allowed to depend on.
type Service interface {
	CreateKnowledgeBase(ctx context.Context, input CreateKnowledgeBaseInput) (KnowledgeBase, error)
	ListKnowledgeBases(ctx context.Context, limit, offset int) ([]KnowledgeBase, int, error)
	GetKnowledgeBase(ctx context.Context, id string) (KnowledgeBase, error)
	// UpdateKnowledgeBase/UploadDocument/DeleteDocument all take
	// userID/role for the same resource-level ownership check agent uses:
	// creator or admin only — see CLAUDE.md's permission model note.
	UpdateKnowledgeBase(ctx context.Context, id, userID, role string, input UpdateKnowledgeBaseInput) (KnowledgeBase, error)

	UploadDocument(ctx context.Context, kbID, userID, role, fileName, fileType string, content []byte) (Document, error)
	ListDocuments(ctx context.Context, kbID string, limit, offset int) ([]Document, int, error)
	GetDocument(ctx context.Context, id string) (Document, error)
	DeleteDocument(ctx context.Context, id, userID, role string) error

	// ProcessDocument is invoked by the asynq task handler (see tasks.go),
	// not over HTTP — it's on the interface (rather than a method on the
	// unexported service struct) because wire.go builds the asynq mux from
	// a Service value, same as every other cross-module dependency.
	ProcessDocument(ctx context.Context, documentID string) error

	// Retrieve embeds query against each knowledgeBaseID's configured
	// model (grouped so a query is only embedded once per distinct
	// model), lets pgvector score and rank chunks in-database, and
	// returns the topK highest across all of them.
	Retrieve(ctx context.Context, knowledgeBaseIDs []string, query string, topK int) ([]RetrievedChunk, error)
}

// service is constructed via NewService in wire.go. providerSvc is
// depended on only through its Service interface, per the layering rule.
type service struct {
	repo        *Repository
	providerSvc provider.Service
	asynqClient *asynq.Client
	storageDir  string
}

func (s *service) CreateKnowledgeBase(ctx context.Context, input CreateKnowledgeBaseInput) (KnowledgeBase, error) {
	if err := s.validateEmbeddingModel(ctx, input.EmbeddingModelID); err != nil {
		return KnowledgeBase{}, err
	}

	chunkSize := input.ChunkSize
	if chunkSize <= 0 {
		chunkSize = defaultChunkSize
	}
	chunkOverlap := input.ChunkOverlap
	if chunkOverlap < 0 {
		chunkOverlap = defaultChunkOverlap
	}

	kb := KnowledgeBase{
		ID:               platform.NewID(),
		Name:             input.Name,
		Description:      input.Description,
		EmbeddingModelID: input.EmbeddingModelID,
		ChunkSize:        chunkSize,
		ChunkOverlap:     chunkOverlap,
		IsActive:         true,
		CreatedBy:        input.CreatedBy,
	}
	if err := s.repo.createKnowledgeBase(ctx, kb); err != nil {
		return KnowledgeBase{}, err
	}
	return s.repo.getKnowledgeBase(ctx, kb.ID)
}

func (s *service) ListKnowledgeBases(ctx context.Context, limit, offset int) ([]KnowledgeBase, int, error) {
	limit = platform.ClampLimit(limit)
	kbs, err := s.repo.listKnowledgeBases(ctx, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countKnowledgeBases(ctx)
	if err != nil {
		return nil, 0, err
	}
	// Small admin-facing list (offset-paginated, per CLAUDE.md's pagination
	// rules) — one extra count query per row to populate TotalChunks is an
	// acceptable N+1 here, same tradeoff already made for conversations'
	// last_message preview.
	for i := range kbs {
		count, err := s.repo.countChunksByKnowledgeBase(ctx, kbs[i].ID)
		if err != nil {
			return nil, 0, err
		}
		kbs[i].TotalChunks = count
	}
	return kbs, total, nil
}

func (s *service) GetKnowledgeBase(ctx context.Context, id string) (KnowledgeBase, error) {
	kb, err := s.repo.getKnowledgeBase(ctx, id)
	if err != nil {
		return KnowledgeBase{}, err
	}
	count, err := s.repo.countChunksByKnowledgeBase(ctx, id)
	if err != nil {
		return KnowledgeBase{}, err
	}
	kb.TotalChunks = count
	return kb, nil
}

func (s *service) UpdateKnowledgeBase(ctx context.Context, id, userID, role string, input UpdateKnowledgeBaseInput) (KnowledgeBase, error) {
	existing, err := s.repo.getKnowledgeBase(ctx, id)
	if err != nil {
		return KnowledgeBase{}, err
	}
	if existing.CreatedBy != userID && role != user.RoleAdmin {
		return KnowledgeBase{}, ErrForbidden
	}
	existing.Name = input.Name
	existing.Description = input.Description
	existing.IsActive = input.IsActive

	if err := s.repo.updateKnowledgeBase(ctx, existing); err != nil {
		return KnowledgeBase{}, err
	}
	return s.repo.getKnowledgeBase(ctx, id)
}

func (s *service) validateEmbeddingModel(ctx context.Context, modelID string) error {
	m, err := s.providerSvc.GetModel(ctx, modelID)
	if err != nil {
		return err
	}
	if m.Capability != provider.CapabilityEmbedding || !m.IsActive {
		return ErrInvalidEmbeddingModel
	}
	return nil
}

func (s *service) UploadDocument(ctx context.Context, kbID, userID, role, fileName, fileType string, content []byte) (Document, error) {
	kb, err := s.repo.getKnowledgeBase(ctx, kbID)
	if err != nil {
		return Document{}, err
	}
	if kb.CreatedBy != userID && role != user.RoleAdmin {
		return Document{}, ErrForbidden
	}
	if fileType != FileTypeTxt && fileType != FileTypeMD && fileType != FileTypePDF {
		return Document{}, ErrUnsupportedFileType
	}
	if len(content) > maxFileSizeBytes {
		return Document{}, ErrFileTooLarge
	}

	docID := platform.NewID()
	storagePath, err := s.saveFile(kbID, docID, fileName, content)
	if err != nil {
		return Document{}, fmt.Errorf("knowledge: save uploaded file: %w", err)
	}

	doc := Document{
		ID:              docID,
		KnowledgeBaseID: kbID,
		FileName:        fileName,
		FileType:        fileType,
		FileSize:        len(content),
		StoragePath:     storagePath,
		Status:          StatusPending,
		CreatedBy:       userID,
	}
	if err := s.repo.createDocument(ctx, doc); err != nil {
		return Document{}, err
	}

	if err := s.enqueueProcessDocument(ctx, docID); err != nil {
		// The document row exists as "pending" with no job in flight —
		// not silently lost (visible in the UI as stuck), but also not
		// masked as success.
		return Document{}, fmt.Errorf("knowledge: enqueue processing job: %w", err)
	}

	return s.repo.getDocument(ctx, docID)
}

func (s *service) saveFile(kbID, docID, fileName string, content []byte) (string, error) {
	dir := filepath.Join(s.storageDir, kbID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	path := filepath.Join(dir, docID+"_"+filepath.Base(fileName))
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}
	return path, nil
}

func (s *service) enqueueProcessDocument(ctx context.Context, documentID string) error {
	task, err := newProcessDocumentTask(documentID)
	if err != nil {
		return err
	}
	_, err = s.asynqClient.EnqueueContext(ctx, task, asynq.MaxRetry(0))
	return err
}

func (s *service) ListDocuments(ctx context.Context, kbID string, limit, offset int) ([]Document, int, error) {
	limit = platform.ClampLimit(limit)
	docs, err := s.repo.listDocumentsByKnowledgeBase(ctx, kbID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.countDocumentsByKnowledgeBase(ctx, kbID)
	if err != nil {
		return nil, 0, err
	}
	return docs, total, nil
}

func (s *service) GetDocument(ctx context.Context, id string) (Document, error) {
	return s.repo.getDocument(ctx, id)
}

func (s *service) DeleteDocument(ctx context.Context, id, userID, role string) error {
	doc, err := s.repo.getDocument(ctx, id)
	if err != nil {
		return err
	}
	// Ownership is checked against the parent knowledge base, not the
	// document's own created_by — the same rule UploadDocument enforces,
	// since "who can modify this KB's contents" is a property of the KB.
	kb, err := s.repo.getKnowledgeBase(ctx, doc.KnowledgeBaseID)
	if err != nil {
		return err
	}
	if kb.CreatedBy != userID && role != user.RoleAdmin {
		return ErrForbidden
	}
	if err := s.repo.deleteDocument(ctx, id); err != nil {
		return err
	}
	// Best-effort: an orphaned file on disk is a cleanup nuisance, not a
	// correctness problem, so its error isn't propagated to the caller.
	if err := os.Remove(doc.StoragePath); err != nil {
		slog.Warn("knowledge: failed to remove document file", "err", err, "document_id", id, "path", doc.StoragePath)
	}
	return nil
}

// ProcessDocument parses, chunks, embeds, and stores one document — see
// tasks.go for how the asynq worker invokes this. Any failure updates the
// document to status=failed with a message instead of leaving it stuck at
// pending; per the plan, there's no automatic retry (MaxRetry(0) at
// enqueue time already enforces that), reprocessing is a manual re-upload.
func (s *service) ProcessDocument(ctx context.Context, documentID string) error {
	doc, err := s.repo.getDocument(ctx, documentID)
	if err != nil {
		return err
	}
	if err := s.repo.updateDocumentStatus(ctx, documentID, StatusProcessing, "", 0); err != nil {
		return err
	}

	kb, err := s.repo.getKnowledgeBase(ctx, doc.KnowledgeBaseID)
	if err != nil {
		return s.failDocument(ctx, documentID, err)
	}

	text, err := parseFile(doc.StoragePath, doc.FileType)
	if err != nil {
		return s.failDocument(ctx, documentID, err)
	}

	pieces := chunkText(text, kb.ChunkSize, kb.ChunkOverlap)
	if len(pieces) == 0 {
		return s.failDocument(ctx, documentID, fmt.Errorf("文档内容为空或无法提取到文本"))
	}

	model, err := s.providerSvc.GetModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		return s.failDocument(ctx, documentID, err)
	}
	client, err := s.providerSvc.ResolveClient(ctx, model.ProviderID)
	if err != nil {
		return s.failDocument(ctx, documentID, err)
	}

	result, err := client.Embed(ctx, provider.EmbedRequest{Model: model.ModelName, Input: pieces})
	if err != nil {
		return s.failDocument(ctx, documentID, provider.WrapClientError(err))
	}
	if len(result.Embeddings) != len(pieces) {
		return s.failDocument(ctx, documentID, fmt.Errorf("向量生成数量（%d）与分块数量（%d）不一致", len(result.Embeddings), len(pieces)))
	}

	chunks := make([]Chunk, 0, len(pieces))
	for i, piece := range pieces {
		chunks = append(chunks, Chunk{
			ID:                 platform.NewID(),
			KnowledgeBaseID:    doc.KnowledgeBaseID,
			DocumentID:         documentID,
			ChunkIndex:         i,
			Content:            piece,
			ContentLength:      len([]rune(piece)),
			Embedding:          result.Embeddings[i],
			EmbeddingDimension: result.Dimension,
		})
	}
	if err := s.repo.createChunks(ctx, chunks); err != nil {
		return s.failDocument(ctx, documentID, err)
	}

	return s.repo.updateDocumentStatus(ctx, documentID, StatusReady, "", len(pieces))
}

func (s *service) failDocument(ctx context.Context, documentID string, cause error) error {
	msg := cause.Error()
	if updateErr := s.repo.updateDocumentStatus(ctx, documentID, StatusFailed, msg, 0); updateErr != nil {
		slog.Error("knowledge: failed to record document failure", "err", updateErr, "document_id", documentID, "cause", cause)
	}
	return cause
}

func (s *service) Retrieve(ctx context.Context, knowledgeBaseIDs []string, query string, topK int) ([]RetrievedChunk, error) {
	if len(knowledgeBaseIDs) == 0 || query == "" {
		return nil, nil
	}

	kbsByModel := make(map[string][]KnowledgeBase)
	for _, id := range knowledgeBaseIDs {
		kb, err := s.repo.getKnowledgeBase(ctx, id)
		if err != nil {
			continue // a deleted/bad ID shouldn't fail the whole conversation turn
		}
		if !kb.IsActive {
			continue
		}
		kbsByModel[kb.EmbeddingModelID] = append(kbsByModel[kb.EmbeddingModelID], kb)
	}

	var candidates []RetrievedChunk
	for modelID, kbs := range kbsByModel {
		queryVector, err := s.embedQuery(ctx, modelID, query)
		if err != nil {
			slog.Warn("knowledge: query embedding failed, skipping these knowledge bases", "err", err, "embedding_model_id", modelID)
			continue
		}
		kbIDs := make([]string, 0, len(kbs))
		for _, kb := range kbs {
			kbIDs = append(kbIDs, kb.ID)
		}
		// Scoring, ranking, and topK all happen inside pgvector now. The
		// old in-loop dimension guard lives on as the SQL dimension filter
		// (searchChunks derives it from len(queryVector)).
		chunks, err := s.repo.searchChunks(ctx, kbIDs, queryVector, topK)
		if err != nil {
			slog.Warn("knowledge: chunk search failed, skipping these knowledge bases", "err", err, "embedding_model_id", modelID)
			continue
		}
		candidates = append(candidates, chunks...)
	}

	// Each model group already returns its own topK ranked by score; the
	// global re-sort + truncate across groups preserves the exact cross-KB
	// global-topK semantics the single-process implementation had.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	if len(candidates) > topK {
		candidates = candidates[:topK]
	}
	return candidates, nil
}

func (s *service) embedQuery(ctx context.Context, modelID, query string) ([]float32, error) {
	model, err := s.providerSvc.GetModel(ctx, modelID)
	if err != nil {
		return nil, err
	}
	client, err := s.providerSvc.ResolveClient(ctx, model.ProviderID)
	if err != nil {
		return nil, err
	}
	result, err := client.Embed(ctx, provider.EmbedRequest{Model: model.ModelName, Input: []string{query}})
	if err != nil {
		return nil, provider.WrapClientError(err)
	}
	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("knowledge: embedding provider returned no vectors")
	}
	return result.Embeddings[0], nil
}
