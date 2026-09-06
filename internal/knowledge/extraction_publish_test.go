package knowledge

import (
	"errors"
	"testing"
	"time"

	"hify/internal/provider"
)

// extraction_publish_test.go 守成功结果的发布（010 T020）。
//
// ⭐ 人物、关系、证据、item 状态、作业计数必须在**同一个事务**里。
// 分开的后果不是"数据不一致"这种抽象说法，而是很具体的两种：
//   - 关系写了、item 没标成功 → 恢复后重跑，同一批关系再抽一遍，钱花两遍；
//   - item 标了成功、关系没写 → 覆盖率分子照涨，而那一段书其实是空的。
// 两者都不报错。

func publishFixture(t *testing.T, repo *Repository, docID, jobID string) (RelationExtractionJob, int, string) {
	t.Helper()
	job, epoch := ledgerJob(t, repo, docID, jobID)
	return job, epoch, firstItemID(t, repo, jobID)
}

func sampleOutcome() extractionOutcome {
	return extractionOutcome{
		Characters: []characterDraft{
			{LocalRef: "m1", DisplayName: "阿Q", FirstSourceOrder: 10},
			{LocalRef: "m2", DisplayName: "赵太爷", FirstSourceOrder: 12},
		},
		Relations: []relationDraft{{
			SubjectRef: "m1", ObjectRef: "m2", Type: "冲突", IsDirected: true,
			FirstSourceOrder: 12, ChapterNumber: intPtr(2),
			Evidence: []evidenceDraft{{
				ChunkID: "c-1", DocumentVersion: 1, SourceOrder: 12,
				SourceStart: 100, SourceEnd: 140, Quote: "赵太爷跳过去给了他一个嘴巴",
			}},
		}},
	}
}

func countRows(t *testing.T, repo *Repository, q string, args ...any) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPublishWritesEverythingInOneTransaction
func TestPublishWritesEverythingInOneTransaction(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub1", "job-pub1")

	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Outcome: sampleOutcome(),
		ExtractResponse: []byte(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("publishItemOutcome: %v", err)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, job.ID); n != 2 {
		t.Errorf("人物 %d 条, want 2", n)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 1 {
		t.Errorf("关系 %d 条, want 1", n)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relation_evidence WHERE job_id=?`, job.ID); n != 1 {
		t.Errorf("证据 %d 条, want 1", n)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE id=? AND state='succeeded'`, item); n != 1 {
		t.Error("item 没有标成功")
	}
	if n := countRows(t, repo,
		`SELECT succeeded_items FROM relation_extraction_jobs WHERE id=?`, job.ID); n != 1 {
		t.Errorf("succeeded_items = %d, want 1", n)
	}
	// 关系两端必须指向本次真的建出来的人物，不能是悬空 ID。
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations r
		JOIN narrative_characters s ON s.id = r.subject_id
		JOIN narrative_characters o ON o.id = r.object_id
		WHERE r.job_id = ?`, job.ID); n != 1 {
		t.Error("关系端点没有指向本次创建的人物")
	}
}

// TestEmptyOutcomeIsStillSuccess——⭐ 一个块里没有关系是**正常结果**，
// 不是失败。判成失败会让 failed_items 里混进一堆其实处理正确的块，
// 而那个数字是要写进报告的"失败率"。
func TestEmptyOutcomeIsStillSuccess(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub2", "job-pub2")

	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: extractionOutcome{}, ExtractResponse: []byte(`{"relations":[]}`),
	}); err != nil {
		t.Fatalf("空结果发布失败：%v", err)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE id=? AND state='succeeded'`, item); n != 1 {
		t.Error("空结果没有被当成成功")
	}
	if n := countRows(t, repo, `SELECT failed_items FROM relation_extraction_jobs WHERE id=?`, job.ID); n != 0 {
		t.Errorf("空结果被计进了失败：failed_items = %d", n)
	}
	// ⚠️ 只查 failed_items 是**抓不住**"空结果没被算成功"的：那样它既不在
	// 失败里也不在成功里，两个数字都正常，只有覆盖率悄悄少了一块。
	// 变异测试逼出了这一条。
	if n := countRows(t, repo, `SELECT succeeded_items FROM relation_extraction_jobs WHERE id=?`, job.ID); n != 1 {
		t.Errorf("succeeded_items = %d, want 1——空结果是成功，覆盖率分子要涨", n)
	}
}

