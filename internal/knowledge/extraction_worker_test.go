package knowledge

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_worker_test.go 守生产工作循环（010 R6-01）。
//
// ⭐ 这个文件要证明的**不是**某个零件能工作，而是「这条链路真的会跑」：
// 一份文档从"开启抽取"到人物与关系落库，中间没有任何一步是测试自己
// 代劳的。R6-01 的全部要害就在这里——之前每个零件都有测试，
// 工作循环却写在测试里，于是生产路径上一次模型调用都不会发生。

// --- 一个会说话的假供应商 ---

type scriptedChatClient struct {
	calls    atomic.Int32
	byPhase  func(input string) (string, error)
	finish   string
	failWith error
}

func (c *scriptedChatClient) Chat(ctx context.Context, req provider.ChatRequest) (provider.Message, error) {
	return provider.Message{}, nil
}

func (c *scriptedChatClient) ChatStream(ctx context.Context, req provider.ChatRequest) (<-chan provider.ChatChunk, error) {
	ch := make(chan provider.ChatChunk)
	close(ch)
	return ch, nil
}

func (c *scriptedChatClient) Embed(ctx context.Context, req provider.EmbedRequest) (provider.EmbedResult, error) {
	return provider.EmbedResult{}, nil
}

func (c *scriptedChatClient) Rerank(ctx context.Context, req provider.RerankRequest) (provider.RerankResult, error) {
	return provider.RerankResult{}, nil
}

func (c *scriptedChatClient) TestConnection(ctx context.Context) error { return nil }

func (c *scriptedChatClient) ChatOnce(ctx context.Context, req provider.ChatRequest, timeout time.Duration) (provider.ChatAttemptResult, error) {
	c.calls.Add(1)
	if c.failWith != nil {
		return provider.ChatAttemptResult{
			Outcome: provider.AttemptFailed, ErrorCode: "boom",
		}, nil
	}
	input := ""
	if len(req.Messages) > 0 {
		input = req.Messages[0].Content
	}
	body, err := c.byPhase(input)
	if err != nil {
		return provider.ChatAttemptResult{}, err
	}
	finish := c.finish
	if finish == "" {
		finish = "stop"
	}
	return provider.ChatAttemptResult{
		Outcome:      provider.AttemptCompleted,
		FinishReason: finish,
		Message:      provider.Message{Role: provider.RoleAssistant, Content: body},
	}, nil
}

// chatProviderService 把上面那个客户端接到 provider.Service 上。
type chatProviderService struct {
	*fakeProviderService
	client *scriptedChatClient
}

func (f *chatProviderService) GetModel(ctx context.Context, id string) (provider.Model, error) {
	if id == "m-chat" {
		return provider.Model{ID: "m-chat", ProviderID: "p-chat", ModelName: "qwen-test",
			Capability: provider.CapabilityChat}, nil
	}
	return f.fakeProviderService.GetModel(ctx, id)
}

func (f *chatProviderService) ResolveClient(ctx context.Context, providerID string) (provider.Client, error) {
	if providerID == "p-chat" {
		return f.client, nil
	}
	return f.fakeProviderService.ResolveClient(ctx, providerID)
}

