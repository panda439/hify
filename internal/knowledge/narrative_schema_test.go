package knowledge

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"hify/internal/db/pggen"
	"hify/internal/testutil"
)

// narrative_schema_test.go 验证 000017 / PG 000006 写下的约束**真的会拒绝**
// 违规行，而不只是写在迁移文件里好看。
//
// ⭐ 这里每一条约束挡的都是同一类事故：**一条看起来完全正常、但含义是假的行**。
// 「非叙事文档开了抽取」不会报错，只会产出一堆没有场景归属的三元组；
// 「usage 未知却把 token 记成 0」不会报错，只会让"没测到"和"真的没花"
// 变得无法区分。数据库拒绝它们，是因为这些错误在运行时不会自己暴露。

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec failed: %v\n%s", err, q)
	}
}

// wantRejected 断言这条写入被数据库拒绝。⚠️ 它不检查具体错误码——
// 只要"被拒绝"即可；断言错误码会让测试绑死在 MySQL 的错误编号上。
func wantRejected(t *testing.T, db *sql.DB, what, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err == nil {
		t.Errorf("%s：这行本该被拒绝，却写进去了", what)
	}
}

func seedNarrativeDoc(t *testing.T, db *sql.DB, id string, narrative, extraction int) error {
	t.Helper()
	_, err := db.Exec(`INSERT INTO documents
		(id, knowledge_base_id, file_name, file_type, file_size, storage_path,
		 status, chunk_count, created_by, is_narrative, is_relation_extraction_enabled)
		VALUES (?, 'kb-1', 'a.txt', 'txt', 1, '/tmp/a', 'ready', 0, 'u1', ?, ?)`,
		id, narrative, extraction)
	return err
}

// TestExtractionRequiresNarrativeDocument——抽取开关只能开在叙事文档上。
// 违反时的表现不是报错，而是初始化阶段拿不到 scene_key，然后**静默产出
// 一批没有场景归属的关系记录**：数量、名字、类型全都正常，只是没法定位。
func TestExtractionRequiresNarrativeDocument(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	if err := seedNarrativeDoc(t, db, "d-ok", 1, 1); err != nil {
		t.Fatalf("叙事文档开抽取本该允许：%v", err)
	}
	if err := seedNarrativeDoc(t, db, "d-plain", 0, 0); err != nil {
		t.Fatalf("普通文档不开抽取本该允许：%v", err)
	}
	if err := seedNarrativeDoc(t, db, "d-bad", 0, 1); err == nil {
		t.Error("非叙事文档开抽取本该被拒绝")
	}
}

// TestDocumentDefaultsKeepOldBehaviour——存量/新建文档不写新列时必须全零。
// 任一新列默认值不是 0，都会让既有文档突然进入叙事路径。
func TestDocumentDefaultsKeepOldBehaviour(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	mustExec(t, db, `INSERT INTO documents
		(id, knowledge_base_id, file_name, file_type, file_size, storage_path,
		 status, chunk_count, created_by)
		VALUES ('d-legacy','kb-1','a.txt','txt',1,'/tmp/a','ready',0,'u1')`)
	var narr, ext int
	var model, job sql.NullString
	if err := db.QueryRow(`SELECT is_narrative, is_relation_extraction_enabled,
		relation_model_id, active_relation_job_id FROM documents WHERE id='d-legacy'`).
		Scan(&narr, &ext, &model, &job); err != nil {
		t.Fatal(err)
	}
	if narr != 0 || ext != 0 || model.Valid || job.Valid {
		t.Errorf("新列默认值不是「关闭」：narrative=%d extraction=%d model=%v job=%v",
			narr, ext, model, job)
	}
}

// insertJob 按调用方给的前缀构造 id / document_id，返回 job id。
// ⚠️ testutil.MySQL 按 name 缓存**同一个库**给包内全部测试共用，
// 所以夹具 id 必须逐测试隔离。第一版全用 'j-1'/'ch-1'，第二个测试就撞主键，
// 报出来的是 Duplicate entry 而不是被测约束的失败——看着像约束写错了，
// 其实是夹具串了，这种失败最容易把人引到错误的方向。
func insertJob(t *testing.T, db *sql.DB, prefix string) string {
	t.Helper()
	id := prefix + "-job"
	mustExec(t, db, `INSERT INTO relation_extraction_jobs
		(id, document_id, knowledge_base_id, document_version, run_number,
		 model_id, config_hash, config_snapshot,
		 approved_item_limit, call_limit, active_ms_limit)
		VALUES (?, ?, 'kb-1', 1, 1, 'm-1', UNHEX(REPEAT('aa',32)), '{}', 500, 3000, 7200000)`,
		id, prefix+"-doc")
	return id
}

