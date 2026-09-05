package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_retry_test.go 守**唯一**的重试层（010 T018）。
//
// ⭐ "唯一"是这一段最重要的性质。provider.ChatOnce 不重试，这里重试。
// 两层都重试的话，一个 item 最坏会打出 3×3 = 9 次真实请求，而账目只会
// 显示 3 次——成本数字直接错三倍，且没有任何症状。
//
// ⚠️ 重试次数落在**数据库**里（items.extract_attempt_count），不在内存。
// 放内存的表现是：worker 崩溃重启后计数归零，于是一个永远会失败的 item
// 被无限重试下去，把预算烧光。这是自动恢复最容易引入的一种死循环。

func completed(finish string) provider.ChatAttemptResult {
	return provider.ChatAttemptResult{Outcome: provider.AttemptCompleted,
		Dispatched: true, FinishReason: finish,
		Message: provider.Message{Content: "{}", FinishReason: finish}}
}

func failedWith(code string) provider.ChatAttemptResult {
	return provider.ChatAttemptResult{Outcome: provider.AttemptFailed,
		Dispatched: true, ErrorCode: code}
}

// TestRetryDecisions——每一格都是一条明确的取舍，不是"看起来合理"。
func TestRetryDecisions(t *testing.T) {
	cases := []struct {
		name    string
		res     provider.ChatAttemptResult
		attempt int
		want    retryDecision
	}{
		{"成功", completed("stop"), 1, decisionAccept},

		// ⚠️ 输出被截断**不重试**：同一个请求再打一次会撞上同一堵墙，
		// 只是把预算再花一遍。要解决只能缩输入，那是上层的事。
		{"输出被截断", completed("length"), 1, decisionGiveUp},

		// 服务端错误、限流：值得重试。
		{"500", failedWith("http_500"), 1, decisionRetry},
		{"429", failedWith("http_429"), 1, decisionRetry},
		{"传输层不确定", provider.ChatAttemptResult{
			Outcome: provider.AttemptUnknown, ErrorCode: "timeout"}, 1, decisionRetry},
		{"熔断打开（确定没发出）", provider.ChatAttemptResult{
			Outcome: provider.AttemptNotDispatched, ErrorCode: "circuit_open"}, 1, decisionRetry},

		// ⭐ 配置类错误**永不重试**。模型不存在、密钥错、请求体不合法——
		// 再打两次只是把同一个错误重复三遍，浪费预算并把真正的原因
		// 埋在一堆重试日志里。
		{"400 请求不合法", failedWith("http_400"), 1, decisionGiveUp},
		{"401 认证失败", failedWith("http_401"), 1, decisionGiveUp},
		{"404 模型不存在", failedWith("http_404"), 1, decisionGiveUp},
		{"422 参数不被接受", failedWith("http_422"), 1, decisionGiveUp},

		// 次数用尽。
		{"第 3 次仍然 500", failedWith("http_500"), maxAttemptsPerPhase, decisionGiveUp},
		{"第 2 次 500 还能再试", failedWith("http_500"), maxAttemptsPerPhase - 1, decisionRetry},
	}
	for _, tc := range cases {
		if got := decideRetry(tc.res, tc.attempt); got != tc.want {
			t.Errorf("%s：decideRetry = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRetryBackoff——1s、2s 两个间隔（3 次尝试之间有 2 个间隔）。
func TestRetryBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second}
	for i, w := range want {
		if got := retryBackoff(i + 1); got != w {
			t.Errorf("第 %d 次尝试之后退避 %v, want %v", i+1, got, w)
		}
	}
	// 次数用尽之后不该再有退避可言。
	if got := retryBackoff(maxAttemptsPerPhase); got != 0 {
		t.Errorf("最后一次尝试之后仍返回退避 %v", got)
	}
}

// TestConsecutiveFailureCircuit——⭐ 连续 5 个 item 最终失败就停整个作业。
//
// ⚠️ 连续失败几乎一定是**系统性**问题（prompt 与 schema 对不上、模型被换掉、
// 语料格式不对），不是运气差。继续跑只是拿剩下几百个 item 把预算烧完，
// 换回一堆同样的失败。而"跑完了但全是失败"看起来比"提前停下"更像正常结束。
func TestConsecutiveFailureCircuit(t *testing.T) {
	for n := 0; n < maxConsecutiveItemFailures; n++ {
		if shouldStopAfterConsecutiveFailures(n) {
			t.Errorf("连续 %d 个失败就停了，阈值是 %d", n, maxConsecutiveItemFailures)
		}
	}
	if !shouldStopAfterConsecutiveFailures(maxConsecutiveItemFailures) {
		t.Errorf("连续 %d 个失败没有停", maxConsecutiveItemFailures)
	}
}

// --- 带数据库的部分：计数必须落库 ---

type scriptedCaller struct {
	results []provider.ChatAttemptResult
	calls   int
}

func (s *scriptedCaller) call(context.Context, int) (provider.ChatAttemptResult, error) {
	i := s.calls
	s.calls++
	if i >= len(s.results) {
		return s.results[len(s.results)-1], nil
	}
	return s.results[i], nil
}

func retryDeps(repo *Repository) *phaseRunner {
	// sleep 换成空实现：退避时长由 TestRetryBackoff 单独验，
	// 这里不该让用例真的睡 3 秒。
	return &phaseRunner{repo: repo, sleep: func(context.Context, time.Duration) error { return nil }}
}

// TestRunPhaseRetriesAndRecordsEveryAttempt——⭐ 每一次尝试都要在账上，
// 包括被重试掉的那些。只记最后一次会让成本少算三分之二。
func TestRunPhaseRetriesAndRecordsEveryAttempt(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-retry1", "job-retry1")
	item := firstItemID(t, repo, job.ID)

	caller := &scriptedCaller{results: []provider.ChatAttemptResult{
		failedWith("http_500"), failedWith("http_503"), completed("stop"),
	}}
	res, err := retryDeps(repo).runPhase(ctx, phaseInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	}, caller.call)
	if err != nil {
		t.Fatalf("runPhase: %v", err)
	}
	if res.Outcome != provider.AttemptCompleted {
		t.Errorf("最终结果 = %v", res.Outcome)
	}
	if caller.calls != 3 {
		t.Errorf("打了 %d 次调用，want 3", caller.calls)
	}
	var attempts, stillReserved, reserved, confirmed int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM relation_extraction_attempts WHERE item_id=?),
		        (SELECT COUNT(*) FROM relation_extraction_attempts WHERE item_id=? AND state='reserved'),
		        (SELECT reserved_calls FROM relation_extraction_jobs WHERE id=?),
		        (SELECT confirmed_dispatches FROM relation_extraction_jobs WHERE id=?)`,
		item, item, job.ID, job.ID).Scan(&attempts, &stillReserved, &reserved, &confirmed); err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Errorf("账上只有 %d 条 attempt，实际打了 3 次——被重试掉的那些也要记", attempts)
	}
	// ⚠️ 只数行数是**抓不住**"被重试掉的那次没结算"的：预留本来就会建行，
	// 没结算的那条停在 reserved 上，行数照样是 3。变异测试逼出了这一条。
	// 而一条停在 reserved 的记录会被恢复扫描改判 unknown——凭空多出一笔
	// "可能花了的钱"，而我们其实**知道**它的结局。
	if stillReserved != 0 {
		t.Errorf("有 %d 条 attempt 停在 reserved 上没结算", stillReserved)
	}
	if confirmed != 3 {
		t.Errorf("confirmed_dispatches = %d, want 3——每一次真实调用都要计进作业账目", confirmed)
	}
	if reserved != 3 {
		t.Errorf("reserved_calls = %d, want 3", reserved)
	}
	// 尝试次数必须落库（items 表），不是只在内存里。
	var stored int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT extract_attempt_count FROM relation_extraction_items WHERE id=?`, item).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 3 {
		t.Errorf("extract_attempt_count = %d, want 3——计数放内存的话崩溃重启就归零", stored)
	}
}

