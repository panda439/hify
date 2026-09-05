package knowledge

import "time"

const (
	FileTypeTxt = "txt"
	FileTypeMD  = "md"
	FileTypePDF = "pdf"
)

// StatusPublishing sits between processing and ready: Embedding is done
// and the new version's chunks are already written to PG (unpublished),
// but the PG publish step (delete obsolete versions + mark this version
// published, see repository.go's publishDocumentVersion) hasn't been
// confirmed to have succeeded yet. Splitting this out from processing
// means ReconcileStuckDocuments never has to redo the (expensive) embed
// work to recover a document stuck here — see ProcessDocument and
// publishAndComplete.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusPublishing = "publishing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
)

// leaseDuration bounds how long a claimed document (processing or
// publishing) can go without a heartbeat before ReconcileStuckDocuments is
// allowed to reclaim it — see ProcessDocument's renewal calls after every
// embed batch and before each state transition. It's sized to comfortably
// cover ONE external call, not the whole document (a document can take
// ~63 batches at maxChunksPerDocument/embedBatchSize, which is exactly
// why a single flat threshold covering the whole run doesn't work).
//
// provider.Client's resilience layer (provider/resilience.go) retries a
// single Embed call up to MaxRetries+1 = 3 times (default MaxRetries=2,
// see registry.go's resilienceConfigFrom), with retry-go backoff starting
// at 500ms and capped at 15s per gap (retry.Delay/retry.MaxDelay) — worst
// case backoff overhead across the two gaps between three attempts is
// well under a minute. The underlying HTTP client has no fixed timeout,
// so in theory one attempt could hang, but for a real (even
// rate-limited/degraded) embedding API, latency for a 32-item batch is
// seconds, not minutes. 3 minutes leaves generous headroom over that
// realistic worst case without approaching anything like "cover the
// entire document".
//
// This is a var, not a const, purely so tests can shrink it to exercise
// real lease expiry with a real (short) sleep instead of mocking time;
// production code never assigns to it.
var leaseDuration = 3 * time.Minute

// stalePendingThreshold is unrelated to leaseDuration — a pending
// document has never been claimed, so it never holds a lease. In normal
// operation a worker picks a pending document up within seconds of
// enqueue; anything stuck longer likely means the enqueue call itself
// never reached Redis (see UploadDocument's enqueueProcessDocument error
// comment), which only a plain elapsed-time scan can catch.
const stalePendingThreshold = 2 * time.Minute

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

	// maxChunksPerDocument is a hard cap, unlike maxChunksPerKnowledgeBase
	// above: a misconfigured chunk_size (e.g. a KB created with a very
	// small size) on a large file could otherwise blow up into hundreds of
	// thousands of chunks, each needing its own embedding call and PG row.
	// ProcessDocument rejects the document outright before ever calling
	// Embed if chunking produces more than this many pieces.
	maxChunksPerDocument = 2000

	// embedBatchSize bounds how many chunks go into a single
	// provider.Client.Embed call — see ProcessDocument's batching loop.
	// The provider layer already retries/rate-limits/circuit-breaks a
	// single Embed call (see provider/resilience.go); this cap exists so
	// one call's payload can't grow unbounded with maxChunksPerDocument,
	// not to duplicate that resilience.
	embedBatchSize = 32

	// maxTopK bounds Retrieve's topK — conversation hardcodes a small
	// topK (see conversation/context.go's retrievalTopK), but
	// workflow/executor.go's TopK is workflow-author-configurable JSON and
	// was previously unbounded. Retrieve silently clamps to this, the same
	// "soft usability cap, not a validation error" convention
	// platform.ClampLimit already uses for pagination.
	maxTopK = 50

	// defaultTopK is what an unset/non-positive topK clamps to — mirrors
	// conversation's own retrievalTopK default so Retrieve's fallback
	// behavior doesn't surprise either caller.
	defaultTopK = 5
)

// clampTopK enforces maxTopK and a sane default, the same "clamp, don't
// reject" convention as platform.ClampLimit — callers (workflow node
// config in particular) must not be trusted to supply a bounded topK
// directly.
func clampTopK(topK int) int {
	if topK <= 0 {
		return defaultTopK
	}
	if topK > maxTopK {
		return maxTopK
	}
	return topK
}

