package knowledge

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/testutil"
)

// extraction_init_test.go 守关系抽取作业的**初始化**（010 T015）。
//
// ⭐ 初始化只做一件事，但这件事必须绝对可靠：把「这份文档当前已发布的全部
// 片段」一条不多一条不少地变成待处理项。数量错了的后果不是报错——
// 是最后那个"覆盖了全书多少"的数字悄悄偏小，而每一项看起来都正常。
//
// ⚠️ 因此这里的断言几乎全都在盯**数量**和**拒绝**，而不是"能不能跑通"。

// seedNarrativeDocument 建一份 ready 的叙事文档，并把它的片段发布出去。
func seedNarrativeDocument(t *testing.T, repo *Repository, docID string, chunks int) []string {
	t.Helper()
	ctx := t.Context()
	body := ""
	for i := 1; i <= chunks; i++ {
		body += fmt.Sprintf("第%s章　题\n", chineseOrdinal(i)) + strings.Repeat("正文。", 20) + "\n"
	}
	pieces := chunkNarrative(body, 500, 0)
	if len(pieces) != chunks {
		t.Fatalf("夹具想要 %d 块，实际切出 %d 块", chunks, len(pieces))
	}
	// ⚠️ is_relation_extraction_enabled 必须是 1：一个抽取作业只可能存在于
	// 开着这个开关的文档上（010 R6-02 之后发布守卫真的核对它）。
	// 夹具留 0 等于造了一个生产中不可能出现的状态。
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO documents
		(id, knowledge_base_id, file_name, file_type, file_size, storage_path,
		 status, chunk_count, created_by, is_narrative, is_relation_extraction_enabled)
		VALUES (?, 'kb-x', 'novel.txt', 'txt', 1, '/tmp/n', 'ready', ?, 'u1', 1, 1)`,
		docID, chunks); err != nil {
		t.Fatal(err)
	}
	var ids []string
	cs := make([]Chunk, 0, chunks)
	for i, p := range pieces {
		id := fmt.Sprintf("%s-c%d", docID, i)
		ids = append(ids, id)
		cs = append(cs, Chunk{ID: id, KnowledgeBaseID: "kb-x", DocumentID: docID,
			DocumentName: "novel.txt", ChunkIndex: i, Content: p.Content,
			ContentLength: len([]rune(p.Content)), Embedding: []float32{0.1},
			EmbeddingDimension: 1, NarrativeMetadata: p.Narrative})
	}
	if err := repo.createChunks(ctx, cs, 1); err != nil {
		t.Fatal(err)
	}
	if err := repo.publishDocumentVersion(ctx, docID, 1); err != nil {
		t.Fatal(err)
	}
	return ids
}

func chineseOrdinal(n int) string {
	digits := []string{"零", "一", "二", "三", "四", "五", "六", "七", "八", "九"}
	if n < 10 {
		return digits[n]
	}
	if n < 20 {
		return "十" + digits[n%10]
	}
	return digits[n/10] + "十" + digits[n%10]
}

func extractionRepo(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(testutil.MySQL(t, "extraction"), testutil.Postgres(t, "extraction"))
}

// TestInitializationEnumeratesEveryPublishedChunk——一条不多一条不少。
func TestInitializationEnumeratesEveryPublishedChunk(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	ids := seedNarrativeDocument(t, repo, "doc-init", 7)

	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-1", "doc-init", 1, "m-1"))
	if err != nil {
		t.Fatalf("initializeExtractionJob: %v", err)
	}
	if job.TotalItems != len(ids) {
		t.Errorf("total_items = %d, want %d", job.TotalItems, len(ids))
	}
	if job.State != jobStateRunning || !job.InitializationComplete {
		t.Errorf("初始化没有提交：state=%q complete=%v", job.State, job.InitializationComplete)
	}
	// 每个 item 的 chunk_id 必须真的对应一个已发布片段，且 chunk_index 连续。
	rows, err := repo.db.QueryContext(ctx,
		`SELECT chunk_id, chunk_index FROM relation_extraction_items WHERE job_id=? ORDER BY chunk_index`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for i := 0; rows.Next(); i++ {
		var cid string
		var idx int
		if err := rows.Scan(&cid, &idx); err != nil {
			t.Fatal(err)
		}
		if idx != i {
			t.Errorf("第 %d 个 item 的 chunk_index = %d", i, idx)
		}
		got = append(got, cid)
	}
	if len(got) != len(ids) {
		t.Fatalf("建了 %d 个 item，已发布片段有 %d 个", len(got), len(ids))
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Errorf("第 %d 个 item 指向 %s，应为 %s", i, got[i], ids[i])
		}
	}
}

// TestInitializationRefusesChunkWithoutMetadata——⭐ 缺元数据的片段
// **让整个作业初始化失败**，绝不"跳过它并把总数减一"。
//
// ⚠️ 跳过是最诱人的选项：作业照样跑得完，数字照样自洽。但那个总数从此
// 不再是"全书"，而后面所有覆盖率、成本、召回率都是拿它当分母算的。
// 一个静默变小的分母会让每一个比率都变好看。
func TestInitializationRefusesChunkWithoutMetadata(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-nometa", 4)
	// 把其中一块的元数据抹掉，模拟"这份文档不是叙事模式入库的"。
	if _, err := repo.pgdb.ExecContext(ctx,
		`UPDATE chunks SET narrative_metadata = NULL WHERE id = 'doc-nometa-c2'`); err != nil {
		t.Fatal(err)
	}

	_, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-nm", "doc-nometa", 1, "m-1"))
	if !errors.Is(err, ErrExtractionChunkMetadataMissing) {
		t.Fatalf("err = %v, want ErrExtractionChunkMetadataMissing", err)
	}
	// 失败必须什么都不留下。
	var items, jobs int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM relation_extraction_items WHERE job_id='job-nm'),
		        (SELECT COUNT(*) FROM relation_extraction_jobs WHERE id='job-nm')`).Scan(&items, &jobs); err != nil {
		t.Fatal(err)
	}
	if items != 0 || jobs != 0 {
		t.Errorf("初始化失败却留下了 %d 个 item、%d 个 job", items, jobs)
	}
	// 文档的事实状态不变。
	var status string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT status FROM documents WHERE id='doc-nometa'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StatusReady {
		t.Errorf("初始化失败改动了文档状态：%q", status)
	}
}

