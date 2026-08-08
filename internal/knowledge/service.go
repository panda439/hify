package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hibiken/asynq"

	"hify/internal/platform"
	"hify/internal/platform/apperr"
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

	// RetryDocument re-queues a pending/failed document for processing
	// under a newly advanced version — see the Document doc comment for
	// why this is a version bump, not a plain re-run. Any other status
	// returns ErrDocumentNotRetryable.
	RetryDocument(ctx context.Context, id, userID, role string) (Document, error)

	// ProcessDocument is invoked by the asynq task handler (see tasks.go),
	// not over HTTP — it's on the interface (rather than a method on the
	// unexported service struct) because wire.go builds the asynq mux from
	// a Service value, same as every other cross-module dependency.
	// version identifies which processing attempt this task instance is
	// authorized to carry out (see Document.Version) — a version mismatch
	// against the document's current row means this task has been
	// superseded and it exits without doing any work.
	ProcessDocument(ctx context.Context, documentID string, version int64) error

	// ReconcileStuckDocuments is invoked periodically by the asynq
	// scheduler (see tasks.go/main.go), not over HTTP, for the same
	// reason ProcessDocument is on the interface. It reclaims documents
	// stuck in processing (presumed-dead worker) or pending (presumed
	// lost enqueue) beyond their staleness thresholds and re-queues them.
	// Returns how many documents it reclaimed, for the caller's log line.
	ReconcileStuckDocuments(ctx context.Context) (int, error)

	// Retrieve embeds query against each knowledgeBaseID's configured
	// model (grouped so a query is only embedded once per distinct
	// model), lets pgvector score and rank chunks in-database, and fuses
	// vector + keyword results (Hybrid Search, hybrid.go) into topK core
	// hits. topK is silently clamped to maxTopK — see model.go.
	//
	// Phase 5 (dedup.go): topK counts DISTINCT-CONTENT core hits, not raw
	// fused rows — rrfFuse content-dedups its full candidate pool BEFORE
	// truncating to topK, so an exact-duplicate-content candidate never
	// consumes a topK slot a uniquely-worded lower-ranked candidate could
	// have filled instead. Neighbor-window expansion (below) only ever
	// runs against these already content-deduped anchors, never against a
	// duplicate that content-dedup already eliminated — a core chunk that
	// lost to content dedup gets no neighbor lookup at all, so its
	// neighbors can never appear in the result either.
	//
	// Phase 4 (neighbor.go): the returned slice is NOT guaranteed to have
	// at most topK elements anymore. topK still governs exactly one thing
	// — how many DISTINCT-CONTENT core Hybrid Search hits ("anchors") are
	// chosen — but every anchor may now be followed, AFTER every other
	// anchor, by up to two neighbor chunks each (its document's
	// chunk_index-1 and chunk_index+1, same document AND same
	// document_version, is_published only) pulled in purely to give the
	// model surrounding context an isolated chunk might be missing. The
	// layout is two full tiers, not per-anchor interleaving: ALL topK
	// anchors first in full rank order, THEN every anchor's own neighbor
	// chunks — see expandWithNeighbors' doc comment in neighbor.go for
	// exactly why (a review fix: an interleaved
	// anchor-then-its-neighbors-then-next-anchor layout let a low-relevance
	// neighbor chunk consume budget a lower-ranked but still-core anchor
	// needed, in conversation/budget.go's strictly-input-order greedy
	// selectEvidence). expandWithNeighbors then runs a second content-dedup
	// pass over that whole two-tier layout (Phase 5): a neighbor chunk
	// whose content exactly duplicates any anchor's, or an earlier
	// neighbor's, is dropped, never the anchor or the earlier neighbor —
	// see that function's doc comment rule 7. The hard ceiling is
	// len(result) <= topK*3, never topK itself, and content dedup only
	// ever shrinks the result further — see
	// expandWithNeighborWindow/expandWithNeighbors' doc comments for the
	// exact dedup rules, and RetrievedChunk.NeighborOf for how a caller
	// tells an anchor from a neighbor. Callers that assumed
	// len(Retrieve(...)) <= topK before Phase 4 must not keep assuming it.
	Retrieve(ctx context.Context, knowledgeBaseIDs []string, query string, topK int) ([]RetrievedChunk, error)
}

