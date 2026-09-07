package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_runner_test.go 守作业运行的串联（010 T021 运行侧）。
//
// ⭐ 这里的用例几乎都在盯**停在什么地方**，而不是"能不能跑通"：
// 预算耗尽、连续失败、租约丢失、重放——每一种都要停，但停下之后
// 留下的状态完全不同，而搞混了不会报错，只会让某个数字不对。

// fakeModel 是一个可编程的单次调用模型。
type fakeModel struct {
	mu      sync.Mutex
	calls   int
	byPhase func(call int, prompt string) provider.ChatAttemptResult
}

func (f *fakeModel) ChatOnce(_ context.Context, _ string, req provider.ChatRequest, _ time.Duration) (provider.ChatAttemptResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.byPhase(f.calls, req.Messages[0].Content), nil
}

func (f *fakeModel) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func completedWith(content string) provider.ChatAttemptResult {
	return provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 5,
		Message: provider.Message{Content: content}, FinishReason: "stop",
	}
}

// 每一章都是一小段有名有姓的原文，让抽取的引用有得可查。
const runnerChapterBody = "赵太爷打了阿Q一个嘴巴。阿Q回到土谷祠，心里想着这件事。"

func seedRunnerDocument(t *testing.T, repo *Repository, docID string, chapters int) []string {
	t.Helper()
	ctx := t.Context()
	var body strings.Builder
	for i := 1; i <= chapters; i++ {
		body.WriteString(fmt.Sprintf("第%s章　题\n", chineseOrdinal(i)))
		body.WriteString(runnerChapterBody + "\n")
	}
	pieces := chunkNarrative(body.String(), 500, 0)
	if len(pieces) != chapters {
		t.Fatalf("夹具想要 %d 块，实际切出 %d 块", chapters, len(pieces))
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO documents
		(id, knowledge_base_id, file_name, file_type, file_size, storage_path,
		 status, chunk_count, created_by, is_narrative)
		VALUES (?, 'kb-x', 'novel.txt', 'txt', 1, '/tmp/n', 'ready', ?, 'u1', 1)`,
		docID, chapters); err != nil {
		t.Fatal(err)
	}
	var ids []string
	cs := make([]Chunk, 0, chapters)
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

// 一份对上面语料合法的抽取响应。
const runnerExtractJSON = `{"mentions":[{"ref":"m1","surface":"阿Q","occurrence":0},
	{"ref":"m2","surface":"赵太爷","occurrence":0}],
	"relations":[{"subject_ref":"m2","object_ref":"m1","type":"欺凌",
		"evidence":[{"quote":"赵太爷打了阿Q一个嘴巴","occurrence":0}]}],
	"alias_proposals":[]}`

// 归一阶段：两个称呼各自独立成新人物，依据引自原文。
const runnerAliasJSON = `{"decisions":[
	{"mention_ref":"m1","action":"new","character_id":"","new_group":"g1",
	 "supports":[{"source_ref":"chunk","quote":"阿Q回到土谷祠","occurrence":0}],"reason_code":"context_identity"},
	{"mention_ref":"m2","action":"new","character_id":"","new_group":"g2",
	 "supports":[{"source_ref":"chunk","quote":"赵太爷打了阿Q一个嘴巴","occurrence":0}],"reason_code":"context_identity"}]}`

// linkingModel 是会**归一**的假模型：看到候选里有同名人物就 link 过去，
// 否则新建。⚠️ 它顺带验证了候选确实被渲染进了提示词——link 需要引用
// 候选自己的依据 ref，而那个 ref 只能从提示词里读到。
func linkingModel() *fakeModel {
	// 候选行形如：- 阿Q（id=xxx）；依据 xxx#0：……
	candidateLine := regexp.MustCompile(`- ([^（\n]+)（id=([^）]+)）(?:；依据 ([^：]+)：)?`)
	mentionLine := regexp.MustCompile(`- ([^（\n]+)（ref=([^）]+)）`)
	return &fakeModel{byPhase: func(_ int, prompt string) provider.ChatAttemptResult {
		if !strings.Contains(prompt, `{"decisions"`) {
			return completedWith(runnerExtractJSON)
		}
		candidates := map[string][2]string{} // 名字 -> {id, 依据 ref}
		for _, m := range candidateLine.FindAllStringSubmatch(prompt, -1) {
			if _, seen := candidates[m[1]]; !seen {
				candidates[m[1]] = [2]string{m[2], m[3]}
			}
		}
		var decisions []string
		for i, m := range mentionLine.FindAllStringSubmatch(prompt, -1) {
			surface, ref := m[1], m[2]
			cand, ok := candidates[surface]
			if ok && cand[1] != "" {
				decisions = append(decisions, fmt.Sprintf(
					`{"mention_ref":%q,"action":"link","character_id":%q,"new_group":"",`+
						`"supports":[{"source_ref":"chunk","quote":%q,"occurrence":0},`+
						`{"source_ref":%q,"quote":"依据","occurrence":0}],"reason_code":"context_identity"}`,
					ref, cand[0], surface, cand[1]))
				continue
			}
			decisions = append(decisions, newDecision(ref, fmt.Sprintf("g%d", i), surface, 0))
		}
		return completedWith(`{"decisions":[` + strings.Join(decisions, ",") + `]}`)
	}}
}

// happyModel 按提示词里出现的固定串区分两个阶段。
func happyModel() *fakeModel {
	return &fakeModel{byPhase: func(_ int, prompt string) provider.ChatAttemptResult {
		if strings.Contains(prompt, `{"decisions"`) {
			return completedWith(runnerAliasJSON)
		}
		return completedWith(runnerExtractJSON)
	}}
}

func runnerFor(t *testing.T, repo *Repository, model singleAttemptModel) *extractionRunner {
	t.Helper()
	r := newExtractionRunner(repo, model)
	// 退避在测试里不真的睡：3 次尝试之间的 1s+2s 会让每个失败用例慢 3 秒。
	r.phases.sleep = func(context.Context, time.Duration) error { return nil }
	return r
}

func jobCounts(t *testing.T, repo *Repository, jobID string) (succeeded, failed int, state string) {
	t.Helper()
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT succeeded_items, failed_items, state FROM relation_extraction_jobs WHERE id=?`,
		jobID).Scan(&succeeded, &failed, &state); err != nil {
		t.Fatal(err)
	}
	return
}

// TestRunJobProcessesEveryItem：把一个作业跑完，人物/关系/证据/计数都落库。
func TestRunJobProcessesEveryItem(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-run1", 3)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-run1", "doc-run1", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	model := happyModel()

	res, err := runnerFor(t, repo, model).runJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if res.ItemsSucceeded != 3 || res.ItemsFailed != 0 || res.StoppedEarly {
		t.Fatalf("res = %+v", res)
	}
	succeeded, failed, _ := jobCounts(t, repo, job.ID)
	if succeeded != 3 || failed != 0 {
		t.Errorf("作业计数 succeeded=%d failed=%d", succeeded, failed)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 3 {
		t.Errorf("关系数 = %d, want 3（每章一条）", n)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relation_evidence WHERE job_id=?`, job.ID); n != 3 {
		t.Errorf("证据数 = %d, want 3", n)
	}
	// 每一次调用都必须在账上。
	if n := countRows(t, repo, `SELECT COUNT(*) FROM relation_extraction_attempts WHERE job_id=?`, job.ID); n != model.callCount() {
		t.Errorf("账上 %d 次调用，实际发生 %d 次", n, model.callCount())
	}
	// 第一块没有任何候选也没有别名提案 → 不该有第二次调用；
	// 后两块有候选（第一块建出来的人物）→ 各多一次。
	if got := model.callCount(); got != 5 {
		t.Errorf("调用次数 = %d, want 5（3 次抽取 + 后两块各 1 次归一）", got)
	}
}

// TestReplayedResponseIsNotCalledAgain：已经落盘的成功响应必须被回放，
// ⚠️ 再打一次不会"结果不一致"，只是那笔钱白花第二遍，而账目看起来完全正常。
func TestReplayedResponseIsNotCalledAgain(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-replay", 1)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-replay", "doc-replay", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	model := happyModel()
	if _, err := runnerFor(t, repo, model).runJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	before := model.callCount()

	// 模拟"模型答完、发布事务回滚之后崩了"：派生结果全部清掉，item 打回
	// pending，只留下已经落盘的那次成功尝试。
	// ⚠️ 派生结果必须一起清：发布是一个事务，它回滚时人物和关系也一起没了。
	// 只把 item 打回 pending 而留着人物，模拟的是一个数据库做不到的状态。
	for _, q := range []string{
		`DELETE FROM narrative_relation_evidence WHERE job_id=?`,
		`DELETE FROM narrative_relations WHERE job_id=?`,
		`DELETE FROM narrative_characters WHERE job_id=?`,
		`UPDATE relation_extraction_items SET state='pending' WHERE job_id=?`,
		// 崩在半路的作业当然还没收尾。
		`UPDATE relation_extraction_jobs SET state='running', finished_at=NULL WHERE id=?`,
	} {
		if _, err := repo.db.ExecContext(ctx, q, job.ID); err != nil {
			t.Fatal(err)
		}
	}
	res, err := runnerFor(t, repo, model).runJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if model.callCount() != before {
		t.Errorf("回放路径又打了 %d 次模型", model.callCount()-before)
	}
	if res.ItemsSucceeded != 1 {
		t.Errorf("重放没有被当成成功处理：%+v", res)
	}
	// 结果只发布一次。
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 1 {
		t.Errorf("重放之后关系变成 %d 条", n)
	}
}

// TestInvalidOutputIsRetriedThenFailsTheItem：结构不合法的输出在**重试循环
// 里面**被判失败——模型偶尔漏个字段是常态，第二次通常就对了；
// 三次都不合法才判这个 item 失败，而三次调用照常记账。
func TestInvalidOutputIsRetriedThenFailsTheItem(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-bad", 1)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-bad", "doc-bad", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{byPhase: func(call int, _ string) provider.ChatAttemptResult {
		return completedWith("```json\n" + runnerExtractJSON + "\n```") // 带围栏，永远不合法
	}}

	res, err := runnerFor(t, repo, model).runJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if res.ItemsFailed != 1 || res.ItemsSucceeded != 0 {
		t.Fatalf("res = %+v", res)
	}
	if model.callCount() != maxAttemptsPerPhase {
		t.Errorf("重试了 %d 次，want %d", model.callCount(), maxAttemptsPerPhase)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM relation_extraction_attempts WHERE job_id=?`, job.ID); n != maxAttemptsPerPhase {
		t.Errorf("账上只有 %d 次调用，实际发生 %d 次", n, model.callCount())
	}
	var code string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT last_error_code FROM relation_extraction_items WHERE job_id=?`, job.ID).Scan(&code); err != nil {
		t.Fatal(err)
	}
	if code != "extract_invalid" {
		t.Errorf("last_error_code = %q", code)
	}
}

// TestFabricatedQuoteFailsTheItem：引用在原文里找不到 → 这个 item 失败，
// ⭐ 绝不"留下能定位的那部分"。
func TestFabricatedQuoteFailsTheItem(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-fab", 1)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-fab", "doc-fab", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	fabricated := strings.Replace(runnerExtractJSON, "赵太爷打了阿Q一个嘴巴", "赵太爷狠狠地打了阿Q一顿", 1)
	model := &fakeModel{byPhase: func(int, string) provider.ChatAttemptResult { return completedWith(fabricated) }}

	res, err := runnerFor(t, repo, model).runJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if res.ItemsFailed != 1 {
		t.Fatalf("编造的引用没有让 item 失败：%+v", res)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("发布了 %d 条没有原文支持的关系", n)
	}
}

// TestConsecutiveFailuresStopTheJob：连续失败几乎一定是系统性问题，
// 继续跑只是拿剩下的 item 把预算烧完。
func TestConsecutiveFailuresStopTheJob(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-runstop", 8)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-runstop", "doc-runstop", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	model := &fakeModel{byPhase: func(int, string) provider.ChatAttemptResult { return completedWith("{}") }}

	res, err := runnerFor(t, repo, model).runJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if res.StopReason != "consecutive_failures" || !res.StoppedEarly {
		t.Fatalf("res = %+v", res)
	}
	if res.ItemsFailed != maxConsecutiveItemFailures {
		t.Errorf("失败 %d 个就该停，实际跑了 %d 个", maxConsecutiveItemFailures, res.ItemsFailed)
	}
	// 剩下的 item 一个都没被碰过。
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE job_id=? AND state='pending'`, job.ID); n != 3 {
		t.Errorf("还剩 %d 个 pending，want 3", n)
	}
}

// TestBudgetExhaustionStopsWithoutFailingItems：预算耗尽不是这个 item 的
// 失败——追加额度之后要能从原地接着跑。
//
// ⚠️ 把它记成失败会让"因为没钱停下"看起来像"这段书抽不出东西"，
// 而后者会进召回率的分母。
func TestBudgetExhaustionStopsWithoutFailingItems(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-runbudget", 4)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-runbudget", "doc-runbudget", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET call_limit = 2 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}

	res, err := runnerFor(t, repo, happyModel()).runJob(ctx, job.ID)
	if !errors.Is(err, ErrExtractionCallBudgetExhausted) {
		t.Fatalf("err = %v, want 调用额度用尽", err)
	}
	if res.StopReason != "budget_exhausted" {
		t.Errorf("StopReason = %q", res.StopReason)
	}
	if res.ItemsFailed != 0 {
		t.Errorf("预算耗尽把 %d 个 item 记成了失败", res.ItemsFailed)
	}
	_, failed, _ := jobCounts(t, repo, job.ID)
	if failed != 0 {
		t.Errorf("作业的 failed_items = %d，预算耗尽不该计失败", failed)
	}
}

// TestSupersededJobStopsBeforeSpendingMore：文档指向了别的 run 之后，
// 旧 worker 必须停手——它花的每一分钱都发布不出去。
func TestSupersededJobStopsBeforeSpendingMore(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-runsuper", 3)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-runsuper", "doc-runsuper", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET active_relation_job_id = 'job-other' WHERE id = 'doc-runsuper'`); err != nil {
		t.Fatal(err)
	}
	model := happyModel()

	res, err := runnerFor(t, repo, model).runJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("runJob: %v", err)
	}
	if res.StopReason != "superseded" {
		t.Fatalf("res = %+v", res)
	}
	if model.callCount() != 0 {
		t.Errorf("被取代的作业还打了 %d 次模型", model.callCount())
	}
}

// TestRunJobRefusesAClaimedJob：租约还在别人手里时安静退出，不重试。
func TestRunJobRefusesAClaimedJob(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-claimed", 1)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-claimed", "doc-claimed", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.claimExtractionJob(ctx, job.ID, extractionLeaseTTL); err != nil || !ok {
		t.Fatalf("第一次接手失败: ok=%v err=%v", ok, err)
	}
	if _, err := runnerFor(t, repo, happyModel()).runJob(ctx, job.ID); !errors.Is(err, ErrExtractionJobNotClaimable) {
		t.Fatalf("err = %v, want 不可接手", err)
	}
}

// TestIdentityEvidenceIsPersisted：新建人物必须带上归一时用的依据，
// 否则下一块看到这个候选时没有任何可核对的身份出处，link 永远做不成。
func TestIdentityEvidenceIsPersisted(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedRunnerDocument(t, repo, "doc-ident", 2)
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-ident", "doc-ident", 1, "m-1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runnerFor(t, repo, happyModel()).runJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.db.QueryContext(ctx,
		`SELECT identity_evidence FROM narrative_characters WHERE job_id=? AND identity_evidence IS NOT NULL`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var supports []aliasSupport
		if err := json.Unmarshal(raw, &supports); err != nil {
			t.Fatalf("identity_evidence 不是 supports 数组：%v", err)
		}
		if len(supports) == 0 || supports[0].Quote == "" {
			t.Errorf("identity_evidence 里没有可核对的引用：%s", raw)
		}
		n++
	}
	if n == 0 {
		t.Error("一个带身份依据的人物都没有")
	}
}