// TestInitializationRefusesWhenCountDisagrees——⭐ 枚举出来的数量必须与
// 数据库现查的总数一致。不一致意味着枚举**中途**有人重新发布了这份文档，
// 这时的 item 集合是两个版本的混合物。
//
// ⚠️ 这个场景不会报错、不会有半条日志：游标翻页本身完全成功，
// 只是翻到的东西横跨了两个版本。
func TestInitializationRefusesWhenCountDisagrees(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-race", 5)

	spec := newExtractionJobSpec("job-race", "doc-race", 1, "m-1")
	// 注入：枚举完成之后、校验之前，再发布一块进来。
	spec.afterEnumerate = func() error {
		_, err := repo.pgdb.ExecContext(ctx, `INSERT INTO chunks
			(id, knowledge_base_id, document_id, document_version, chunk_index, content,
			 content_length, embedding, embedding_dimension, document_name, is_published)
			VALUES ('doc-race-late','kb-x','doc-race',1,99,'迟到的正文',5,'[0.1]'::vector,1,'novel.txt',true)`)
		return err
	}
	if _, err := repo.initializeExtractionJob(ctx, spec); !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v, want ErrExtractionSourceChanged", err)
	}
}

// TestInitializationIsGuardedAgainstDoubleCommit——两个 worker 同时初始化
// 同一个 job 时只有一个能提交。⚠️ 没有守卫的话第二个会把 total_items 覆盖成
// 自己数出来的值，而它枚举的可能是另一个版本，数字看起来完全正常。
func TestInitializationIsGuardedAgainstDoubleCommit(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-twice", 3)

	if _, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-t", "doc-twice", 1, "m-1")); err != nil {
		t.Fatal(err)
	}
	// 同一个 job id 再初始化一次：主键就该拦住它。
	if _, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-t", "doc-twice", 1, "m-1")); err == nil {
		t.Error("同一个 job 被初始化了两次")
	}
	var total int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT total_items FROM relation_extraction_jobs WHERE id='job-t'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total_items 被改成了 %d", total)
	}
}

// TestInitializationPointsDocumentAtTheJob——文档的 active_relation_job_id
// 与作业创建必须在**同一个事务**里。分开的话崩溃点会落在中间，留下一个
// 没有任何东西指向它的作业，或者一个指向不存在作业的文档指针。
func TestInitializationPointsDocumentAtTheJob(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-ptr", 2)

	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-p", "doc-ptr", 1, "m-7"))
	if err != nil {
		t.Fatal(err)
	}
	row, err := repo.queries.GetDocumentExtractionState(ctx, "doc-ptr")
	if err != nil {
		t.Fatal(err)
	}
	if !row.ActiveRelationJobID.Valid || row.ActiveRelationJobID.String != job.ID {
		t.Errorf("文档没有指向新作业：%v", row.ActiveRelationJobID)
	}
	if !row.RelationModelID.Valid || row.RelationModelID.String != "m-7" {
		t.Errorf("所选模型没有落下：%v", row.RelationModelID)
	}
}

// TestInitializationRefusesStaleVersion——文档已经改了版本，这次开启必须失败，
// 而不是把作业挂到一批已经不是真相的片段上。
func TestInitializationRefusesStaleVersion(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-stale", 2)

	spec := newExtractionJobSpec("job-s", "doc-stale", 99, "m-1") // 版本对不上
	if _, err := repo.initializeExtractionJob(ctx, spec); !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v, want ErrExtractionSourceChanged", err)
	}
}