// workerFixture 造一份两块的叙事文档，登记好 pending 意图，返回 jobID。
func workerFixture(t *testing.T, repo *Repository, docID, jobID string, chunks int) string {
	t.Helper()
	ctx := t.Context()
	seedNarrativeDocument(t, repo, docID, chunks)
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET is_relation_extraction_enabled=1 WHERE id=?`, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO relation_extraction_jobs
		(id, document_id, knowledge_base_id, document_version, run_number, model_id,
		 config_hash, config_snapshot, state, approved_item_limit, call_limit, active_ms_limit)
		VALUES (?, ?, 'kb-x', 1, 1, 'm-chat', UNHEX(REPEAT('00',32)), '{}', 'pending', 500, 3000, 7200000)`,
		jobID, docID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET active_relation_job_id=?, relation_model_id='m-chat' WHERE id=?`,
		jobID, docID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

// emptyThenEmpty 是"什么都没抽到"的合法响应。
func alwaysEmptyExtraction(input string) (string, error) {
	if strings.Contains(input, aliasInstruction) {
		return `{"decisions":[]}`, nil
	}
	return `{"mentions":[],"relations":[],"alias_proposals":[]}`, nil
}

func newWorkerService(t *testing.T, repo *Repository, client *scriptedChatClient) *service {
	t.Helper()
	fp := &chatProviderService{fakeProviderService: newFakeProvider(), client: client}
	return NewService(repo, fp, nil, t.TempDir(), false, "", 1500*time.Millisecond, false).(*service)
}

// TestWorkerRunsAJobEndToEnd——⭐ 本文件的立论：这条链路真的会跑。
//
// 一个 pending 意图 → 初始化补 items → 逐个调模型 → 落库 → 作业停成
// succeeded。⚠️ 少了这一条，所有零件测试全绿而**生产环境一次调用都不会
// 发生**，用户看到的是永远 0/0 的进度条。
func TestWorkerRunsAJobEndToEnd(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	client := &scriptedChatClient{byPhase: alwaysEmptyExtraction}
	svc := newWorkerService(t, repo, client)
	jobID := workerFixture(t, repo, "doc-w-e2e", "job-w-e2e", 3)

	if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
		t.Fatalf("RunRelationExtraction: %v", err)
	}

	var state string
	var total, succeeded, failed int
	var initComplete bool
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state, initialization_complete, total_items, succeeded_items, failed_items
		 FROM relation_extraction_jobs WHERE id=?`, jobID).
		Scan(&state, &initComplete, &total, &succeeded, &failed); err != nil {
		t.Fatal(err)
	}
	if !initComplete || total != 3 {
		t.Fatalf("初始化没完成：complete=%v total=%d", initComplete, total)
	}
	if succeeded != 3 || failed != 0 {
		t.Errorf("succeeded=%d failed=%d，want 3/0", succeeded, failed)
	}
	if state != jobStateSucceeded {
		t.Errorf("state = %q, want succeeded", state)
	}
	// ⭐ 模型必须真的被调到。⚠️ 这一格是本文件的核心：一个"什么都不做"
	// 的工作循环会让上面每一条断言都失败得不明显，而这条直接说明问题。
	if n := client.calls.Load(); n < 3 {
		t.Errorf("模型只被调了 %d 次，3 个片段至少要 3 次", n)
	}
	// 租约必须释放，否则下一轮谁都抢不到。
	var leaseHeld bool
	if err := repo.db.QueryRowContext(ctx,
		`SELECT lease_until IS NOT NULL FROM relation_extraction_jobs WHERE id=?`, jobID).
		Scan(&leaseHeld); err != nil {
		t.Fatal(err)
	}
	if leaseHeld {
		t.Error("跑完之后租约没释放")
	}
}

// TestWorkerWritesRealCharactersAndRelations——抽到东西时必须真的落库。
func TestWorkerWritesRealCharactersAndRelations(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	// seedNarrativeDocument 的正文是「正文。」重复，里面没有人名；
	// 这里让模型引用它确实包含的一句话，并声明两个人物。
	client := &scriptedChatClient{byPhase: func(input string) (string, error) {
		if strings.Contains(input, aliasInstruction) {
			return `{"decisions":[]}`, nil
		}
		return `{"mentions":[{"ref":"m1","surface":"正文","occurrence":0},
		         {"ref":"m2","surface":"正文","occurrence":1}],
		         "relations":[{"subject_ref":"m1","object_ref":"m2","type":"冲突",
		           "evidence":[{"quote":"正文。正文。","occurrence":0}]}],
		         "alias_proposals":[]}`, nil
	}}
	svc := newWorkerService(t, repo, client)
	jobID := workerFixture(t, repo, "doc-w-rel", "job-w-rel", 2)

	if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
		t.Fatalf("RunRelationExtraction: %v", err)
	}
	var chars, rels, evid int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM narrative_characters WHERE job_id=?),
		        (SELECT COUNT(*) FROM narrative_relations WHERE job_id=?),
		        (SELECT COUNT(*) FROM narrative_relation_evidence WHERE job_id=?)`,
		jobID, jobID, jobID).Scan(&chars, &rels, &evid); err != nil {
		t.Fatal(err)
	}
	if chars == 0 || rels == 0 || evid == 0 {
		t.Fatalf("跑完了却什么都没落库：人物 %d 关系 %d 证据 %d", chars, rels, evid)
	}
	// ⭐ 账目也必须真的记上——成本数字的全部依据。
	var confirmed int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT confirmed_dispatches FROM relation_extraction_jobs WHERE id=?`, jobID).
		Scan(&confirmed); err != nil {
		t.Fatal(err)
	}
	if confirmed == 0 {
		t.Error("跑完了但账目里一次确认调用都没有——成本数字会是 0")
	}
}

