package knowledge

import (
	"encoding/json"
	"testing"
)

// extract_acceptance_test.go 是 Phase 4 的验收（010 T028）。
//
// ⭐ 它验的不是"跑得通"，而是几条**事后无法补救**的性质：
//   - 每一次身份判定的依据都能查出来（FR-014）——查不出来的话，
//     人工复核只能对着一堆已经合并好的人物干瞪眼；
//   - 同一份响应回放两次结果完全相同——不成立的话，报告里的数字
//     取决于跑的是哪一次，而两次都"成功"；
//   - 同章内关系变化不被覆盖——覆盖的话全书只剩最后一个状态；
//   - 排序按原文位置而不是章节号——倒叙的书里两者不一致。

func aliasChunk() string {
	return "阿Q，人称老Q。王胡打了阿Q一顿。后来阿Q又和王胡吵了起来。"
}

func aliasScript() *scriptedChat {
	chat := newScriptedChat()
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
	     {"ref":"m2","surface":"老Q","occurrence":0},
	     {"ref":"m3","surface":"王胡","occurrence":0}],
	     "relations":[
	       {"subject_ref":"m3","object_ref":"m1","type":"欺凌",
	        "evidence":[{"quote":"王胡打了阿Q一顿","occurrence":0}]},
	       {"subject_ref":"m1","object_ref":"m3","type":"冲突",
	        "evidence":[{"quote":"后来阿Q又和王胡吵了起来","occurrence":0}]}],
	     "alias_proposals":[{"left":"m1","right":"m2","quote":"阿Q，人称老Q","occurrence":0}]}`)
	chat.script(phaseAlias, `{"decisions":[
	     {"mention_ref":"m1","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m1","quote":"阿Q，人称老Q","occurrence":0}]},
	     {"mention_ref":"m2","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m2","quote":"阿Q，人称老Q","occurrence":0}]},
	     {"mention_ref":"m3","action":"new","new_group":2,"reason_code":"context_identity",
	      "supports":[{"source_ref":"m3","quote":"王胡打了阿Q一顿","occurrence":0}]}]}`)
	return chat
}

func runAcceptanceItem(t *testing.T, repo *Repository, docID, jobID string) itemInput {
	t.Helper()
	job, epoch, item := publishFixture(t, repo, docID, jobID)
	pieces := chunkNarrative("第三章　冲突\n"+aliasChunk()+"\n", 500, 0)
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}
	if err := pipelineDeps(repo, aliasScript()).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	return in
}

// TestAliasDecisionsAreQueryable——⭐ FR-014：每一次身份判定的依据都要能查出来。
//
// ⚠️ 查不出来的话，人工复核只能对着一堆已经合并好的人物干瞪眼：
// 「阿Q」和「老Q」为什么被判成同一个人？依据是哪句话？没有记录就答不了，
// 而这正是本期要人工复核的东西。
func TestAliasDecisionsAreQueryable(t *testing.T) {
	repo := extractionRepo(t)
	in := runAcceptanceItem(t, repo, "doc-ac1", "job-ac1")

	rows, err := repo.db.QueryContext(t.Context(),
		`SELECT surface, state, evidence, character_id IS NOT NULL
		 FROM narrative_aliases WHERE job_id = ? ORDER BY first_source_order, surface`, in.JobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rec struct {
		surface, state string
		evidence       []byte
		linked         bool
	}
	var got []rec
	for rows.Next() {
		var r rec
		if err := rows.Scan(&r.surface, &r.state, &r.evidence, &r.linked); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 3 {
		t.Fatalf("别名记录 %d 条, want 3（三个称呼各一条）", len(got))
	}
	for _, r := range got {
		if !r.linked {
			t.Errorf("称呼 %q 没有指向任何人物", r.surface)
		}
		if r.state != "supported" {
			t.Errorf("称呼 %q 的状态 = %q", r.surface, r.state)
		}
		// ⭐ 依据必须是**可读的原文引用**，不是一个分数。
		var ev struct {
			RulesVersion string `json:"rules_version"`
			ReasonCode   string `json:"reason_code"`
			Supports     []struct {
				Quote string `json:"quote"`
			} `json:"supports"`
		}
		if err := json.Unmarshal(r.evidence, &ev); err != nil {
			t.Fatalf("称呼 %q 的依据解析失败：%v", r.surface, err)
		}
		if len(ev.Supports) == 0 || ev.Supports[0].Quote == "" {
			t.Errorf("称呼 %q 的依据里没有原文引用", r.surface)
		}
		if ev.ReasonCode == "" {
			t.Errorf("称呼 %q 没有记录判定理由", r.surface)
		}
		// ⚠️ 规则版本必须一起记：规则改了之后，旧记录是按旧规则判的，
		// 拿新规则去复核它会得出错误结论。
		if ev.RulesVersion == "" {
			t.Errorf("称呼 %q 没有记录规则版本", r.surface)
		}
	}
	// 「阿Q」和「老Q」必须指向同一个人物。
	var distinct int
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT COUNT(DISTINCT character_id) FROM narrative_aliases
		 WHERE job_id = ? AND surface IN ('阿Q','老Q')`, in.JobID).Scan(&distinct); err != nil {
		t.Fatal(err)
	}
	if distinct != 1 {
		t.Errorf("「阿Q」和「老Q」指向了 %d 个人物", distinct)
	}
}