// TestPublishTwiceDoesNotDoubleCount——⭐ 重复投递（asynq 重发、
// 提交后丢 ACK）不得把计数加两遍。
func TestPublishTwiceDoesNotDoubleCount(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub3", "job-pub3")

	in := publishInput{JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`)}
	if err := repo.publishItemOutcome(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := repo.publishItemOutcome(ctx, in); !errors.Is(err, ErrItemAlreadyPublished) {
		t.Fatalf("第二次发布 err = %v, want ErrItemAlreadyPublished", err)
	}
	if n := countRows(t, repo, `SELECT succeeded_items FROM relation_extraction_jobs WHERE id=?`, job.ID); n != 1 {
		t.Errorf("succeeded_items = %d，重复投递把分子加了两遍", n)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 1 {
		t.Errorf("关系 %d 条——同一处出处的同一条关系被写了两遍", n)
	}
}

// TestStaleEpochCannotPublish——过期 worker 的迟到发布必须被拒。
// ⚠️ 它此刻手上那份结果算的是**旧版本**的语料。
func TestStaleEpochCannotPublish(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub4", "job-pub4")

	// 别人抢走了租约。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET epoch = epoch + 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	})
	if !errors.Is(err, ErrExtractionEpochLost) {
		t.Fatalf("err = %v, want ErrExtractionEpochLost", err)
	}
	// ⭐ 整个事务回滚：人物、关系一条都不能留下。
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("过期 worker 留下了 %d 个人物", n)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("过期 worker 留下了 %d 条关系", n)
	}
}

// TestUndirectedRelationKeyIsOrderIndependent——⭐ 无向关系按实体 ID 排序
// 后再算 key。不排序的话 (A,B) 与 (B,A) 会变成两条，
// 而"同乡"这类关系被记两遍会直接虚增关系总数——那是要进报告的数字。
func TestUndirectedRelationKeyIsOrderIndependent(t *testing.T) {
	a, b := "id-aaa", "id-zzz"
	k1 := relationKeyHash(a, b, "同乡", false, 100)
	k2 := relationKeyHash(b, a, "同乡", false, 100)
	if string(k1) != string(k2) {
		t.Error("无向关系换个顺序算出了不同的 key")
	}
	// 有向关系必须区分方向。
	d1 := relationKeyHash(a, b, "杀害", true, 100)
	d2 := relationKeyHash(b, a, "杀害", true, 100)
	if string(d1) == string(d2) {
		t.Error("有向关系的方向被抹掉了——「甲杀害乙」和「乙杀害甲」不是一回事")
	}
	// 不同出处必须是不同的记录（关系历史不覆盖）。
	if string(relationKeyHash(a, b, "同乡", false, 100)) ==
		string(relationKeyHash(a, b, "同乡", false, 5000)) {
		t.Error("不同出处算出了同一个 key——关系历史会被覆盖成最后一个状态")
	}
}

// TestEvidenceKeyIgnoresChunkID——⭐ 相邻 chunk 因 overlap 含同一段原文。
// 按 chunk_id 去重会把同一处出处记成两条证据，虚增报告里的证据条数。
func TestEvidenceKeyIgnoresChunkID(t *testing.T) {
	e1 := evidenceDraft{ChunkID: "c-1", SourceStart: 100, SourceEnd: 140, Quote: "同一句话"}
	e2 := evidenceDraft{ChunkID: "c-2", SourceStart: 100, SourceEnd: 140, Quote: "同一句话"}
	if string(evidenceKeyHash(e1)) != string(evidenceKeyHash(e2)) {
		t.Error("同一处出处因为来自不同 chunk 被算成两条证据")
	}
	e3 := evidenceDraft{ChunkID: "c-1", SourceStart: 200, SourceEnd: 240, Quote: "同一句话"}
	if string(evidenceKeyHash(e1)) == string(evidenceKeyHash(e3)) {
		t.Error("不同位置的同一句话被合并了——原文里重复出现的句子是不同的出处")
	}
}

// TestReplayUsesStoredResponseInsteadOfCallingAgain——⭐ 响应已落盘、
// 发布前崩溃，恢复后必须**回放**，不能再打一次模型。
//
// ⚠️ 再打一次的后果不是"结果不一致"，是那笔钱白花第二遍，
// 而账目上看起来完全正常——两次都是真实发生的调用。
func TestReplayUsesStoredResponseInsteadOfCallingAgain(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub5", "job-pub5")

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	const body = `{"mentions":[],"relations":[],"alias_proposals":[]}`
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 100,
		FinishReason: "stop", Message: provider.Message{Content: body},
	}); err != nil {
		t.Fatal(err)
	}
	// 此刻崩溃：item 仍是 running，响应已落盘。
	replay, ok, err := repo.findReplayableResponse(ctx, item, phaseExtract)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("响应已落盘却找不到可回放的尝试——恢复后会再打一次模型")
	}
	if replay.Body != body {
		t.Errorf("回放内容不是当时那份：%q", replay.Body)
	}
	if replay.FinishReason != "stop" {
		t.Errorf("回放丢了 finish_reason：%q", replay.FinishReason)
	}

	// ⚠️ 只有 completed 且落了盘的才可回放。unknown 的不行——
	// 那次调用可能根本没产生响应，拿它当结果就是凭空造数据。
	job2, epoch2, item2 := publishFixture(t, repo, "doc-pub6", "job-pub6")
	att2, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job2.ID, ItemID: item2, Epoch: epoch2, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att2, provider.ChatAttemptResult{
		Outcome: provider.AttemptUnknown, ErrorCode: "timeout", ElapsedMs: 60000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.findReplayableResponse(ctx, item2, phaseExtract); err != nil || ok {
		t.Errorf("unknown 的尝试被当成可回放：ok=%v err=%v", ok, err)
	}
}

// TestLedgerSurvivesPublishRollback——⭐ T017 那条"账先落、业务后落"的
// 设计，在这里第一次能端到端验证。
//
// ⚠️ 发布失败会回滚，而那次调用的钱已经花了。账目跟着回滚就等于把一笔
// 真实成本抹掉，而且抹得干干净净——没有任何地方留下痕迹。
func TestLedgerSurvivesPublishRollback(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub7", "job-pub7")

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.settleExtractionAttempt(ctx, att, provider.ChatAttemptResult{
		Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: 250,
		Message: provider.Message{Content: `{}`},
	}); err != nil {
		t.Fatal(err)
	}
	// 让发布失败：epoch 被抢走。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_jobs SET epoch = epoch + 1 WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	}); !errors.Is(err, ErrExtractionEpochLost) {
		t.Fatalf("err = %v", err)
	}
	// 账目必须还在。
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_attempts WHERE id=? AND state='completed'`,
		att.ID); n != 1 {
		t.Error("发布回滚把已经花掉的那次调用从账上抹掉了")
	}
	_, confirmed, _, activeMs := jobLedger(t, repo, job.ID)
	if confirmed != 1 || activeMs != 250 {
		t.Errorf("作业级账目被发布回滚带走了：confirmed=%d active_ms=%d", confirmed, activeMs)
	}
}

