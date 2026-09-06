package knowledge

import (
	"context"
	"errors"
	"testing"
)

// relation_verify_test.go 盯着 010 T034 的核验（relation_verify.go）。
//
// ⭐ 这些测试要防的不是"核验报错了"，而是**核验静默通过**：
// 一条早已对不上的引用被原样送进模型，两边都不会报错。

func ptrInt(v int) *int { return &v }

// buildVerifyChunk 造一个"整块都可引用、文档坐标从 docStart 开始"的块。
func buildVerifyChunk(content string, docStart int) verifyChunk {
	n := len([]rune(content))
	end := docStart + n
	return verifyChunk{
		Content:      content,
		DocumentName: "阿Q正传.txt",
		Meta: &narrativeMetadata{
			SchemaVersion: narrativeMetadataSchemaVersion,
			BoundaryKind:  boundaryChapter,
			Segments: []narrativeSegment{{
				ChunkStart: 0, ChunkEnd: n,
				DocumentStart: ptrInt(docStart), DocumentEnd: ptrInt(end),
			}},
		},
	}
}

// TestVerifyKeepsCitationStillMatching——底线用例：没有任何变化时，
// 引用必须原样留下。⚠️ 少了它，一个"什么都丢掉"的实现能让下面每一条
// 测试都通过。
func TestVerifyKeepsCitationStillMatching(t *testing.T) {
	chunks := map[string]verifyChunk{"c1": buildVerifyChunk("阿Q走进未庄的酒店", 100)}
	kept, dropped := verifyCitationsAgainstChunks([]RelationCitation{{
		ChunkID: "c1", Quote: "未庄", SourceStart: 104, SourceEnd: 106,
	}}, chunks)
	if dropped != 0 || len(kept) != 1 {
		t.Fatalf("对得上的引用被丢掉了：kept=%d dropped=%d", len(kept), dropped)
	}
}

// TestVerifyDropsCitationWhoseTextChanged——⭐ 文档被重新处理之后，
// 同一个区间落在了别的字上。
//
// ⚠️ 这正是不核验时最坏的那个后果：用户点开引用看到的原文是真的，
// 与回答里那句"证据"却毫无关系，而系统的每一层都认为自己工作正常。
func TestVerifyDropsCitationWhoseTextChanged(t *testing.T) {
	chunks := map[string]verifyChunk{"c1": buildVerifyChunk("阿Q走进未庄的酒店", 100)}
	kept, dropped := verifyCitationsAgainstChunks([]RelationCitation{{
		ChunkID: "c1", Quote: "赵府", SourceStart: 104, SourceEnd: 106,
	}}, chunks)
	if dropped != 1 || len(kept) != 0 {
		t.Fatalf("区间上的文字已经变了，引用却留下来了：kept=%d dropped=%d", len(kept), dropped)
	}
}

// TestVerifyDoesNotFallBackToSearchingTheQuote——⭐ 核验比对的是
// **这段区间现在映射到的文字**，不是"这句话在块里找不找得到"。
//
// ⚠️ 后者是一个看起来很合理、实际形同虚设的实现：一句在书里出现多次的话，
// 无论区间偏成什么样都能"找到"。这里的引文确实在块里，但区间指着别处。
func TestVerifyDoesNotFallBackToSearchingTheQuote(t *testing.T) {
	chunks := map[string]verifyChunk{"c1": buildVerifyChunk("阿Q走进未庄的酒店", 100)}
	kept, dropped := verifyCitationsAgainstChunks([]RelationCitation{{
		ChunkID: "c1", Quote: "阿Q", SourceStart: 104, SourceEnd: 106,
	}}, chunks)
	if dropped != 1 || len(kept) != 0 {
		t.Fatalf("引文在块里就放行了，等于没有核验：kept=%d dropped=%d", len(kept), dropped)
	}
}

// TestVerifyDropsCitationWhoseChunkIsGone——块已经不在当前已发布版本里。
func TestVerifyDropsCitationWhoseChunkIsGone(t *testing.T) {
	kept, dropped := verifyCitationsAgainstChunks([]RelationCitation{{
		ChunkID: "c-missing", Quote: "未庄", SourceStart: 104, SourceEnd: 106,
	}}, map[string]verifyChunk{})
	if dropped != 1 || len(kept) != 0 {
		t.Fatalf("块都不在了，引用却留下来了：kept=%d dropped=%d", len(kept), dropped)
	}
}

