package knowledge

import (
	"errors"
	"fmt"
	"testing"
)

// relations_test.go 守关系查询（010 T031/T032/T034）。
//
// ⭐ 用例的重心在**四种"没有答案"的区别**上：范围外、未开启、没记录、
// 读不出来。把它们混成一句"没有找到关系"，用户会当成"书里确实没写"，
// 而其中三种都不是那个意思。

func relationScope(kb string) RelationScope {
	return RelationScope{KnowledgeBaseIDs: []string{kb}}
}

// queryFixture 跑完一个真实的抽取作业，得到可查询的关系数据。
func queryFixture(t *testing.T, kbID, docID string, chapters int) (Service, *Repository, Document) {
	t.Helper()
	repo := setupIntegration(t)
	svc := startTestService(t, repo, "qwen2.5:14b")
	seedKB(t, repo, kbID, "m3", "u1", true)
	if _, err := repo.db.ExecContext(t.Context(),
		"UPDATE knowledge_bases SET chunk_size=200,chunk_overlap=0 WHERE id=?", kbID); err != nil {
		t.Fatal(err)
	}
	doc, err := svc.UploadDocumentWithOptions(t.Context(), kbID, "u1", "member",
		"novel.txt", FileTypeTxt, narrativeUpload(chapters),
		UploadOptions{Narrative: true, RelationExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(t.Context(), doc.ID, doc.Version); err != nil {
		t.Fatal(err)
	}
	ready, err := svc.GetDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	// ⚠️ 用会归一的假模型：不归一的话每一章都会新建一个"阿Q"，
	// 于是每次查询都变成"命中多个实体"——那是保守设计的真实代价，
	// 但这里要验的是归一成功之后的查询路径。
	if _, err := runnerFor(t, repo, linkingModel()).runJob(t.Context(), ready.ActiveRelationJobID); err != nil {
		t.Fatal(err)
	}
	ready, err = svc.GetDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	return svc, repo, ready
}

// TestQueryReturnsEveryChapterRecord：关系不按当前状态覆盖写，
// 所以查询也要把每一章的记录都给出来。
func TestQueryReturnsEveryChapterRecord(t *testing.T) {
	svc, _, doc := queryFixture(t, "kb-rel-found", "doc-rel-found", 3)

	res, err := svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-found"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q",
	})
	if err != nil {
		t.Fatalf("QueryRelations: %v", err)
	}
	if res.Status != RelationStatusFound {
		t.Fatalf("status=%q，应当找到关系", res.Status)
	}
	if len(res.Records) != 3 {
		t.Fatalf("三章各一条记录，得到 %d 条", len(res.Records))
	}
	// 按原文顺序，不按章节号——倒叙的书里两者不一致。
	for i := 1; i < len(res.Records); i++ {
		if res.Records[i-1].SourceOrder > res.Records[i].SourceOrder {
			t.Errorf("记录没有按原文顺序排：%d > %d",
				res.Records[i-1].SourceOrder, res.Records[i].SourceOrder)
		}
	}
	for _, rec := range res.Records {
		if len(rec.Evidence) == 0 {
			t.Error("一条没有任何证据的关系被返回了")
		}
		if rec.Type != "欺凌" || !rec.IsDirected {
			t.Errorf("关系类型/方向不对：%+v", rec)
		}
		if rec.SubjectName != "赵太爷" || rec.ObjectName != "阿Q" {
			t.Errorf("端点名字不对：%s -> %s", rec.SubjectName, rec.ObjectName)
		}
	}
	if res.Coverage.TotalItems == nil || *res.Coverage.TotalItems != 3 {
		t.Errorf("覆盖度没带上总数：%+v", res.Coverage)
	}
	if !res.Coverage.Complete {
		t.Errorf("三章全成功却报不完整：%+v", res.Coverage)
	}
}

