package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// claimDocumentProcessing is the CAS that starts a processing attempt:
// pending/failed -> processing, guarded by (id, version), seeding the
// initial heartbeat lease (see model.go's leaseDuration) in the same
// statement. See service.go's ProcessDocument for the full state machine
// — claimed=false means either a concurrent duplicate delivery lost the
// race, or this task instance's version/status no longer matches, and
// the caller must treat that as a safe no-op, not an error.
func (r *Repository) claimDocumentProcessing(ctx context.Context, id string, version int64, leaseExpiresAt time.Time) (bool, error) {
	n, err := r.queries.ClaimDocumentProcessing(ctx, gen.ClaimDocumentProcessingParams{
		ID:             id,
		Version:        version,
		LeaseExpiresAt: toNullTime(leaseExpiresAt),
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: claim document processing: %w", err)
	}
	return n == 1, nil
}

// renewDocumentLease is the heartbeat CAS a worker calls after every embed
// batch and before each risky step (writing chunks, publishing) — guarded
// by (id, version, status) so a stale worker whose document has already
// been reclaimed (version bumped, or status changed) gets renewed=false
// and must stop immediately: no further batches, no chunk writes, no
// publish. status is a parameter because processing and publishing share
// this one statement.
func (r *Repository) renewDocumentLease(ctx context.Context, id string, version int64, status string, leaseExpiresAt time.Time) (bool, error) {
	n, err := r.queries.RenewDocumentLease(ctx, gen.RenewDocumentLeaseParams{
		LeaseExpiresAt: toNullTime(leaseExpiresAt),
		ID:             id,
		Version:        version,
		Status:         status,
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: renew document lease: %w", err)
	}
	return n == 1, nil
}

// markDocumentPublishing is the CAS that locks in "embedding is done,
// chunks are already written unpublished to PG, only the publish step is
// left": processing -> publishing, guarded by (id, version), with a fresh
// lease. Splitting this out from markDocumentReady is what lets
// ReconcileStuckDocuments recover a stuck document by only retrying the
// (idempotent) publish, never redoing the embedding work.
// markDocumentPublishing also records the two ways pages failed to make it
// into this version — pages with no text layer, and pages the parser could
// not read at all.
//
// ⭐ They are written HERE, not at markDocumentReady, because this step is
// the one that locks in "the work is done, only publishing is left" — and
// these lists are part of that work's result. Publishing confirmation can
// be performed by a recovery pass that never parsed the file and therefore
// cannot know them; recording them here means that pass has nothing to
// overwrite. The (id, version, status='processing') guard gives the same
// "belongs to the attempt that won" property the previous location had.
//
// Passing nil for either list clears it, so a document that no longer has
// that kind of failure stops reporting it.
func (r *Repository) markDocumentPublishing(ctx context.Context, id string, version int64, leaseExpiresAt time.Time, unextractedPages, unparseablePages []int) (bool, error) {
	n, err := r.queries.MarkDocumentPublishing(ctx, gen.MarkDocumentPublishingParams{
		ID:               id,
		Version:          version,
		LeaseExpiresAt:   toNullTime(leaseExpiresAt),
		UnextractedPages: encodePageList(unextractedPages),
		UnparseablePages: encodePageList(unparseablePages),
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: mark document publishing: %w", err)
	}
	return n == 1, nil
}

// markDocumentReady is the final confirmation after a successful PG
// publish: publishing -> ready, guarded by (id, version). ok=false means
// another runner (the original worker, or a concurrent reconciliation
// recovery pass) already completed this exact transition — a benign
// idempotent race, not an error.
// markDocumentReady no longer touches the two page-list columns.
//
// ⚠️ Do not add them back. ReconcileStuckDocuments calls this too, and it
// has not re-parsed the file, so it could only pass nil — which would wipe
// the values markDocumentPublishing just wrote correctly. That is the exact
// defect 008 exists to fix, and it fails silently: the user simply sees no
// notice. See queries/documents.sql for the full reasoning.
func (r *Repository) markDocumentReady(ctx context.Context, id string, version int64, chunkCount int) (bool, error) {
	n, err := r.queries.MarkDocumentReady(ctx, gen.MarkDocumentReadyParams{
		ChunkCount: int32(chunkCount),
		ID:         id,
		Version:    version,
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: mark document ready: %w", err)
	}
	return n == 1, nil
}

// markDocumentFailed is processing -> failed, same CAS shape as
// markDocumentReady. A publishing-phase failure never comes through here
// — by design the document stays in publishing and ReconcileStuckDocuments
// recovers it by retrying the idempotent publish, not by failing it.
func (r *Repository) markDocumentFailed(ctx context.Context, id string, version int64, errMsg string) (bool, error) {
	n, err := r.queries.MarkDocumentFailed(ctx, gen.MarkDocumentFailedParams{
		ErrorMessage: stringToNullString(errMsg),
		ID:           id,
		Version:      version,
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: mark document failed: %w", err)
	}
	return n == 1, nil
}

// reclaimDocumentForRetry is the manual-retry CAS: pending/failed ->
// pending with version advanced by one, fencing off any late task
// instance still carrying oldVersion.
func (r *Repository) reclaimDocumentForRetry(ctx context.Context, id string, oldVersion, newVersion int64) (bool, error) {
	n, err := r.queries.ReclaimDocumentForRetry(ctx, gen.ReclaimDocumentForRetryParams{
		NewVersion: newVersion,
		ID:         id,
		OldVersion: oldVersion,
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: reclaim document for retry: %w", err)
	}
	return n == 1, nil
}

// reclaimStaleProcessingDocument is reconciliation's CAS: processing ->
// pending with version advanced by one and lease cleared, used to take
// back a document whose processing lease expired (worker presumed dead,
// see model.go's leaseDuration). The embedding work is not resumable at
// this point (nothing was durably written yet for this attempt), so this
// resets all the way back to pending for a from-scratch retry — contrast
// with a stuck publishing document, which skips straight to a republish
// via publishAndComplete instead of going through this method.
//
// expiredBefore re-validates the lease at UPDATE time, not just at the
// SELECT time of listLeaseExpiredProcessingDocuments — without it this is
// a check-then-act race: an active worker can renew between the scan and
// this CAS, and id/version/status alone wouldn't notice. The caller must
// pass the same timestamp it used for the scan (ReconcileStuckDocuments'
// now), not a freshly computed one, or the two checks stop being the same
// point in time and the race reopens.
func (r *Repository) reclaimStaleProcessingDocument(ctx context.Context, id string, oldVersion, newVersion int64, expiredBefore time.Time) (bool, error) {
	n, err := r.queries.ReclaimStaleProcessingDocument(ctx, gen.ReclaimStaleProcessingDocumentParams{
		NewVersion:    newVersion,
		ID:            id,
		OldVersion:    oldVersion,
		ExpiredBefore: toNullTime(expiredBefore),
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: reclaim stale processing document: %w", err)
	}
	return n == 1, nil
}

// claimExpiredPublishingRecovery is the CAS a recovery attempt (originally
// ReconcileStuckDocuments, but written generically enough that any runner
// could call it) must win before touching a stuck publishing document —
// see reclaimStaleProcessingDocument's doc comment for why id/version/
// status alone isn't enough to rule out a TOCTOU race against an active
// worker renewing between the scan and the recovery action. PG's publish
// being idempotent is not a substitute for this: idempotency means two
// concurrent publishers can't corrupt data, not that recovery should
// proceed without ever establishing who's allowed to act.
//
// On success the lease is refreshed to newLeaseExpiresAt (the caller
// passes newLeaseDeadline()) — if the subsequent publishAndComplete then
// fails, the document stays in publishing under this fresh lease rather
// than being immediately eligible for another recovery attempt in the
// same or the very next reconciliation pass.
func (r *Repository) claimExpiredPublishingRecovery(ctx context.Context, id string, version int64, expiredBefore, newLeaseExpiresAt time.Time) (bool, error) {
	n, err := r.queries.ClaimExpiredPublishingRecovery(ctx, gen.ClaimExpiredPublishingRecoveryParams{
		NewLeaseExpiresAt: toNullTime(newLeaseExpiresAt),
		ID:                id,
		Version:           version,
		ExpiredBefore:     toNullTime(expiredBefore),
	})
	if err != nil {
		return false, fmt.Errorf("knowledge: claim expired publishing recovery: %w", err)
	}
	return n == 1, nil
}

// listLeaseExpiredProcessingDocuments is ReconcileStuckDocuments' scan for
// presumed-dead workers — status='processing' and the heartbeat lease has
// expired (see model.go's leaseDuration), not a flat elapsed-time
// threshold: a legitimately slow but alive worker keeps renewing its
// lease and never shows up here regardless of total run time.
func (r *Repository) listLeaseExpiredProcessingDocuments(ctx context.Context, before time.Time) ([]Document, error) {
	rows, err := r.queries.ListLeaseExpiredProcessingDocuments(ctx, toNullTime(before))
	if err != nil {
		return nil, fmt.Errorf("knowledge: list lease-expired processing documents: %w", err)
	}
	out := make([]Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainDocument(row))
	}
	return out, nil
}

// listLeaseExpiredPublishingDocuments is the publishing-phase counterpart
// — embedding already finished for these, only the publish step is
// outstanding (PG publish failed, or the worker crashed after PG publish
// succeeded but before the ready CAS). See publishAndComplete for the
// idempotent recovery.
func (r *Repository) listLeaseExpiredPublishingDocuments(ctx context.Context, before time.Time) ([]Document, error) {
	rows, err := r.queries.ListLeaseExpiredPublishingDocuments(ctx, toNullTime(before))
	if err != nil {
		return nil, fmt.Errorf("knowledge: list lease-expired publishing documents: %w", err)
	}
	out := make([]Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainDocument(row))
	}
	return out, nil
}

func (r *Repository) listStalePendingDocuments(ctx context.Context, before time.Time) ([]Document, error) {
	rows, err := r.queries.ListStalePendingDocuments(ctx, before)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list stale pending documents: %w", err)
	}
	out := make([]Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainDocument(row))
	}
	return out, nil
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
//
// Rows are always written with is_published=false — this is the "new
// version write" step of the reprocessing model (see the pgmigrations
// 000002 comment): the caller must follow up with publishDocumentVersion
// once MySQL's CAS gate confirms this version is still the authoritative
// attempt, never before.
func (r *Repository) createChunks(ctx context.Context, chunks []Chunk, version int64) error {
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
				DocumentVersion:    version,
				IsPublished:        false,
				DocumentName:       c.DocumentName,
				PageNumber:         intPtrToNullInt32(c.PageNumber),
				PageEnd:            intPtrToNullInt32(c.PageEnd),
				SectionTitle:       stringPtrToNullString(c.SectionTitle),
			}); err != nil {
				return fmt.Errorf("knowledge: create chunk: %w", err)
			}
		}
		return nil
	})
}

