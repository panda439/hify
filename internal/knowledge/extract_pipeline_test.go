package knowledge

import (
	"context"
	"strings"
	"testing"
	"time"

	"hify/internal/provider"
)

// extract_pipeline_test.go 守两阶段抽取的编排（010 T027）。
//
// ⭐ 最要紧的一条：**归一阶段不合法时，第一阶段的结果也一条都不发布**。
//
// 部分发布听起来更宽容——关系已经抽出来了，人物归一没做成而已。但那样
// 产出的关系端点是一批**未经归一的临时人物**：同一个人在不同块里各建一个，
// 而这些碎片再也没有机会被合并（item 已经标成功了，不会重跑）。
// 结果是关系数据看起来完整，人物图却是碎的，且碎得没有规律。

const pipelineChunk = "赵太爷跳过去给了他一个嘴巴。阿Q摸着左颊，和地保退出去了。"

func stageOneBody() string {
	return `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
	                     {"ref":"m2","surface":"阿Q","occurrence":0}],
	         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"欺凌",
	                       "evidence":[{"quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}],
	         "alias_proposals":[]}`
}

// scriptedChat 按阶段返回预设响应，并记录每个阶段被调了几次。
type scriptedChat struct {
	byPhase map[string][]provider.ChatAttemptResult
	calls   map[string]int
	inputs  map[string][]string
}

func newScriptedChat() *scriptedChat {
	return &scriptedChat{
		byPhase: map[string][]provider.ChatAttemptResult{},
		calls:   map[string]int{},
		inputs:  map[string][]string{},
	}
}

func (s *scriptedChat) script(phase string, bodies ...string) {
	for _, b := range bodies {
		s.byPhase[phase] = append(s.byPhase[phase], provider.ChatAttemptResult{
			Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 10,
			FinishReason: "stop", Message: provider.Message{Content: b, FinishReason: "stop"},
		})
	}
}

func (s *scriptedChat) call(_ context.Context, phase, input string, _ int) (provider.ChatAttemptResult, error) {
	i := s.calls[phase]
	s.calls[phase]++
	s.inputs[phase] = append(s.inputs[phase], input)
	seq := s.byPhase[phase]
	if len(seq) == 0 {
		return provider.ChatAttemptResult{}, nil
	}
	if i >= len(seq) {
		i = len(seq) - 1
	}
	return seq[i], nil
}

func pipelineDeps(repo *Repository, chat *scriptedChat) extractionPipeline {
	return extractionPipeline{
		repo:   repo,
		runner: &phaseRunner{repo: repo, sleep: func(context.Context, time.Duration) error { return nil }},
		call:   chat.call,
	}
}

func pipelineItem(t *testing.T, repo *Repository, docID, jobID string) itemInput {
	t.Helper()
	job, epoch, item := publishFixture(t, repo, docID, jobID)
	pieces := chunkNarrative("第一章　甲\n"+pipelineChunk+"\n", 500, 0)
	if len(pieces) != 1 {
		t.Fatalf("夹具切出了 %d 块", len(pieces))
	}
	return itemInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		ChunkID: "c-1", DocumentVersion: 1,
		Content: pieces[0].Content, Metadata: *pieces[0].Narrative,
	}
}

// TestPipelineSkipsAliasCallWhenNothingToResolve——⭐ 没有候选也没有提案时
// 只调一次。⚠️ 照样调第二次的后果是白花一半的钱。
func TestPipelineSkipsAliasCallWhenNothingToResolve(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	chat.script(phaseExtract, stageOneBody())
	in := pipelineItem(t, repo, "doc-pl1", "job-pl1")

	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if chat.calls[phaseExtract] != 1 {
		t.Errorf("抽取阶段调了 %d 次, want 1", chat.calls[phaseExtract])
	}
	if chat.calls[phaseAlias] != 0 {
		t.Errorf("没有可归一的东西却调了 %d 次归一", chat.calls[phaseAlias])
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, in.JobID); n != 1 {
		t.Errorf("关系 %d 条, want 1", n)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, in.JobID); n != 2 {
		t.Errorf("人物 %d 个, want 2", n)
	}
}