var _ = time.Second

// TestDanglingRelationEndpointFailsTheItem——⭐ 变异测试逼出来的缺口。
// 关系引用了一个本次没有创建的人物时，**整个 item 失败**，绝不跳过这条关系。
//
// ⚠️ 跳过的表现是关系总数悄悄变小，而那是要进报告的数字；而且这种响应
// 本身就说明模型输出的引用是坏的，静默丢掉一条只会让坏输出看起来正常。
func TestDanglingRelationEndpointFailsTheItem(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub8", "job-pub8")

	// ⚠️ 主体和客体是**两道对称的守卫**，必须各验一次。
	// 第一版只验了客体那一格，于是把主体那一格删掉时测试照样全绿——
	// 变异测试暴露了这一点（我最初误以为是变异写错了，其实是断言少一半）。
	for _, tc := range []struct {
		name    string
		subject string
		object  string
	}{
		{"客体悬空", "m1", "m9"},
		{"主体悬空", "m9", "m1"},
	} {
		bad := extractionOutcome{
			Characters: []characterDraft{{LocalRef: "m1", DisplayName: "阿Q", FirstSourceOrder: 1}},
			Relations: []relationDraft{{
				SubjectRef: tc.subject, ObjectRef: tc.object,
				Type: "冲突", IsDirected: true, FirstSourceOrder: 2,
			}},
		}
		if err := repo.publishItemOutcome(ctx, publishInput{
			JobID: job.ID, ItemID: item, Epoch: epoch, Outcome: bad,
			ExtractResponse: []byte(`{}`),
		}); err == nil {
			t.Fatalf("%s：悬空端点没有让发布失败", tc.name)
		}
		// 整个事务回滚：那个合法的人物也不能留下。
		if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, job.ID); n != 0 {
			t.Errorf("%s：失败的发布留下了 %d 个人物", tc.name, n)
		}
		if n := countRows(t, repo,
			`SELECT COUNT(*) FROM relation_extraction_items WHERE id=? AND state='succeeded'`, item); n != 0 {
			t.Errorf("%s：悬空端点却把 item 标成了成功", tc.name)
		}
	}
}