// TestVerifyDropsCitationOutsideTheChunk——区间只有一部分落在这个块里。
//
// ⚠️ 钳到边界上会让核验拿半句话去比对；比对失败当然也是丢掉，
// 但如果实现改成"比对被钳短的那部分"，一条其实对不上的引用就能通过。
func TestVerifyDropsCitationOutsideTheChunk(t *testing.T) {
	chunks := map[string]verifyChunk{"c1": buildVerifyChunk("阿Q走进未庄的酒店", 100)}
	kept, dropped := verifyCitationsAgainstChunks([]RelationCitation{{
		ChunkID: "c1", Quote: "的酒店里", SourceStart: 106, SourceEnd: 110,
	}}, chunks)
	if dropped != 1 || len(kept) != 0 {
		t.Fatalf("区间超出块的范围却通过了核验：kept=%d dropped=%d", len(kept), dropped)
	}
}

// TestVerifyFillsDocumentName——书名只有 PG 侧有，而对话层渲染引用要用。
// ⚠️ 不补的话，用户看到的引用来自"（空）"。
func TestVerifyFillsDocumentName(t *testing.T) {
	chunks := map[string]verifyChunk{"c1": buildVerifyChunk("阿Q走进未庄的酒店", 100)}
	kept, _ := verifyCitationsAgainstChunks([]RelationCitation{{
		ChunkID: "c1", Quote: "未庄", SourceStart: 104, SourceEnd: 106,
	}}, chunks)
	if len(kept) != 1 || kept[0].DocumentName != "阿Q正传.txt" {
		t.Fatalf("核验没有补上书名：%+v", kept)
	}
}

// TestVerifySkipsOverlapCopyRegion——⭐ overlap 拷贝没有文档区间，
// 它在投影里根本不存在，所以指向它的引用必须被刷掉。
func TestVerifySkipsOverlapCopyRegion(t *testing.T) {
	content := "前一块的尾巴阿Q走进未庄"
	chunk := verifyChunk{
		Content:      content,
		DocumentName: "阿Q正传.txt",
		Meta: &narrativeMetadata{
			SchemaVersion: narrativeMetadataSchemaVersion,
			BoundaryKind:  boundaryNone,
			Segments: []narrativeSegment{
				{ChunkStart: 0, ChunkEnd: 6, IsOverlapCopy: true},
				{ChunkStart: 6, ChunkEnd: 12,
					DocumentStart: ptrInt(200), DocumentEnd: ptrInt(206)},
			},
		},
	}
	chunks := map[string]verifyChunk{"c1": chunk}
	// 可引用段里的引用留下。
	kept, dropped := verifyCitationsAgainstChunks([]RelationCitation{
		{ChunkID: "c1", Quote: "未庄", SourceStart: 204, SourceEnd: 206},
		{ChunkID: "c1", Quote: "尾巴", SourceStart: 194, SourceEnd: 196},
	}, chunks)
	if len(kept) != 1 || dropped != 1 {
		t.Fatalf("overlap 段里的引用没有被刷掉：kept=%+v dropped=%d", kept, dropped)
	}
	if kept[0].Quote != "未庄" {
		t.Fatalf("留下的不是可引用段里的那条：%+v", kept[0])
	}
}

// TestCitationChunkIDsDedupes——相邻块因 overlap 覆盖同一段原文，
// 同一个 chunk 会出现在多条引用上；批量查询不该为此多传几遍。
// 顺序必须确定（宪法第 V 条）。
func TestCitationChunkIDsDedupes(t *testing.T) {
	ids := citationChunkIDs([]RelationCitation{
		{ChunkID: "b"}, {ChunkID: "a"}, {ChunkID: "b"}, {ChunkID: "c"},
	})
	want := []string{"b", "a", "c"}
	if len(ids) != len(want) {
		t.Fatalf("去重结果不对：%v", ids)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("顺序不确定：got %v want %v", ids, want)
		}
	}
}