// TestPipelinePublishesNothingWhenAliasIsInvalid——⭐ 本文件的立论。
//
// ⚠️ 部分发布产出的关系端点是一批**未经归一的临时人物**：同一个人在不同块里
// 各建一个，而这些碎片再也没有机会被合并（item 已经标成功，不会重跑）。
// 结果是关系数据看起来完整，人物图却是碎的，且碎得没有规律。
func TestPipelinePublishesNothingWhenAliasIsInvalid(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	// 第一阶段带一个别名提案，于是必须走第二阶段。
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
	     {"ref":"m2","surface":"阿Q","occurrence":0}],
	     "relations":[{"subject_ref":"m1","object_ref":"m2","type":"欺凌",
	       "evidence":[{"quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}],
	     "alias_proposals":[{"left":"m1","right":"m2",
	       "quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}`)
	// 第二阶段返回一份结构非法的响应（漏了 m2 的决策）。
	chat.script(phaseAlias, `{"decisions":[{"mention_ref":"m1","action":"new","new_group":1,
	     "reason_code":"context_identity",
	     "supports":[{"source_ref":"m1","quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}]}`)

	in := pipelineItem(t, repo, "doc-pl2", "job-pl2")
	err := pipelineDeps(repo, chat).processItem(t.Context(), in)
	if err == nil {
		t.Fatal("归一非法却没有报错")
	}
	for _, table := range []string{"narrative_characters", "narrative_relations", "narrative_relation_evidence"} {
		if n := countRows(t, repo,
			`SELECT COUNT(*) FROM `+table+` WHERE job_id=?`, in.JobID); n != 0 {
			t.Errorf("%s 留下了 %d 条——归一非法时第一阶段的结果也不该发布", table, n)
		}
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE id=? AND state='succeeded'`,
		in.ItemID); n != 0 {
		t.Error("归一非法却把 item 标成了成功")
	}
	// ⭐ 但账目必须留下：两个阶段的调用都真的花了钱。
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE item_id=?`, in.ItemID); n < 2 {
		t.Errorf("账上只有 %d 次尝试——已经花掉的调用被抹掉了", n)
	}
}

// TestPipelineReplaysStageOneInsteadOfCallingAgain——⭐ 第一阶段响应已落盘时
// 直接复用。⚠️ 再打一次的后果是那笔钱白花第二遍，而账目上两次都是真实调用，
// 看起来完全正常。
func TestPipelineReplaysStageOneInsteadOfCallingAgain(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	chat := newScriptedChat()
	chat.script(phaseExtract, stageOneBody())
	in := pipelineItem(t, repo, "doc-pl3", "job-pl3")

	// 第一次跑：落盘响应，但让归一阶段失败，于是 item 没成功。
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 10,
		FinishReason: "stop", Message: provider.Message{Content: stageOneBody()},
	}); err != nil {
		t.Fatal(err)
	}

	// 恢复后重跑：不该再打抽取阶段。
	if err := pipelineDeps(repo, chat).processItem(ctx, in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if chat.calls[phaseExtract] != 0 {
		t.Errorf("响应已落盘却又打了 %d 次抽取调用", chat.calls[phaseExtract])
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, in.JobID); n != 1 {
		t.Errorf("回放没有产出关系：%d 条", n)
	}
}

// TestPipelineKeepsAmbiguousIdentitiesApart——⭐ 合法的 ambiguous 是成功处理：
// 各自建独立人物并打上歧义标记，**不合并**。
func TestPipelineKeepsAmbiguousIdentitiesApart(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	chunk := "老王来了。后来另一个老王也来了。老王和老王吵了起来。"
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"老王","occurrence":0},
	     {"ref":"m2","surface":"老王","occurrence":1}],
	     "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突",
	       "evidence":[{"quote":"老王和老王吵了起来","occurrence":0}]}],
	     "alias_proposals":[{"left":"m1","right":"m2","quote":"老王和老王吵了起来","occurrence":0}]}`)
	chat.script(phaseAlias, `{"decisions":[
	     {"mention_ref":"m1","action":"ambiguous","new_group":1,"reason_code":"insufficient",
	      "supports":[{"source_ref":"m1","quote":"老王来了","occurrence":0}]},
	     {"mention_ref":"m2","action":"ambiguous","new_group":2,"reason_code":"insufficient",
	      "supports":[{"source_ref":"m2","quote":"后来另一个老王也来了","occurrence":0}]}]}`)

	job, epoch, item := publishFixture(t, repo, "doc-pl4", "job-pl4")
	pieces := chunkNarrative("第一章　甲\n"+chunk+"\n", 500, 0)
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}

	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, in.JobID); n != 2 {
		t.Errorf("歧义的两个称呼被合成了 %d 个人物, want 2", n)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM narrative_characters WHERE job_id=? AND has_ambiguity=1`, in.JobID); n != 2 {
		t.Errorf("歧义标记只打了 %d 个——查询时无法提示用户这里分不清", n)
	}
	// ⚠️ 别名记录的状态也必须是 ambiguous，不能是 rejected。
	// 混成 rejected 就把"系统看不出来"说成了"系统看出来不是"，
	// 而人工复核看到 rejected 会以为这里已经判定过了。变异测试逼出这条。
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM narrative_aliases WHERE job_id=? AND state='ambiguous'`, in.JobID); n != 2 {
		t.Errorf("state='ambiguous' 的别名记录 %d 条, want 2", n)
	}
}

// TestSameMentionRefInDifferentItemsKeepsBothDecisions——⭐ 变异测试逼出来的缺口。
//
// 模型在每个块里都从 m1 开始编号，所以**不同块的 mention ref 会重名**。
// 判定去重键如果只含 ref，第二个块的 m1 会撞上第一个块的 m1，
// 被 INSERT IGNORE 静默丢掉——那个称呼的判定依据从此查不到，
// 而人物和关系照样在，看不出少了什么。
func TestSameMentionRefInDifferentItemsKeepsBothDecisions(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-pl10", "job-pl10")

	rows, err := repo.db.QueryContext(ctx,
		`SELECT id FROM relation_extraction_items WHERE job_id=? ORDER BY chunk_index`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var items []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		items = append(items, id)
	}
	rows.Close()
	if len(items) < 2 {
		t.Fatalf("夹具只有 %d 个 item", len(items))
	}

	// 两个块各有一个 ref 为 m1 的称呼，但名字与位置都不同。
	for i, surface := range []string{"阿Q", "王胡"} {
		chat := newScriptedChat()
		chat.script(phaseExtract,
			`{"mentions":[{"ref":"m1","surface":"`+surface+`","occurrence":0}],
			  "relations":[],"alias_proposals":[]}`)
		pieces := chunkNarrative("第一章　甲\n"+surface+"来了。\n", 500, 0)
		in := itemInput{JobID: job.ID, ItemID: items[i], Epoch: epoch,
			ChunkID: "c-" + itoa(i), DocumentVersion: 1,
			Content: pieces[0].Content, Metadata: *pieces[0].Narrative}
		if err := pipelineDeps(repo, chat).processItem(ctx, in); err != nil {
			t.Fatalf("第 %d 个 item: %v", i, err)
		}
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM narrative_aliases WHERE job_id=?`, job.ID); n != 2 {
		t.Errorf("别名记录 %d 条, want 2——两个块的 m1 撞成了一条", n)
	}
}