// TestReplayRejectsNonCompletedAttempt——⭐ 变异测试逼出来的第二个缺口。
//
// 上一条回放用例里的 unknown 尝试**没有** raw_response（超时本来就没有响应体），
// 所以 "raw_response IS NOT NULL" 自己就把它挡掉了，state='completed' 这个
// 条件从来没被验过——删掉它测试全绿。
//
// 这里直接插一条"有响应体但状态不是 completed"的记录。这种行现在的代码
// 产生不出来，但只要将来把失败响应的 body 也存下来（为了排查，很合理），
// 它立刻就会出现。届时拿它当结果就是**凭空造数据**：那次调用的结局是
// "不知道"，它的响应体未必完整。
func TestReplayRejectsNonCompletedAttempt(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-pub9", "job-pub9")

	att, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
		JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
		RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE relation_extraction_attempts
		 SET state='unknown', raw_response='{"relations":[]}', dispatch_confirmed=0
		 WHERE id=?`, att.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := repo.findReplayableResponse(ctx, item, phaseExtract); err != nil || ok {
		t.Errorf("状态不是 completed 的响应被当成可回放：ok=%v err=%v", ok, err)
	}
}

// --- 010 R6-02：发布与调用预留的状态/当前 run/租约守卫 ---

// TestPublishRejectedOnceAnotherRunTakesOver——⭐ 文档已经指向别的 run 时
// **不得再发布**。
//
// ⚠️ verifyJobSourceStillCurrent 的注释一直声称在核对这件事，而
// LockDocumentForExtraction 根本没把 active_relation_job_id 选出来——
// 那句核对是空话。restart 换了 run 之后，旧 worker 的结果照样发布进去，
// 与新 run 的结果混在一起：人物重复、关系重复、覆盖率的分母对不上，
// 而两边都不会报错。
func TestPublishRejectedOnceAnotherRunTakesOver(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-r602a", "job-r602a")

	// 模拟 restart：文档改指另一个 run。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET active_relation_job_id='job-r602a-new' WHERE id='doc-r602a'`); err != nil {
		t.Fatal(err)
	}
	err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Outcome: sampleOutcome(),
		ExtractResponse: []byte(`{"ok":true}`),
	})
	if !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("文档已经指向新 run，旧 run 却发布成功了：%v", err)
	}
	for _, table := range []string{"narrative_characters", "narrative_relations"} {
		if n := countRows(t, repo,
			`SELECT COUNT(*) FROM `+table+` WHERE job_id=?`, job.ID); n != 0 {
			t.Errorf("%s 留下了 %d 条", table, n)
		}
	}
}