// TestAttemptUsageCannotFakeZero——⭐ 本文件里最要紧的一条。
// 不是所有兼容供应商都返回 token 用量。usage_known=0 时把 token 记成 0，
// 「没测到」和「真的没花」就永久不可区分了，而本期最重要的产出正是
// 一个可复核的成本数字。约束逼调用方要么记 NULL，要么如实置 usage_known=1。
func TestAttemptUsageCannotFakeZero(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	job := insertJob(t, db, "usage")
	mustExec(t, db, `INSERT INTO relation_extraction_items
		(id, job_id, chunk_id, chunk_index, content_hash)
		VALUES ('usage-item', ?, 'c-1', 0, UNHEX(REPEAT('bb',32)))`, job)

	base := `INSERT INTO relation_extraction_attempts
		(id, job_id, item_id, epoch, phase, attempt_number, request_hash, max_output_tokens,
		 state, usage_known, input_tokens, output_tokens, cost_kind, cost_amount)
		VALUES (?, '` + job + `', 'usage-item', 1, 'extract', ?,
		        UNHEX(REPEAT('cc',32)), 2048, ?, ?, ?, ?, ?, ?)`

	// 合法：未知用量 → token 为 NULL
	mustExec(t, db, base, "a-1", 1, "unknown", 0, nil, nil, "not_applicable", nil)
	// 合法：已知用量 → token 有值
	mustExec(t, db, base, "a-2", 2, "completed", 1, 120, 30, "not_applicable", nil)

	wantRejected(t, db, "usage_known=0 却记了 token（拿 0 冒充未知）",
		base, "a-3", 3, "completed", 0, 0, 0, "not_applicable", nil)
	wantRejected(t, db, "cost_kind=measured 却没有金额",
		base, "a-4", 4, "completed", 1, 1, 1, "measured", nil)
	wantRejected(t, db, "cost_kind=not_applicable 却填了金额",
		base, "a-5", 5, "completed", 1, 1, 1, "not_applicable", "0.5")
	wantRejected(t, db, "同一 item/phase 的 attempt_number 重复",
		base, "a-6", 1, "completed", 1, 1, 1, "not_applicable", nil)
}

// TestAliasSupportedMustPointAtACharacter——只有 supported 才能指向人物。
// ambiguous 自动挑一个，就是在制造一条无法追责的人物合并。
func TestAliasSupportedMustPointAtACharacter(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	job := insertJob(t, db, "alias")
	mustExec(t, db, `INSERT INTO narrative_characters
		(id, job_id, display_name, first_source_order) VALUES ('alias-ch', ?, '阿Q', 0)`, job)

	ins := `INSERT INTO narrative_aliases
		(id, job_id, character_id, surface, surface_hash, state, evidence,
		 first_source_order, decision_key_hash)
		VALUES (?, '` + job + `', ?, ?, UNHEX(REPEAT('dd',32)), ?, '{}', 0, UNHEX(?))`
	mustExec(t, db, ins, "al-1", "alias-ch", "阿Q", "supported", strings.Repeat("11", 32))
	mustExec(t, db, ins, "al-2", nil, "阿贵", "ambiguous", strings.Repeat("22", 32))
	wantRejected(t, db, "supported 却没有指向任何人物",
		ins, "al-3", nil, "老Q", "supported", strings.Repeat("33", 32))
}

// TestSameSurfaceMayBelongToDifferentCharacters——⚠️ 反向断言：
// 这里要证明数据库**没有**按名字唯一的约束。一本书里可以有两个「老王」，
// 而按名字合并之后再也分不开。
func TestSameSurfaceMayBelongToDifferentCharacters(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	job := insertJob(t, db, "surface")
	mustExec(t, db, `INSERT INTO narrative_characters (id, job_id, display_name, first_source_order)
		VALUES ('sf-1', ?, '老王', 0), ('sf-2', ?, '老王', 900)`, job, job)
	ins := `INSERT INTO narrative_aliases
		(id, job_id, character_id, surface, surface_hash, state, evidence,
		 first_source_order, decision_key_hash)
		VALUES (?, '` + job + `', ?, '老王', UNHEX(REPEAT('ee',32)), 'supported', '{}', 0, UNHEX(?))`
	mustExec(t, db, ins, "sfa-1", "sf-1", strings.Repeat("11", 32))
	if _, err := db.Exec(ins, "sfa-2", "sf-2", strings.Repeat("22", 32)); err != nil {
		t.Fatalf("同名不同人本该允许，却被拒绝了——说明存在按名字唯一的约束：%v", err)
	}
}

