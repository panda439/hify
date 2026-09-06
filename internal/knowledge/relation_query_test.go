package knowledge

import (
	"testing"
)

// relation_query_test.go 守关系查询（010 T031）。
//
// ⭐ 这是「A 和 B 是什么关系」真正被回答的地方，也是这一期立项的那个问题。
//
// ⚠️ 这一层最要紧的不是"能查到"，而是**把几种"查不到"分清楚**：
//   - 这份文档根本没开启抽取；
//   - 抽取开了但还没跑完，已跑的部分里没有；
//   - 抽取跑完了，书里确实没有这两个人的关系记录；
//   - 查询本身出错了。
// 四种情况给用户的下一步完全不同，而把它们都答成"没有找到相关信息"
// 是最省事也最没用的做法——用户无法判断该等一等、该重跑、还是该换个问法。

func seedRelationQueryFixture(t *testing.T, repo *Repository, docID, jobID string) itemInput {
	t.Helper()
	chunk := "赵太爷跳过去给了他一个嘴巴。阿Q摸着左颊，和地保退出去了。"
	chat := newScriptedChat()
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
	     {"ref":"m2","surface":"阿Q","occurrence":0}],
	     "relations":[{"subject_ref":"m1","object_ref":"m2","type":"欺凌",
	       "evidence":[{"quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}],
	     "alias_proposals":[]}`)

	job, epoch, item := publishFixture(t, repo, docID, jobID)
	pieces := chunkNarrative("第一章　甲\n"+chunk+"\n", 500, 0)
	// ⭐ PG 里那个已发布的块必须**就是**抽取用的这一段正文。
	// ⚠️ T034 的入模前核验会拿引用的文档区间回到 PG 逐字比对；
	// 夹具里两边内容不同的话，每一条引用都会被正当地刷掉，
	// 而现象是"查得到关系但一条证据都没有"，看不出根因在夹具上。
	chunkID := docID + "-c0"
	setNarrativeChunkContent(t, repo, chunkID, pieces[0])
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: chunkID,
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}
	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// 标成已完成，让"跑完了"与"没跑完"能区分开。
	if _, err := repo.db.ExecContext(t.Context(),
		`UPDATE relation_extraction_jobs SET state='succeeded', initialization_complete=1,
		 total_items=1, succeeded_items=1 WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(t.Context(),
		`UPDATE documents SET is_relation_extraction_enabled=1 WHERE id=?`, docID); err != nil {
		t.Fatal(err)
	}
	return in
}

// TestQueryFindsTheRelation——正常路径。
func TestQueryFindsTheRelation(t *testing.T) {
	repo := extractionRepo(t)
	in := seedRelationQueryFixture(t, repo, "doc-q1", "job-q1")

	res, err := repo.queryRelations(t.Context(), relationQueryInput{
		DocumentIDs: []string{"doc-q1"}, Subject: "赵太爷", Object: "阿Q",
	})
	if err != nil {
		t.Fatalf("queryRelations: %v", err)
	}
	if res.Outcome != relationQueryFound {
		t.Fatalf("outcome = %q, want found（%+v）", res.Outcome, res)
	}
	if len(res.Relations) != 1 {
		t.Fatalf("关系 %d 条, want 1", len(res.Relations))
	}
	rel := res.Relations[0]
	if rel.Type != "欺凌" {
		t.Errorf("type = %q", rel.Type)
	}
	if len(rel.Evidence) == 0 || rel.Evidence[0].Quote == "" {
		t.Error("关系没有带上引文——没有引文的答案不可核验")
	}
	if rel.ChapterNumber == nil || *rel.ChapterNumber != 1 {
		t.Errorf("章节号 = %v, want 1", rel.ChapterNumber)
	}
	_ = in
}

// TestQueryWithNoRoomForEvidenceIsNotFound——⭐ 变异测试逼出来的缺口。
//
// 查到了关系、但预算一条证据都装不下时，结局**不能仍然是 found**。
// ⚠️ 留着 found 而引用为空，对话层会走"有证据"分支去做受限生成，
// 而它手上一条证据都没有——模型只能编。而且编出来的答案看起来
// 和一个有依据的答案完全一样。
func TestQueryWithNoRoomForEvidenceIsNotFound(t *testing.T) {
	repo := extractionRepo(t)
	seedRelationQueryFixture(t, repo, "doc-q11", "job-q11")
	svc := newTestService(repo, newFakeProvider(), t.TempDir())

	ans, err := svc.QueryRelations(t.Context(), []string{"doc-q11"}, "赵太爷", "阿Q", 0)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Outcome == RelationOutcomeFound {
		t.Error("预算装不下任何证据却仍然报 found——对话层会拿着空证据去生成")
	}
	if len(ans.Citations) != 0 {
		t.Errorf("预算为 0 却给出了 %d 条引用", len(ans.Citations))
	}
	if !ans.Truncated {
		t.Error("没有报告截断")
	}
	// 预算充足时正常返回 found。
	ans, err = svc.QueryRelations(t.Context(), []string{"doc-q11"}, "赵太爷", "阿Q", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if ans.Outcome != RelationOutcomeFound || len(ans.Citations) == 0 {
		t.Errorf("预算充足却没有正常返回：outcome=%q citations=%d",
			ans.Outcome, len(ans.Citations))
	}
}

// TestQueryIsSymmetricForUndirectedTypes——无向关系两个方向都能查到。
// ⚠️ 只查一个方向的话，「阿Q 和 王胡 是什么关系」有答案而
// 「王胡 和 阿Q 是什么关系」没有——同一个问题换个语序就查不到了。
func TestQueryIsSymmetricForUndirectedTypes(t *testing.T) {
	repo := extractionRepo(t)
	seedRelationQueryFixture(t, repo, "doc-q2", "job-q2")

	forward, err := repo.queryRelations(t.Context(), relationQueryInput{
		DocumentIDs: []string{"doc-q2"}, Subject: "赵太爷", Object: "阿Q"})
	if err != nil {
		t.Fatal(err)
	}
	backward, err := repo.queryRelations(t.Context(), relationQueryInput{
		DocumentIDs: []string{"doc-q2"}, Subject: "阿Q", Object: "赵太爷"})
	if err != nil {
		t.Fatal(err)
	}
	// 欺凌是有向的：反过来问同样要能查到这条记录，但答案里要能看出方向。
	if forward.Outcome != relationQueryFound || backward.Outcome != relationQueryFound {
		t.Fatalf("方向对调后查不到：forward=%q backward=%q", forward.Outcome, backward.Outcome)
	}
	if len(backward.Relations) != 1 {
		t.Fatalf("反向查到 %d 条", len(backward.Relations))
	}
	if backward.Relations[0].SubjectName != "赵太爷" {
		t.Errorf("反向查询把方向也反了：subject = %q，应当仍是施加者",
			backward.Relations[0].SubjectName)
	}
}

// TestQueryDistinguishesFourKindsOfEmpty——⭐ 本文件的立论。
func TestQueryDistinguishesFourKindsOfEmpty(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()

	// 1) 没开启抽取。
	seedNarrativeDocument(t, repo, "doc-q3", 2)
	res, err := repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q3"}, Subject: "甲", Object: "乙"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQueryNotExtracted {
		t.Errorf("未开启抽取的文档：outcome = %q, want not_extracted", res.Outcome)
	}

	// 2) 开了但没跑完。
	seedRelationQueryFixture(t, repo, "doc-q4", "job-q4")
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET state='running', succeeded_items=0, total_items=5
		 WHERE id='job-q4'`); err != nil {
		t.Fatal(err)
	}
	res, err = repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q4"}, Subject: "从未出现的人", Object: "另一个"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQueryIncomplete {
		t.Errorf("抽取未完成：outcome = %q, want incomplete", res.Outcome)
	}
	// ⚠️ 未完成时必须能说出**还剩多少**，否则用户不知道该不该等。
	if res.RemainingItems == nil || *res.RemainingItems != 5 {
		t.Errorf("没有报告剩余片段数：%v", res.RemainingItems)
	}

	// 3) 跑完了，两个人都在书里，但他们之间确实没有关系记录。
	// ⚠️ 夹具必须让两个人**都存在**：第一版用了书里根本没有的「王胡」，
	// 于是返回的是 unknown_name——那是正确行为，而我把它当成了失败。
	// 两种"查不到"混在一个用例里，等于哪一种都没验。
	in := seedRelationQueryFixture(t, repo, "doc-q5", "job-q5")
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO narrative_characters (id, job_id, display_name, first_source_order)
		 VALUES ('lonely-wang', ?, '王胡', 500)`, in.JobID); err != nil {
		t.Fatal(err)
	}
	res, err = repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q5"}, Subject: "赵太爷", Object: "王胡"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQueryNoRecords {
		t.Errorf("跑完但没有这条关系：outcome = %q, want no_records", res.Outcome)
	}

	// 4) ⭐ 变异测试逼出来的缺口：两个人**都在书里**、之间暂时没有关系记录，
	// 而抽取**还没跑完**——这时必须答 incomplete 而不是 no_records。
	// ⚠️ 答成 no_records 等于告诉用户"书里就是没写"，而实际上还有几百段
	// 没抽到；用户会据此下一个错误的结论，且不会再来问第二次。
	// 上一条用例走的是"名字都不认识"那个分支，这一条走的是"关系为空"分支，
	// 两个分支各有一处 incomplete 判断，只验一处等于漏了另一处。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET state='running', succeeded_items=0, total_items=7
		 WHERE id='job-q5'`); err != nil {
		t.Fatal(err)
	}
	res, err = repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q5"}, Subject: "赵太爷", Object: "王胡"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQueryIncomplete {
		t.Errorf("两人都在书里、关系为空且抽取未完成：outcome = %q, want incomplete", res.Outcome)
	}
	if res.RemainingItems == nil || *res.RemainingItems != 7 {
		t.Errorf("没有报告剩余片段数：%v", res.RemainingItems)
	}
}

// TestQueryReportsUnknownName——问到书里根本没有的名字，要单独说明，
// 不能和"这两个人之间没有关系"混为一谈。
// ⚠️ 混为一谈的话，用户把名字打错了却以为书里真的没写他们的关系。
func TestQueryReportsUnknownName(t *testing.T) {
	repo := extractionRepo(t)
	seedRelationQueryFixture(t, repo, "doc-q6", "job-q6")
	res, err := repo.queryRelations(t.Context(), relationQueryInput{
		DocumentIDs: []string{"doc-q6"}, Subject: "赵太爷", Object: "根本没这个人"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQueryUnknownName {
		t.Fatalf("outcome = %q, want unknown_name", res.Outcome)
	}
	if res.UnknownName != "根本没这个人" {
		t.Errorf("没有指出是哪个名字找不到：%q", res.UnknownName)
	}
}

// TestQueryAsksForClarificationOnAmbiguousName——⭐ 同名多人时要求澄清，
// **不挑一个**。⚠️ 挑一个的后果是把甲的事答成乙的，而用户完全看不出来。
func TestQueryAsksForClarificationOnAmbiguousName(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	in := seedRelationQueryFixture(t, repo, "doc-q7", "job-q7")
	// 再造一个同名人物。
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO narrative_characters (id, job_id, display_name, first_source_order)
		 VALUES ('another-zhao', ?, '赵太爷', 9999)`, in.JobID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q7"}, Subject: "赵太爷", Object: "阿Q"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQueryAmbiguous {
		t.Fatalf("outcome = %q, want ambiguous", res.Outcome)
	}
	if len(res.Candidates) != 2 {
		t.Errorf("候选 %d 个, want 2", len(res.Candidates))
	}
	for _, c := range res.Candidates {
		if c.FirstSourceOrder < 0 {
			t.Errorf("候选没有带上首次出现位置，用户无法区分：%+v", c)
		}
	}
}