// publishDocumentVersion is the "publish new version -> clean up old
// version" step, run only after MySQL's CAS gate (markDocumentReady) has
// confirmed version is still the authoritative attempt. Both statements
// run in one PostgreSQL transaction so an external reader never observes
// "both versions visible" or "neither version visible" — see the
// pgmigrations 000002 comment for the full ordering rationale.
func (r *Repository) publishDocumentVersion(ctx context.Context, documentID string, version int64) error {
	return platform.WithTx(ctx, r.pgdb, func(tx *sql.Tx) error {
		q := r.pgQueries.WithTx(tx)
		if err := q.DeleteObsoleteChunkVersions(ctx, pggen.DeleteObsoleteChunkVersionsParams{
			DocumentID:      documentID,
			DocumentVersion: version,
		}); err != nil {
			return fmt.Errorf("knowledge: delete obsolete chunk versions: %w", err)
		}
		if err := q.PublishChunkVersion(ctx, pggen.PublishChunkVersionParams{
			DocumentID:      documentID,
			DocumentVersion: version,
		}); err != nil {
			return fmt.Errorf("knowledge: publish chunk version: %w", err)
		}
		return nil
	})
}

// deleteChunksByDocumentVersion is the best-effort cleanup a worker runs
// when markDocumentReady/markDocumentFailed's CAS is rejected (this
// attempt was superseded): the rows it just wrote are unpublished and
// will never become searchable on their own, but deleting them avoids
// leaving dead weight behind.
func (r *Repository) deleteChunksByDocumentVersion(ctx context.Context, documentID string, version int64) error {
	if err := r.pgQueries.DeleteChunksByDocumentVersion(ctx, pggen.DeleteChunksByDocumentVersionParams{
		DocumentID:      documentID,
		DocumentVersion: version,
	}); err != nil {
		return fmt.Errorf("knowledge: delete chunks by document version: %w", err)
	}
	return nil
}