// TestRelationHistoryIsNotOverwritten——同一对人物、同一类型，在不同出处
// 各存一条。⭐ 这是整个功能的立论：关系随剧情变化，覆盖写会让全书只剩
// 最后一个状态，而那恰恰是「人物关系」这类问题最没用的答案。
func TestRelationHistoryIsNotOverwritten(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	job := insertJob(t, db, "hist")
	mustExec(t, db, `INSERT INTO narrative_characters (id, job_id, display_name, first_source_order)
		VALUES ('hist-a', ?, '甲', 0), ('hist-b', ?, '乙', 1)`, job, job)
	ins := `INSERT INTO narrative_relations
		(id, job_id, subject_id, object_id, relation_type, relation_key_hash,
		 first_source_order, chapter_number)
		VALUES (?, '` + job + `', 'hist-a', 'hist-b', ?, UNHEX(?), ?, ?)`
	mustExec(t, db, ins, "r-1", "师徒", strings.Repeat("11", 32), 100, 3)
	mustExec(t, db, ins, "r-2", "反目", strings.Repeat("22", 32), 5000, 57)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM narrative_relations
		WHERE subject_id='hist-a' AND object_id='hist-b'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("同一对人物的两段关系只剩 %d 条——关系历史被覆盖了", n)
	}
	// 章节号未知必须能留空，不得填 0 冒充第 0 章。
	mustExec(t, db, ins, "r-3", "同乡", strings.Repeat("33", 32), 9000, nil)
	wantRejected(t, db, "章节号填 0（未知冒充第 0 章）",
		ins, "r-4", "同乡", strings.Repeat("44", 32), 9100, 0)
}

// TestEvidenceKeyIsNotChunkID——⚠️ 反向断言：相邻 chunk 因 overlap 会包含
// 同一段原文。证据唯一性若以 chunk_id 为依据，同一处出处会被记成两条，
// 从而**虚增**后面要写进报告的「证据条数」。这里证明唯一键落在
// evidence_key_hash 上：同一 relation + 同 key 只能有一条，
// 而不同 chunk_id 不构成新证据的理由。
func TestEvidenceKeyIsNotChunkID(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	job := insertJob(t, db, "ev")
	mustExec(t, db, `INSERT INTO narrative_characters (id, job_id, display_name, first_source_order)
		VALUES ('ev-a', ?, '甲', 0), ('ev-b', ?, '乙', 1)`, job, job)
	mustExec(t, db, `INSERT INTO narrative_relations
		(id, job_id, subject_id, object_id, relation_type, relation_key_hash, first_source_order)
		VALUES ('ev-rel', ?, 'ev-a', 'ev-b', '师徒', UNHEX(REPEAT('11',32)), 100)`, job)
	ins := `INSERT INTO narrative_relation_evidence
		(id, job_id, relation_id, chunk_id, document_version, source_order,
		 source_start, source_end, quote, evidence_key_hash)
		VALUES (?, '` + job + `', 'ev-rel', ?, 1, 100, ?, ?, '原文', UNHEX(?))`
	mustExec(t, db, ins, "e-1", "c-1", 10, 30, strings.Repeat("aa", 32))
	wantRejected(t, db, "同一 evidence_key 换个 chunk_id 就当成新证据",
		ins, "e-2", "c-2", 10, 30, strings.Repeat("aa", 32))
	wantRejected(t, db, "空区间（start >= end）",
		ins, "e-3", "c-3", 30, 30, strings.Repeat("bb", 32))
	wantRejected(t, db, "负数起点",
		ins, "e-4", "c-4", -1, 30, strings.Repeat("cc", 32))
}