// TestWorkerLeavesPendingWhenDocumentIsNotReady——文档还没解析完时，
// 作业必须**原样留在 pending**，不能判失败也不能空转。
func TestWorkerLeavesPendingWhenDocumentIsNotReady(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	client := &scriptedChatClient{byPhase: alwaysEmptyExtraction}
	svc := newWorkerService(t, repo, client)
	jobID := workerFixture(t, repo, "doc-w-notready", "job-w-notready", 2)
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET status='processing' WHERE id='doc-w-notready'`); err != nil {
		t.Fatal(err)
	}

	if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
		t.Fatalf("RunRelationExtraction: %v", err)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != jobStatePending {
		t.Errorf("文档没就绪时 state = %q，want pending（下一轮再来）", state)
	}
	if n := client.calls.Load(); n != 0 {
		t.Errorf("文档都没就绪却调了 %d 次模型", n)
	}
}

// TestWorkerDoesNotTouchPausedJobs——⭐ paused / budget_exhausted 是
// **用户或预算做出的决定**。
//
// ⚠️ 自动跑起来等于系统擅自推翻一次显式的停止，而用户会看到一个自己
// 明明暂停过的作业又开始花钱。
func TestWorkerDoesNotTouchPausedJobs(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	for _, state := range []string{jobStatePaused, jobStateBudgetExhausted, jobStateSucceeded} {
		client := &scriptedChatClient{byPhase: alwaysEmptyExtraction}
		svc := newWorkerService(t, repo, client)
		docID, jobID := "doc-w-"+state, "job-w-"+state
		workerFixture(t, repo, docID, jobID, 2)
		if _, err := repo.db.ExecContext(ctx,
			`UPDATE relation_extraction_jobs SET state=? WHERE id=?`, state, jobID); err != nil {
			t.Fatal(err)
		}
		if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if n := client.calls.Load(); n != 0 {
			t.Errorf("状态 %s 的作业被跑起来了（调了 %d 次模型）", state, n)
		}
		var got string
		if err := repo.db.QueryRowContext(ctx,
			`SELECT state FROM relation_extraction_jobs WHERE id=?`, jobID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != state {
			t.Errorf("状态 %s 被改成了 %s", state, got)
		}
	}
}

// TestWorkerStopsAfterConsecutiveFailures——⚠️ 配置问题（模型不存在、
// prompt 与解析器对不上）会让**每一个** item 都失败。不停手的话，
// 一本书会一个 item 一个 item 地把额度烧完，而每一条都失败。
func TestWorkerStopsAfterConsecutiveFailures(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	// 永远返回解析不了的东西。
	client := &scriptedChatClient{byPhase: func(string) (string, error) {
		return "这不是 JSON", nil
	}}
	svc := newWorkerService(t, repo, client)
	jobID := workerFixture(t, repo, "doc-w-fail", "job-w-fail", 20)

	if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
		t.Fatalf("RunRelationExtraction: %v", err)
	}
	var state string
	var failed int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state, failed_items FROM relation_extraction_jobs WHERE id=?`, jobID).
		Scan(&state, &failed); err != nil {
		t.Fatal(err)
	}
	if state != jobStateFailed {
		t.Errorf("连续失败之后 state = %q，want failed", state)
	}
	// ⭐ 必须在上限处停手，不能把 20 个片段全烧完。
	if failed > maxConsecutiveItemFailures {
		t.Errorf("失败了 %d 个才停，上限是 %d——额度被白烧",
			failed, maxConsecutiveItemFailures)
	}
}

// TestReconcileActuallyEnqueues——⭐ R6-01 的核心症状：扫描报告
// "已重排 N 个"，而一个作业都没排进队。
//
// ⚠️ 没有队列客户端时必须**报错**，不能静默成功——静默成功正是那个
// 让所有人都以为一切正常的形态。
func TestReconcileActuallyEnqueues(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	workerFixture(t, repo, "doc-w-enq", "job-w-enq", 2)

	// asynqClient 为 nil 的 service：扫描本身仍然成功（其余清理工作要做完），
	// 但入队会失败并记日志。这里断言的是"入队这件事真的被尝试了"。
	svc := newWorkerService(t, repo, &scriptedChatClient{byPhase: alwaysEmptyExtraction})
	if err := svc.enqueueRunExtraction(ctx, "job-w-enq"); err == nil {
		t.Error("没有队列客户端却入队成功了——这正是 R6-01 的形态")
	}

	// 带真实队列时必须真的排进去。
	client := newTestAsynqClient(t)
	svc2 := NewService(repo, newFakeProvider(), client, t.TempDir(),
		false, "", 1500*time.Millisecond, false).(*service)
	if err := svc2.enqueueRunExtraction(ctx, "job-w-enq"); err != nil {
		t.Errorf("有队列却没能入队：%v", err)
	}
}