// service is constructed via NewService in wire.go. providerSvc is
// depended on only through its Service interface, per the layering rule.
type service struct {
	repo        *Repository
	providerSvc provider.Service
	asynqClient *asynq.Client
	storageDir  string

	// findNeighborBatch is expandWithNeighborWindow's only way to reach
	// the database — deliberately a method-value field, not a direct
	// s.repo.findPublishedNeighborChunksBatch(...) call in the method
	// body, and deliberately not a change to *Repository's field type
	// into some broader repository interface. This is the "minimal
	// internal interface/pure orchestration helper" Phase 7's task spec
	// asked for so its "normal path calls the batch query exactly once"
	// requirement has automated proof: neighbor_batch_test.go constructs a
	// bare &service{findNeighborBatch: spy} (same package, unexported
	// field, no exported API surface added to Service) and counts spy
	// calls directly — no mocking framework, no interface satisfied by a
	// hand-rolled fake repository. NewService sets this to
	// repo.findPublishedNeighborChunksBatch so production code is
	// unaffected; every other service method still reaches the database
	// through s.repo exactly as before.
	findNeighborBatch func(ctx context.Context, requests []neighborRequest) ([]RetrievedChunk, error)
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

	// version starts at 1, matching the DB column's default — this task
	// instance is authorized to process exactly this (as yet unclaimed)
	// attempt.
	if err := s.enqueueProcessDocument(ctx, docID, 1); err != nil {
		// The document row exists as "pending" with no job in flight —
		// not silently lost (ReconcileStuckDocuments will pick it back up
		// once stalePendingThreshold elapses, and it's visible in the UI
		// as stuck in the meantime), but also not masked as success.
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

func (s *service) enqueueProcessDocument(ctx context.Context, documentID string, version int64) error {
	task, err := newProcessDocumentTask(documentID, version)
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

// newLeaseDeadline is the heartbeat lease every claim/renewal/publishing
// transition sets — see model.go's leaseDuration for why 3 minutes.
func newLeaseDeadline() time.Time {
	return time.Now().UTC().Add(leaseDuration)
}

// validateEmbedBatch enforces dimension consistency across a document's
// embed batches before any of a batch's result is trusted enough to reach
// PG. Trusting result.Dimension blindly (the old behavior: overwrite a
// package-level variable with the last batch's value and stamp every
// chunk with it) lets a provider bug — a batch reporting a different
// dimension than an earlier one, a non-positive dimension, or a vector
// whose actual length disagrees with the batch's own declared dimension —
// write a wrong dimension tag into PG, where it would only surface much
// later as a pgvector query failure instead of being caught here.
//
// expectedDimension is 0 on the first batch (nothing to compare against
// yet, but the batch must still be internally consistent and positive);
// on every later batch it's the dimension the first batch established.
func validateEmbedBatch(result provider.EmbedResult, batchSize, expectedDimension int) error {
	if len(result.Embeddings) != batchSize {
		return ErrEmbeddingCountMismatch
	}
	if result.Dimension <= 0 {
		return ErrEmbeddingDimensionMismatch
	}
	if expectedDimension > 0 && result.Dimension != expectedDimension {
		return ErrEmbeddingDimensionMismatch
	}
	for _, vec := range result.Embeddings {
		if len(vec) != result.Dimension {
			return ErrEmbeddingDimensionMismatch
		}
	}
	return nil
}

// ProcessDocument parses, chunks, embeds, and stores one document — see
// tasks.go for how the asynq worker invokes this. It is idempotent and
// safe under concurrent/duplicate/stale delivery:
//
//  1. document not found (deleted mid-flight) -> no-op.
//  2. doc.Version != version (this attempt has been superseded by a
//     retry or a reconciliation reclaim) -> no-op.
//  3. doc.Status == ready (duplicate delivery of an already-completed
//     attempt) -> no-op.
//  4. CAS-claim pending/failed -> processing, seeding a heartbeat lease.
//     0 rows affected means a concurrent duplicate lost the race, or
//     status/version already moved on -> no-op either way.
//  5. parse -> chunk (hard-capped by maxChunksPerDocument) -> embed in
//     batches of at most embedBatchSize, validating every batch's
//     dimension against the first batch's (see validateEmbedBatch) and
//     renewing the lease after every batch. Any failure here happens
//     before any PG write, so a partial batch failure can never result in
//     a partial publish. A renewal failure means this worker has been
//     fenced out (reconciliation reclaimed it) and must stop immediately.
//  6. write the new chunks to PG unpublished, tagged with this version —
//     "new version write", untouched by anything published under an
//     older version.
//  7. CAS processing -> publishing, guarded by version, with a fresh
//     lease. This locks in "embedding is done, only the publish is left"
//     so a stuck recovery never has to redo the (expensive) embedding.
//     0 rows affected means reconciliation already reclaimed this
//     document out from under us — the freshly written chunks are
//     deleted (best-effort) and never published.
//  8. publishAndComplete: publish this version (idempotent PG delete
//     obsolete + mark published) and, only once that has actually
//     succeeded, CAS publishing -> ready. A publish failure is returned
//     to the caller, not swallowed — the document stays in publishing for
//     ReconcileStuckDocuments to recover via the same idempotent path.
//
// A pre-publish failure path is the mirror CAS (processing -> failed);
// per the plan, there's no automatic asynq retry (MaxRetry(0) at enqueue
// time), recovery is RetryDocument or ReconcileStuckDocuments.
func (s *service) ProcessDocument(ctx context.Context, documentID string, version int64) error {
	doc, err := s.repo.getDocument(ctx, documentID)
	if errors.Is(err, ErrDocumentNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if doc.Version != version {
		return nil
	}
	if doc.Status == StatusReady {
		return nil
	}

	claimed, err := s.repo.claimDocumentProcessing(ctx, documentID, version, newLeaseDeadline())
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	kb, err := s.repo.getKnowledgeBase(ctx, doc.KnowledgeBaseID)
	if err != nil {
		return s.failDocument(ctx, documentID, version, err)
	}

	parsed, err := parseFile(doc.StoragePath, doc.FileType)
	if err != nil {
		return s.failDocument(ctx, documentID, version, err)
	}

	// Structure-aware chunking (chunkDocument dispatches by file type —
	// see chunk.go): markdown keeps headings/paragraphs/lists/code
	// blocks/tables intact where possible, txt splits on
	// paragraph/sentence boundaries, pdf chunks per page so PageNumber is
	// never attributed across a page boundary. Every path still falls
	// back to the fixed-length chunkText for a single structural unit
	// too large to fit in kb.ChunkSize on its own.
	pieces := chunkDocument(doc.FileType, parsed, kb.ChunkSize, kb.ChunkOverlap)
	if len(pieces) == 0 {
		return s.failDocument(ctx, documentID, version, ErrEmptyContent)
	}
	if len(pieces) > maxChunksPerDocument {
		return s.failDocument(ctx, documentID, version, ErrTooManyChunks)
	}
	contents := make([]string, len(pieces))
	for i, piece := range pieces {
		contents[i] = piece.Content
	}

	model, err := s.providerSvc.GetModel(ctx, kb.EmbeddingModelID)
	if err != nil {
		return s.failDocument(ctx, documentID, version, err)
	}
	client, err := s.providerSvc.ResolveClient(ctx, model.ProviderID)
	if err != nil {
		return s.failDocument(ctx, documentID, version, err)
	}

	// Batched Embed calls — provider.Client already retries/rate-limits/
	// circuit-breaks a single call (see provider/resilience.go); batching
	// here only bounds one call's payload size, it doesn't re-implement
	// that resilience. Every batch must succeed and pass
	// validateEmbedBatch before anything is written to PG, so a bad batch
	// (network failure, count mismatch, dimension mismatch) never results
	// in a partial or mis-tagged publish.
	embeddings := make([][]float32, 0, len(pieces))
	var expectedDimension int
	for batchIndex, batch := range batchStrings(contents, embedBatchSize) {
		result, err := client.Embed(ctx, provider.EmbedRequest{Model: model.ModelName, Input: batch})
		if err != nil {
			return s.failDocument(ctx, documentID, version, provider.WrapClientError(err))
		}
		if verr := validateEmbedBatch(result, len(batch), expectedDimension); verr != nil {
			slog.Error("knowledge: embedding batch validation failed",
				"err", verr, "batch_index", batchIndex, "expected_dimension", expectedDimension,
				"actual_dimension", result.Dimension, "document_id", documentID)
			return s.failDocument(ctx, documentID, version, verr)
		}
		if expectedDimension == 0 {
			expectedDimension = result.Dimension
		}
		embeddings = append(embeddings, result.Embeddings...)

		// Heartbeat: renew after every batch so a slow-but-alive worker is
		// never mistaken for dead — see leaseDuration. A renewal failure
		// means reconciliation already reclaimed this document; stop
		// immediately, do not start another batch, do not write chunks.
		renewed, err := s.repo.renewDocumentLease(ctx, documentID, version, StatusProcessing, newLeaseDeadline())
		if err != nil {
			return err
		}
		if !renewed {
			slog.Info("knowledge: lease renewal fenced mid-embedding, stopping stale worker", "document_id", documentID, "version", version, "batch_index", batchIndex)
			return nil
		}
	}

	// DocumentName is the Citation V1 source-attribution snapshot (see
	// Chunk's doc comment) — doc.FileName at processing time, not a live
	// pointer back to the documents row. PageNumber/SectionTitle come
	// straight from chunkDocument's per-piece metadata — nil wherever the
	// source format/parser genuinely can't support it (txt never sets
	// either; pdf never sets SectionTitle; md never sets PageNumber), never
	// fabricated.
	chunks := make([]Chunk, 0, len(pieces))
	for i, piece := range pieces {
		chunks = append(chunks, Chunk{
			ID:                 platform.NewID(),
			KnowledgeBaseID:    doc.KnowledgeBaseID,
			DocumentID:         documentID,
			DocumentName:       doc.FileName,
			ChunkIndex:         i,
			Content:            piece.Content,
			ContentLength:      len([]rune(piece.Content)),
			Embedding:          embeddings[i],
			EmbeddingDimension: expectedDimension,
			PageNumber:         piece.PageNumber,
			SectionTitle:       piece.SectionTitle,
		})
	}
	if err := s.repo.createChunks(ctx, chunks, version); err != nil {
		return s.failDocument(ctx, documentID, version, err)
	}

	publishing, err := s.repo.markDocumentPublishing(ctx, documentID, version, newLeaseDeadline())
	if err != nil {
		return s.failDocument(ctx, documentID, version, err)
	}
	if !publishing {
		// Superseded between claiming and finishing embedding — this
		// attempt lost, must not publish. The rows just written are
		// unpublished and thus already invisible to search; delete them
		// so they don't linger.
		if delErr := s.repo.deleteChunksByDocumentVersion(ctx, documentID, version); delErr != nil {
			slog.Error("knowledge: failed to clean up superseded chunk version", "err", delErr, "document_id", documentID, "version", version)
		}
		return nil
	}

	// One more renewal right before the publish attempt — covers
	// publishWithRetry's own execution window.
	renewed, err := s.repo.renewDocumentLease(ctx, documentID, version, StatusPublishing, newLeaseDeadline())
	if err != nil {
		return err
	}
	if !renewed {
		slog.Info("knowledge: lease renewal fenced before publish, stopping stale worker", "document_id", documentID, "version", version)
		return nil
	}

	if err := s.publishAndComplete(ctx, documentID, version); err != nil {
		// Per the plan: a publish failure must NOT be swallowed as
		// success. The document stays "publishing" — that's what makes
		// ReconcileStuckDocuments's idempotent republish path the
		// recovery mechanism instead of silently losing the work.
		return fmt.Errorf("knowledge: publish chunk version: %w", err)
	}
	return nil
}

// publishWithRetry retries the local, fast PG publish transaction a few
// times before giving up — it's not calling out to any external service,
// so a short fixed backoff is enough headroom for a transient connection
// hiccup without masking a persistent problem. The caller (publishAndComplete)
// is responsible for propagating a final failure rather than swallowing it.
func (s *service) publishWithRetry(ctx context.Context, documentID string, version int64) error {
	const maxAttempts = 3
	backoff := 100 * time.Millisecond
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = s.repo.publishDocumentVersion(ctx, documentID, version)
		if lastErr == nil {
			return nil
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return lastErr
}

// publishAndComplete is the one piece of recovery logic shared by
// ProcessDocument's own attempt and ReconcileStuckDocuments's recovery of
// a stuck-in-publishing document — both cases reduce to "publish is
// idempotent, safe to (re)run; only flip to ready once it has actually
// succeeded". It never swallows a publish failure as success: the caller
// gets the error back and the document stays in publishing.
//
// chunk_count is queried fresh from PG rather than threaded through as a
// parameter — ReconcileStuckDocuments runs in a different call than the
// one that originally computed len(pieces), so PG's actual published row
// count is the only value both callers can rely on.
func (s *service) publishAndComplete(ctx context.Context, documentID string, version int64) error {
	if err := s.publishWithRetry(ctx, documentID, version); err != nil {
		return err
	}
	chunkCount, err := s.repo.countChunksByDocumentVersion(ctx, documentID, version)
	if err != nil {
		return err
	}
	ready, err := s.repo.markDocumentReady(ctx, documentID, version, chunkCount)
	if err != nil {
		return err
	}
	if !ready {
		// Someone else (the original worker, or another reconciliation
		// pass) already completed this exact transition — a benign
		// idempotent race, not an error.
		slog.Info("knowledge: publishing->ready CAS already completed by another runner", "document_id", documentID, "version", version)
	}
	return nil
}

// failDocument is ProcessDocument's failure-path CAS (processing ->
// failed), mirroring markDocumentReady's publish gate. ok=false means this
// attempt was superseded before it could even record its own failure —
// the newer attempt owns the document's state now, so this isn't reported
// as a task failure.
func (s *service) failDocument(ctx context.Context, documentID string, version int64, cause error) error {
	msg := userFacingFailureMessage(cause)
	ok, err := s.repo.markDocumentFailed(ctx, documentID, version, msg)
	if err != nil {
		slog.Error("knowledge: failed to record document failure", "err", err, "document_id", documentID, "cause", cause)
		return cause
	}
	if !ok {
		slog.Info("knowledge: document processing attempt superseded before failure could be recorded", "document_id", documentID, "version", version)
		return nil
	}
	return cause
}

// RetryDocument re-queues a pending/failed document, advancing its
// version so any lingering task instance from the superseded attempt is
// fenced out by ProcessDocument's version check.
func (s *service) RetryDocument(ctx context.Context, id, userID, role string) (Document, error) {
	doc, err := s.repo.getDocument(ctx, id)
	if err != nil {
		return Document{}, err
	}
	kb, err := s.repo.getKnowledgeBase(ctx, doc.KnowledgeBaseID)
	if err != nil {
		return Document{}, err
	}
	if kb.CreatedBy != userID && role != user.RoleAdmin {
		return Document{}, ErrForbidden
	}

	newVersion := doc.Version + 1
	ok, err := s.repo.reclaimDocumentForRetry(ctx, id, doc.Version, newVersion)
	if err != nil {
		return Document{}, err
	}
	if !ok {
		return Document{}, ErrDocumentNotRetryable
	}

	if err := s.enqueueProcessDocument(ctx, id, newVersion); err != nil {
		return Document{}, fmt.Errorf("knowledge: enqueue retry processing job: %w", err)
	}
	return s.repo.getDocument(ctx, id)
}

// ReconcileStuckDocuments is the minimal, no-new-infrastructure recovery
// path from the plan: a periodic scan (see tasks.go/main.go) rather than
// a dedicated queue/lock service. It reclaims three kinds of stuck
// documents; ProcessDocument's own idempotency (version fencing, the
// claim CAS) makes it safe to re-enqueue a document that might actually
// still be in flight — worst case is a wasted no-op task, never a
// duplicate publish.
func (s *service) ReconcileStuckDocuments(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	reclaimed := 0

	// processing lease expired (see model.go's leaseDuration): worker
	// presumed dead, and nothing durable was written for this attempt
	// yet, so reclaim with a version bump — fences the original worker
	// out at its own next renewal/publish/fail CAS — and re-queue a
	// from-scratch attempt under the new version. now (not a freshly
	// computed timestamp) is passed as expiredBefore so the CAS
	// re-validates the exact same "still expired" fact the scan above
	// just observed — see reclaimStaleProcessingDocument's doc comment
	// for the TOCTOU this closes: an active worker renewing between the
	// scan and this CAS must make the CAS see 0 rows affected, not
	// silently reclaim a live document.
	stuckProcessing, err := s.repo.listLeaseExpiredProcessingDocuments(ctx, now)
	if err != nil {
		return reclaimed, err
	}
	for _, doc := range stuckProcessing {
		newVersion := doc.Version + 1
		ok, err := s.repo.reclaimStaleProcessingDocument(ctx, doc.ID, doc.Version, newVersion, now)
		if err != nil {
			slog.Error("knowledge: failed to reclaim stale processing document", "err", err, "document_id", doc.ID)
			continue
		}
		if !ok {
			continue // renewed, or already moved on, since the scan — not ours to reclaim
		}
		if err := s.enqueueProcessDocument(ctx, doc.ID, newVersion); err != nil {
			slog.Error("knowledge: failed to re-enqueue reclaimed document", "err", err, "document_id", doc.ID)
			continue
		}
		reclaimed++
	}

	// publishing lease expired: the embedding work is already done and
	// safely written (unpublished) to PG — no need to redo it or bump the
	// version, just retry the idempotent publish via publishAndComplete.
	// This is what recovers both fault windows from the plan: "PG publish
	// failed" and "worker crashed after PG publish succeeded but before
	// the MySQL ready CAS" — publishDocumentVersion is idempotent either
	// way, so the same recovery call handles both.
	//
	// PG idempotency is not a substitute for claiming recovery rights
	// first: claimExpiredPublishingRecovery must win (same TOCTOU-closing
	// shape as the processing CAS above, using the same now) before
	// publishAndComplete is ever called, otherwise a concurrently
	// renewing worker — or another Hify instance's reconciliation pass —
	// could act on the same document at the same time.
	stuckPublishing, err := s.repo.listLeaseExpiredPublishingDocuments(ctx, now)
	if err != nil {
		return reclaimed, err
	}
	for _, doc := range stuckPublishing {
		claimed, err := s.repo.claimExpiredPublishingRecovery(ctx, doc.ID, doc.Version, now, newLeaseDeadline())
		if err != nil {
			slog.Error("knowledge: failed to claim publishing recovery", "err", err, "document_id", doc.ID)
			continue
		}
		if !claimed {
			continue // renewed, published, or claimed by another recovery attempt since the scan
		}
		if err := s.publishAndComplete(ctx, doc.ID, doc.Version); err != nil {
			// Publish failed again: the document stays in publishing under
			// the fresh recovery lease just claimed above, not eligible for
			// another recovery attempt until that lease itself expires.
			slog.Error("knowledge: failed to recover stuck publishing document", "err", err, "document_id", doc.ID, "version", doc.Version)
			continue
		}
		reclaimed++
	}

	// pending with no worker ever having claimed it: nothing to fence, so
	// just re-queue under the same version — ProcessDocument's claim CAS
	// naturally dedupes if the original enqueue also eventually arrives.
	stuckPending, err := s.repo.listStalePendingDocuments(ctx, now.Add(-stalePendingThreshold))
	if err != nil {
		return reclaimed, err
	}
	for _, doc := range stuckPending {
		if err := s.enqueueProcessDocument(ctx, doc.ID, doc.Version); err != nil {
			slog.Error("knowledge: failed to re-enqueue stale pending document", "err", err, "document_id", doc.ID)
			continue
		}
		reclaimed++
	}

	return reclaimed, nil
}

// userFacingFailureMessage is what documentResponse.ErrorMessage shows the
// user. cause is often a plain fmt.Errorf chain from parseFile/chunking/DB
// code (English, and for a DB failure potentially raw driver text) rather
// than an *apperr.AppError — those must not reach the client verbatim, per
// CLAUDE.md's "用户可见 Message 必须是中文" rule. Only an already-Chinese
// AppError.Message is safe to surface as-is; anything else falls back to a
// generic message while the real cause goes to the log (same
// don't-leak-internals shape as httperr.WriteInternal uses for the
// synchronous request path).
func userFacingFailureMessage(cause error) string {
	var ae *apperr.AppError
	if errors.As(cause, &ae) {
		return ae.Message
	}
	slog.Error("knowledge: document processing failed with an internal error", "err", cause)
	return "文档处理失败，请检查文件内容或稍后重试"
}

// classifyRetrieveErr distinguishes "this ctx/deadline is actually done —
// propagate immediately" from "some best-effort sub-call failed for its
// own reasons — log and move on". Retrieve's failure-isolation contract
// (skip a bad KB ID, skip a model whose embedding call failed, skip a
// search path that errored) only applies to the latter; a cancelled or
// timed-out context must never be swallowed as just another skippable
// error, or the caller (conversation's turn handler) would have no way to
// tell "no results" apart from "the request was actually aborted".
// ctx.Err() is checked directly (not just errors.Is against err) because
// not every wrapped error in this call chain is guaranteed to preserve
// context.Canceled/DeadlineExceeded through %w — e.g. provider.Client's
// own error wrapping around an HTTP call. ctx.Err() is the one place that
// can never lie about whether the context is actually done.
func classifyRetrieveErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// Retrieve runs Hybrid Search: a per-embedding-model vector path (pgvector
// cosine similarity, unchanged in spirit from before Phase 3) and a single
// embedding-independent keyword path (pg_trgm word-similarity — see
// repository.go's searchKeywordChunks), each fetching up to candidateK(topK)
// rows, fused by Reciprocal Rank Fusion (hybrid.go's rrfFuse) into
// ID-deduped, content-deduped (Phase 5, dedup.go), globally-ranked topK core
// hits ("anchors") — then, since Phase 4, best-effort expanded with each
// anchor's immediate document-order neighbors
// (expandWithNeighborWindow/neighbor.go), followed by a second Phase 5
// content-dedup pass over that whole two-tier result, before returning.
// topK still bounds DISTINCT-CONTENT anchor count exactly as it always
// bounded anchor count; it does not bound the final returned slice length
// anymore — see the Service interface doc comment above and
// RetrievedChunk.NeighborOf.
//
// Failure isolation is best-effort at every sub-call, same convention the
// pre-Hybrid-Search version already had for a bad knowledge base ID or a
// failed embedding call — extended in Phase 3 to the two search paths
// themselves (a failed vector search for one embedding model only drops
// that model's candidates; a failed keyword search only drops the keyword
// candidates) and in Phase 4 to neighbor expansion (a failed neighbor
// lookup for one document version only drops that group's neighbors,
// never an anchor — see expandWithNeighborWindow). Every one of these
// failing, or all of them returning nothing, is not an error — it's an
// empty (or anchors-only) result, same as the pre-Hybrid-Search contract.
// The one thing that is never treated as best-effort anywhere in this
// call chain is the context itself being cancelled or timed out — see
// classifyRetrieveErr.
func (s *service) Retrieve(ctx context.Context, knowledgeBaseIDs []string, query string, topK int) ([]RetrievedChunk, error) {
	if len(knowledgeBaseIDs) == 0 || query == "" {
		return nil, nil
	}
	topK = clampTopK(topK)
	cK := candidateK(topK)

	kbsByModel := make(map[string][]KnowledgeBase)
	var activeKBIDs []string
	for _, id := range knowledgeBaseIDs {
		kb, err := s.repo.getKnowledgeBase(ctx, id)
		if err != nil {
			if cerr := classifyRetrieveErr(ctx, err); cerr != nil {
				return nil, cerr
			}
			continue // a deleted/bad ID shouldn't fail the whole conversation turn
		}
		if !kb.IsActive {
			continue
		}
		kbsByModel[kb.EmbeddingModelID] = append(kbsByModel[kb.EmbeddingModelID], kb)
		activeKBIDs = append(activeKBIDs, kb.ID)
	}

	// --- vector path: per embedding-model group, same grouping rationale
	// as before Phase 3 — a query is only embedded once per distinct
	// model, and pgvector's <=> can't compare vectors of different
	// dimensions anyway (see searchVectorChunks's dimension filter).
	var vectorCandidates []RetrievedChunk
	for modelID, kbs := range kbsByModel {
		queryVector, err := s.embedQuery(ctx, modelID, query)
		if err != nil {
			if cerr := classifyRetrieveErr(ctx, err); cerr != nil {
				return nil, cerr
			}
			// Best-effort: this model's knowledge bases contribute no
			// vector candidates, but the keyword path below doesn't
			// depend on embeddings at all and still runs — this is what
			// keeps Hybrid Search partially available when the embedding
			// provider is down.
			slog.Warn("knowledge: query embedding failed, skipping these knowledge bases for vector search", "err", err, "embedding_model_id", modelID)
			continue
		}
		kbIDs := make([]string, 0, len(kbs))
		for _, kb := range kbs {
			kbIDs = append(kbIDs, kb.ID)
		}
		chunks, err := s.repo.searchVectorChunks(ctx, kbIDs, queryVector, cK)
		if err != nil {
			if cerr := classifyRetrieveErr(ctx, err); cerr != nil {
				return nil, cerr
			}
			slog.Warn("knowledge: vector chunk search failed, skipping these knowledge bases", "err", err, "embedding_model_id", modelID)
			continue
		}
		vectorCandidates = append(vectorCandidates, chunks...)
	}
	// Each model group already returns its own candidateK ranked by
	// score; re-sort + truncate to candidateK across groups so a vector
	// "rank" fed into rrfFuse reflects the true cross-model order, not
	// just within-group order — mirrors what the pre-Hybrid-Search
	// version did at topK before returning. See
	// sortVectorCandidatesByScoreThenID's doc comment (hybrid.go) for why
	// this needs a deterministic ID tie-break, not just a Score sort.
	sortVectorCandidatesByScoreThenID(vectorCandidates)
	if len(vectorCandidates) > cK {
		vectorCandidates = vectorCandidates[:cK]
	}

	// --- keyword path: one global trigram search across every active
	// knowledge base regardless of embedding model — see
	// searchKeywordChunks's doc comment for why it doesn't need one. This
	// is deliberately not inside the model-group loop above: it must run
	// (and be able to return results) even when every model group's
	// vector search failed or was skipped.
	var keywordCandidates []RetrievedChunk
	if len(activeKBIDs) > 0 {
		kw, err := s.repo.searchKeywordChunks(ctx, activeKBIDs, query, cK)
		if err != nil {
			if cerr := classifyRetrieveErr(ctx, err); cerr != nil {
				return nil, cerr
			}
			slog.Warn("knowledge: keyword chunk search failed, continuing with vector-only results", "err", err)
		} else {
			keywordCandidates = kw
		}
	}

	anchors, coreDuplicateCount := rrfFuse(vectorCandidates, keywordCandidates, topK)
	expanded, neighborDuplicateCount, err := s.expandWithNeighborWindow(ctx, anchors)
	if err != nil {
		return nil, err
	}
	// Phase 5 review fix (待修复项 3): a safe, content-free structured debug
	// line — only counts, never Content/query text/a fingerprint of either.
	// core_duplicate_count is how many core Hybrid Search candidates
	// rrfFuse's content-dedup dropped before ever becoming anchors;
	// neighbor_duplicate_count is how many neighbor-window chunks
	// expandWithNeighbors' second dedup pass dropped (always a neighbor
	// losing to an anchor or an earlier neighbor, never an anchor — see
	// expandWithNeighbors' doc comment). Both are 0 in the overwhelmingly
	// common case of no duplicate content; only logged when at least one
	// duplicate was actually suppressed — Retrieve runs on every
	// conversation turn with RAG enabled, so an unconditional debug line
	// here would be constant zero-value noise instead of a signal worth
	// looking at.
	if coreDuplicateCount > 0 || neighborDuplicateCount > 0 {
		slog.Debug("knowledge: content dedup suppressed duplicate chunks",
			"core_duplicate_count", coreDuplicateCount,
			"neighbor_duplicate_count", neighborDuplicateCount,
			"topK", topK)
	}
	return expanded, nil
}

// expandWithNeighborWindow is Neighbor Window Retrieval's DB-facing half —
// the pure assembly rules live in neighbor.go's expandWithNeighbors; this
// method's only job is turning anchors into the single batch lookup that
// function needs, and enforcing the same failure-isolation contract every
// other best-effort sub-call in Retrieve already follows.
//
// Phase 7: this method used to loop — one findPublishedNeighborChunks call
// per (document_id, document_version) group (buildNeighborGroups),
// producing an N+1 pattern where a Retrieve spanning K distinct document
// versions cost K neighbor-window round trips. It now flattens every
// anchor's wanted neighbor coordinates into one deduplicated request set
// via buildNeighborRequests and issues exactly one s.findNeighborBatch
// call for the whole set — a normal success path is always ONE database
// round trip here, regardless of how many anchors or document versions are
// involved. See neighbor_batch_test.go for the spy-based proof of "exactly
// once on the normal path, zero times on an empty request set".
//
// Failure isolation: because this is now a single call instead of one per
// group, a failure here is necessarily whole-batch — there is no longer a
// "this one group's neighbors failed, the rest still succeeded" middle
// ground, because there is no longer more than one call to partially fail.
// The contract from Phase 4 is preserved at the only granularity that
// still makes sense: every anchor, regardless of which document version it
// came from, still makes it into the result untouched (see rule 1 in
// expandWithNeighbors' doc comment: anchor rank/presence is never at the
// mercy of neighbor lookups) — a failed batch call just means the result
// has zero neighbors this time, exactly as "a hit somewhere for the core
// feature, a miss for the enrichment, results still exist" already meant
// for Retrieve's vector-path-per-model and keyword-path isolation above. A
// logged failure never includes the query text or any chunk content (see
// CLAUDE.md's don't-log-content convention already followed by every other
// Warn line in this file) — only the error and how many anchors/document
// versions were affected, none of which is user-authored free text.
//
// The one thing that is NOT best-effort here, same as everywhere else in
// Retrieve: the context itself being cancelled or timed out. A cancelled
// context must propagate immediately as an error from this method (and
// therefore from Retrieve), never get silently treated as "the neighbor
// batch just didn't work out" — see classifyRetrieveErr.
func (s *service) expandWithNeighborWindow(ctx context.Context, anchors []RetrievedChunk) ([]RetrievedChunk, int, error) {
	if len(anchors) == 0 {
		return anchors, 0, nil
	}

	requests := buildNeighborRequests(anchors)
	if len(requests) == 0 {
		// No anchor has any valid neighbor coordinate to ask for — nothing
		// to batch, so don't issue a call for an empty request set (see
		// buildNeighborRequests' and findPublishedNeighborChunksBatch's
		// doc comments for why this guard exists at both layers).
		expanded, neighborDuplicateCount := expandWithNeighbors(anchors, nil)
		return expanded, neighborDuplicateCount, nil
	}

	neighbors, err := s.findNeighborBatch(ctx, requests)
	if err != nil {
		if cerr := classifyRetrieveErr(ctx, err); cerr != nil {
			return nil, 0, cerr
		}
		slog.Warn("knowledge: neighbor window batch lookup failed, returning core hits without neighbors",
			"err", err, "anchor_count", len(anchors), "requested_coordinate_count", len(requests))
		expanded, neighborDuplicateCount := expandWithNeighbors(anchors, nil)
		return expanded, neighborDuplicateCount, nil // best-effort: every anchor still proceeds
	}

	expanded, neighborDuplicateCount := expandWithNeighbors(anchors, neighbors)
	return expanded, neighborDuplicateCount, nil
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
