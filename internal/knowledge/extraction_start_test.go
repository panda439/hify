package knowledge

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// extraction_start_test.go 守"文档 ready 之后抽取作业真的开起来了"这条路径。
//
// ⭐ 这是 010 里最容易悄悄断掉的一环：开关存下了、文档 ready 了、界面显示
// 已开启，而后台什么都没发生。这种失败没有任何症状——用户只会觉得
// "怎么一直没结果"，而系统里连一条作业记录都没有，恢复扫描也无从捡起
// （它只捡**已经存在**的作业）。

func startTestService(t *testing.T, repo *Repository, modelID string) Service {
	t.Helper()
	return NewService(repo, newFakeProvider(), newTestAsynqClient(t), t.TempDir(),
		false, "", time.Second, false, modelID)
}

func narrativeUpload(chapters int) []byte {
	var sb strings.Builder
	for i := 1; i <= chapters; i++ {
		sb.WriteString("第" + chineseOrdinal(i) + "章　题\n")
		sb.WriteString("赵太爷打了阿Q一个嘴巴。阿Q回到土谷祠。\n")
	}
	return []byte(sb.String())
}

// TestReadyDocumentStartsExtractionJob：勾了开关的文档 ready 之后，
// 必须有一条挂在它当前版本上的作业，item 一条不少。
func TestReadyDocumentStartsExtractionJob(t *testing.T) {
	repo := setupIntegration(t)
	svc := startTestService(t, repo, "qwen2.5:14b")
	ctx := t.Context()
	seedKB(t, repo, "kb-start", "m3", "u1", true)
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE knowledge_bases SET chunk_size=200,chunk_overlap=0 WHERE id='kb-start'"); err != nil {
		t.Fatal(err)
	}

	doc, err := svc.UploadDocumentWithOptions(ctx, "kb-start", "u1", "member",
		"novel.txt", FileTypeTxt, narrativeUpload(3),
		UploadOptions{Narrative: true, RelationExtraction: true})
	if err != nil {
		t.Fatalf("上传被拒：%v", err)
	}
	if err := svc.ProcessDocument(ctx, doc.ID, doc.Version); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	var jobID sql.NullString
	if err := repo.db.QueryRowContext(ctx,
		`SELECT active_relation_job_id FROM documents WHERE id=?`, doc.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if !jobID.Valid || jobID.String == "" {
		t.Fatal("文档 ready 了，却没有任何抽取作业指向它")
	}
	var state string
	var total, version int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state, total_items, document_version FROM relation_extraction_jobs WHERE id=?`,
		jobID.String).Scan(&state, &total, &version); err != nil {
		t.Fatal(err)
	}
	if state != jobStateRunning {
		t.Errorf("作业 state=%q", state)
	}
	if total != 3 {
		t.Errorf("total_items=%d，语料是 3 章", total)
	}
	if version != int(doc.Version) {
		t.Errorf("作业挂在版本 %d 上，文档是版本 %d", version, doc.Version)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_items WHERE job_id=?`, jobID.String); n != 3 {
		t.Errorf("item 数 = %d, want 3", n)
	}
}

// TestDocumentWithoutTheSwitchGetsNoJob：没勾开关的文档一条作业都不该有。
// ⚠️ 反向的守卫同样重要：抽取要打成百上千次本地推理，"顺手都跑一下"
// 的代价是几个小时的 GPU 和一份没人要的账单。
func TestDocumentWithoutTheSwitchGetsNoJob(t *testing.T) {
	repo := setupIntegration(t)
	svc := startTestService(t, repo, "qwen2.5:14b")
	ctx := t.Context()
	seedKB(t, repo, "kb-nostart", "m3", "u1", true)
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE knowledge_bases SET chunk_size=200,chunk_overlap=0 WHERE id='kb-nostart'"); err != nil {
		t.Fatal(err)
	}

	doc, err := svc.UploadDocumentWithOptions(ctx, "kb-nostart", "u1", "member",
		"novel.txt", FileTypeTxt, narrativeUpload(2), UploadOptions{Narrative: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, doc.ID, doc.Version); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_jobs WHERE document_id=?`, doc.ID); n != 0 {
		t.Errorf("没勾开关却建了 %d 条作业", n)
	}
}

// TestStartRefusesWithoutAConfiguredModel：没配模型时上传就被拒，
// 不是存下开关然后什么都不做。
func TestStartRefusesWithoutAConfiguredModel(t *testing.T) {
	repo := setupIntegration(t)
	svc := startTestService(t, repo, "") // 本部署没开这个功能
	ctx := t.Context()
	seedKB(t, repo, "kb-nomodel", "m3", "u1", true)

	_, err := svc.UploadDocumentWithOptions(ctx, "kb-nomodel", "u1", "member",
		"novel.txt", FileTypeTxt, narrativeUpload(1),
		UploadOptions{Narrative: true, RelationExtraction: true})
	if err == nil {
		t.Fatal("没配模型却接受了抽取开关")
	}
	if got := err.Error(); !strings.Contains(got, "关系抽取") {
		t.Errorf("错误信息没说清是抽取模型的问题：%v", err)
	}
}
