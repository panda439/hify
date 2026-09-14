package knowledge

import (
	"context"
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_reconcile_test.go 守恢复扫描（010 T021）。
//
// ⭐ 这里最要紧的不是"能捡回来"，而是**哪些不该捡**。
// 自动恢复把用户暂停的作业重新跑起来，是一个系统擅自推翻显式决定的行为——
// 用户会看到一个自己明明暂停过的作业又开始花钱，而系统认为自己很尽责。

func seedJobInState(t *testing.T, repo *Repository, docID, jobID, state string, leaseUntil *time.Time) {
	t.Helper()
	seedNarrativeDocument(t, repo, docID, 2)
	if _, err := repo.initializeExtractionJob(t.Context(),
		newExtractionJobSpec(jobID, docID, 1, "m-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(t.Context(),
		`UPDATE relation_extraction_jobs SET state = ?, lease_until = ? WHERE id = ?`,
		state, leaseUntil, jobID); err != nil {
		t.Fatal(err)
	}
}

func recoverableIDs(t *testing.T, repo *Repository) map[string]bool {
	t.Helper()
	jobs, err := repo.listRecoverableExtractionJobs(t.Context(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, j := range jobs {
		out[j.ID] = true
	}
	return out
}

// TestReconcileNeverResumesUserOrBudgetStops——⭐ 本文件的立论。
func TestReconcileNeverResumesUserOrBudgetStops(t *testing.T) {
	repo := extractionRepo(t)
	past := time.Now().UTC().Add(-time.Hour)

	// 该捡的：崩溃留下的租约过期作业。
	seedJobInState(t, repo, "doc-rc1", "job-crashed", jobStateRunning, &past)
	// 该捡的：从没被人接手过（入队丢了）。
	seedJobInState(t, repo, "doc-rc2", "job-never", jobStateRunning, nil)
	// ⚠️ 不该捡：用户暂停。
	seedJobInState(t, repo, "doc-rc3", "job-paused", jobStatePaused, &past)
	// ⚠️ 不该捡：预算耗尽——追加额度是用户的决定，不是系统的。
	seedJobInState(t, repo, "doc-rc4", "job-rc-budget", "budget_exhausted", &past)
	// 不该捡：已经结束的。
	seedJobInState(t, repo, "doc-rc5", "job-done", jobStateSucceeded, &past)
	seedJobInState(t, repo, "doc-rc6", "job-failed", jobStateFailed, &past)
	// 不该捡：租约还在别人手里。
	future := time.Now().UTC().Add(time.Hour)
	seedJobInState(t, repo, "doc-rc7", "job-held", jobStateRunning, &future)

	got := recoverableIDs(t, repo)
	for _, id := range []string{"job-crashed", "job-never"} {
		if !got[id] {
			t.Errorf("%s 该被捡回来却没有", id)
		}
	}
	for _, id := range []string{"job-paused", "job-rc-budget", "job-done", "job-failed", "job-held"} {
		if got[id] {
			t.Errorf("%s 不该被自动恢复", id)
		}
	}
}

// TestReconcilePagesByID——分页游标必须真的推进，否则每分钟都只扫同一页，
// 后面的作业永远等不到人。
func TestReconcilePagesByID(t *testing.T) {
	repo := extractionRepo(t)
	past := time.Now().UTC().Add(-time.Hour)
	for i, id := range []string{"job-p1", "job-p2", "job-p3"} {
		seedJobInState(t, repo, "doc-pg"+string(rune('a'+i)), id, jobStateRunning, &past)
	}
	// ⚠️ 断言不能依赖"库里只有我建的这三条"：testutil 按包名缓存**同一个库**
	// 给包内全部测试共用，别的用例建的作业同样是可恢复的。
	// 第一版就是这么挂的，报的是"第二页 2 条 want 1"——看起来像分页坏了，
	// 其实是夹具串了。改成断言分页本身的性质：不重复、严格递增、全部覆盖。
	seen := map[string]bool{}
	prev := ""
	after := ""
	for page := 0; page < 50; page++ {
		got, err := repo.listRecoverableExtractionJobs(t.Context(), after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			break
		}
		for _, j := range got {
			if seen[j.ID] {
				t.Fatalf("翻页翻出了重复的作业 %s——游标没有推进", j.ID)
			}
			if prev != "" && j.ID <= prev {
				t.Fatalf("翻页顺序不是严格递增：%s 之后出现了 %s", prev, j.ID)
			}
			seen[j.ID] = true
			prev = j.ID
		}
		after = got[len(got)-1].ID
	}
	for _, id := range []string{"job-p1", "job-p2", "job-p3"} {
		if !seen[id] {
			t.Errorf("%s 没有被翻到——后面的作业会永远等不到人接手", id)
		}
	}
}

// TestReconcileAlsoFixesStaleReservations——恢复扫描顺带把停留过久的
// reserved 改判 unknown。⚠️ 这两件事必须在同一个周期任务里：
// 分开的话，一个再也不会被接手的作业上的孤儿预留会永远停在 reserved，
// 而它代表的那笔"可能花掉的钱"就永远不在账上。
func TestReconcileAlsoFixesStaleReservations(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-rcs", "job-rcs")
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: firstItemID(t, repo, job.ID), Epoch: epoch,
		Phase: phaseExtract, AttemptNumber: 1,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_attempts SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-2*time.Hour), att.ID); err != nil {
		t.Fatal(err)
	}
	res, err := repo.reconcileRelationExtractions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.ReservationsResolved != 1 {
		t.Errorf("孤儿预留改判了 %d 条, want 1", res.ReservationsResolved)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_attempts WHERE id=?`, att.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "unknown" {
		t.Errorf("state = %q, want unknown", state)
	}
}

func TestArchiveExpiredAttemptKeepsUnknownUsageLedger(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-archive", "job-archive")
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{JobID: job.ID,
		ItemID: firstItemID(t, repo, job.ID), Epoch: epoch, Phase: phaseExtract,
		AttemptNumber: 1, RequestHash: make([]byte, 32), MaxOutputTokens: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx, `UPDATE relation_extraction_attempts SET created_at=? WHERE id=?`,
		time.Now().UTC().Add(-31*24*time.Hour), att.ID); err != nil {
		t.Fatal(err)
	}
	n, err := repo.archiveExpiredExtractionAttempts(ctx, time.Now().UTC().Add(-30*24*time.Hour), 100)
	if err != nil || n != 1 {
		t.Fatalf("archive = %d, %v; want 1, nil", n, err)
	}
	if got := countRows(t, repo, `SELECT COUNT(*) FROM relation_extraction_attempts WHERE id=?`, att.ID); got != 0 {
		t.Fatalf("expired attempt remains: %d", got)
	}
	var archived int
	if err := repo.db.QueryRowContext(ctx, `SELECT CAST(JSON_UNQUOTE(JSON_EXTRACT(archived_ledger_summary, '$.unknown_usage_attempts')) AS UNSIGNED) FROM relation_extraction_jobs WHERE id=?`, job.ID).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if archived != 1 {
		t.Fatalf("archived unknown usage = %d, want 1", archived)
	}
	doc, err := repo.getDocument(ctx, job.DocumentID)
	if err != nil {
		t.Fatal(err)
	}
	status, err := repo.extractionStatus(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if status.UnknownUsageAttempts != 1 {
		t.Fatalf("status unknown usage = %d, want 1 after attempt deletion", status.UnknownUsageAttempts)
	}
}

func TestCleanupSupersededJobRemovesDerivedDataOnly(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-clean-stale", "job-clean-stale")
	if err := repo.publishItemOutcome(ctx, publishInput{JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := repo.supersedeExtractionJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	n, err := repo.cleanupStaleExtractionJobs(ctx, 100)
	if err != nil || n < 1 {
		t.Fatalf("cleanup = %d, %v; want at least this fixture, nil", n, err)
	}
	for _, table := range []string{"narrative_relation_evidence", "narrative_relations", "narrative_aliases", "narrative_characters", "relation_extraction_items"} {
		if got := countRows(t, repo, "SELECT COUNT(*) FROM "+table+" WHERE job_id=?", job.ID); got != 0 {
			t.Fatalf("%s still has %d stale rows", table, got)
		}
	}
}

func TestCleanupDeletedDocumentRemovesJobAfterAuditIsGone(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, _, _ := publishFixture(t, repo, "doc-clean-deleted", "job-clean-deleted")
	if err := repo.queries.DeleteDocument(ctx, job.DocumentID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.cleanupStaleExtractionJobs(ctx, 100); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, repo, `SELECT COUNT(*) FROM relation_extraction_jobs WHERE id=?`, job.ID); got != 0 {
		t.Fatalf("deleted document's job remains after its audit rows are gone: %d", got)
	}
}

// TestWalkRecoverableJobsAdvancesCursor——⭐ 变异测试逼出来的缺口。
// 生产批大小是 100，而任何合理夹具都造不出 100 条作业，
// 于是"游标推进"这段循环实际上没有任何测试。批大小做成参数之后就能验了。
//
// ⚠️ 游标不推进的表现是每一轮只扫同一页，排在后面的作业永远等不到人接手，
// 而扫描本身每次都"成功"——日志上看不出任何异常。
func TestWalkRecoverableJobsAdvancesCursor(t *testing.T) {
	repo := extractionRepo(t)
	past := time.Now().UTC().Add(-time.Hour)
	for i, id := range []string{"job-w1", "job-w2", "job-w3", "job-w4", "job-w5"} {
		seedJobInState(t, repo, "doc-w"+string(rune('a'+i)), id, jobStateRunning, &past)
	}
	seen := map[string]int{}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	n, err := repo.walkRecoverableJobs(ctx, 2, func(j recoverableJob) {
		seen[j.ID]++
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"job-w1", "job-w2", "job-w3", "job-w4", "job-w5"} {
		if seen[id] != 1 {
			t.Errorf("%s 被访问了 %d 次, want 1", id, seen[id])
		}
	}
	if n != len(seen) {
		t.Errorf("返回计数 %d 与实际访问数 %d 不符", n, len(seen))
	}
}

// TestFreshReservationIsNotReclassified——⭐ 变异测试逼出来的第二个缺口。
// 阈值只被"两小时前的预留"验过，而那在任何阈值下都会被改判。
//
// ⚠️ 阈值取得太小的后果是把**正在进行中**的调用误判成孤儿：那次调用
// 还在等服务端返回，却已经被记成 unknown；等它真的返回并结算时，
// 同一次调用在账上出现了两种结局。凭空多出的那笔 unknown 会让成本虚高，
// 而且没有任何东西报错。
func TestFreshReservationIsNotReclassified(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-fresh", "job-fresh")
	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: firstItemID(t, repo, job.ID), Epoch: epoch,
		Phase: phaseExtract, AttemptNumber: 1,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 一次刚刚开始、还在等服务端返回的调用：做旧到"一次调用超时"那么久，
	// 仍然远在阈值之内。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_attempts SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-extractionCallTimeout), att.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.reconcileRelationExtractions(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_attempts WHERE id=?`, att.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "reserved" {
		t.Errorf("state = %q——一次还在进行中的调用被误判成孤儿了", state)
	}
}
