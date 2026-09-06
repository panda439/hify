package knowledge

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"hify/internal/provider"
)

func isValidUTF8(s string) bool { return utf8.ValidString(s) }

// extraction_ledger_test.go 守 attempt 账目（010 T017）。
//
// ⭐ 这张表是「全书抽取成本」这个数字可信度的**全部**来源。本期最重要的
// 产出之一就是它。因此这里的断言几乎全在盯**不许少记**和**不许多记**，
// 而不是"能不能跑通"。
//
// ⚠️ 少记和多记的严重性不对称：多记会被人发现（数字比账单大，有人会查）；
// 少记不会——它让系统显得更便宜，没有人会去质疑一个好看的数字。
// 所以每一条"可能花了钱"的路径都必须落在账上。

func ledgerJob(t *testing.T, repo *Repository, docID, jobID string) (RelationExtractionJob, int) {
	t.Helper()
	job := claimTestJob(t, repo, docID, jobID)
	epoch, ok, err := repo.claimExtractionJob(t.Context(), job.ID, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return job, epoch
}

func firstItemID(t *testing.T, repo *Repository, jobID string) string {
	t.Helper()
	var id string
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT id FROM relation_extraction_items WHERE job_id=? ORDER BY chunk_index LIMIT 1`,
		jobID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func jobLedger(t *testing.T, repo *Repository, jobID string) (reserved, confirmed, unknown int, activeMs int64) {
	t.Helper()
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT reserved_calls, confirmed_dispatches, unknown_attempts, active_ms_used
		 FROM relation_extraction_jobs WHERE id=?`, jobID).
		Scan(&reserved, &confirmed, &unknown, &activeMs); err != nil {
		t.Fatal(err)
	}
	return
}

// TestAttemptIsReservedBeforeDispatch——⭐ 顺序不可颠倒。
// 颠倒的后果：进程在"已发出、未收到"之间崩掉，这次调用不留任何痕迹，
// 而它的钱已经花了。
func TestAttemptIsReservedBeforeDispatch(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led1", "job-led1")

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: firstItemID(t, repo, job.ID), Epoch: epoch,
		Phase:       phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_attempts WHERE id=?`, att.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "reserved" {
		t.Errorf("预留后的状态 = %q, want reserved", state)
	}
	if r, _, _, _ := jobLedger(t, repo, job.ID); r != 1 {
		t.Errorf("reserved_calls = %d, want 1", r)
	}
}

// TestCallBudgetIsEnforcedAtReservation——预算检查必须在**同一条 UPDATE 的
// WHERE 里**。先读后写在两个 worker 之间必然超发，而超发的表现是账单超了，
// 不报错。
func TestCallBudgetIsEnforcedAtReservation(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-budget", 2)
	spec := newExtractionJobSpec("job-budget", "doc-budget", 1, "m-1")
	spec.CallLimit = 2
	job, err := repo.initializeExtractionJob(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	epoch, _, err := repo.claimExtractionJob(ctx, job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	item := firstItemID(t, repo, job.ID)
	res := func(n int) error {
		_, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 64,
		})
		return err
	}
	if err := res(1); err != nil {
		t.Fatal(err)
	}
	if err := res(2); err != nil {
		t.Fatal(err)
	}
	if err := res(3); !errors.Is(err, ErrExtractionCallBudgetExhausted) {
		t.Fatalf("第三次预留 err = %v, want ErrExtractionCallBudgetExhausted", err)
	}
	// ⚠️ 预算耗尽时**不能**留下一个孤儿 attempt 行。
	var n int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE job_id=?`, job.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("预算耗尽却留下了 %d 个 attempt，want 2", n)
	}
}