// TestFromDocumentIsInverseOfToDocument——⭐ 核验的正确性完全依赖
// fromDocument 与 toDocument 互为逆映射。
//
// ⚠️ 两者哪怕差一个 rune，核验都会把**全部**引用刷掉，而表现出来的现象是
// "关系查询突然什么都答不出来了"，看不出根因在坐标换算上。
func TestFromDocumentIsInverseOfToDocument(t *testing.T) {
	content := "前一块的尾巴阿Q走进未庄的酒店"
	meta := narrativeMetadata{
		SchemaVersion: narrativeMetadataSchemaVersion,
		BoundaryKind:  boundaryNone,
		Segments: []narrativeSegment{
			{ChunkStart: 0, ChunkEnd: 6, IsOverlapCopy: true},
			{ChunkStart: 6, ChunkEnd: 15,
				DocumentStart: ptrInt(200), DocumentEnd: ptrInt(209)},
		},
	}
	proj, err := newChunkProjection(content, meta)
	if err != nil {
		t.Fatalf("建立投影失败：%v", err)
	}
	n := len([]rune(proj.Text))
	for start := 0; start < n; start++ {
		for end := start + 1; end <= n; end++ {
			ds, de, ok := proj.toDocument(start, end)
			if !ok {
				continue
			}
			gs, ge, ok := proj.fromDocument(ds, de)
			if !ok {
				t.Fatalf("[%d,%d) → 文档 [%d,%d) 之后映射不回来", start, end, ds, de)
			}
			if gs != start || ge != end {
				t.Fatalf("往返不一致：[%d,%d) → [%d,%d)", start, end, gs, ge)
			}
		}
	}
}

// TestFromDocumentRejectsPartialCoverage——区间有一半在块外时必须拒绝，
// 而不是返回块里能覆盖到的那一段。
func TestFromDocumentRejectsPartialCoverage(t *testing.T) {
	meta := narrativeMetadata{
		SchemaVersion: narrativeMetadataSchemaVersion,
		BoundaryKind:  boundaryNone,
		Segments: []narrativeSegment{{
			ChunkStart: 0, ChunkEnd: 4,
			DocumentStart: ptrInt(100), DocumentEnd: ptrInt(104),
		}},
	}
	proj, err := newChunkProjection("阿Q走进", meta)
	if err != nil {
		t.Fatalf("建立投影失败：%v", err)
	}
	if _, _, ok := proj.fromDocument(102, 110); ok {
		t.Error("区间只有一部分落在块里，却被接受了")
	}
	if _, _, ok := proj.fromDocument(90, 102); ok {
		t.Error("区间起点在块之前，却被接受了")
	}
}

// --- 010 T034：入模前核验 ---