// TestJobIdempotencyKeyPreventsDoubleRun——同一个 start/restart 请求重放
// 不能开出第二个 run；同一份文档同版本的 run_number 也不能重复。
func TestJobIdempotencyKeyPreventsDoubleRun(t *testing.T) {
	db := testutil.MySQL(t, "narrschema")
	ins := `INSERT INTO relation_extraction_jobs
		(id, document_id, knowledge_base_id, document_version, run_number,
		 model_id, config_hash, config_snapshot, approved_item_limit, call_limit,
		 active_ms_limit, operation_key_hash)
		VALUES (?, 'idem-doc','kb-1',1, ?, 'm-1', UNHEX(REPEAT('aa',32)), '{}', 500, 3000, 7200000, UNHEX(?))`
	mustExec(t, db, ins, "idem-1", 1, strings.Repeat("11", 32))
	wantRejected(t, db, "同一幂等键重放开出了第二个 run",
		ins, "idem-2", 2, strings.Repeat("11", 32))
	wantRejected(t, db, "同文档同版本的 run_number 重复",
		ins, "idem-3", 1, strings.Repeat("22", 32))
	// 换个幂等键、换个 run_number 才是合法的 restart。
	mustExec(t, db, ins, "idem-4", 2, strings.Repeat("22", 32))
}

