package knowledge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/platform"
	"hify/internal/platform/apperr"
)

// extraction.go 是关系抽取作业的编排（010 Phase 3）。
//
// ⭐ 初始化只做一件事，但这件事必须绝对可靠：把「这份文档当前已发布的全部
// 片段」一条不多一条不少地变成待处理项。数量错了的后果不是报错——是最后
// 那个"覆盖了全书多少"的数字悄悄偏小，而每一项看起来都正常，因为
// total_items 同时是分母。

const (
	// jobStatePending 是"登记了意图但还没开始枚举语料"。
	// ⚠️ 与 initializing 分开：后者的意思是"正在枚举语料"，而 pending 时
	// 可能连语料都还没有。混用会让恢复扫描把一个什么都没开始的意图
	// 当成"初始化到一半崩了"去接手。
	jobStatePending      = "pending"
	jobStateInitializing = "initializing"
	jobStateRunning      = "running"
	jobStatePaused       = "paused"
	jobStateSucceeded    = "succeeded"
	jobStateFailed       = "failed"
	jobStateSuperseded   = "superseded"
	// jobStateBudgetExhausted 与 failed 分开：⚠️ 前者是**额度**用完了，
	// 追加额度点续跑就继续；后者是内容或配置有问题，续跑只会再失败一次。
	// 两者的下一步完全不同，合成一个状态等于让用户无从判断该做什么。
	// 恢复扫描的白名单里同样不含它——见 ListRecoverableExtractionJobs。
	jobStateBudgetExhausted = "budget_exhausted"

	itemStatePending   = "pending"
	itemStateRunning   = "running"
	itemStateSucceeded = "succeeded"
	itemStateFailed    = "failed"
)

// 三重预算的默认值（plan §预算）。三个维度各自独立，任一耗尽都停。
//
// ⚠️ 只有 item 上限会**在初始化时**直接拒绝：另外两个是运行期的闸门。
// 一份超过 item 上限的文档不该先跑掉几百次调用再停——那些钱白花了。
const (
	defaultApprovedItemLimit = 500
	defaultCallLimit         = 3000
	defaultActiveMsLimit     = 7200 * 1000
)

// extractionChunkPageSize 是枚举时每页取多少片段。
// ⚠️ 与 pgqueries 里 LIMIT ≤ 200 的口径一致，不在这里放大。
const extractionChunkPageSize = 200

var (
	// ErrExtractionSourceChanged：语料在初始化过程中变了，或者根本不是
	// 调用方以为的那个版本。
	//
	// ⚠️ 这个场景不报错、不留日志：游标翻页本身完全成功，只是翻到的东西
	// 横跨了两个版本。只能靠数量核对发现。
	ErrExtractionSourceChanged = apperr.Conflict(
		"knowledge.extraction_source_changed",
		"文档内容在准备抽取的过程中发生了变化，请重新开始")

	// ErrExtractionChunkMetadataMissing：某个片段没有叙事来源坐标。
	//
	// ⭐ 这里**让整个初始化失败，绝不跳过它并把总数减一**。跳过是最诱人的
	// 选项——作业照样跑得完，数字照样自洽。但那个总数从此不再是"全书"，
	// 而覆盖率、成本、召回率都是拿它当分母算的。一个静默变小的分母会让
	// 每一个比率都变好看。
	ErrExtractionChunkMetadataMissing = apperr.InvalidInput(
		"knowledge.extraction_chunk_metadata_missing",
		"该文档存在没有场景坐标的片段，请重新处理文档后再开启关系抽取")

	// ErrExtractionTooManyItems：超过 item 上限。在初始化时就拒绝，
	// 不先跑掉几百次调用再停。
	ErrExtractionTooManyItems = apperr.InvalidInput(
		"knowledge.extraction_too_many_items",
		"该文档的片段数超出单次抽取上限，请缩小范围或调大分块大小")
)