// searchVectorChunks pushes similarity scoring and topK/candidateK
// selection down to pgvector (`ORDER BY embedding <=> query LIMIT n`),
// replacing the old load-everything-and-score-in-Go path. The dimension
// filter comes from the query vector itself: only chunks embedded by a
// same-dimension model are comparable, which is what keeps mixed-dimension
// knowledge bases in one table from ever colliding. n is candidateK in the
// Hybrid Search path (service.go's Retrieve) — a wider window than the
// final topK, left for rrfFuse to re-rank and truncate — but the method
// itself doesn't know or care which caller it is; it just returns the top
// n rows by cosine similarity.
// filterToPGParams 把领域层的 RetrieveFilter 翻译成 sqlc 的三个可空参数
// （002-metadata-filter）。这里只做翻译，不做任何校验——校验是 service 层
// Retrieve 入口的职责（repository 只做 CRUD，不含业务判断）。
//
// ⚠️ documentIDs 在"不限定"时必须返回 nil 切片，不能返回长度为 0 的非 nil
// 切片：sqlc 生成的代码用 pq.Array 传参，nil 切片 Value() 出来是 SQL NULL
// （谓词恒真短路，等于不过滤），而空的非 nil 切片会变成 '{}' 这个**空数组**，
// 于是 document_id = ANY('{}') 恒为 FALSE，把所有候选都挡光。两者在 Go 里
// 都满足 len()==0，差别只在这一层显现，所以统一在这里归一化，不指望每个
// 调用方自己记得区分。
func filterToPGParams(f RetrieveFilter) (documentIDs []string, pageMin, pageMax sql.NullInt32) {
	if len(f.DocumentIDs) > 0 {
		documentIDs = f.DocumentIDs
	}
	if f.PageMin != nil {
		pageMin = sql.NullInt32{Int32: int32(*f.PageMin), Valid: true}
	}
	if f.PageMax != nil {
		pageMax = sql.NullInt32{Int32: int32(*f.PageMax), Valid: true}
	}
	return documentIDs, pageMin, pageMax
}

