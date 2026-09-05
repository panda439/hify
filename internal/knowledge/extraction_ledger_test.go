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
		Phase: phaseExtract, AttemptNumber: 1,
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
			AttemptNumber: n, RequestHash: make([]byte, 32), MaxOutputTokens: 64,
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

	for i, tc := range []struct {
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
			AttemptNumber: i + 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
		AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
		AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
		AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
		AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
		AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
			AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
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