// TestGroupDisplayNameComesFromTheFirstOccurrence——同一组里用**原文中最先
// 出现**的那个称呼作展示名。
//
// ⚠️ 用模型输出顺序里的第一个会让展示名取决于模型这次先列了谁，
// 而那在两次运行之间可以不同——回放稳定性检查抓不到它（两次用的是
// 同一份响应），只有换一份顺序不同的响应才看得出来。
func TestGroupDisplayNameComesFromTheFirstOccurrence(t *testing.T) {
	repo := extractionRepo(t)
	chunk := "阿Q，人称老Q。"
	// ⚠️ 模型把「老Q」列在前面，但它在原文里出现得更晚。
	chat := newScriptedChat()
	chat.script(phaseExtract, `{"mentions":[{"ref":"m2","surface":"老Q","occurrence":0},
	     {"ref":"m1","surface":"阿Q","occurrence":0}],
	     "relations":[],
	     "alias_proposals":[{"left":"m1","right":"m2","quote":"阿Q，人称老Q","occurrence":0}]}`)
	chat.script(phaseAlias, `{"decisions":[
	     {"mention_ref":"m1","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m1","quote":"阿Q，人称老Q","occurrence":0}]},
	     {"mention_ref":"m2","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m2","quote":"阿Q，人称老Q","occurrence":0}]}]}`)

	job, epoch, item := publishFixture(t, repo, "doc-pl11", "job-pl11")
	pieces := chunkNarrative("第一章　甲\n"+chunk+"\n", 500, 0)
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}
	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	var name string
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT display_name FROM narrative_characters WHERE job_id=?`, in.JobID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "阿Q" {
		t.Errorf("展示名 = %q, want 阿Q——用的是模型输出顺序而不是原文顺序", name)
	}
}

// TestPipelineRetriesAliasWithoutRecallingStageOne——⭐ 两个阶段**独立重试**。
// ⚠️ 归一失败就把抽取也重打一遍，等于每次归一重试都要多花一次抽取的钱，
// 而抽取那次的结果本来是好的。
func TestPipelineRetriesAliasWithoutRecallingStageOne(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"赵太爷","occurrence":0},
	     {"ref":"m2","surface":"阿Q","occurrence":0}],
	     "relations":[],
	     "alias_proposals":[{"left":"m1","right":"m2",
	       "quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]}`)
	// 归一先坏一次，再给一份合法的。
	chat.script(phaseAlias,
		`not json at all`,
		`{"decisions":[
		   {"mention_ref":"m1","action":"new","new_group":1,"reason_code":"context_identity",
		    "supports":[{"source_ref":"m1","quote":"赵太爷跳过去给了他一个嘴巴","occurrence":0}]},
		   {"mention_ref":"m2","action":"new","new_group":2,"reason_code":"context_identity",
		    "supports":[{"source_ref":"m2","quote":"阿Q摸着左颊","occurrence":0}]}]}`)

	in := pipelineItem(t, repo, "doc-pl5", "job-pl5")
	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if chat.calls[phaseExtract] != 1 {
		t.Errorf("抽取阶段被重打了：%d 次, want 1", chat.calls[phaseExtract])
	}
	if chat.calls[phaseAlias] != 2 {
		t.Errorf("归一阶段调了 %d 次, want 2", chat.calls[phaseAlias])
	}
}