// RelationExtractionJob 是一次抽取运行的领域视图。
// 账目字段留给 T017 之后填，这里只放初始化需要的部分。
type RelationExtractionJob struct {
	ID                     string
	DocumentID             string
	KnowledgeBaseID        string
	DocumentVersion        int64
	RunNumber              int
	ModelID                string
	ConfigHash             []byte
	SourceHash             []byte
	State                  string
	Epoch                  int
	InitializationComplete bool
	TotalItems             int
	ApprovedItemLimit      int
	CallLimit              int
	ActiveMsLimit          int64
}

// extractionJobSpec 是初始化一次作业需要的全部输入。
type extractionJobSpec struct {
	JobID           string
	DocumentID      string
	KnowledgeBaseID string
	DocumentVersion int64
	RunNumber       int
	ModelID         string
	ConfigHash      []byte
	ConfigSnapshot  []byte
	OperationKey    []byte
	RequestHash     []byte

	ApprovedItemLimit int
	CallLimit         int
	ActiveMsLimit     int64

	// afterEnumerate 只给测试用：在枚举完成、核对之前注入一次并发改动。
	// ⚠️ 生产路径恒为 nil。这个竞态**没有任何自然发生的窗口可供测试**——
	// 它要求恰好在两次数据库往返之间有人重新发布文档，靠等是等不到的。
	afterEnumerate func() error

	// JobAlreadyExists 表示作业行已经由 enable/upload 登记好了（010 R6-01），
	// 这次初始化只补 items 与完成标志。
	//
	// ⚠️ 不区分的话，给一个已存在的 pending 意图做初始化会撞主键——
	// 而"从零开一个 run"和"把一个意图落实成作业"是两条真实存在的路径，
	// 前者至今没有生产调用方。
	JobAlreadyExists bool
}

// newExtractionJobSpec 造一个用默认预算的 spec。
func newExtractionJobSpec(jobID, documentID string, version int64, modelID string) extractionJobSpec {
	return extractionJobSpec{
		JobID: jobID, DocumentID: documentID, DocumentVersion: version, ModelID: modelID,
		KnowledgeBaseID: "kb-x", RunNumber: 1,
		ConfigHash: make([]byte, 32), ConfigSnapshot: []byte("{}"),
		ApprovedItemLimit: defaultApprovedItemLimit,
		CallLimit:         defaultCallLimit,
		ActiveMsLimit:     defaultActiveMsLimit,
	}
}