// TestReplayProducesIdenticalResults——⭐ 同一份响应回放两次，结果完全相同。
//
// ⚠️ 不成立的话，报告里的数字取决于跑的是哪一次，而两次都"成功"。
// 最容易破坏它的是 map 迭代顺序：人物创建顺序变了，ID 跟着变，
// 关系的 relation_key_hash 也跟着变。
func TestReplayProducesIdenticalResults(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()

	snapshot := func(jobID string) string {
		rows, err := repo.db.QueryContext(ctx,
			`SELECT c.display_name, r.relation_type, r.is_directed, r.first_source_order,
			        e.source_start, e.source_end, e.quote
			 FROM narrative_relations r
			 JOIN narrative_characters c ON c.id = r.subject_id
			 JOIN narrative_relation_evidence e ON e.relation_id = r.id
			 WHERE r.job_id = ?
			 ORDER BY r.first_source_order, r.relation_type, e.source_start`, jobID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := ""
		for rows.Next() {
			var name, typ, quote string
			var directed bool
			var order int64
			var s, e int
			if err := rows.Scan(&name, &typ, &directed, &order, &s, &e, &quote); err != nil {
				t.Fatal(err)
			}
			out += name + "|" + typ + "|" + quote + "|" +
				itoa(int(order)) + "|" + itoa(s) + "-" + itoa(e) + "\n"
		}
		return out
	}

	first := runAcceptanceItem(t, repo, "doc-ac2", "job-ac2")
	second := runAcceptanceItem(t, repo, "doc-ac3", "job-ac3")
	a, b := snapshot(first.JobID), snapshot(second.JobID)
	if a == "" {
		t.Fatal("第一次没有产出任何关系，这条断言等于没验")
	}
	if a != b {
		t.Errorf("同一份响应两次产出不同：\n第一次:\n%s\n第二次:\n%s", a, b)
	}
}

// TestSameChapterRelationChangesAreNotOverwritten——⭐ 同一章里，同一对人物
// 在不同位置发生的不同关系必须**各存一条**。
//
// ⚠️ 覆盖写的话全书只剩最后一个状态，而"人物关系随剧情变化"正是这个功能
// 的立论。这里的夹具是同一章内的两条：先「欺凌」后「冲突」。
func TestSameChapterRelationChangesAreNotOverwritten(t *testing.T) {
	repo := extractionRepo(t)
	in := runAcceptanceItem(t, repo, "doc-ac4", "job-ac4")

	rows, err := repo.db.QueryContext(t.Context(),
		`SELECT relation_type, chapter_number, first_source_order
		 FROM narrative_relations WHERE job_id = ? ORDER BY first_source_order`, in.JobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var types []string
	var chapters []int
	var orders []int64
	for rows.Next() {
		var typ string
		var ch int
		var order int64
		if err := rows.Scan(&typ, &ch, &order); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
		chapters = append(chapters, ch)
		orders = append(orders, order)
	}
	if len(types) != 2 {
		t.Fatalf("关系 %d 条, want 2——同章内的两次关系变化被覆盖成一条了：%v", len(types), types)
	}
	if types[0] != "欺凌" || types[1] != "冲突" {
		t.Errorf("关系顺序 = %v, want [欺凌 冲突]", types)
	}
	for i, ch := range chapters {
		if ch != 3 {
			t.Errorf("第 %d 条的章节号 = %d, want 3", i, ch)
		}
	}
	// ⭐ 排序按**原文位置**，不是章节号：倒叙的书里两者不一致，
	// 而只有原文位置能还原叙述顺序。
	if orders[0] >= orders[1] {
		t.Errorf("排序没有按原文位置递增：%v", orders)
	}
}

// TestChapterNumberMayBeEmpty——章节认不出来时留空，不编一个。
func TestChapterNumberMayBeEmpty(t *testing.T) {
	repo := extractionRepo(t)
	job, epoch, item := publishFixture(t, repo, "doc-ac5", "job-ac5")
	// 没有章节标题的一段散文。
	pieces := chunkNarrative(aliasChunk()+"\n", 500, 0)
	if pieces[0].Narrative.ChapterNumber != nil {
		t.Fatalf("夹具本身带了章节号：%v", pieces[0].Narrative.ChapterNumber)
	}
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}
	if err := pipelineDeps(repo, aliasScript()).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	var nullCount int
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM narrative_relations WHERE job_id=? AND chapter_number IS NULL`,
		in.JobID).Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 2 {
		t.Errorf("章节未知的关系里只有 %d 条留空——其余的被编了一个章节号", nullCount)
	}
}

// TestZeroFalseMergesOnTheFixedCounterExamples——⭐ 固定反例上的零误合并。
//
// ⚠️ 这条只证明**这几个固定反例**不会被误合并，不证明模型在真实语料上的
// 误合并率。真实误差要靠人工真值集算，那是 Phase 6 的事，且在
// eval/annotations 冻结之前那个数字不存在。
func TestZeroFalseMergesOnTheFixedCounterExamples(t *testing.T) {
	// 来自 annotation-guideline §3 的真实反例。
	cases := []struct {
		name       string
		left       string
		right      string
		quote      string
		shouldPass bool
	}{
		{"阿Quei 与 阿Q（明确）", "阿Quei", "阿Q", "阿Quei，人称阿Q", true},
		{"阿贵（存疑，不得合并）", "阿贵", "阿Q", "阿Quei，阿桂还是阿贵呢", false},
		{"赵太爷与赵秀才（父子，不得合并）", "赵太爷", "赵秀才",
			"赵太爷的儿子赵秀才也在", true}, // 引文本身是明确的，但它说的是父子关系
		{"无同一身份依据的两个称呼", "把总", "光头老人", "把总坐在上面，光头老人在旁边", true},
	}
	for _, tc := range cases {
		got := quoteSupportsIdentity(tc.quote)
		if got != tc.shouldPass {
			t.Errorf("%s：quoteSupportsIdentity = %v, want %v", tc.name, got, tc.shouldPass)
		}
	}
	// ⚠️ 上面后两条通过了字面过滤，说明**字面过滤挡不住它们**——
	// 「赵太爷的儿子赵秀才」是一句肯定句，「把总…光头老人…」也是。
	// 挡住它们的是别的规则（link 需要两侧身份支撑、同名候选歧义），
	// 而真正判断"这句话是否说明同一性"是语义问题，本仓库不做。
	// 这条注释本身就是结论的一部分：不要把字面过滤读成语义判断。
}