// TestSettleRecordsUsageOnlyWhenKnown——⚠️ 供应商没返回用量时写 NULL，不写 0。
// 写 0 会让"没测到"和"真的没花"永久不可区分，而本期最重要的产出正是
// 一个可复核的成本数字。000017 的 CHECK 在数据库层守同一件事。
func TestSettleRecordsUsageOnlyWhenKnown(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led2", "job-led2")
	item := firstItemID(t, repo, job.ID)

	for _, tc := range []struct {
		name   string
		result provider.ChatAttemptResult
		known  bool
	}{
		{"供应商返回了用量", provider.ChatAttemptResult{
			Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 120,
			UsageKnown: true, InputTokens: 900, OutputTokens: 40,
			Message: provider.Message{Content: "{}"}}, true},
		{"供应商没返回用量", provider.ChatAttemptResult{
			Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 90,
			Message: provider.Message{Content: "{}"}}, false},
	} {
		att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.settleExtractionAttempt(ctx, att, tc.result); err != nil {
			t.Fatal(err)
		}
		var known bool
		var in, out sql.NullInt32
		if err := repo.db.QueryRowContext(ctx,
			`SELECT usage_known, input_tokens, output_tokens
			 FROM relation_extraction_attempts WHERE id=?`, att.ID).Scan(&known, &in, &out); err != nil {
			t.Fatal(err)
		}
		if known != tc.known {
			t.Errorf("%s：usage_known = %v", tc.name, known)
		}
		if !tc.known && (in.Valid || out.Valid) {
			t.Errorf("%s：用量未知却写了 token（%v/%v）——0 不是「花了 0 个 token」",
				tc.name, in, out)
		}
	}
}

// TestNotDispatchedRefundsButKeepsTheRecord——⭐ 确定没发出去的调用退还额度，
// **但控制记录保留**。删掉记录会让"这次尝试发生过、只是被限流挡住了"
// 这件事消失，而重试次数、等待耗时都是要进报告的。
func TestNotDispatchedRefundsButKeepsTheRecord(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led3", "job-led3")
	item := firstItemID(t, repo, job.ID)

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptNotDispatched, ErrorCode: "circuit_open", ElapsedMs: 3,
	}); err != nil {
		t.Fatal(err)
	}
	reserved, confirmed, unknown, _ := jobLedger(t, repo, job.ID)
	if reserved != 0 {
		t.Errorf("确定没发出去却没退还额度：reserved_calls = %d", reserved)
	}
	if confirmed != 0 || unknown != 0 {
		t.Errorf("没发出去的调用被计进了发生过的账：confirmed=%d unknown=%d", confirmed, unknown)
	}
	var n int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE id=? AND state='not_dispatched'`,
		att.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("控制记录被删掉了——重试次数和等待耗时都要进报告")
	}
}

// TestUnknownNeverRefunds——⭐ 与上一条对照，也是这一组里最要紧的一格。
// unknown 意味着请求**可能**已经到达服务端并被处理，那笔钱可能已经花了。
// 退还它就是在系统性低估成本，而且偏差方向恒定：永远偏小。
func TestUnknownNeverRefunds(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led4", "job-led4")
	item := firstItemID(t, repo, job.ID)

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptUnknown, ErrorCode: "timeout", ElapsedMs: 60000,
	}); err != nil {
		t.Fatal(err)
	}
	reserved, confirmed, unknown, activeMs := jobLedger(t, repo, job.ID)
	if reserved != 1 {
		t.Errorf("unknown 被退还了额度：reserved_calls = %d, want 1", reserved)
	}
	if unknown != 1 {
		t.Errorf("unknown_attempts = %d, want 1", unknown)
	}
	if confirmed != 0 {
		t.Errorf("不确定的调用被算成确定发生过：confirmed = %d", confirmed)
	}
	if activeMs != 60000 {
		t.Errorf("active_ms_used = %d——超时那 60 秒是真实消耗，必须计入", activeMs)
	}
}

// TestSettleIsIdempotentlyGuarded——一次尝试只能结算一次。
// ⚠️ 重复结算会让 usage、费用、活跃时长被重复累加进作业级聚合。
func TestSettleIsIdempotentlyGuarded(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led5", "job-led5")
	item := firstItemID(t, repo, job.ID)

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := provider.ChatAttemptResult{Outcome: provider.AttemptCompleted,
		Dispatched: true, ElapsedMs: 100, Message: provider.Message{Content: "{}"}}
	if err := repo.settleExtractionAttempt(ctx, att, res); err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, res); !errors.Is(err, ErrAttemptAlreadySettled) {
		t.Fatalf("重复结算 err = %v, want ErrAttemptAlreadySettled", err)
	}
	_, confirmed, _, activeMs := jobLedger(t, repo, job.ID)
	if confirmed != 1 || activeMs != 100 {
		t.Errorf("重复结算把账目加了两次：confirmed=%d active_ms=%d", confirmed, activeMs)
	}
}

// TestRawResponseIsStoredAndCapped——原始响应必须落盘（回放与复核的唯一依据），
// 且有 64 KiB 上限。⚠️ 截断要**显式记录**，否则一份被悄悄截短的响应
// 会让后来的回放得出与当时不同的结论。
func TestRawResponseIsStoredAndCapped(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led6", "job-led6")
	item := firstItemID(t, repo, job.ID)

	huge := strings.Repeat("字", 40000) // 远超 64 KiB（每字 3 字节）
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 10,
		Message: provider.Message{Content: huge},
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := repo.queries.GetExtractionAttemptRawResponse(ctx, att.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !raw.Valid {
		t.Fatal("原始响应没有落盘")
	}
	if len(raw.String) > maxRawResponseBytes {
		t.Errorf("原始响应 %d 字节，超过上限 %d", len(raw.String), maxRawResponseBytes)
	}
	var errCode sql.NullString
	if err := repo.db.QueryRowContext(ctx,
		`SELECT error_code FROM relation_extraction_attempts WHERE id=?`, att.ID).Scan(&errCode); err != nil {
		t.Fatal(err)
	}
	if errCode.String != "response_truncated" {
		t.Errorf("截断没有被显式记录：error_code = %q", errCode.String)
	}
	// ⚠️ 截断必须切在 rune 边界上，不能把一个多字节字符劈成两半。
	if !isValidUTF8(raw.String) {
		t.Error("截断切坏了 UTF-8 字符")
	}
}

// TestStaleReservedBecomesUnknown——⭐ 恢复扫描把停留过久的 reserved
// 改判 **unknown**，不是删掉、也不是标 failed。
// 那次调用可能已经到达服务端并被处理，说"没发生过"就是少记一笔真实成本。
func TestStaleReservedBecomesUnknown(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led7", "job-led7")
	item := firstItemID(t, repo, job.ID)

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 把它做旧。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_attempts SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour), att.ID); err != nil {
		t.Fatal(err)
	}
	n, err := repo.reconcileStaleReservedAttempts(ctx, 30*time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("改判了 %d 条，want 1", n)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_attempts WHERE id=?`, att.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Errorf("state = %q, want unknown——那次调用可能已经被服务端处理过", state)
	}
	if _, _, unknown, _ := jobLedger(t, repo, job.ID); unknown != 1 {
		t.Errorf("改判后没有计进 unknown_attempts：%d", unknown)
	}
}