// initializeExtractionJob 枚举语料、建作业与全部待处理项，并把文档指向它。
//
// ⭐ 顺序不可调换，每一步都在防一件具体的事：
//  1. 先确认文档确实是 ready 且版本相符 —— 否则作业会挂到一批不是真相的片段上；
//  2. 游标枚举全部已发布片段 —— 走 000006 新建的复合索引；
//  3. 每一片段都必须有可用的场景坐标 —— 缺一个就整体失败（见上面的注释）；
//  4. 与数据库现查的总数核对 —— 抓住枚举中途的重新发布；
//  5. 一个 MySQL 事务里写 job + items + 完成标志 + 文档指针。
//
// ⚠️ 第 5 步是一个事务不是四个。分开的话崩溃点会落在中间，留下一个没有任何
// 东西指向它的作业，或者一个指向不存在作业的文档指针——两者都不报错。
func (r *Repository) initializeExtractionJob(ctx context.Context, spec extractionJobSpec) (RelationExtractionJob, error) {
	doc, err := r.queries.GetDocumentExtractionState(ctx, spec.DocumentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RelationExtractionJob{}, ErrDocumentNotFound
		}
		return RelationExtractionJob{}, fmt.Errorf("knowledge: load document for extraction: %w", err)
	}
	if doc.Status != StatusReady || doc.Version != spec.DocumentVersion {
		return RelationExtractionJob{}, ErrExtractionSourceChanged
	}

	rows, err := r.enumeratePublishedNarrativeChunks(ctx, spec.DocumentID, spec.DocumentVersion)
	if err != nil {
		return RelationExtractionJob{}, err
	}
	if len(rows) == 0 {
		return RelationExtractionJob{}, ErrEmptyContent
	}
	if len(rows) > spec.ApprovedItemLimit {
		return RelationExtractionJob{}, ErrExtractionTooManyItems
	}
	for _, row := range rows {
		meta, err := decodeNarrativeMetadata(row.NarrativeMetadata)
		if err != nil {
			return RelationExtractionJob{}, fmt.Errorf("knowledge: chunk %s: %w", row.ID, err)
		}
		if meta == nil {
			return RelationExtractionJob{}, ErrExtractionChunkMetadataMissing
		}
	}

	if spec.afterEnumerate != nil {
		if err := spec.afterEnumerate(); err != nil {
			return RelationExtractionJob{}, err
		}
	}

	// 核对：枚举出来的数量必须与现查总数一致。
	total, err := r.pgQueries.CountChunksByDocumentVersion(ctx,
		pggen.CountChunksByDocumentVersionParams{
			DocumentID: spec.DocumentID, DocumentVersion: spec.DocumentVersion})
	if err != nil {
		return RelationExtractionJob{}, fmt.Errorf("knowledge: count published chunks: %w", err)
	}
	if int(total) != len(rows) {
		return RelationExtractionJob{}, ErrExtractionSourceChanged
	}

	sourceHash := hashPublishedChunks(rows)
	startedAt := time.Now().UTC()

	err = platform.WithTx(ctx, r.db, func(tx *sql.Tx) error {
		q := r.queries.WithTx(tx)
		// ⭐ 锁顺序 document → job → item：每个涉及多张表的事务都从锁文档
		// 开始，顺序不一致会造成偶发死锁（见 LockDocumentForExtraction）。
		if _, err := q.LockDocumentForExtraction(ctx, spec.DocumentID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrDocumentNotFound
			}
			return fmt.Errorf("knowledge: lock document: %w", err)
		}
		if !spec.JobAlreadyExists {
			if err := q.CreateRelationExtractionJob(ctx, gen.CreateRelationExtractionJobParams{
				ID: spec.JobID, DocumentID: spec.DocumentID, KnowledgeBaseID: spec.KnowledgeBaseID,
				DocumentVersion: int32(spec.DocumentVersion), RunNumber: int32(spec.RunNumber),
				ModelID: spec.ModelID, ConfigHash: spec.ConfigHash, ConfigSnapshot: spec.ConfigSnapshot,
				ApprovedItemLimit: int32(spec.ApprovedItemLimit), CallLimit: int32(spec.CallLimit),
				ActiveMsLimit:        spec.ActiveMsLimit,
				OperationKeyHash:     nullBytes(spec.OperationKey),
				OperationRequestHash: nullBytes(spec.RequestHash),
			}); err != nil {
				return fmt.Errorf("knowledge: create extraction job: %w", err)
			}
		}
		for _, row := range rows {
			sum := sha256.Sum256([]byte(row.Content))
			if err := q.CreateRelationExtractionItem(ctx, gen.CreateRelationExtractionItemParams{
				ID: platform.NewID(), JobID: spec.JobID, ChunkID: row.ID,
				ChunkIndex: row.ChunkIndex, ContentHash: sum[:],
			}); err != nil {
				return fmt.Errorf("knowledge: create extraction item: %w", err)
			}
		}
		n, err := q.CompleteJobInitialization(ctx, gen.CompleteJobInitializationParams{
			TotalItems: int32(len(rows)), SourceHash: nullBytes(sourceHash),
			StartedAt: sql.NullTime{Time: startedAt, Valid: true}, ID: spec.JobID,
		})
		if err != nil {
			return fmt.Errorf("knowledge: complete initialization: %w", err)
		}
		if n == 0 {
			// 另一个 worker 已经初始化过这个 job。
			return ErrExtractionSourceChanged
		}
		// ⭐ 把这份文档上此前的作业全部标为 superseded，**与建新作业同事务**。
		//
		// ⚠️ 只把文档指针改到新作业是不够的：旧作业的 state 还是 running，
		// 恢复扫描会把它当成"崩溃的作业"捡回来接着跑——于是两个 run 同时对
		// 同一份文档花钱，而两者看起来都健康。
		if _, err := q.SupersedePriorExtractionJobs(ctx, gen.SupersedePriorExtractionJobsParams{
			FinishedAt: sql.NullTime{Time: startedAt, Valid: true},
			DocumentID: spec.DocumentID, ID: spec.JobID,
		}); err != nil {
			return fmt.Errorf("knowledge: supersede prior jobs: %w", err)
		}
		m, err := q.SetDocumentRelationJob(ctx, gen.SetDocumentRelationJobParams{
			ActiveRelationJobID: sql.NullString{String: spec.JobID, Valid: true},
			RelationModelID:     sql.NullString{String: spec.ModelID, Valid: true},
			ID:                  spec.DocumentID, Version: spec.DocumentVersion,
		})
		if err != nil {
			return fmt.Errorf("knowledge: point document at job: %w", err)
		}
		if m == 0 {
			// 文档在这中间改了版本或状态：整个事务回滚。
			return ErrExtractionSourceChanged
		}
		return nil
	})
	if err != nil {
		return RelationExtractionJob{}, err
	}

	return RelationExtractionJob{
		ID: spec.JobID, DocumentID: spec.DocumentID, KnowledgeBaseID: spec.KnowledgeBaseID,
		DocumentVersion: spec.DocumentVersion, RunNumber: spec.RunNumber, ModelID: spec.ModelID,
		ConfigHash: spec.ConfigHash, SourceHash: sourceHash,
		State: jobStateRunning, InitializationComplete: true, TotalItems: len(rows),
		ApprovedItemLimit: spec.ApprovedItemLimit, CallLimit: spec.CallLimit,
		ActiveMsLimit: spec.ActiveMsLimit,
	}, nil
}