// TestPipelineDoesNotReplayAMalformedResponse——⭐ 变异测试逼出来的缺口。
//
// ⚠️ 一份**格式坏掉**的响应如果照样回放，恢复之后每一轮都会拿它重来一次：
// item 永远好不了，不再花钱也不再产出——一个安静的死循环，日志上只有
// 一条重复的解析错误，没有任何东西说明它永远不会自愈。
func TestPipelineDoesNotReplayAMalformedResponse(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	chat := newScriptedChat()
	chat.script(phaseExtract, stageOneBody())
	in := pipelineItem(t, repo, "doc-pl7", "job-pl7")

	// 落一份坏响应：状态 completed、内容不是合法 JSON。
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 10,
		FinishReason: "stop", Message: provider.Message{Content: "这不是 JSON"},
	}); err != nil {
		t.Fatal(err)
	}
	// ⚠️ 手动预留 attempt 时必须同步 extract_attempt_count——生产路径上这两件事
	// 由 runPhase 一起做，只做一半会留下一个**生产下不可能出现**的状态，
	// 下一次 runPhase 从 attempt 1 重新开始，撞上 (item,phase,attempt) 唯一键。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_items SET extract_attempt_count = 1 WHERE id = ?`,
		in.ItemID); err != nil {
		t.Fatal(err)
	}

	if err := pipelineDeps(repo, chat).processItem(ctx, in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if chat.calls[phaseExtract] == 0 {
		t.Error("坏响应被回放了——恢复之后每一轮都会拿它重来，item 永远好不了")
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, in.JobID); n != 1 {
		t.Errorf("重新调用之后没有产出关系：%d 条", n)
	}
}