// TestRunPhaseStopsAtConfigError——配置类错误只打一次。
func TestRunPhaseStopsAtConfigError(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-retry2", "job-retry2")
	item := firstItemID(t, repo, job.ID)

	caller := &scriptedCaller{results: []provider.ChatAttemptResult{failedWith("http_404")}}
	res, err := retryDeps(repo).runPhase(ctx, phaseInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	}, caller.call)
	if err != nil {
		t.Fatal(err)
	}
	if caller.calls != 1 {
		t.Errorf("配置类错误打了 %d 次，want 1——重试只是把同一个错误重复三遍", caller.calls)
	}
	if res.Outcome != provider.AttemptFailed {
		t.Errorf("outcome = %v", res.Outcome)
	}
}

// TestRunPhaseResumesFromStoredCount——⭐ 自动恢复**不重置**计数。
//
// ⚠️ 这是自动恢复最容易引入的死循环：worker 崩溃重启、计数归零、
// 一个永远会失败的 item 被无限重试，把预算烧光。而每一轮看起来都正常。
func TestRunPhaseResumesFromStoredCount(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-retry3", "job-retry3")
	item := firstItemID(t, repo, job.ID)

	// 模拟上一轮已经用掉 2 次。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_items SET extract_attempt_count = 2 WHERE id = ?`, item); err != nil {
		t.Fatal(err)
	}
	caller := &scriptedCaller{results: []provider.ChatAttemptResult{failedWith("http_500")}}
	if _, err := retryDeps(repo).runPhase(ctx, phaseInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	}, caller.call); err != nil {
		t.Fatal(err)
	}
	if caller.calls != 1 {
		t.Errorf("恢复后又打了 %d 次，只该剩 1 次额度", caller.calls)
	}
	var stored int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT extract_attempt_count FROM relation_extraction_items WHERE id=?`, item).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != maxAttemptsPerPhase {
		t.Errorf("extract_attempt_count = %d, want %d", stored, maxAttemptsPerPhase)
	}
}

// TestRunPhaseStopsWhenBudgetExhausted——额度用尽时立刻停手，
// 且**不吞掉这个错误**：调用方要据此把作业停成 budget_exhausted 而不是 failed。
func TestRunPhaseStopsWhenBudgetExhausted(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-retry4", 2)
	spec := newExtractionJobSpec("job-retry4", "doc-retry4", 1, "m-1")
	spec.CallLimit = 1
	job, err := repo.initializeExtractionJob(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	item := firstItemID(t, repo, job.ID)
	caller := &scriptedCaller{results: []provider.ChatAttemptResult{failedWith("http_500")}}
	_, err = retryDeps(repo).runPhase(ctx, phaseInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	}, caller.call)
	if !errors.Is(err, ErrExtractionCallBudgetExhausted) {
		t.Fatalf("err = %v, want ErrExtractionCallBudgetExhausted", err)
	}
	if caller.calls != 1 {
		t.Errorf("额度只够 1 次，却打了 %d 次", caller.calls)
	}
}