// TestQueryDropsCitationsThatNoLongerMatchTheDocument——⭐ 文档被重新处理
// 之后，同一段区间落在了别的字上，这条引用必须被刷掉。
//
// ⚠️ 不刷掉的后果不是报错，而是**引用悄悄指向别的文字**：用户点开引用
// 看到的原文是真的，与回答里那句"证据"却毫无关系，而系统的每一层都
// 认为自己工作正常。这里把 PG 里的正文改掉、rune 长度不变，
// 模拟的正是"重新分块之后偏移仍然合法但内容变了"这个最难发现的情形。
func TestQueryDropsCitationsThatNoLongerMatchTheDocument(t *testing.T) {
	repo := extractionRepo(t)
	seedRelationQueryFixture(t, repo, "doc-q12", "job-q12")
	svc := newTestService(repo, newFakeProvider(), t.TempDir())

	before, err := svc.QueryRelations(t.Context(), []string{"doc-q12"}, "赵太爷", "阿Q", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if before.Outcome != RelationOutcomeFound || len(before.Citations) == 0 {
		t.Fatalf("改动之前就查不到，用例没有意义：outcome=%q citations=%d",
			before.Outcome, len(before.Citations))
	}

	// 等长替换：偏移仍然合法，落在上面的字变了。
	if _, err := repo.pgdb.ExecContext(t.Context(),
		`UPDATE chunks SET content = REPLACE(content, '赵太爷', '王胡子') WHERE id=$1`,
		"doc-q12-c0"); err != nil {
		t.Fatal(err)
	}

	after, err := svc.QueryRelations(t.Context(), []string{"doc-q12"}, "赵太爷", "阿Q", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Citations) != 0 {
		t.Errorf("原文已经变了，引用却留下来了：%+v", after.Citations)
	}
	// ⚠️ 核验刷掉了东西必须反映成"不完整"。默默少给几条，
	// 回答会以剩下的为全部，说出"他们之间只有这些关系"。
	if !after.Truncated {
		t.Error("核验刷掉了引用却没有报告不完整")
	}
	if after.Outcome == RelationOutcomeFound {
		t.Error("一条引用都不剩却仍然报 found——对话层会拿着空证据去生成")
	}
}

// TestQueryReportsPartialDropAsIncomplete——⭐ **只刷掉一部分**引用时
// 同样必须报告不完整。
//
// ⚠️ 这一条是变异测试逼出来的：上一个用例里引用被全部刷光，
// 而"一条都不剩"那个分支自己也会把 Truncated 置上——于是
// 「刷掉了就报告不完整」这句代码删掉之后，上一个用例照样通过。
// 它测的是另一回事，我却以为它守着这里。
//
// 真正危险的恰恰是部分刷掉：还剩几条，回答看起来有理有据，
// 而模型会以这几条为全部，说出"他们之间只有这些关系"。
func TestQueryReportsPartialDropAsIncomplete(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRelationQueryFixture(t, repo, "doc-q15", "job-q15")
	svc := newTestService(repo, newFakeProvider(), t.TempDir())

	// 再挂一条证据到一个 PG 里根本不存在的片段上：它必然被核验刷掉，
	// 而原来那条仍然对得上。
	var relID string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT id FROM narrative_relations WHERE job_id='job-q15' LIMIT 1`).Scan(&relID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO narrative_relation_evidence
		 (id, job_id, relation_id, chunk_id, document_version, source_order,
		  source_start, source_end, quote, evidence_key_hash)
		 VALUES ('ev-ghost','job-q15',?,'chunk-that-is-gone',1,999,999,1010,
		         '这段原文所在的片段已经不在了', UNHEX(REPEAT('AB',32)))`, relID); err != nil {
		t.Fatal(err)
	}

	ans, err := svc.QueryRelations(ctx, []string{"doc-q15"}, "赵太爷", "阿Q", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ans.Citations) == 0 {
		t.Fatal("对得上的那条也被刷掉了，用例测不到「部分刷掉」")
	}
	for _, c := range ans.Citations {
		if c.ChunkID == "chunk-that-is-gone" {
			t.Errorf("指向已不存在片段的引用被留下了：%+v", c)
		}
	}
	if !ans.Truncated {
		t.Error("刷掉了一部分引用却没有报告不完整——模型会以剩下的为全部")
	}
}

