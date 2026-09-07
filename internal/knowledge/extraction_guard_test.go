package knowledge

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// extraction_guard_test.go 守「作业与它的语料是否还配套」（010 T022）。
//
// ⭐ 这一组防的全是**迟到的写入**：一次调用发出去的时候一切正常，
// 等它回来时世界已经变了——文档被重新处理、被删除，或者作业被别人接管。
// 此刻手上那份结果算的是**已经不存在的那个版本**。
//
// ⚠️ 它们的共同点是不会报错。关系照样能插进去，端点、章节号、引文全都合法，
// 只是指向的原文已经不是用户现在看到的那份了。用户点开引用会看到一段
// 对不上的文字，而系统认为自己一切正常。

// TestPublishRefusedAfterDocumentReprocessed——⭐ 文档被重新处理（版本变了）
// 之后，旧作业的结果必须发不进去。
func TestPublishRefusedAfterDocumentReprocessed(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-g1", "job-g1")

	// 用户重新上传/重新处理了这份文档。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET version = version + 1 WHERE id = 'doc-g1'`); err != nil {
		t.Fatal(err)
	}
	err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	})
	if !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v, want ErrExtractionSourceChanged", err)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("旧版本的结果被发布了 %d 条", n)
	}
}

// TestPublishRefusedAfterDocumentDeleted——⭐ 删除期间响应返回。
//
// ⚠️ 跨库没有外键，删掉文档不会连带删掉这些行。发布不挡的话会留下一批
// 指向不存在文档的关系：查询时它们要么被静默过滤（覆盖率数字对不上），
// 要么带着一个打不开的来源出现在回答里。
func TestPublishRefusedAfterDocumentDeleted(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-g2", "job-g2")

	if _, err := repo.db.ExecContext(ctx, `DELETE FROM documents WHERE id = 'doc-g2'`); err != nil {
		t.Fatal(err)
	}
	err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	})
	if !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v, want ErrExtractionSourceChanged", err)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("文档已删除却留下了 %d 个人物", n)
	}
}

// TestStartingNewRunSupersedesTheOldOne——⭐ restart 时旧作业必须被标 superseded。
//
// ⚠️ 只把文档指针改到新作业是不够的：旧作业的 state 还是 running，
// 恢复扫描会把它当成"崩溃的作业"捡回来接着跑——于是两个 run 同时对同一份
// 文档花钱，而两者看起来都健康。
func TestStartingNewRunSupersedesTheOldOne(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-g3", 2)
	if _, err := repo.initializeExtractionJob(ctx,
		newExtractionJobSpec("job-g3-run1", "doc-g3", 1, "m-1")); err != nil {
		t.Fatal(err)
	}
	spec2 := newExtractionJobSpec("job-g3-run2", "doc-g3", 1, "m-1")
	spec2.RunNumber = 2
	if _, err := repo.initializeExtractionJob(ctx, spec2); err != nil {
		t.Fatal(err)
	}

	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_jobs WHERE id='job-g3-run1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != jobStateSuperseded {
		t.Errorf("旧作业 state = %q, want superseded——否则恢复扫描会把它当崩溃作业捡回来", state)
	}
	// 被取代的作业不得再出现在恢复扫描里。
	if recoverableIDs(t, repo)["job-g3-run1"] {
		t.Error("被取代的作业仍然会被自动恢复")
	}
	// 新作业才是文档指向的那个。
	row, err := repo.queries.GetDocumentExtractionState(ctx, "doc-g3")
	if err != nil {
		t.Fatal(err)
	}
	if row.ActiveRelationJobID.String != "job-g3-run2" {
		t.Errorf("文档指向 %q", row.ActiveRelationJobID.String)
	}
	// ⚠️ 还要确认**新**作业没有被自己那条取代语句连带标掉。
	// 变异测试发现少了这一条：把 `id <> ?` 去掉之后，新作业刚建好就是
	// superseded，而上面那些断言全都照样通过——文档指向它、旧作业也确实
	// 被取代了，只是这个 run 一步都跑不动，且没有任何东西说明原因。
	var newState string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state FROM relation_extraction_jobs WHERE id=?`, "job-g3-run2").Scan(&newState); err != nil {
		t.Fatal(err)
	}
	if newState != jobStateRunning {
		t.Errorf("新作业 state = %q, want running", newState)
	}
}

// TestJobPinsItsModelAndSource——⭐ 作业固定自己的模型与语料指纹。
//
// ⚠️ 从 documents.relation_model_id 现查的后果是：用户中途换了模型，
// 同一个 run 的前半段用 A、后半段用 B，而报告里只会写一个模型名。
// 那份"某模型的准确率"从此不可复现，且没有任何地方记录发生过切换。
func TestJobPinsItsModelAndSource(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-g4", 2)
	job, err := repo.initializeExtractionJob(ctx,
		newExtractionJobSpec("job-g4", "doc-g4", 1, "m-original"))
	if err != nil {
		t.Fatal(err)
	}
	// 用户在文档上换了模型。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET relation_model_id = 'm-switched' WHERE id = 'doc-g4'`); err != nil {
		t.Fatal(err)
	}
	reloaded, err := repo.queries.GetRelationExtractionJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ModelID != "m-original" {
		t.Errorf("作业的模型跟着文档变了：%q——同一个 run 会前后用两个模型", reloaded.ModelID)
	}
	if !reloaded.SourceHash.Valid || len(reloaded.SourceHash.String) != 32 {
		t.Error("语料指纹没有固定在作业上")
	}
}