// TestWorkerStopsOnBudgetExhaustion——⭐ 额度用完停成 budget_exhausted，
// **不是** failed。
//
// ⚠️ 两者的下一步完全不同：前者追加额度点续跑就继续，后者续跑只会再失败
// 一次。合成一个状态等于让用户无从判断该做什么。而且额度用完的那个 item
// 一条都没跑，不能记进 failed_items——记了的话追加额度之后它不会被重跑，
// 那一段书就永远缺着，而覆盖率看起来是满的。
func TestWorkerStopsOnBudgetExhaustion(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	client := &scriptedChatClient{byPhase: alwaysEmptyExtraction}
	svc := newWorkerService(t, repo, client)
	jobID := workerFixture(t, repo, "doc-w-budget", "job-w-budget", 4)
	// 只给 2 次调用额度，4 个片段跑不完。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET call_limit=2 WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}

	if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
		t.Fatalf("RunRelationExtraction: %v", err)
	}
	var state string
	var succeeded, failed int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state, succeeded_items, failed_items FROM relation_extraction_jobs WHERE id=?`,
		jobID).Scan(&state, &succeeded, &failed); err != nil {
		t.Fatal(err)
	}
	if state != jobStateBudgetExhausted {
		t.Errorf("state = %q，want budget_exhausted", state)
	}
	if failed != 0 {
		t.Errorf("额度用完被记成了 %d 个失败片段——追加额度后它们不会被重跑", failed)
	}
	if succeeded == 0 {
		t.Error("额度耗尽前那几次也没成功，用例没测到想测的边界")
	}
}

// TestWorkerFailsAJobThatSucceededNothing——全部片段都失败、但没达到连续
// 失败上限时，作业**不能停成 succeeded**。
//
// ⚠️ 停成 succeeded 的表现最坏：状态说"完成"，而一条关系都没有——
// 用户会以为这本书里就是没写人物关系。
func TestWorkerFailsAJobThatSucceededNothing(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	client := &scriptedChatClient{byPhase: func(string) (string, error) {
		return "这不是 JSON", nil
	}}
	svc := newWorkerService(t, repo, client)
	// 3 个片段 < maxConsecutiveItemFailures，循环会正常走完再收尾。
	jobID := workerFixture(t, repo, "doc-w-none", "job-w-none", 3)

	if err := svc.RunRelationExtraction(ctx, jobID); err != nil {
		t.Fatalf("RunRelationExtraction: %v", err)
	}
	var state string
	var succeeded, failed int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state, succeeded_items, failed_items FROM relation_extraction_jobs WHERE id=?`,
		jobID).Scan(&state, &succeeded, &failed); err != nil {
		t.Fatal(err)
	}
	if succeeded != 0 || failed != 3 {
		t.Fatalf("夹具没走到想要的形态：succeeded=%d failed=%d", succeeded, failed)
	}
	if state != jobStateFailed {
		t.Errorf("一个都没成功却 state = %q，want failed", state)
	}
}

// TestReconcileEnqueuesEveryRecoverableJob——⭐ 扫描必须**真的**把作业
// 交出去，而不是数一遍就算。
//
// ⚠️ 这一格盯的正是 R6-01 的原始形态：回调只有一行 slog.Info，
// JobsRequeued 照样累加，扫描每分钟报告"已重排 N 个"，
// 而一个作业都不会被跑。
func TestReconcileEnqueuesEveryRecoverableJob(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	workerFixture(t, repo, "doc-w-rq1", "job-w-rq1", 2)
	workerFixture(t, repo, "doc-w-rq2", "job-w-rq2", 2)

	var handed []string
	res, err := repo.reconcileRelationExtractionsWith(ctx, func(job recoverableJob) {
		handed = append(handed, job.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.JobsRequeued != len(handed) {
		t.Errorf("报告重排了 %d 个，实际交出去 %d 个——数字与事实对不上",
			res.JobsRequeued, len(handed))
	}
	got := map[string]bool{}
	for _, id := range handed {
		got[id] = true
	}
	for _, want := range []string{"job-w-rq1", "job-w-rq2"} {
		if !got[want] {
			t.Errorf("%s 没有被交给接手方", want)
		}
	}
}