// TestAliasPhaseSharesTheSameLedger——归一阶段的调用同样记账。
// ⚠️ 只记抽取阶段会让成本少算将近一半，而两个阶段用的是同一个模型、
// 同一个额度池。
func TestAliasPhaseSharesTheSameLedger(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-led8", "job-led8")
	item := firstItemID(t, repo, job.ID)

	for _, phase := range []string{phaseExtract, phaseAlias} {
		att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phase,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
		if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
			Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 50,
			UsageKnown: true, InputTokens: 100, OutputTokens: 10,
			Message: provider.Message{Content: "{}"},
		}); err != nil {
			t.Fatalf("%s: %v", phase, err)
		}
	}
	reserved, confirmed, _, activeMs := jobLedger(t, repo, job.ID)
	if reserved != 2 || confirmed != 2 || activeMs != 100 {
		t.Errorf("归一阶段没有同账：reserved=%d confirmed=%d active_ms=%d",
			reserved, confirmed, activeMs)
	}
}

// --- 010 R6-06：被截断的响应不得当成可用结果 ---

// TestTruncatedResponseIsNotReplayable——⭐ 一份**存不下全文**的响应，
// 账要照记，但**不能被回放成结果**。
//
// ⚠️ 64KiB 上限触发时只写了 error_code='response_truncated'，
// state 仍然是 completed。回放查询此前完全不看 error_code，
// 于是恢复之后把这份被截掉内容的响应当成一次成功结果取回来——
// 它解析出的是**少了后半段**的结果：一条关系凭空消失，
// 而失败率显示为 0，item 显示为成功。
func TestTruncatedResponseIsNotReplayable(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	in := pipelineItem(t, repo, "doc-trunc", "job-trunc")

	// 落一份超过 64KiB 的响应：settle 会把它截短并打上标记。
	oversized := `{"mentions":[],"relations":[],"alias_proposals":[]}` +
		strings.Repeat("x", maxRawResponseBytes)
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: maxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, FinishReason: "stop", Dispatched: true,
		Message: provider.Message{Content: oversized},
	}); err != nil {
		t.Fatal(err)
	}

	// ⭐ 账必须记着：这次调用真的发生过，钱真的花了。
	var errCode string
	var confirmed int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COALESCE(a.error_code,''), j.confirmed_dispatches
		 FROM relation_extraction_attempts a
		 JOIN relation_extraction_jobs j ON j.id = a.job_id
		 WHERE a.id = ?`, att.ID).Scan(&errCode, &confirmed); err != nil {
		t.Fatal(err)
	}
	if errCode != errorCodeResponseTruncated {
		t.Errorf("error_code = %q，want response_truncated", errCode)
	}
	if confirmed == 0 {
		t.Error("被截断的调用没有计进账目——这笔钱确实花了")
	}

	// ⭐ 但它不能被回放。
	if _, ok, err := repo.findReplayableResponse(ctx, in.ItemID, phaseExtract); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("被截断的响应被当成可回放的结果取回来了")
	}
}

// TestLengthFinishReasonIsNotReplayable——finish_reason=length 同理：
// 模型自己说了输出被截断，这份结果一样不可用。
func TestLengthFinishReasonIsNotReplayable(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	in := pipelineItem(t, repo, "doc-lentrunc", "job-lentrunc")

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: in.JobID, ItemID: in.ItemID, Epoch: in.Epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: maxOutputTokens,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, FinishReason: finishReasonLength, Dispatched: true,
		Message: provider.Message{Content: `{"mentions":[],"relations":[],"alias_proposals":[]}`},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.findReplayableResponse(ctx, in.ItemID, phaseExtract); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("finish_reason=length 的响应被当成可回放的结果取回来了")
	}
}

// TestFreshAndReplayShareTheSameVerdict——⭐ 首次调用与回放**必须共用
// 同一个判据**。
//
// ⚠️ 分成两套的表现是：同一份响应第一次被拒、重启之后被接受，
// 或者反过来——而两条路径各自看起来都自洽。此前首次调用只看
// finish_reason，超过 64KiB 而会被落盘截断的响应会被原样接受，
// 于是首次用完整正文、恢复后用截短那份，同一个 item 两次跑出不同结果。
func TestFreshAndReplayShareTheSameVerdict(t *testing.T) {
	oversized := strings.Repeat("x", maxRawResponseBytes+1)
	cases := []struct {
		name         string
		finishReason string
		body         string
		wantUnusable bool
	}{
		{"正常结果", "stop", `{"mentions":[]}`, false},
		{"模型说输出被截断", finishReasonLength, `{"mentions":[]}`, true},
		{"正文存不下", "stop", oversized, true},
		{"恰好等于上限", "stop", strings.Repeat("x", maxRawResponseBytes), false},
	}
	for _, tc := range cases {
		got := extractionResultUnusable(tc.finishReason, tc.body)
		if (got != "") != tc.wantUnusable {
			t.Errorf("%s：unusable=%q，want %v", tc.name, got, tc.wantUnusable)
		}
	}
}

// TestOversizedResponseIsRejectedOnTheFirstCall——首次调用拿到一份存不下的
// 响应时必须**当场拒绝**，不能拿完整正文接着算。
//
// ⚠️ 接受的话，这个 item 这一次是成功的，而重启之后回放拿到的是截短的
// 那份、解析出不同的结果——同一个 item 的产出取决于它有没有崩过。
func TestOversizedResponseIsRejectedOnTheFirstCall(t *testing.T) {
	repo := extractionRepo(t)
	chat := newScriptedChat()
	// 一份**语法完全合法**、只是太长的响应：拒绝的理由只能是长度。
	padding := strings.Repeat("正", maxRawResponseBytes)
	chat.script(phaseExtract, `{"mentions":[{"ref":"m1","surface":"`+padding+`","occurrence":0}],
	     "relations":[],"alias_proposals":[]}`)
	in := pipelineItem(t, repo, "doc-bigresp", "job-bigresp")

	err := pipelineDeps(repo, chat).processItem(t.Context(), in)
	if err == nil {
		t.Fatal("一份存不下的响应被当成成功结果接受了")
	}
	if !strings.Contains(err.Error(), "unusable") {
		t.Errorf("拒绝理由不是「结果不可用」：%v", err)
	}
}

// --- 010 R6-03：编号分配与计数递增必须同一个事务 ---

// TestReservationAssignsAndCountsInOneTransaction——⭐ 预留一次尝试之后，
// **attempt 行和 item 上的计数必须同时存在**。
//
// ⚠️ 此前编号由调用方按"已用次数 + 1"算好传进来，而递增计数是预留提交
// **之后**的另一次提交。两次提交之间崩掉的话，attempt 行已经存在、
// item.extract_attempt_count 仍是 0——恢复之后仍然从 1 开始预留，
// 撞上 uk_rea_item_phase_attempt 重复键；而恢复扫描把旧 attempt 改判
// unknown 并不会修复计数，于是这个 item 每一轮都撞同一个错，
// 永远好不了，且不再花钱也不再产出。
func TestReservationAssignsAndCountsInOneTransaction(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-r603a", "job-r603a")
	item := firstItemID(t, repo, job.ID)

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if att.AttemptNumber != 1 {
		t.Errorf("第一次的编号 = %d，want 1", att.AttemptNumber)
	}
	counts, err := repo.queries.GetItemAttemptCounts(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if int(counts.ExtractAttemptCount) != att.AttemptNumber {
		t.Fatalf("attempt 行的编号 %d 与 item 上的计数 %d 对不上——"+
			"两者不在同一个事务里，中间崩掉就会永久撞重复键",
			att.AttemptNumber, counts.ExtractAttemptCount)
	}

	// 第二次必须拿到 2，且不撞唯一键。
	att2, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatalf("第二次预留失败（编号没有从持久事实接着算）：%v", err)
	}
	if att2.AttemptNumber != 2 {
		t.Errorf("第二次的编号 = %d，want 2", att2.AttemptNumber)
	}
}

// TestReservationStopsAtTheAttemptCapAndRollsBack——⭐ 编号超过上限时
// **整个事务回滚**：额度没占、行没建、计数也没加。
//
// ⚠️ 只在循环外面判上限的话，第 4 次仍然会占掉一次调用额度、建一行 attempt，
// 然后才被发现超限——那次额度再也拿不回来，而账上多了一次从未发生的调用。
func TestReservationStopsAtTheAttemptCapAndRollsBack(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-r603b", "job-r603b")
	item := firstItemID(t, repo, job.ID)

	for i := 1; i <= maxAttemptsPerPhase; i++ {
		if _, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		}); err != nil {
			t.Fatalf("第 %d 次预留失败：%v", i, err)
		}
	}
	reservedBefore, _, _, _ := jobLedger(t, repo, job.ID)

	_, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if !errors.Is(err, errAttemptsExhausted) {
		t.Fatalf("超过上限却没有停手：%v", err)
	}
	reservedAfter, _, _, _ := jobLedger(t, repo, job.ID)
	if reservedAfter != reservedBefore {
		t.Errorf("被拒的那次仍然占掉了额度：%d → %d——那次调用从未发生",
			reservedBefore, reservedAfter)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE item_id=? AND phase=?`,
		item, phaseExtract); n != maxAttemptsPerPhase {
		t.Errorf("attempt 行有 %d 条，want %d——被拒的那次留下了行",
			n, maxAttemptsPerPhase)
	}
	counts, err := repo.queries.GetItemAttemptCounts(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if int(counts.ExtractAttemptCount) != maxAttemptsPerPhase {
		t.Errorf("计数 = %d，want %d——被拒的那次把计数加上去了",
			counts.ExtractAttemptCount, maxAttemptsPerPhase)
	}
}

// TestTwoPhasesCountSeparately——两个阶段各自计数，互不占用对方的次数。
func TestTwoPhasesCountSeparately(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-r603c", "job-r603c")
	item := firstItemID(t, repo, job.ID)

	for i := 0; i < maxAttemptsPerPhase; i++ {
		if _, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// 抽取用完了，归一必须还能跑。
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseAlias,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatalf("抽取阶段用完却把归一阶段也堵死了：%v", err)
	}
	if att.AttemptNumber != 1 {
		t.Errorf("归一阶段的第一次编号 = %d，want 1", att.AttemptNumber)
	}
}