// TestPublishRejectedOnceExtractionIsDisabled——用户关掉开关之后不得再写入。
//
// ⚠️ 继续发布的表现是：用户看到一份自己已经关停的抽取还在往库里加东西。
func TestPublishRejectedOnceExtractionIsDisabled(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-r602b", "job-r602b")

	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET is_relation_extraction_enabled=0 WHERE id='doc-r602b'`); err != nil {
		t.Fatal(err)
	}
	if err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch, Outcome: sampleOutcome(),
		ExtractResponse: []byte(`{"ok":true}`),
	}); !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("开关已关闭，发布却成功了：%v", err)
	}
}

// TestPublishRejectedOnceTheJobHasStopped——已经停下的作业不得接受发布。
//
// ⚠️ 此前只挡 superseded，于是一个已经 paused / failed / succeeded 的作业
// 照样能被一条迟到的发布改写——succeeded_items 因此可以超过 total_items，
// 覆盖率算出大于 100% 的数。
func TestPublishRejectedOnceTheJobHasStopped(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	for _, state := range []string{jobStatePaused, jobStateFailed, jobStateSucceeded, jobStateBudgetExhausted} {
		docID, jobID := "doc-r602c-"+state, "job-r602c-"+state
		job, epoch, item := publishFixture(t, repo, docID, jobID)
		if _, err := repo.db.ExecContext(ctx,
			`UPDATE relation_extraction_jobs SET state=? WHERE id=?`, state, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := repo.publishItemOutcome(ctx, publishInput{
			JobID: job.ID, ItemID: item, Epoch: epoch, Outcome: sampleOutcome(),
			ExtractResponse: []byte(`{"ok":true}`),
		}); !errors.Is(err, ErrExtractionSourceChanged) {
			t.Errorf("状态 %s 的作业接受了发布：%v", state, err)
		}
	}
}

// TestReservationRequiresALiveJobAndLease——⭐ 预留调用额度光守 epoch 不够。
//
// ⚠️ epoch 只在**别人抢走租约**时才变，而下面两件事都不会动它：
//   - 用户点了暂停 → worker 手上的 epoch 照样有效，于是它继续一次次花钱，
//     而界面显示"已暂停"；
//   - 自己的租约已经过期但还没被别人抢 → 同样继续花钱，而下一个 worker
//     随时可能接手同一批 item，那部分钱花两遍。
func TestReservationRequiresALiveJobAndLease(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()

	t.Run("暂停之后不得再预留", func(t *testing.T) {
		job, epoch := ledgerJob(t, repo, "doc-r602d", "job-r602d")
		item := firstItemID(t, repo, job.ID)
		if _, err := repo.db.ExecContext(ctx,
			`UPDATE relation_extraction_jobs SET state='paused' WHERE id=?`, job.ID); err != nil {
			t.Fatal(err)
		}
		_, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err == nil {
			t.Fatal("作业已暂停，却仍然预留成功——它会继续花钱，而界面显示已暂停")
		}
	})

	t.Run("租约过期之后不得再预留", func(t *testing.T) {
		job, epoch := ledgerJob(t, repo, "doc-r602e", "job-r602e")
		item := firstItemID(t, repo, job.ID)
		if _, err := repo.db.ExecContext(ctx,
			`UPDATE relation_extraction_jobs SET lease_until = DATE_SUB(NOW(3), INTERVAL 1 MINUTE)
			 WHERE id=?`, job.ID); err != nil {
			t.Fatal(err)
		}
		_, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		})
		if err == nil {
			t.Fatal("租约已过期，却仍然预留成功——下一个 worker 随时会接手同一批 item")
		}
	})

	t.Run("正常持有时照常预留", func(t *testing.T) {
		// ⚠️ 底线用例：少了它，一个"永远拒绝"的实现能让上面两条都通过。
		job, epoch := ledgerJob(t, repo, "doc-r602f", "job-r602f")
		item := firstItemID(t, repo, job.ID)
		if _, err := repo.reserveExtractionAttempt(ctx, attemptReservation{
			JobID: job.ID, ItemID: item, Epoch: epoch, Phase: phaseExtract,
			RequestHash: make([]byte, 32), MaxOutputTokens: 2048,
		}); err != nil {
			t.Fatalf("正常持有租约却预留失败：%v", err)
		}
	})
}