// TestInitializationCrashAfterPublishLeavesNothingOrphaned——⭐ 故障注入：
// PG 已经发布、MySQL 的作业还没建出来就崩了。
//
// ⚠️ 这是初始化最危险的一格：片段已经是"真相"，而没有任何作业指向它们。
// 必须能原样重来，且重来之后 item 数与片段数仍然严格相等——
// 如果第一次失败留下了半截 item，第二次就会在它们之上再建一批，
// 总数翻倍而每一条看起来都正常。
func TestInitializationCrashAfterPublishLeavesNothingOrphaned(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	seedNarrativeDocument(t, repo, "doc-g5", 4)

	// 第一次初始化在事务中途失败（文档版本被挪走）。
	spec := newExtractionJobSpec("job-g5-a", "doc-g5", 1, "m-1")
	spec.afterEnumerate = func() error {
		_, err := repo.db.ExecContext(ctx,
			`UPDATE documents SET version = 2 WHERE id = 'doc-g5'`)
		return err
	}
	if _, err := repo.initializeExtractionJob(ctx, spec); !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v", err)
	}
	// 把版本改回去（模拟"其实没变，只是当时那一刻读到了中间态"）。
	if _, err := repo.db.ExecContext(ctx,
		`UPDATE documents SET version = 1 WHERE id = 'doc-g5'`); err != nil {
		t.Fatal(err)
	}
	job, err := repo.initializeExtractionJob(ctx, newExtractionJobSpec("job-g5-b", "doc-g5", 1, "m-1"))
	if err != nil {
		t.Fatalf("重来失败：%v", err)
	}
	if job.TotalItems != 4 {
		t.Errorf("total_items = %d, want 4", job.TotalItems)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE job_id='job-g5-a'`); n != 0 {
		t.Errorf("失败的那次留下了 %d 个 item", n)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE job_id=?`, job.ID); n != 4 {
		t.Errorf("重来后 item 数 = %d, want 4", n)
	}
}

// TestConcurrentPublishesDoNotDeadlock——⭐ 锁顺序 document → job → item。
//
// ⚠️ 顺序不一致的表现是**偶发死锁**：两个 worker 各持一半的锁互相等，
// MySQL 在超时后杀掉其中一个。它只在并发发布同一份文档时出现，
// 单元测试跑一百次可能一次都不复现，而生产上会周期性地丢掉一个 item
// 并留下一条难以归因的错误。
func TestConcurrentPublishesDoNotDeadlock(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch := ledgerJob(t, repo, "doc-g6", "job-g6")

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

	var wg sync.WaitGroup
	errs := make([]error, len(items))
	for i, item := range items {
		wg.Add(1)
		go func(i int, item string) {
			defer wg.Done()
			errs[i] = repo.publishItemOutcome(ctx, publishInput{
				JobID: job.ID, ItemID: item, Epoch: epoch,
				Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
			})
		}(i, item)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("第 %d 个 item 并发发布失败：%v", i, err)
		}
	}
	if n := countRows(t, repo,
		`SELECT succeeded_items FROM relation_extraction_jobs WHERE id=?`, job.ID); n != len(items) {
		t.Errorf("succeeded_items = %d, want %d", n, len(items))
	}
}

// TestSupersededJobCannotPublish——⭐ 变异测试逼出来的缺口。
//
// restart 之后，旧作业那个**还在飞的**调用会返回，然后尝试发布。
// 此时文档还在、版本也没变——前两道守卫都放行，只有「这个作业已被取代」
// 这一条能拦住它。删掉它测试全绿，而后果是一份被用户显式替换掉的 run
// 仍然在往库里写关系，和新 run 的结果混在一起。
func TestSupersededJobCannotPublish(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	job, epoch, item := publishFixture(t, repo, "doc-g7", "job-g7-run1")

	spec2 := newExtractionJobSpec("job-g7-run2", "doc-g7", 1, "m-1")
	spec2.RunNumber = 2
	if _, err := repo.initializeExtractionJob(ctx, spec2); err != nil {
		t.Fatal(err)
	}
	err := repo.publishItemOutcome(ctx, publishInput{
		JobID: job.ID, ItemID: item, Epoch: epoch,
		Outcome: sampleOutcome(), ExtractResponse: []byte(`{}`),
	})
	if !errors.Is(err, ErrExtractionSourceChanged) {
		t.Fatalf("err = %v, want ErrExtractionSourceChanged", err)
	}
	if n := countRows(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, job.ID); n != 0 {
		t.Errorf("被取代的 run 仍然写进了 %d 条关系", n)
	}
}

var _ = time.Second