// maxFilterDocumentIDs 限制一个 RetrieveFilter 最多能指定多少份文档
// （002-metadata-filter，FR-015）。50 对齐上面的 maxTopK：一次检索指定超过
// 50 份文档，语义上已经不是"缩小范围"了，而缩小范围正是这个过滤器存在的
// 全部理由。
//
// 与 clampTopK 的"截断而不拒绝"惯例相反，超限在这里是**硬错误**：静默截断
// 调用方的文档列表，等于在不告诉他的情况下改变了他要求的范围，正是 FR-009
// 禁止的那种"自动放宽已指定的过滤条件"。截断对一个"返回几条"的旋钮是安全的，
// 对一个界定范围的谓词永远不安全。
// 004-agent-document-scope 起导出：agent 模块保存 Agent 的文档范围时要执行
// 同一个上限。导出而不是在 agent 里另写一个 50，是为了不让两个本该相等的
// 常量各自漂移——它们表达的是同一条业务规则。
const MaxFilterDocumentIDs = 50

// maxFilterDocumentIDs 是包内沿用的别名，保持既有引用不变。
const maxFilterDocumentIDs = MaxFilterDocumentIDs

// RetrieveFilter 限定一次 Retrieve 调用可以从哪些 chunk 里取候选
// （002-metadata-filter）。零值合法且是默认值，语义为"不限定"，
// 此时输出 MUST 与本功能上线前逐字一致（FR-006/FR-013，由确定性检索门禁断言）。
//
// 它**不是**知识库的隔离边界——knowledgeBaseIDs 仍然是 Retrieve 单独的、
// 必传的参数，这里的任何过滤条件都不可能突破它向外扩大范围。
//
// 过滤刻意是布尔的，绝不参与打分：它只缩小候选来源。RetrievedChunk.Score
// 的语义、RRF 融合、Phase 8 准入阈值、Phase 5 内容去重都不受它影响（FR-012）。
// 这里的每一个条件都被下推进**两路**召回查询的 SQL（见 pgqueries/chunks.sql），
// 而不是对它们的结果在 Go 里做筛选——召回之后再过滤会让过滤**吃掉**召回名额，
// 而不是**重新定向**召回范围（FR-007）。
//
// 条件语义（FR-008）：**同一**条件的多个取值之间是「或」（document_id = ANY(...)），
// **不同**条件之间是「与」。
type RetrieveFilter struct {
	// DocumentIDs 把候选限定在这几份文档内。空/nil 表示"不限文档"。
	// 引用了不存在的、或属于另一个知识库的 ID，就是单纯匹配不到任何东西——
	// 那是一个空结果，不是错误，也绝不是把这个条件悄悄丢掉（FR-010）。
	DocumentIDs []string

	// PageMin/PageMax 是一个 1-indexed 的**闭区间**，任意一端可以为 nil，
	// 表示那一侧不设限。字段本身与 002-metadata-filter 时期完全一致，
	// **变的是"什么叫匹配"**。
	//
	// ⭐ 006-pdf-layout-chunking：匹配规则从「**点落区间**」改成「**区间相交**」。
	// chunk 现在携带的是一个区间 [PageNumber, PageEnd]（见 Chunk 的文档注释），
	// 判定标准是它与 [PageMin, PageMax] **有交集**，而不是某一个页码落在里面。
	//
	// 对存量行与全部单页片段（PageEnd == PageNumber）这次改写是**逐字节等价**
	// 的，因此没有任何一格是"原本命中的现在不命中"；只有真正跨页的片段上多出
	// "原本漏掉的现在命中了"——过滤「第 4 页」本来就该命中一个覆盖 3-4 页的
	// 片段，因为它确实包含第 4 页的内容。完整对照表见
	// specs/006-pdf-layout-chunking/contracts/retrieval-page-range.md §3.3。
	//
	// PageNumber 为 NULL 的 chunk——全部 txt/md chunk，以及 000003 迁移之前
	// 写入的存量行——只要设了任意一端，就**不匹配**。这一点由 SQL 的三值逻辑
	// 保证，而不是靠显式的 IS NOT NULL 判断，详见 pgqueries/chunks.sql 里
	// SearchVectorChunks 的注释；改写后依然成立，因为 PageEnd 与 PageNumber
	// 同为 NULL（不变量 C1，由数据库约束强制）。
	// ⚠️ 绝不要用 COALESCE 给它加默认值来"修复"——那等于给一个本来没有页码的
	// chunk 编造出一个页码。这条禁令对 page_end 一字不改地同样适用。
	PageMin *int
	PageMax *int
}

// IsEmpty 报告这个过滤器是否什么都没限定。它是 FR-006「空过滤器等价于今天
// 的无过滤行为」的判定入口：空过滤器绝不能产生错误、绝不受功能开关影响、
// 也绝不改变 Retrieve 的返回结果。
func (f RetrieveFilter) IsEmpty() bool {
	return len(f.DocumentIDs) == 0 && f.PageMin == nil && f.PageMax == nil
}