// TestRecheckStopsOnConcurrentChange——⭐ 入模前的 MySQL 复检。
//
// ⚠️ 文档被删掉、改版，或者用户重新发起了一次抽取之后，手上这批引用
// 属于一个**已经不存在的上下文**。继续拿它们去回答，用户看到的是
// 上一轮的结论配着这一轮的进度，而没有任何一层会报错。
func TestRecheckStopsOnConcurrentChange(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRelationQueryFixture(t, repo, "doc-q13", "job-q13")

	var version int64
	if err := repo.db.QueryRowContext(ctx,
		`SELECT version FROM documents WHERE id='doc-q13'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	// 没有任何变化时必须放行——少了这一条，一个"永远 stale"的实现
	// 能让下面每一项都通过。
	if err := repo.recheckRelationScope(ctx, "doc-q13", version, "job-q13"); err != nil {
		t.Fatalf("什么都没变却判成了变化：%v", err)
	}

	for _, tc := range []struct {
		name    string
		docID   string
		version int64
		jobID   string
	}{
		{"文档被删掉", "doc-gone", version, "job-q13"},
		{"文档改版了", "doc-q13", version + 1, "job-q13"},
		{"新的一轮抽取接管了这份文档", "doc-q13", version, "job-other"},
	} {
		err := repo.recheckRelationScope(ctx, tc.docID, tc.version, tc.jobID)
		if !errors.Is(err, errRelationScopeChanged) {
			t.Errorf("%s：err = %v, want errRelationScopeChanged", tc.name, err)
		}
	}

	// 抽取被关掉也算变化：记录还在，但用户已经表示不再用它了。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET is_relation_extraction_enabled=0 WHERE id='doc-q13'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.recheckRelationScope(ctx, "doc-q13", version, "job-q13"); !errors.Is(err, errRelationScopeChanged) {
		t.Errorf("抽取已关闭：err = %v, want errRelationScopeChanged", err)
	}
}

// TestQueryStopsWhenANewRunTakesOverMidQuery——⭐ 并发变化必须**停止本轮**。
//
// ⚠️ 降级答成"没有记录"是错的：那会让用户以为书里没写，而真实情况是
// 这份资料刚刚被重新抽取过，再问一次就能拿到答案。他不会再问第二次。
//
// 变化发生在"读完记录"与"入模前复检"之间——从外部改数据库造不出这个窗口，
// 所以用 service.beforeRelationRecheck 这个挂载点，见它的注释。
func TestQueryStopsWhenANewRunTakesOverMidQuery(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRelationQueryFixture(t, repo, "doc-q14", "job-q14")
	svc := newTestService(repo, newFakeProvider(), t.TempDir()).(*service)

	// 没有并发变化时正常返回，作为对照。
	ok, err := svc.QueryRelations(ctx, []string{"doc-q14"}, "赵太爷", "阿Q", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if ok.Outcome != RelationOutcomeFound {
		t.Fatalf("对照组就没查到，用例没有意义：%q", ok.Outcome)
	}

	// 在窗口里让一次新的抽取接管这份文档。
	svc.beforeRelationRecheck = func(ctx context.Context) {
		if _, err := repo.db.ExecContext(ctx,
			`UPDATE documents SET active_relation_job_id='job-q14-new' WHERE id='doc-q14'`); err != nil {
			t.Error(err)
		}
	}
	ans, err := svc.QueryRelations(ctx, []string{"doc-q14"}, "赵太爷", "阿Q", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Outcome != RelationOutcomeStale {
		t.Errorf("并发变化后 outcome = %q, want stale", ans.Outcome)
	}
	if len(ans.Citations) != 0 {
		t.Errorf("已经停止本轮却还带回了 %d 条引用", len(ans.Citations))
	}
}

// TestFromDocumentRejectsNonAdjacentSegments——⭐ 一个区间跨过两段在**文档上
// 不相邻**的正文时必须拒绝。
//
// ⚠️ 接受的后果是把中间几百字都圈进这条引用：用户点开看到一大段与这句
// 证据无关的原文，而偏移、长度、块 id 每一项单看都是对的。
func TestFromDocumentRejectsNonAdjacentSegments(t *testing.T) {
	// 两段可引用正文，文档上中间隔着 100 个 rune。
	meta := narrativeMetadata{
		SchemaVersion: narrativeMetadataSchemaVersion,
		BoundaryKind:  boundaryNone,
		Segments: []narrativeSegment{
			{ChunkStart: 0, ChunkEnd: 4,
				DocumentStart: ptrInt(100), DocumentEnd: ptrInt(104)},
			{ChunkStart: 4, ChunkEnd: 8,
				DocumentStart: ptrInt(204), DocumentEnd: ptrInt(208)},
		},
	}
	proj, err := newChunkProjection("阿Q走进未庄的酒", meta)
	if err != nil {
		t.Fatalf("建立投影失败：%v", err)
	}
	if _, _, ok := proj.fromDocument(102, 206); ok {
		t.Error("区间跨过了文档上不相邻的两段，却被接受了")
	}
	// 各自段内仍然正常。
	if _, _, ok := proj.fromDocument(102, 104); !ok {
		t.Error("第一段内的区间被拒了")
	}
	if _, _, ok := proj.fromDocument(204, 206); !ok {
		t.Error("第二段内的区间被拒了")
	}
}