// TestPipelineMergesMentionsInTheSameGroup——⭐ 变异测试逼出来的第二个缺口。
//
// 同一组的两个称呼必须落到**同一个人物**上。各建一个的话，这一块自己就先碎了——
// 而归一阶段刚刚判定它们是同一个人。
func TestPipelineMergesMentionsInTheSameGroup(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	chunk := "阿Q，人称老Q。王胡打了阿Q一顿。"
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
	     {"ref":"m2","surface":"老Q","occurrence":0},
	     {"ref":"m3","surface":"王胡","occurrence":0}],
	     "relations":[{"subject_ref":"m3","object_ref":"m1","type":"欺凌",
	       "evidence":[{"quote":"王胡打了阿Q一顿","occurrence":0}]}],
	     "alias_proposals":[{"left":"m1","right":"m2","quote":"阿Q，人称老Q","occurrence":0}]}`)
	chat.script(phaseAlias, `{"decisions":[
	     {"mention_ref":"m1","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m1","quote":"阿Q，人称老Q","occurrence":0}]},
	     {"mention_ref":"m2","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m2","quote":"阿Q，人称老Q","occurrence":0}]},
	     {"mention_ref":"m3","action":"new","new_group":2,"reason_code":"context_identity",
	      "supports":[{"source_ref":"m3","quote":"王胡打了阿Q一顿","occurrence":0}]}]}`)

	job, epoch, item := publishFixture(t, repo, "doc-pl8", "job-pl8")
	pieces := chunkNarrative("第一章　甲\n"+chunk+"\n", 500, 0)
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}

	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, in.JobID); n != 2 {
		t.Errorf("人物 %d 个, want 2——「阿Q」和「老Q」同组却各建了一个", n)
	}
}

// TestPipelineRejectsSelfRelationAfterMerge——⭐ 变异测试逼出来的第三个缺口。
//
// 归一之后关系两端落到同一个人物：这条关系没有意义。
// ⚠️ 悄悄丢掉它会让关系总数少一条而没有任何迹象——而关系总数是报告里的数字。
func TestPipelineRejectsSelfRelationAfterMerge(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	chunk := "阿Q，人称老Q。阿Q和老Q吵了起来。"
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
	     {"ref":"m2","surface":"老Q","occurrence":0}],
	     "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突",
	       "evidence":[{"quote":"阿Q和老Q吵了起来","occurrence":0}]}],
	     "alias_proposals":[{"left":"m1","right":"m2","quote":"阿Q，人称老Q","occurrence":0}]}`)
	chat.script(phaseAlias, `{"decisions":[
	     {"mention_ref":"m1","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m1","quote":"阿Q，人称老Q","occurrence":0}]},
	     {"mention_ref":"m2","action":"new","new_group":1,"reason_code":"explicit_alias",
	      "supports":[{"source_ref":"m2","quote":"阿Q，人称老Q","occurrence":0}]}]}`)

	job, epoch, item := publishFixture(t, repo, "doc-pl9", "job-pl9")
	pieces := chunkNarrative("第一章　甲\n"+chunk+"\n", 500, 0)
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: pieces[0].Content, Metadata: *pieces[0].Narrative}

	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err == nil {
		t.Fatal("归一后自己和自己的关系被放行了")
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, in.JobID); n != 0 {
		t.Errorf("留下了 %d 条关系", n)
	}
}

// TestPipelineFeedsOnlyCitableText——⭐ 送给模型的正文里不含不可引用的部分。
// ⚠️ 模型看得见它就可能引用它，而我们拿不出那段话在书里的位置。
func TestPipelineFeedsOnlyCitableText(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	chat.script(phaseExtract, `{"mentions":[],"relations":[],"alias_proposals":[]}`)

	job, epoch, item := publishFixture(t, repo, "doc-pl6", "job-pl6")
	pieces := chunkNarrative("第一章　甲\n"+strings.Repeat("甲的正文。", 60)+"\n", 150, 40)
	var withCopy chunkPiece
	for _, p := range pieces {
		for _, seg := range p.Narrative.Segments {
			if seg.IsOverlapCopy {
				withCopy = p
			}
		}
		if withCopy.Content != "" {
			break
		}
	}
	if withCopy.Content == "" {
		t.Fatal("夹具没有产生带 overlap 拷贝的块")
	}
	in := itemInput{JobID: job.ID, ItemID: item, Epoch: epoch, ChunkID: "c-1",
		DocumentVersion: 1, Content: withCopy.Content, Metadata: *withCopy.Narrative}
	if err := pipelineDeps(repo, chat).processItem(t.Context(), in); err != nil {
		t.Fatalf("processItem: %v", err)
	}
	// ⚠️ 不能只比长度：指令头现在也一起送进去了（R6-01），
	// 加上指令之后总长必然比块内容长。要比的是**正文那一段**。
	// 也不能断言"送出去的文本不含 overlap 那几个字"——这个夹具是
	// 「甲的正文。」重复 60 次，那几个字在可引用区里也照样出现。
	proj, err := newChunkProjection(withCopy.Content, *withCopy.Narrative)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(proj.Text)) >= len([]rune(withCopy.Content)) {
		t.Errorf("投影没有短于块内容，overlap 拷贝没被去掉：%d vs %d",
			len([]rune(proj.Text)), len([]rune(withCopy.Content)))
	}
	sent := chat.inputs[phaseExtract][0]
	if want := extractInstruction + "\n\n" + proj.Text; sent != want {
		t.Errorf("送给模型的不是「指令 + 可引用正文」：\n got %q\nwant %q", sent, want)
	}
}