// Validate 执行 FR-015 的上限校验。它**拒绝**而不**修补**：这里不会截断过长的
// 文档列表，也不会把颠倒的页码范围调换过来——任何这类"修补"都等于悄悄塞给调用方
// 一个他没有要求过的范围（见 maxFilterDocumentIDs）。
//
// 空过滤器永远合法——Retrieve 不能把"我什么都没限定"变成一个错误。
func (f RetrieveFilter) Validate() error {
	if len(f.DocumentIDs) > maxFilterDocumentIDs {
		return ErrTooManyFilterDocuments
	}
	// 页码是 1-indexed（parse.go 的 pdfPage.Number 从 1 开始），因此 0 和负数
	// 不是"不限"而是无意义的输入——真想表达"不限"的调用方，那个字段本来就是 nil。
	if f.PageMin != nil && *f.PageMin < 1 {
		return ErrInvalidPageRange
	}
	if f.PageMax != nil && *f.PageMax < 1 {
		return ErrInvalidPageRange
	}
	if f.PageMin != nil && f.PageMax != nil && *f.PageMin > *f.PageMax {
		return ErrInvalidPageRange
	}
	return nil
}

// RetrieveOptions 承载 Retrieve 的可选参数。做成结构体而不是再加一个裸参数，
// 是为了让将来新增一个检索选项时不必再一次改动 Retrieve 的签名
// （以及跟着它一起改的每一个调用方和测试替身）。零值就是今天的行为。
type RetrieveOptions struct {
	Filter RetrieveFilter
}

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
// pending -> processing -> publishing -> ready, or processing -> failed.
// Version identifies the current processing attempt (not a retry
// counter) — every status transition in ProcessDocument is a
// compare-and-swap keyed on (id, version, expected old status). Version
// only advances when a new attempt is deliberately started (RetryDocument,
// or ReconcileStuckDocuments reclaiming a document whose processing lease
// expired); a normal pending->processing->publishing->ready run never
// changes it. This is what lets a late/duplicate task instance recognize
// it has been superseded and exit without republishing stale chunks — see
// service.go's ProcessDocument. LeaseExpiresAt is non-nil only while
// Status is processing or publishing — see leaseDuration.
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
	Version         int64
	LeaseExpiresAt  *time.Time
	CreatedBy       string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Chunk is one embedded slice of a Document. It's immutable derived data —
// content changes mean delete-and-regenerate, never update-in-place, same
// as conversation.Message.
//
// DocumentName is a snapshot of Document.FileName taken at processing time
// (see service.go's ProcessDocument) — a source-attribution label, not a
// live join back to MySQL's documents table, so it survives the document
// being renamed, reprocessed, or later deleted. Chunks written before this
// field existed carry DocumentName == "" (see pgmigrations 000003); callers
// needing a display name must fall back to DocumentID themselves rather
// than treating "" as an error.
//
// PageNumber/PageEnd/SectionTitle are nil unless the parser can honestly
// produce them (see chunk.go's chunkDocument and its per-file-type
// chunkers): txt chunks never set any of them, md chunks may set
// SectionTitle but never a page, pdf chunks set both page fields.
// Never fabricate a value here.
//
// PageNumber and PageEnd are a closed 1-indexed INTERVAL, not two loose
// numbers: PageNumber is the first page the chunk covers and PageEnd the
// last. Before 006-pdf-layout-chunking a chunk could not span pages (PDFs
// were chunked page by page), so PageNumber alone was enough; now that a
// paragraph broken across a page boundary is reassembled into one chunk,
// reporting a single page would mean picking one end and calling it the
// answer — a fabricated citation (FR-011). Invariants, all three enforced:
//
//	C1  PageEnd == nil  ⟺  PageNumber == nil   (DB: chunks_page_range_valid)
//	C2  both non-nil ⇒ *PageNumber <= *PageEnd (DB: chunks_page_range_valid)
//	C3  both non-nil ⇒ 1 <= *PageNumber and *PageEnd <= the document's page
//	    count — the upper half is NOT checkable by the database (it does not
//	    know how many pages a document has), so it is asserted where the
//	    values are produced, in layout.go, and pinned by layout_test.go.
//
// C1 is load-bearing for retrieval, not decoration: the page filter's lower
// bound reads page_end while its upper bound reads page_number, so a row
// with one set and the other NULL is silently dropped by any filtered
// search — no error, no log line. See pgqueries/chunks.sql.
type Chunk struct {
	ID                 string
	KnowledgeBaseID    string
	DocumentID         string
	DocumentName       string
	ChunkIndex         int
	Content            string
	ContentLength      int
	Embedding          []float32
	EmbeddingDimension int
	PageNumber         *int
	PageEnd            *int
	SectionTitle       *string
	CreatedAt          time.Time

	// DocumentVersion identifies which processing attempt (see Document's
	// doc comment) this chunk belongs to — Phase 4's neighbor-window
	// expansion (neighbor.go) is the reason this needs to exist on Chunk at
	// all: finding chunk_index-1/+1 for a hit requires knowing not just
	// "which document" but "which processing attempt", or a document that's
	// been reprocessed since this chunk was written could get its neighbor
	// query silently answered by the NEW version's chunk at the same index
	// — a cross-version content splice with no error, no log line, nothing
	// to notice. See repository.go's findPublishedNeighborChunks and
	// pgqueries/chunks.sql's FindPublishedNeighborChunks for where this
	// actually gets used as a filter. Internal only — never serialized to
	// Citation/SSE (conversation's Evidence type has no equivalent field,
	// same as it never exposes any other pure-plumbing field).
	DocumentVersion int64
}