// TestNarrativeMetadataDefaultsToNull——PG 侧：不写元数据的片段必须是 NULL，
// 关闭模式下的行为与改动前完全一致。
func TestNarrativeMetadataDefaultsToNull(t *testing.T) {
	db := testutil.Postgres(t, "narrschema")
	mustExec(t, db, `INSERT INTO chunks
		(id, knowledge_base_id, document_id, document_version, chunk_index,
		 content, content_length, embedding, embedding_dimension)
		VALUES ('c-1','kb-1','d-1',1,0,'正文',2,'[0.1]'::vector,1)`)
	var meta sql.NullString
	if err := db.QueryRow(`SELECT narrative_metadata FROM chunks WHERE id='c-1'`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.Valid {
		t.Errorf("未开叙事模式的片段带上了元数据：%s", meta.String)
	}
}

// ---- 元数据落库往返（PG 000006）----

func narrativeRepo(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(testutil.MySQL(t, "narrschema"), testutil.Postgres(t, "narrschema"))
}

func narrativeChunk(id string, idx int, content string, meta *narrativeMetadata) Chunk {
	return Chunk{
		ID: id, KnowledgeBaseID: "kb-n", DocumentID: "doc-n", DocumentName: "书.txt",
		ChunkIndex: idx, Content: content, ContentLength: len([]rune(content)),
		Embedding: []float32{0.1}, EmbeddingDimension: 1, NarrativeMetadata: meta,
	}
}

// TestNarrativeMetadataRoundTrip——写进去什么，读回来必须是什么。
// ⚠️ 一并盯着「同一条 INSERT 写入」：元数据不是事后 UPDATE 补的，
// 否则崩溃点会落在中间，留下一批有正文没有来源坐标的片段——
// 它们检索得到、任何引用都定位不了，而且不报错。
func TestNarrativeMetadataRoundTrip(t *testing.T) {
	repo := narrativeRepo(t)
	ctx := t.Context()

	pieces := chunkNarrative("第七章　甲\n"+strings.Repeat("甲的正文。", 60)+"\n", 150, 30)
	if len(pieces) < 3 {
		t.Fatalf("夹具只切出 %d 块", len(pieces))
	}
	chunks := make([]Chunk, 0, len(pieces))
	for i, p := range pieces {
		chunks = append(chunks, narrativeChunk(fmt.Sprintf("nc-%d", i), i, p.Content, p.Narrative))
	}
	if err := repo.createChunks(ctx, chunks, 1); err != nil {
		t.Fatalf("createChunks: %v", err)
	}
	if err := repo.publishDocumentVersion(ctx, "doc-n", 1); err != nil {
		t.Fatalf("publish: %v", err)
	}

	rows, err := repo.pgQueries.ListPublishedNarrativeChunks(ctx,
		pggen.ListPublishedNarrativeChunksParams{
			DocumentID: "doc-n", DocumentVersion: 1, ChunkIndex: -1, Limit: 200,
		})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != len(chunks) {
		t.Fatalf("读回 %d 块，写了 %d 块", len(rows), len(chunks))
	}
	for i, row := range rows {
		got, err := decodeNarrativeMetadata(row.NarrativeMetadata)
		if err != nil {
			t.Fatalf("第 %d 块元数据解码失败：%v", i, err)
		}
		if got == nil {
			t.Fatalf("第 %d 块元数据是 NULL", i)
		}
		want := pieces[i].Narrative
		if got.SourceOrder != want.SourceOrder ||
			got.NormalizedDocumentHash != want.NormalizedDocumentHash ||
			got.BoundaryKind != want.BoundaryKind ||
			len(got.Segments) != len(want.Segments) {
			t.Errorf("第 %d 块往返后不一致：\n got=%+v\nwant=%+v", i, *got, *want)
			continue
		}
		if got.ChapterNumber == nil || *got.ChapterNumber != 7 {
			t.Errorf("第 %d 块章节号往返丢了：%v", i, got.ChapterNumber)
		}
		for j := range got.Segments {
			// ⚠️ 不能直接用 != 比 narrativeSegment：它有指针字段，
			// 比的是地址而不是值，往返后必然不相等——第一版就是这么写的，
			// 报出来的差异里两边数字一模一样、只有指针不同。
			if !sameSegment(got.Segments[j], want.Segments[j]) {
				t.Errorf("第 %d 块段 %d 往返后不一致：%s vs %s",
					i, j, showSegment(got.Segments[j]), showSegment(want.Segments[j]))
			}
		}
		// 存回来的区间必须仍然指得回原文（不是只有结构对）。
		if err := validateNarrativeMetadata(*got, len([]rune(row.Content))); err != nil {
			t.Errorf("第 %d 块读回后校验不过：%v", i, err)
		}
	}
}

// TestNonNarrativeChunkStoresNull——关闭模式必须与改动前逐字节一致。
// 这一列默认写成 '{}' 之类的"空对象"就会让存量片段看起来像叙事片段。
func TestNonNarrativeChunkStoresNull(t *testing.T) {
	repo := narrativeRepo(t)
	ctx := t.Context()
	if err := repo.createChunks(ctx, []Chunk{
		narrativeChunk("plain-0", 0, "普通片段", nil),
	}, 7); err != nil {
		t.Fatalf("createChunks: %v", err)
	}
	var meta sql.NullString
	if err := repo.pgdb.QueryRowContext(ctx,
		`SELECT narrative_metadata FROM chunks WHERE id='plain-0'`).Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.Valid {
		t.Errorf("非叙事片段存成了 %q，应为 NULL", meta.String)
	}
}

// TestBatchLookupFiltersUnpublished——⭐ 批量核验必须过滤 is_published。
// 一条引用如果指向已被取代的版本，展示出来的原文和用户现在看到的文档对不上，
// 而两边都不会报错。
func TestBatchLookupFiltersUnpublished(t *testing.T) {
	repo := narrativeRepo(t)
	ctx := t.Context()
	mk := func(id string, idx int) Chunk { return narrativeChunk(id, idx, "正文"+id, nil) }
	if err := repo.createChunks(ctx, []Chunk{mk("pub-0", 0), mk("pub-1", 1)}, 3); err != nil {
		t.Fatal(err)
	}
	if err := repo.publishDocumentVersion(ctx, "doc-n", 3); err != nil {
		t.Fatal(err)
	}
	// 第 4 版写进去但**不发布**——它不是真相。
	if err := repo.createChunks(ctx, []Chunk{mk("draft-0", 0)}, 4); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.pgQueries.GetPublishedNarrativeChunksByIDs(ctx,
		pggen.GetPublishedNarrativeChunksByIDsParams{
			DocumentID: "doc-n", DocumentVersion: 4, Column3: []string{"draft-0", "pub-0"},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("未发布版本的片段被核验通过了：%d 条", len(rows))
	}
}

func sameIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameSegment(a, b narrativeSegment) bool {
	return a.ChunkStart == b.ChunkStart && a.ChunkEnd == b.ChunkEnd &&
		sameIntPtr(a.DocumentStart, b.DocumentStart) &&
		sameIntPtr(a.DocumentEnd, b.DocumentEnd) &&
		sameIntPtr(a.Page, b.Page) &&
		a.IsGeneratedSeparator == b.IsGeneratedSeparator &&
		a.IsOverlapCopy == b.IsOverlapCopy
}

func showSegment(s narrativeSegment) string {
	d := "nil"
	if s.DocumentStart != nil && s.DocumentEnd != nil {
		d = fmt.Sprintf("[%d,%d)", *s.DocumentStart, *s.DocumentEnd)
	}
	return fmt.Sprintf("chunk[%d,%d) doc%s copy=%v sep=%v",
		s.ChunkStart, s.ChunkEnd, d, s.IsOverlapCopy, s.IsGeneratedSeparator)
}