// TestInitializationRecordsSourceHash——语料 hash 必须落下来。
// ⚠️ 没有它，一次 restart 无法区分"同一本书重跑"和"换了内容重跑"，
// 而两者的账目和指标不能混在一起比。
func TestInitializationRecordsSourceHash(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-hash", 3)

	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-h", "doc-hash", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(job.SourceHash) != 32 {
		t.Fatalf("source_hash 长度 = %d, want 32", len(job.SourceHash))
	}
	// 同一份语料两次算出来必须相同。
	rows, err := repo.pgQueries.ListPublishedNarrativeChunks(ctx,
		pggen.ListPublishedNarrativeChunksParams{
			DocumentID: "doc-hash", DocumentVersion: 1, ChunkIndex: -1, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if got := hashPublishedChunks(rows); string(got) != string(job.SourceHash) {
		t.Error("source_hash 不可复现")
	}
}

// TestInitializationRollsBackWhenDocumentMovesUnderIt——⭐ 变异测试逼出来的缺口。
//
// 前面那条"版本对不上"的用例被**入口处**的检查拦下了，事务里那道
// SetDocumentRelationJob 的守卫从来没被走到过——把它删掉，测试全绿。
// 这里在枚举之后、事务之前把文档版本改掉，让入口检查已经通过、
// 只剩事务内的守卫能拦。⚠️ 拦不住的后果是作业被挂到一批已经不是真相的片段上，
// 而作业本身、item、总数看起来全都正常。
func TestInitializationRollsBackWhenDocumentMovesUnderIt(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-moved", 3)

	spec := newExtractionJobSpec("job-moved", "doc-moved", 1, "m-1")
	spec.afterEnumerate = func() error {
		_, err := repo.db.ExecContext(ctx,
			`UPDATE documents SET version = version + 1 WHERE id = 'doc-moved'`)
		return err
	}
	if _, err := repo.initializeExtractionJob(ctx, spec); !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v, want ErrExtractionSourceChanged", err)
	}
	// 整个事务必须回滚：作业和 item 一个都不能留下。
	var jobs, items int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM relation_extraction_jobs WHERE id='job-moved'),
		        (SELECT COUNT(*) FROM relation_extraction_items WHERE job_id='job-moved')`).
		Scan(&jobs, &items); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 || items != 0 {
		t.Errorf("事务没有回滚：留下了 %d 个 job、%d 个 item", jobs, items)
	}
}

// TestCompleteInitializationIsIdempotentlyGuarded——⭐ 第二个变异逃逸的补课。
//
// CompleteJobInitialization 的守卫（state='initializing' 且未完成）目前
// 在 initializeExtractionJob 这条路径上**走不到**：job id 是新生成的，
// 主键冲突会先一步拦住重复初始化。但 T016 的恢复路径会去接手一个停在
// initializing 的作业，那时它就是唯一的防线，所以在查询这一层直接验它。
//
// ⚠️ 没有它，两个 worker 同时接手同一个作业，第二个会把 total_items 覆盖成
// 自己数出来的值——而它枚举的可能是另一个版本的片段，数字看起来完全正常。
func TestCompleteInitializationIsIdempotentlyGuarded(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-guard", 2)

	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-g", "doc-guard", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	// 模拟第二个 worker 接手同一个已完成的作业。
	n, err := repo.queries.CompleteJobInitialization(ctx, gen.CompleteJobInitializationParams{
		TotalItems: 999, SourceHash: nullBytes(make([]byte, 32)),
		StartedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}, ID: job.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("守卫没生效：第二次完成影响了 %d 行", n)
	}
	var total int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT total_items FROM relation_extraction_jobs WHERE id=?`, job.ID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("total_items 被覆盖成了 %d", total)
	}
}

// TestSourceHashSeparatesChunkBoundaries——⭐ 长度前缀不是装饰。
// 片段 ["ab","c"] 与 ["a","bc"] 直接拼起来完全一样。没有长度前缀，
// 两份**切法不同因而 item 集合不同**的语料会算出同一个指纹，
// 于是一次 restart 无法区分"同一本书重跑"和"换了分块配置重跑"——
// 而两者的账目和指标不能混在一起比。
func TestSourceHashSeparatesChunkBoundaries(t *testing.T) {
	mk := func(contents ...string) []pggen.ListPublishedNarrativeChunksRow {
		rows := make([]pggen.ListPublishedNarrativeChunksRow, 0, len(contents))
		for i, c := range contents {
			rows = append(rows, pggen.ListPublishedNarrativeChunksRow{
				ID: fmt.Sprintf("c%d", i), ChunkIndex: int32(i), Content: c})
		}
		return rows
	}
	a := hashPublishedChunks(mk("ab", "c"))
	b := hashPublishedChunks(mk("a", "bc"))
	if string(a) == string(b) {
		t.Error("切法不同的两份语料算出了同一个 source_hash")
	}
	if string(a) != string(hashPublishedChunks(mk("ab", "c"))) {
		t.Error("同一份语料两次算出的 source_hash 不同")
	}
}