// TestQuerySameEntityIsItsOwnAnswer——两个名字指向同一个人时单独回复，
// 而不是答"没有关系"。⚠️ 「阿Q 和 老Q 是什么关系」的正确答案是
// "他们是同一个人"，答成"没有找到关系"是错的。
func TestQuerySameEntityIsItsOwnAnswer(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	in := seedRelationQueryFixture(t, repo, "doc-q8", "job-q8")
	var charID string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT id FROM narrative_characters WHERE job_id=? AND display_name='阿Q'`,
		in.JobID).Scan(&charID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO narrative_aliases
		 (id, job_id, character_id, surface, surface_hash, state, evidence,
		  first_source_order, decision_key_hash)
		 VALUES ('al-laoq', ?, ?, '老Q', UNHEX(REPEAT('ab',32)), 'supported', '{}', 5,
		         UNHEX(REPEAT('cd',32)))`, in.JobID, charID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q8"}, Subject: "阿Q", Object: "老Q"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != relationQuerySameEntity {
		t.Errorf("outcome = %q, want same_entity", res.Outcome)
	}
}

// TestQueryScopeIsNeverSilentlyWidened——⭐ **空范围不等于全库**。
//
// ⚠️ 这是 002/004 已经确立过的口径：一个 Agent 的文档范围为空时，
// 是"限定在它绑定的知识库内"，不是"不限定"。而这里还多一层：
// 如果连知识库都没绑，那就什么都查不到——绝不退化成全库查询。
// 退化的表现是 Agent 悄悄用起了范围外的资料，而回答看起来完全正常。
func TestQueryScopeIsNeverSilentlyWidened(t *testing.T) {
	repo := extractionRepo(t)
	seedRelationQueryFixture(t, repo, "doc-q9", "job-q9")

	// 没有任何范围：必须查不到，而不是全库。
	res, err := repo.queryRelations(t.Context(), relationQueryInput{
		Subject: "赵太爷", Object: "阿Q"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == relationQueryFound {
		t.Error("空范围被当成了全库查询——Agent 会悄悄用起范围外的资料")
	}
	if res.Outcome != relationQueryOutOfScope {
		t.Errorf("outcome = %q, want out_of_scope", res.Outcome)
	}
	// 指定了别的文档：同样查不到。
	res, err = repo.queryRelations(t.Context(), relationQueryInput{
		DocumentIDs: []string{"doc-somewhere-else"}, Subject: "赵太爷", Object: "阿Q"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == relationQueryFound {
		t.Error("查到了范围外文档的关系")
	}
}

// TestQueryOnlyReadsTheCurrentRun——⭐ 只读文档当前指向的那个 run。
// ⚠️ 读到旧 run 的记录，用户会看到自己已经"重新开始"过的那一次的结果，
// 而界面显示的进度是新 run 的。
func TestQueryOnlyReadsTheCurrentRun(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	in := seedRelationQueryFixture(t, repo, "doc-q10", "job-q10")
	// 把文档指向一个别的（空的）run。
	spec := newExtractionJobSpec("job-q10-new", "doc-q10", 1, "m-1")
	spec.RunNumber = 2
	if _, err := repo.initializeExtractionJob(ctx, spec); err != nil {
		t.Fatal(err)
	}
	res, err := repo.queryRelations(ctx, relationQueryInput{
		DocumentIDs: []string{"doc-q10"}, Subject: "赵太爷", Object: "阿Q"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome == relationQueryFound {
		t.Error("读到了已经被取代的那个 run 的记录")
	}
	_ = in
}

// setNarrativeChunkContent 把 PG 里某个已发布块的正文与元数据换成 piece。
//
// ⚠️ 直接改 PG 而不是重新建块：seedNarrativeDocument 已经建好并发布了
// 这个块，重复插入会撞主键。
func setNarrativeChunkContent(t *testing.T, repo *Repository, chunkID string, piece chunkPiece) {
	t.Helper()
	meta, err := encodeNarrativeMetadata(piece.Narrative)
	if err != nil {
		t.Fatal(err)
	}
	res, err := repo.pgdb.ExecContext(t.Context(),
		`UPDATE chunks SET content=$1, content_length=$2, narrative_metadata=$3 WHERE id=$4`,
		piece.Content, len([]rune(piece.Content)), meta, chunkID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("要改的块 %s 不存在（影响 %d 行）", chunkID, n)
	}
	// ⚠️ 把同文档的其余块删掉：它们是 seedNarrativeDocument 用**另一段正文**
	// 切出来的，normalized_document_hash 与刚换上去的这块不同。
	// 留着的话，同一个文档版本里混着两次不同处理的产物——而这正是
	// 010 R6-05 的来源一致性检查要拒绝的情形（它确实拒绝了，
	// 这条注释就是那次失败逼出来的）。
	if _, err := repo.pgdb.ExecContext(t.Context(),
		`DELETE FROM chunks WHERE document_id = (SELECT document_id FROM chunks WHERE id=$1)
		   AND id <> $1`, chunkID); err != nil {
		t.Fatal(err)
	}
}