func (r *Repository) searchVectorChunks(ctx context.Context, kbIDs []string, queryVec []float32, n int, filter RetrieveFilter) ([]RetrievedChunk, error) {
	filterDocumentIDs, filterPageMin, filterPageMax := filterToPGParams(filter)
	rows, err := r.pgQueries.SearchVectorChunks(ctx, pggen.SearchVectorChunksParams{
		QueryEmbedding:     pgvector.NewVector(queryVec),
		KnowledgeBaseIds:   kbIDs,
		EmbeddingDimension: int32(len(queryVec)),
		FilterDocumentIds:  filterDocumentIDs,
		FilterPageMin:      filterPageMin,
		FilterPageMax:      filterPageMax,
		TopK:               int32(n),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: search vector chunks: %w", err)
	}
	out := make([]RetrievedChunk, 0, len(rows))
	for _, row := range rows {
		out = append(out, RetrievedChunk{
			Chunk: Chunk{
				ID:              row.ID,
				KnowledgeBaseID: row.KnowledgeBaseID,
				DocumentID:      row.DocumentID,
				DocumentName:    row.DocumentName,
				ChunkIndex:      int(row.ChunkIndex),
				Content:         row.Content,
				ContentLength:   int(row.ContentLength),
				// Embedding stays nil on purpose: no consumer reads it
				// (conversation/workflow only use Content), and shipping
				// vectors back defeats the point of scoring in-database.
				EmbeddingDimension: int(row.EmbeddingDimension),
				PageNumber:         nullInt32ToIntPtr(row.PageNumber),
				PageEnd:            nullInt32ToIntPtr(row.PageEnd),
				SectionTitle:       nullStringToStringPtr(row.SectionTitle),
				CreatedAt:          row.CreatedAt,
				// DocumentVersion: Phase 4's neighbor-window expansion
				// needs this to scope its own SQL to the exact processing
				// attempt this anchor came from — see Chunk.DocumentVersion
				// and findPublishedNeighborChunks below.
				DocumentVersion: row.DocumentVersion,
			},
			Score: row.Score,
		})
	}
	return out, nil
}

// searchKeywordChunks is the trigram/lexical counterpart to
// searchVectorChunks — pg_trgm word-similarity instead of pgvector cosine
// distance, no embedding involved at all (see pgqueries/chunks.sql's
// SearchKeywordChunks for why that's exactly what lets this path keep
// working when the embedding provider is down). Empty query returns
// (nil, nil) without a round trip — SQL also guards this (query_text <> ”),
// but short-circuiting here means a caller sees literally zero DB cost for
// the case Retrieve already treats as "nothing to search".
func (r *Repository) searchKeywordChunks(ctx context.Context, kbIDs []string, query string, n int, filter RetrieveFilter) ([]RetrievedChunk, error) {
	if query == "" || len(kbIDs) == 0 {
		return nil, nil
	}
	filterDocumentIDs, filterPageMin, filterPageMax := filterToPGParams(filter)
	rows, err := r.pgQueries.SearchKeywordChunks(ctx, pggen.SearchKeywordChunksParams{
		QueryText:         query,
		KnowledgeBaseIds:  kbIDs,
		FilterDocumentIds: filterDocumentIDs,
		FilterPageMin:     filterPageMin,
		FilterPageMax:     filterPageMax,
		CandidateK:        int32(n),
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: search keyword chunks: %w", err)
	}
	out := make([]RetrievedChunk, 0, len(rows))
	for _, row := range rows {
		out = append(out, RetrievedChunk{
			Chunk: Chunk{
				ID:                 row.ID,
				KnowledgeBaseID:    row.KnowledgeBaseID,
				DocumentID:         row.DocumentID,
				DocumentName:       row.DocumentName,
				ChunkIndex:         int(row.ChunkIndex),
				Content:            row.Content,
				ContentLength:      int(row.ContentLength),
				EmbeddingDimension: int(row.EmbeddingDimension),
				PageNumber:         nullInt32ToIntPtr(row.PageNumber),
				PageEnd:            nullInt32ToIntPtr(row.PageEnd),
				SectionTitle:       nullStringToStringPtr(row.SectionTitle),
				CreatedAt:          row.CreatedAt,
				DocumentVersion:    row.DocumentVersion,
			},
			Score: row.Score,
		})
	}
	return out, nil
}

// findPublishedNeighborChunks is Phase 4's single SQL entry point for
// neighbor-window expansion (see neighbor.go) — one call fetches every
// chunk_index a caller needs within ONE (documentID, documentVersion)
// group, never one call per anchor or per direction (see
// pgqueries/chunks.sql's FindPublishedNeighborChunks doc comment for why
// that grouping is a hard requirement, not just an optimization). The
// returned chunks carry Score=0 and NeighborOf="" — this method knows
// nothing about which anchor(s) asked for which index, or what Score they
// should inherit; that's expandWithNeighbors' job (hybrid_test.go covers
// it independent of any DB), not this repository call's.
//
// chunkIndexes is the caller's responsibility to keep non-negative and
// non-empty — this method does not special-case an empty slice (an empty
// []int32 array in the ANY(...) predicate simply matches nothing, which is
// the correct "not asking for anything" behavior on its own, no early
// return needed).
func (r *Repository) findPublishedNeighborChunks(ctx context.Context, documentID string, documentVersion int64, chunkIndexes []int) ([]RetrievedChunk, error) {
	idxs := make([]int32, len(chunkIndexes))
	for i, idx := range chunkIndexes {
		idxs[i] = int32(idx)
	}
	rows, err := r.pgQueries.FindPublishedNeighborChunks(ctx, pggen.FindPublishedNeighborChunksParams{
		DocumentID:      documentID,
		DocumentVersion: documentVersion,
		ChunkIndexes:    idxs,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: find published neighbor chunks: %w", err)
	}
	out := make([]RetrievedChunk, 0, len(rows))
	for _, row := range rows {
		out = append(out, RetrievedChunk{
			Chunk: Chunk{
				ID:                 row.ID,
				KnowledgeBaseID:    row.KnowledgeBaseID,
				DocumentID:         row.DocumentID,
				DocumentName:       row.DocumentName,
				ChunkIndex:         int(row.ChunkIndex),
				Content:            row.Content,
				ContentLength:      int(row.ContentLength),
				EmbeddingDimension: int(row.EmbeddingDimension),
				PageNumber:         nullInt32ToIntPtr(row.PageNumber),
				PageEnd:            nullInt32ToIntPtr(row.PageEnd),
				SectionTitle:       nullStringToStringPtr(row.SectionTitle),
				CreatedAt:          row.CreatedAt,
				DocumentVersion:    row.DocumentVersion,
			},
			// Score/NeighborOf are deliberately left zero-value here — see
			// this method's doc comment.
		})
	}
	return out, nil
}

// findPublishedNeighborChunksBatch is Phase 7's single SQL entry point for
// neighbor-window expansion — the batch counterpart to
// findPublishedNeighborChunks above (see pgqueries/chunks.sql's
// FindPublishedNeighborChunksBatch doc comment for the SQL-level design).
// One call fetches every (document_id, document_version, chunk_index)
// triple in requests, across any number of distinct document versions —
// callers no longer loop this method per document-version group the way
// the old findPublishedNeighborChunks required; see service.go's
// expandWithNeighborWindow, which now calls this exactly once per
// Retrieve (never once per anchor, never once per document-version
// group).
//
// requests should already be deduplicated by the caller (buildNeighborRequests
// in neighbor.go guarantees this on the normal production path) — this
// method does not dedup again in Go, and doesn't need to: the SQL itself
// (FindPublishedNeighborChunksBatch's requested CTE) also deduplicates via
// SELECT DISTINCT, so a duplicate triple in requests resolves to exactly
// one result row per matching chunk regardless (see the SQL doc comment;
// this used to not be true — Codex's first-round Phase 7 review caught a
// real bug where a duplicated request coordinate produced a duplicated
// result row, since fixed). Go-level dedup in buildNeighborRequests stays
// as the primary mechanism (it's what keeps the parameter arrays sent to
// the database from growing with redundant coordinates on the normal
// path); the SQL-level DISTINCT is a second, independent correctness
// guarantee that doesn't rely on every caller always going through
// buildNeighborRequests first.
//
// An empty requests slice returns (nil, nil) without a round trip — this
// mirrors searchKeywordChunks' empty-query short-circuit above and is the
// other half of the "不发起无意义数据库查询" requirement: buildNeighborRequests
// already returns an empty slice when there's nothing to ask for (e.g. no
// anchors), and this method makes sure that empty request set never turns
// into an actual query even if a future caller forgets to check len(...)
// itself.
//
// Like findPublishedNeighborChunks, the returned chunks carry Score=0 and
// NeighborOf="" — assembling those from the request is expandWithNeighbors'
// job, not this repository call's.
func (r *Repository) findPublishedNeighborChunksBatch(ctx context.Context, requests []neighborRequest) ([]RetrievedChunk, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	docIDs := make([]string, len(requests))
	docVersions := make([]int64, len(requests))
	chunkIdxs := make([]int32, len(requests))
	for i, req := range requests {
		docIDs[i] = req.documentID
		docVersions[i] = req.documentVersion
		chunkIdxs[i] = int32(req.chunkIndex)
	}
	rows, err := r.pgQueries.FindPublishedNeighborChunksBatch(ctx, pggen.FindPublishedNeighborChunksBatchParams{
		DocumentIds:      docIDs,
		DocumentVersions: docVersions,
		ChunkIndexes:     chunkIdxs,
	})
	if err != nil {
		return nil, fmt.Errorf("knowledge: find published neighbor chunks batch: %w", err)
	}
	out := make([]RetrievedChunk, 0, len(rows))
	for _, row := range rows {
		out = append(out, RetrievedChunk{
			Chunk: Chunk{
				ID:                 row.ID,
				KnowledgeBaseID:    row.KnowledgeBaseID,
				DocumentID:         row.DocumentID,
				DocumentName:       row.DocumentName,
				ChunkIndex:         int(row.ChunkIndex),
				Content:            row.Content,
				ContentLength:      int(row.ContentLength),
				EmbeddingDimension: int(row.EmbeddingDimension),
				PageNumber:         nullInt32ToIntPtr(row.PageNumber),
				PageEnd:            nullInt32ToIntPtr(row.PageEnd),
				SectionTitle:       nullStringToStringPtr(row.SectionTitle),
				CreatedAt:          row.CreatedAt,
				DocumentVersion:    row.DocumentVersion,
			},
			// Score/NeighborOf are deliberately left zero-value here — see
			// this method's doc comment.
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

// countChunksByDocumentVersion is publishAndComplete's source of truth
// for chunk_count when it's ReconcileStuckDocuments (not the original
// worker) completing the publish — reconciliation runs in a different
// call than the one that computed len(pieces), so the count has to come
// from what's actually published in PG.
func (r *Repository) countChunksByDocumentVersion(ctx context.Context, documentID string, version int64) (int, error) {
	n, err := r.pgQueries.CountChunksByDocumentVersion(ctx, pggen.CountChunksByDocumentVersionParams{
		DocumentID:      documentID,
		DocumentVersion: version,
	})
	if err != nil {
		return 0, fmt.Errorf("knowledge: count chunks by document version: %w", err)
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
		Version:         row.Version,
		LeaseExpiresAt:  fromNullTime(row.LeaseExpiresAt),
		// All five document queries share this column list, so they all
		// share this one mapping — there is no per-query place to forget.
		UnextractedPages: decodePageList(row.UnextractedPages),
		UnparseablePages: decodePageList(row.UnparseablePages),
		CreatedBy:        row.CreatedBy,
		CreatedAt:        row.CreatedAt,
		UpdatedAt:        row.UpdatedAt,
	}
}

// toNullTime/fromNullTime bridge time.Time <-> sql.NullTime at the sqlc
// boundary — lease_expires_at is nullable (NULL outside processing/
// publishing), same rationale as stringToNullString below for
// error_message.
func toNullTime(t time.Time) sql.NullTime {
	return sql.NullTime{Time: t, Valid: true}
}

func fromNullTime(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func stringToNullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// intPtrToNullInt32/stringPtrToNullString/nullInt32ToIntPtr/
// nullStringToStringPtr bridge Chunk's PageNumber/SectionTitle (*int/
// *string — genuinely optional, unlike the empty-string-means-absent fields
// above) at the pggen boundary. A nil pointer is the only way to write PG
// NULL here; an empty string is a real (if unlikely) section title, not
// "absent", so stringToNullString's empty-string convention would be wrong
// for this field.
func intPtrToNullInt32(p *int) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Int32: int32(*p), Valid: true}
}

func stringPtrToNullString(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func nullInt32ToIntPtr(n sql.NullInt32) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int32)
	return &v
}

func nullStringToStringPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// documentCoverages 批量取这些文档的缺失情况，完整的（两列都空）不进结果——
// 过滤在这里做完，调用方拿到的每一条都确实有话可说（009 的 FR-008）。
func (r *Repository) documentCoverages(ctx context.Context, documentIDs []string) (map[string]DocumentCoverage, error) {
	rows, err := r.queries.ListDocumentCoverages(ctx, documentIDs)
	if err != nil {
		return nil, fmt.Errorf("knowledge: list document coverages: %w", err)
	}
	out := make(map[string]DocumentCoverage, len(rows))
	for _, row := range rows {
		c := DocumentCoverage{
			DocumentID:       row.ID,
			UnextractedPages: decodePageList(row.UnextractedPages),
			UnparseablePages: decodePageList(row.UnparseablePages),
		}
		if c.IsComplete() {
			continue
		}
		out[c.DocumentID] = c
	}
	return out, nil
}