// RetrievedChunk is a Chunk annotated with its relevance score against a
// query — what conversation's context assembly (budget.go's
// ragMinSimilarityScore floor) and the SSE debug panel consume.
//
// Score means two different things depending on NeighborOf, and callers
// must not conflate them:
//   - NeighborOf == "" (a core Hybrid Search hit, an "anchor"): Score is a
//     real, directly-measured 0..1 relevance number against the query —
//     this is the load-bearing invariant Phase 3's Hybrid Search must not
//     break (see hybrid.go's rrfFuse doc comment). Concretely: vector-only
//     hit = cosine similarity; keyword-only hit = pg_trgm word-similarity;
//     hit by both paths = the larger of the two.
//   - NeighborOf != "" (a Phase 4 neighbor-window chunk, see neighbor.go):
//     Score is NOT independently measured — a neighbor chunk never went
//     through vector or keyword search at all, so it has no cosine
//     similarity or word-similarity of its own to report. It *inherits*
//     its owning anchor's Score verbatim, purely so
//     conversation/budget.go's ragMinSimilarityScore floor and greedy
//     budget fill treat the whole anchor+neighbors window as one
//     relevance-priority group instead of the neighbor chunks silently
//     scoring 0 and always being the first thing budget pressure drops.
//     Never describe this as "the neighbor's own cosine similarity" or
//     "the neighbor's own keyword similarity" — it is neither; it is a
//     borrowed priority number for budget purposes only.
//
// The Reciprocal Rank Fusion score that actually decided an anchor's
// position in rrfFuse's output (fusionScore) is intentionally NOT a field
// here — it's an internal-only ranking number (see hybrid.go), typically
// two orders of magnitude smaller than Score, and would silently zero out
// every retrieval if it ever leaked into this field (budget.go would
// filter everything below ragMinSimilarityScore=0.2). Anchor order is
// final by the time rrfFuse returns; neighbor placement (see
// expandWithNeighbors) never re-sorts anchors by Score either, and never
// interleaves a neighbor between two anchors — every anchor comes first,
// in full rrfFuse rank order, and only then every anchor's own neighbor
// chunks as a strictly lower-priority second tier. See
// expandWithNeighbors' doc comment for why this two-tier layout (not
// anchor-then-its-own-neighbors-then-next-anchor) is what keeps a core hit
// from ever losing its place in a tight conversation/budget.go budget to
// someone else's neighbor chunk.
type RetrievedChunk struct {
	Chunk
	Score float64

	// NeighborOf is Phase 4's core/neighbor discriminator (see neighbor.go):
	// "" means this is a core Hybrid Search hit (an anchor) — the value
	// Retrieve returned before Phase 4 ever existed. A non-empty value is
	// the chunk ID of the anchor this chunk was pulled in to provide
	// surrounding context for — see expandWithNeighbors' dedup rule for
	// what happens when a chunk would otherwise be a neighbor of more than
	// one anchor (it's attributed to exactly one, the higher-ranked
	// anchor) and for what happens when a would-be neighbor is itself
	// separately an anchor (it only ever appears once, as the anchor, never
	// additionally as a neighbor). Internal only — never serialized to
	// Citation/SSE; conversation's Evidence type has no equivalent field,
	// and a neighbor chunk's own real DocumentName/PageNumber/SectionTitle
	// (not the anchor's) is what a citation for it must show, exactly like
	// any other RetrievedChunk.
	NeighborOf string
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