// enumeratePublishedNarrativeChunks 按 chunk_index 游标翻页取全部已发布片段。
//
// ⚠️ 用游标而不是一次 SELECT 全量：一份 2000 片段的文档一次性拉回来是几 MB
// 的 JSON 元数据。翻页的代价是中途可能撞上重新发布——所以调用方必须做数量核对。
func (r *Repository) enumeratePublishedNarrativeChunks(ctx context.Context, documentID string, version int64) ([]pggen.ListPublishedNarrativeChunksRow, error) {
	var out []pggen.ListPublishedNarrativeChunksRow
	after := int32(-1)
	for {
		page, err := r.pgQueries.ListPublishedNarrativeChunks(ctx,
			pggen.ListPublishedNarrativeChunksParams{
				DocumentID: documentID, DocumentVersion: version,
				ChunkIndex: after, Limit: extractionChunkPageSize,
			})
		if err != nil {
			return nil, fmt.Errorf("knowledge: enumerate published chunks: %w", err)
		}
		if len(page) == 0 {
			return out, nil
		}
		out = append(out, page...)
		after = page[len(page)-1].ChunkIndex
	}
}

// hashPublishedChunks 算语料指纹。
//
// ⚠️ 内容之间用**长度前缀**分隔，不用分隔符拼接：两个片段 "ab"+"c" 与
// "a"+"bc" 拼起来完全一样，会让两份不同的语料算出同一个 hash。
// 没有它，一次 restart 无法区分"同一本书重跑"和"换了内容重跑"，
// 而两者的账目和指标不能混在一起比。
func hashPublishedChunks(rows []pggen.ListPublishedNarrativeChunksRow) []byte {
	h := sha256.New()
	var buf [8]byte
	for _, row := range rows {
		binary.BigEndian.PutUint64(buf[:], uint64(len(row.Content)))
		h.Write(buf[:])
		h.Write([]byte(row.Content))
	}
	return h.Sum(nil)
}

// nullBytes 把可空的 BINARY(32) 列在 Go 侧的 sql.NullString 表示上桥接。
//
// ⚠️ 空切片映射成 SQL NULL 而不是零长度字符串：这几列的 NULL 有确定含义
// （"还没算出来"），而一个零长度的 hash 会满足唯一约束并参与比较，
// 让两个"都还没算"的行看起来相同。
func nullBytes(b []byte) sql.NullString {
	if len(b) == 0 {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}
