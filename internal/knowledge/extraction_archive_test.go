package knowledge

import (
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_archive_test.go 守清理与账目归档（010 T023）。
//
// ⭐ 清理是这条链路上**唯一会让数据变少**的动作，也因此是唯一一个能把
// 已经花掉的钱从账上抹掉的地方。本期最重要的产出之一是一个可复核的成本数字，
// 而清理跑完之后那个数字必须**一分不差**。
//
// ⚠️ 两个方向的偏差都不报错：
//   - 归档汇总没加上就删 → 费用凭空变少，系统显得更便宜；
//   - 重复归档 → 费用凭空变多，而没人能解释多出来的部分从哪来。

// settleN 在一个 item 上做 n 次已完成的调用，每次 elapsed/token 固定。
func settleN(t *testing.T, repo *Repository, jobID, itemID string, epoch, n int) {
	t.Helper()
	ctx := t.Context()
	for i := 1; i <= n; i++ {
		att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: jobID, ItemID: itemID, Epoch: epoch, Phase: phaseExtract,
			AttemptNumber: i, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
			Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 100,
			UsageKnown: true, InputTokens: 500, OutputTokens: 20,
			Message: provider.Message{Content: "{}"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// settleN2 与 settleN 相同，但 attempt_number 从 from 到 to——
// 归档第二批时不能和第一批撞 UNIQUE(item_id, phase, attempt_number)。
func settleN2(t *testing.T, repo *Repository, jobID, itemID string, epoch, from, to int) {
	t.Helper()
	ctx := t.Context()
	for i := from; i <= to; i++ {
		att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: jobID, ItemID: itemID, Epoch: epoch, Phase: phaseExtract,
			AttemptNumber: i, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
			Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 100,
			UsageKnown: true, InputTokens: 500, OutputTokens: 20,
			Message: provider.Message{Content: "{}"},
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func ageAttempts(t *testing.T, repo *Repository, jobID string, age time.Duration) {
	t.Helper()
	if _, err := repo.db.ExecContext(t.Context(),
		`UPDATE relation_extraction_attempts SET finished_at = ? WHERE job_id = ?`,
		time.Now().UTC().Add(-age), jobID); err != nil {
		t.Fatal(err)
	}
}

// TestArchivingPreservesTheTotalCost——⭐ 本文件的立论。
// 归档前后，「活账 + 归档汇总」必须完全相等。
func TestArchivingPreservesTheTotalCost(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-ar1", "job-ar1")
	settleN(t, repo, job.ID, firstItemID(t, repo, job.ID), epoch, 5)

	before, err := repo.jobLedgerTotals(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Attempts != 5 || before.ActiveMs != 500 || before.InputTokens != 2500 {
		t.Fatalf("归档前的账目不对：%+v", before)
	}

	ageAttempts(t, repo, job.ID, 40*24*time.Hour)
	n, err := repo.archiveExpiredAttempts(ctx, attemptRetention, reconcileBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 {
		t.Errorf("归档了 %d 条, want 5", n)
	}
	after, err := repo.jobLedgerTotals(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("归档改变了总账：\nbefore=%+v\nafter =%+v", before, after)
	}
	// 行确实被删了——否则这条测试只是在验"什么都没做"。
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("过期 attempt 还剩 %d 条", n)
	}
}

// TestRepeatedArchivingDoesNotDoubleCount——⭐ 重复跑清理不得重复累加。
// ⚠️ 多出来的费用没有任何地方能解释它从哪来。
func TestRepeatedArchivingDoesNotDoubleCount(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-ar2", "job-ar2")
	settleN(t, repo, job.ID, firstItemID(t, repo, job.ID), epoch, 3)
	ageAttempts(t, repo, job.ID, 40*24*time.Hour)

	before, _ := repo.jobLedgerTotals(ctx, job.ID)
	for i := 0; i < 3; i++ {
		if _, err := repo.archiveExpiredAttempts(ctx, attemptRetention, reconcileBatchSize); err != nil {
			t.Fatal(err)
		}
	}
	after, err := repo.jobLedgerTotals(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("重复清理改变了总账：\nbefore=%+v\nafter =%+v", before, after)
	}
}

// TestArchivingInTwoWavesAccumulates——⭐ 变异测试逼出来的缺口。
//
// 上面那条"重复清理"用例跑三次归档，但第一次之后已经没有可归档的行了，
// `sum.Attempts == 0` 直接返回——**汇总里的 `prev.X +` 累加从来没被执行过**。
// 把 `prev.Attempts + sum.Attempts` 改成 `sum.Attempts`，测试照样全绿，
// 而真实后果是：第二批归档会把第一批的历史费用整个盖掉。
func TestArchivingInTwoWavesAccumulates(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-ar7", "job-ar7")
	item := firstItemID(t, repo, job.ID)

	settleN(t, repo, job.ID, item, epoch, 2)
	ageAttempts(t, repo, job.ID, 40*24*time.Hour)
	if _, err := repo.archiveExpiredAttempts(ctx, attemptRetention, reconcileBatchSize); err != nil {
		t.Fatal(err)
	}
	// 第二批：再跑两次调用，再归档。
	settleN2(t, repo, job.ID, item, epoch, 3, 4)
	ageAttempts(t, repo, job.ID, 40*24*time.Hour)
	if _, err := repo.archiveExpiredAttempts(ctx, attemptRetention, reconcileBatchSize); err != nil {
		t.Fatal(err)
	}

	totals, err := repo.jobLedgerTotals(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Attempts != 4 {
		t.Errorf("attempts = %d, want 4——第二批归档把第一批的历史盖掉了", totals.Attempts)
	}
	if totals.ActiveMs != 400 {
		t.Errorf("active_ms = %d, want 400", totals.ActiveMs)
	}
	if totals.InputTokens != 2000 {
		t.Errorf("input_tokens = %d, want 2000", totals.InputTokens)
	}
}

// TestSucceededJobDerivedRowsSurviveCleanup——⭐ 变异测试逼出来的第二个缺口。
//
// 上面那条清理用例里的"活着的作业"从没结束过（finished_at 为 NULL），
// 于是 `finished_at < cutoff` 自己就把它排除了，state 白名单里加不加
// succeeded 完全没区别。真正要验的是：一个**早就成功结束**的作业，
// 它的关系数据必须原样保留——那是用户现在查得到的东西。
func TestSucceededJobDerivedRowsSurviveCleanup(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-ar8", "job-ar8")
	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	// 很久以前就成功结束了。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET state='succeeded', finished_at=? WHERE id=?`,
		time.Now().UTC().Add(-90*24*time.Hour), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.cleanupDeadJobDerivedRows(ctx, derivedRowRetention, reconcileBatchSize); err != nil {
		t.Fatal(err)
	}
	if c := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); c != 1 {
		t.Errorf("成功作业的关系被清掉了：剩 %d 条, want 1", c)
	}
	if c := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, job.ID); c != 2 {
		t.Errorf("成功作业的人物被清掉了：剩 %d 个, want 2", c)
	}
}

// TestCorruptArchivedSummaryIsAnError——⭐ 变异测试逼出来的第三个缺口。
//
// ⚠️ 损坏的汇总**报错**，不降级成零值。降级的表现是这个作业的历史费用
// 突然归零，而报告照样出得来——一个凭空变便宜的数字，且没有任何地方
// 说明它为什么变了。这与"读不出来就当没有"的直觉相反，但方向是对的：
// 账目宁可报错也不能悄悄变小。
func TestCorruptArchivedSummaryIsAnError(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-ar9", "job-ar9")
	settleN(t, repo, job.ID, firstItemID(t, repo, job.ID), epoch, 1)

	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET archived_ledger_summary = ? WHERE id = ?`,
		`{"attempts": "not a number"}`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.jobLedgerTotals(ctx, job.ID); err == nil {
		t.Error("损坏的归档汇总被当成零值——这个作业的历史费用会凭空归零")
	}
}

// TestArchivingKeepsRecentAttempts——保留期内的不动。
func TestArchivingKeepsRecentAttempts(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-ar3", "job-ar3")
	settleN(t, repo, job.ID, firstItemID(t, repo, job.ID), epoch, 2)
	ageAttempts(t, repo, job.ID, 3*24*time.Hour) // 只有 3 天

	if n, err := repo.archiveExpiredAttempts(ctx, attemptRetention, reconcileBatchSize); err != nil || n != 0 {
		t.Fatalf("保留期内的 attempt 被归档了 %d 条 (err=%v)", n, err)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE job_id=?`, job.ID); n != 2 {
		t.Errorf("剩余 %d 条, want 2", n)
	}
}

// TestArchivedSummaryKeepsUsageKnownSeparate——⭐ 未知用量不许被当成 0 累加。
//
// ⚠️ 把未知当 0 相加，就是把"没测到"和"真的没花"混成一个数。
// 归档之后原始行没了，这个区分**再也无法恢复**——所以汇总里必须自带
// "有多少次调用是知道用量的"，报告才能说出"token 数只覆盖 N/M 次调用"。
func TestArchivedSummaryKeepsUsageKnownSeparate(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-ar4", "job-ar4")
	item := firstItemID(t, repo, job.ID)

	// 一次有用量、一次没有。
	for i, known := range []bool{true, false} {
		att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			AttemptNumber: i + 1, RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err != nil {
			t.Fatal(err)
		}
		res := provider.ChatAttemptResult{Outcome: provider.AttemptCompleted,
			Dispatched: true, ElapsedMs: 100, Message: provider.Message{Content: "{}"}}
		if known {
			res.UsageKnown, res.InputTokens, res.OutputTokens = true, 500, 20
		}
		if err := repo.settleExtractionAttempt(ctx, att, res); err != nil {
			t.Fatal(err)
		}
	}
	ageAttempts(t, repo, job.ID, 40*24*time.Hour)
	if _, err := repo.archiveExpiredAttempts(ctx, attemptRetention, reconcileBatchSize); err != nil {
		t.Fatal(err)
	}
	totals, err := repo.jobLedgerTotals(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", totals.Attempts)
	}
	if totals.UsageKnownAttempts != 1 {
		t.Errorf("usage_known_attempts = %d, want 1——归档之后这个区分再也恢复不了",
			totals.UsageKnownAttempts)
	}
	if totals.InputTokens != 500 {
		t.Errorf("input_tokens = %d, want 500（未知的那次不该按 0 计入）", totals.InputTokens)
	}
}

// TestDeadJobDerivedRowsAreCleaned——被取代/失败作业的派生记录可以清，
// **成功作业的绝不能碰**（那是用户现在查得到的关系数据）。
func TestDeadJobDerivedRowsAreCleaned(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-ar5", "job-ar5-alive")
	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	// restart：旧作业被取代，它的派生记录成为可清理的。
	spec2 := newExtractionJobSpec("job-ar5-new", "doc-ar5", 1, "m-1")
	spec2.RunNumber = 2
	if _, err := repo.initializeExtractionJob(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET finished_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-40*24*time.Hour), job.ID); err != nil {
		t.Fatal(err)
	}
	// 另建一个成功作业，它的数据必须原样保留。
	live, epoch2, item2 := publishFixture(t, repo, "doc-ar6", "job-ar6")
	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: live.ID, ItemID: item2, Epoch: epoch2,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	n, err := repo.cleanupDeadJobDerivedRows(ctx, derivedRowRetention, reconcileBatchSize)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("一个作业都没被清理，这条断言等于没验")
	}
	if c := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); c != 0 {
		t.Errorf("被取代作业还剩 %d 条关系", c)
	}
	if c := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, job.ID); c != 0 {
		t.Errorf("被取代作业还剩 %d 个人物", c)
	}
	if c := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, live.ID); c != 1 {
		t.Errorf("清理动了活着的作业：关系剩 %d 条, want 1", c)
	}
	// ⭐ 清理派生记录**不动账目**：钱是真花过的，作业记录和它的费用要留下。
	if c := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_jobs WHERE id=?`, job.ID); c != 1 {
		t.Error("清理把作业行本身删掉了——它的费用记录跟着没了")
	}
}