// TestQueryFindsCharacterByAlias：归一成功之后，用别的称呼也要能查到。
// ⚠️ 不存别名的话，归一明明成功了，查询却说"没找到"，且没有任何线索。
func TestQueryFindsCharacterByAlias(t *testing.T) {
	svc, repo, doc := queryFixture(t, "kb-rel-alias", "doc-rel-alias", 1)
	// 给"阿Q"这个人物补一个 supported 别名"老Q"（归一阶段的产物形态）。
	var charID string
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT id FROM narrative_characters WHERE job_id=? AND display_name='阿Q'`,
		doc.ActiveRelationJobID).Scan(&charID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(t.Context(),
		`INSERT INTO narrative_aliases (id, job_id, character_id, surface, surface_hash,
		 state, evidence, first_source_order, decision_key_hash)
		 VALUES (?, ?, ?, '老Q', UNHEX(SHA2('老Q', 256)), 'supported', '[]', 0, UNHEX(SHA2('k1', 256)))`,
		"alias-1", doc.ActiveRelationJobID, charID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-alias"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "老Q",
	})
	if err != nil {
		t.Fatalf("QueryRelations: %v", err)
	}
	if res.Status != RelationStatusFound {
		t.Fatalf("别名没能命中人物：status=%q", res.Status)
	}
}

// TestAmbiguousSurfaceAsksInsteadOfGuessing：命中多个实体就澄清。
// ⭐ 猜一个的代价是把甲的关系说成乙的，而回答读起来完全正常。
func TestAmbiguousSurfaceAsksInsteadOfGuessing(t *testing.T) {
	svc, repo, doc := queryFixture(t, "kb-rel-ambig", "doc-rel-ambig", 1)
	// 再造一个同名人物：同一个称呼现在指向两个实体。
	if _, err := repo.db.ExecContext(t.Context(),
		`INSERT INTO narrative_characters (id, job_id, display_name, first_source_order, has_ambiguity)
		 VALUES ('char-dup', ?, '阿Q', 99, 0)`, doc.ActiveRelationJobID); err != nil {
		t.Fatal(err)
	}

	res, err := svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-ambig"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q",
	})
	if err != nil {
		t.Fatalf("QueryRelations: %v", err)
	}
	if res.Status != RelationStatusAmbiguous {
		t.Fatalf("同名两个人却没有澄清：status=%q", res.Status)
	}
	if len(res.ObjectCandidates) != 2 {
		t.Fatalf("候选数=%d，应当把两个都给出来", len(res.ObjectCandidates))
	}
	for _, c := range res.ObjectCandidates {
		if c.Context == "" {
			t.Error("候选没有可以分辨的上下文")
		}
	}

	// 用户选定其中一个之后就能查出来。
	res, err = svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-ambig"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q",
		ObjectCharacterID: res.ObjectCandidates[0].CharacterID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RelationStatusFound && res.Status != RelationStatusNotFound {
		t.Fatalf("选定候选之后 status=%q", res.Status)
	}

	// ⚠️ 一个属于本作业、但与这次称呼无关的人物 ID 不能被接受。
	res, err = svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-ambig"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q", ObjectCharacterID: "char-not-a-candidate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RelationStatusAmbiguous {
		t.Fatalf("无关的人物 ID 被接受了：status=%q", res.Status)
	}
}

// TestSameEntityIsNotNotFound：两个称呼指向同一个人是一种独立的答案。
func TestSameEntityIsNotNotFound(t *testing.T) {
	svc, _, doc := queryFixture(t, "kb-rel-same", "doc-rel-same", 1)
	res, err := svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-same"), DocumentID: doc.ID,
		Subject: "阿Q", Object: "阿Q",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RelationStatusSameEntity {
		t.Fatalf("status=%q，应当报「这两个称呼指向同一人物」", res.Status)
	}
}

// TestScopeIsServerSideOnly：范围校验的三条底线。
func TestScopeIsServerSideOnly(t *testing.T) {
	svc, _, doc := queryFixture(t, "kb-rel-scope", "doc-rel-scope", 1)

	// 1. 空 KB 范围 = 没有可查询的知识库，**不是**全库。
	_, err := svc.QueryRelations(t.Context(), RelationQuery{
		DocumentID: doc.ID, Subject: "赵太爷", Object: "阿Q",
	})
	if !errors.Is(err, ErrRelationScopeEmpty) {
		t.Fatalf("空范围被当成了全库：err=%v", err)
	}

	// 2. 文档不在 KB 范围内。
	_, err = svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-somewhere-else"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q",
	})
	if !errors.Is(err, ErrRelationDocumentOutOfScope) {
		t.Fatalf("范围外的文档被查到了：err=%v", err)
	}

	// 3. DocumentIDs 非空时，不在列表里的文档也要拒绝。
	_, err = svc.QueryRelations(t.Context(), RelationQuery{
		Scope: RelationScope{
			KnowledgeBaseIDs: []string{"kb-rel-scope"},
			DocumentIDs:      []string{"another-doc"},
		},
		DocumentID: doc.ID, Subject: "赵太爷", Object: "阿Q",
	})
	if !errors.Is(err, ErrRelationDocumentOutOfScope) {
		t.Fatalf("不在 DocumentIDs 列表里的文档被查到了：err=%v", err)
	}
}

// TestDisabledAndNotFoundAreDifferent：未开启、没记录是两种答案。
func TestDisabledAndNotFoundAreDifferent(t *testing.T) {
	svc, repo, doc := queryFixture(t, "kb-rel-states", "doc-rel-states", 1)

	// 没有记录的一对人物。
	res, err := svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-states"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "土谷祠",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RelationStatusAmbiguous && res.Status != RelationStatusNotFound {
		t.Fatalf("查一个不存在的人物 status=%q", res.Status)
	}

	// 关掉抽取之后是 disabled，不是 not_found。
	if err := repo.setRelationExtractionEnabled(t.Context(), doc.ID, false); err != nil {
		t.Fatal(err)
	}
	res, err = svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-states"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != RelationStatusDisabled {
		t.Fatalf("关掉抽取之后 status=%q", res.Status)
	}
}

// TestStaleEvidenceStopsTheAnswer：入模前核验发现引用对不上当前原文时，
// **整轮放弃**，不是剔掉那条再答。
func TestStaleEvidenceStopsTheAnswer(t *testing.T) {
	svc, repo, doc := queryFixture(t, "kb-rel-stale", "doc-rel-stale", 2)
	// 原文被改掉（模拟文档重新处理后的内容变化）。
	if _, err := repo.pgdb.ExecContext(t.Context(),
		`UPDATE chunks SET content = '完全不同的内容' WHERE document_id = $1`, doc.ID); err != nil {
		t.Fatal(err)
	}

	_, err := svc.QueryRelations(t.Context(), RelationQuery{
		Scope: relationScope("kb-rel-stale"), DocumentID: doc.ID,
		Subject: "赵太爷", Object: "阿Q",
	})
	if !errors.Is(err, ErrRelationEvidenceStale) {
		t.Fatalf("引用已经对不上原文却照样回答：err=%v", err)
	}
}

// --- 证据选取（T032，纯函数）---

func evidenceAt(order int64, quote string) RelationEvidenceItem {
	return RelationEvidenceItem{SourceOrder: order, Quote: quote, ChunkID: fmt.Sprintf("c%d", order)}
}

// TestEvidenceSelectionKeepsOneOfEachKindFirst：先保证"有几种关系"这件事
// 不被同一种关系的大量证据挤掉。
func TestEvidenceSelectionKeepsOneOfEachKindFirst(t *testing.T) {
	var records []RelationRecord
	// 一条"欺凌"带 20 条证据，全都排在前面。
	bully := RelationRecord{RelationID: "r1", Type: "欺凌", IsDirected: true,
		SubjectName: "赵太爷", ObjectName: "阿Q", SourceOrder: 0}
	for i := 0; i < 20; i++ {
		bully.Evidence = append(bully.Evidence, evidenceAt(int64(i), "打"))
	}
	records = append(records, bully)
	// 一条"亲属"只有一条证据，排在很后面。
	records = append(records, RelationRecord{RelationID: "r2", Type: "亲属", SourceOrder: 500,
		SubjectName: "赵太爷", ObjectName: "阿Q",
		Evidence: []RelationEvidenceItem{evidenceAt(500, "本家")}})

	selected, truncated := selectRelationEvidence(records, maxRelationEvidenceItems)
	if !truncated {
		t.Error("20+1 条证据被裁到 12 条，却没有标记截断")
	}
	kinds := map[string]bool{}
	total := 0
	for _, rec := range selected {
		kinds[rec.Type] = true
		total += len(rec.Evidence)
	}
	if total > maxRelationEvidenceItems {
		t.Errorf("选出了 %d 条证据，上限是 %d", total, maxRelationEvidenceItems)
	}
	if !kinds["亲属"] {
		t.Error("排在最后的那种关系被大量同类证据挤掉了")
	}
}

// TestEvidenceSelectionDropsRecordsWithoutEvidence：一条没有任何证据入选的
// 关系不进结果——给出断言却拿不出原文，正是这个功能要避免的。
func TestEvidenceSelectionDropsRecordsWithoutEvidence(t *testing.T) {
	records := []RelationRecord{
		{RelationID: "r1", Type: "冲突", SourceOrder: 1,
			Evidence: []RelationEvidenceItem{evidenceAt(1, "吵")}},
		{RelationID: "r2", Type: "冲突", SourceOrder: 2}, // 没有证据
	}
	selected, truncated := selectRelationEvidence(records, maxRelationEvidenceItems)
	if len(selected) != 1 || selected[0].RelationID != "r1" {
		t.Fatalf("没有证据的关系没有被丢掉：%+v", selected)
	}
	if !truncated {
		t.Error("丢掉了一条关系却没有标记截断")
	}
}
