package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"hify/internal/db/gen"
	"hify/internal/db/pggen"
	"hify/internal/platform/apperr"
	"hify/internal/user"
)

// narrative_upload_test.go 守 010 US1 的上传开关（T012/T013）。
//
// ⭐ 这里最要紧的不是"新功能能开"，而是**默认关闭时一切不变**。
// 新增一个开关最典型的事故不是开关坏了，而是它的存在改变了没勾它的那条路径——
// 而那条路径覆盖的是系统里已有的全部文档。

// --- 分块分发：默认关闭必须逐字节同改动前 ---

// TestDefaultOffChunkingIsUnchanged——三种格式、开关关闭时，分块结果必须与
// 不带 narrative 参数的旧实现完全一致。
//
// ⚠️ 这条是**回归基线**，不是新功能验证。它的价值在于证明叙事分支的引入
// 没有波及不该波及的路径；删掉它，`chunkDocument` 里那个前置 if 就没有任何
// 东西盯着了。
func TestDefaultOffChunkingIsUnchanged(t *testing.T) {
	cases := map[string]struct {
		fileType string
		parsed   parsedContent
	}{
		"txt": {FileTypeTxt, parsedContent{Text: "第一章　甲\n甲的正文。\n\n第二段。\n"}},
		"md":  {FileTypeMD, parsedContent{Text: "# 标题\n\n正文一。\n\n## 小节\n\n正文二。\n"}},
		"pdf": {FileTypePDF, parsedContent{Pages: []pdfPage{
			{Number: 1, Text: "第一页正文。"}, {Number: 2, Text: "第二页正文。"}}}},
	}
	for name, tc := range cases {
		off := chunkDocument(tc.fileType, tc.parsed, 200, 20, false)
		if len(off) == 0 {
			t.Fatalf("%s：夹具没切出任何块", name)
		}
		for i, p := range off {
			// 关闭模式下这三个字段必须一个都不出现——它们一旦有值就会被
			// repository 写进 narrative_metadata，而存量文档必须是 NULL。
			if p.Narrative != nil || p.SceneKey != nil || p.ChapterNumber != nil {
				t.Errorf("%s 第 %d 块在关闭模式下带上了叙事字段：narrative=%v scene=%v chapter=%v",
					name, i, p.Narrative != nil, p.SceneKey, p.ChapterNumber)
			}
		}
	}
	// md 关闭时必须走标题栈而不是场景切分：两者都不报错，只有内容能区分。
	md := chunkDocument(FileTypeMD, parsedContent{Text: "# 标题\n\n正文。\n"}, 200, 0, false)
	if md[0].SectionTitle == nil || *md[0].SectionTitle != "标题" {
		t.Errorf("md 关闭模式没有走标题栈：SectionTitle=%v", md[0].SectionTitle)
	}
}

// TestNarrativeOnDispatchesBeforeFileType——⭐ 叙事分支在格式分发**之前**。
// 一份 md 格式的小说，开了叙事就必须按场景切，而不是按 markdown 标题栈。
// 放在 default 分支里的话这份文档会走标题栈，且不报错。
func TestNarrativeOnDispatchesBeforeFileType(t *testing.T) {
	text := "# 不是章节标题\n\n第一章　甲\n甲的正文。\n第二章　乙\n乙的正文。\n"
	for _, ft := range []string{FileTypeTxt, FileTypeMD} {
		pieces := chunkDocument(ft, parsedContent{Text: text}, 500, 0, true)
		var chapters []int
		for _, p := range pieces {
			if p.Narrative == nil {
				t.Fatalf("%s 开启叙事后仍有块没有元数据", ft)
			}
			if p.ChapterNumber != nil {
				chapters = append(chapters, *p.ChapterNumber)
			}
		}
		if len(chapters) != 2 || chapters[0] != 1 || chapters[1] != 2 {
			t.Errorf("%s 没有按场景切：识别到的章节号 %v", ft, chapters)
		}
	}
}

// --- 上传选项的三条拒绝理由 ---

// TestUploadOptionsRejectionsAreExplicit——⚠️ 三条守卫的共同点是
// **明确报错，绝不"接受开关然后什么都不做"**。静默接受的表现都一样：
// 用户勾了一个开关、界面显示已开启，而实际行为与没勾完全相同。
func TestUploadOptionsRejectionsAreExplicit(t *testing.T) {
	cases := []struct {
		name     string
		fileType string
		opts     UploadOptions
		want     error
	}{
		{"默认全关", FileTypeTxt, UploadOptions{}, nil},
		{"叙事 txt", FileTypeTxt, UploadOptions{Narrative: true}, nil},
		{"叙事 md", FileTypeMD, UploadOptions{Narrative: true}, nil},
		{"叙事 pdf 明确拒绝", FileTypePDF, UploadOptions{Narrative: true},
			ErrNarrativeUnsupportedFileType},
		{"抽取但没开叙事", FileTypeTxt,
			UploadOptions{RelationExtraction: true}, ErrRelationExtractionRequiresNarrative},
		// ⚠️ 勾了抽取却没给模型必须**报错**，不能默默接受。
		// 接受的表现是文档带着一个"已开启"的开关停在那里，
		// 而没有任何作业会开始——恢复扫描只捡已存在的作业。
		{"抽取但没指定模型", FileTypeTxt,
			UploadOptions{Narrative: true, RelationExtraction: true},
			ErrRelationModelRequired},
		{"抽取模型只有空白也算没给", FileTypeTxt,
			UploadOptions{Narrative: true, RelationExtraction: true, RelationModelID: "   "},
			ErrRelationModelRequired},
		{"叙事 + 抽取 + 模型", FileTypeTxt,
			UploadOptions{Narrative: true, RelationExtraction: true, RelationModelID: "m-1"},
			nil},
		// ⚠️ PDF 且关闭叙事必须照常通过——拒绝理由不能扩大到不该管的路径。
		{"普通 pdf 不受影响", FileTypePDF, UploadOptions{}, nil},
	}
	for _, tc := range cases {
		err := validateUploadOptions(tc.fileType, tc.opts)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s：got %v, want %v", tc.name, err, tc.want)
		}
	}
}

// --- HTTP 层 ---

// doUpload 打一次真实的 multipart 上传进 handler。
func doUpload(t *testing.T, svc Service, kbID, fileName string, body []byte, fields map[string]string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", fileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(body); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(svc)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: kbID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", &buf)
	c.Request.Header.Set("Content-Type", mw.FormDataContentType())

	if err := h.UploadDocument(c); err != nil {
		var appErr *apperr.AppError
		if errors.As(err, &appErr) {
			return statusForKind(appErr.Kind), appErr.Code
		}
		return http.StatusInternalServerError, err.Error()
	}
	return rec.Code, rec.Body.String()
}

// uploadOptionsSpy 只记录 handler 解析出了什么选项。
// ⚠️ 用 spy 而不是走真实 Service，是因为这里要测的**只有 multipart 解析**：
// 真实上传要落盘、要入队 asynq，把这条断言和那些失败源绑在一起，
// 一次红了分不清是解析错了还是 Redis 没起。
type uploadOptionsSpy struct {
	Service
	got UploadOptions
}

func (s *uploadOptionsSpy) UploadDocumentWithOptions(
	_ context.Context, _, _, _, _, _ string, _ []byte, opts UploadOptions) (Document, error) {
	s.got = opts
	return Document{ID: "d-1", FileName: "a.txt", FileType: FileTypeTxt,
		Status: StatusPending, IsNarrative: opts.Narrative,
		IsRelationExtractionEnabled: opts.RelationExtraction}, nil
}

// TestUploadFormFieldsParseStrictly——⭐ 表单字段缺失（旧客户端）必须等于关闭。
// 这是整个开关最重要的一格：所有现有的上传调用都不会带这两个字段。
func TestUploadFormFieldsParseStrictly(t *testing.T) {
	cases := map[string]struct {
		fields map[string]string
		want   UploadOptions
	}{
		"字段缺失（旧客户端）": {nil, UploadOptions{}},
		"显式 true":    {map[string]string{"narrative_mode": "true"}, UploadOptions{Narrative: true}},
		"显式 false":   {map[string]string{"narrative_mode": "false"}, UploadOptions{}},
		// ⚠️ 这两格是故意的：只有精确的 "true" 算开。
		// strconv.ParseBool 会把 "1" 当真、把 "ture" 当解析错误返回 400，
		// 而这里想要的是"看不懂就当没勾"，绝不因为一个打错的开关值让上传失败。
		"数字 1 不算开":     {map[string]string{"narrative_mode": "1"}, UploadOptions{}},
		"打错的 ture 不算开": {map[string]string{"narrative_mode": "ture"}, UploadOptions{}},
		"两个开关都显式 true": {
			map[string]string{"narrative_mode": "true", "extract_relations": "true"},
			UploadOptions{Narrative: true, RelationExtraction: true}},
	}
	for name, tc := range cases {
		spy := &uploadOptionsSpy{}
		if code, body := doUpload(t, spy, "kb-1", "a.txt", []byte("正文"), tc.fields); code != http.StatusOK {
			t.Fatalf("%s：上传失败 %d %s", name, code, body)
		}
		if spy.got != tc.want {
			t.Errorf("%s：handler 解析出 %+v, want %+v", name, spy.got, tc.want)
		}
	}
}

// TestUploadResponseEchoesFlags——响应里回显开关，前端才能显示"这份文档
// 是按场景切的"。回显的是**服务端存下来的值**，不是请求里的值。
func TestUploadResponseEchoesFlags(t *testing.T) {
	spy := &uploadOptionsSpy{}
	code, body := doUpload(t, spy, "kb-1", "a.txt", []byte("正文"),
		map[string]string{"narrative_mode": "true"})
	if code != http.StatusOK {
		t.Fatalf("上传失败 %d %s", code, body)
	}
	if !strings.Contains(body, `"is_narrative":true`) {
		t.Errorf("响应没有回显 is_narrative：%s", body)
	}
	if !strings.Contains(body, `"is_relation_extraction_enabled":false`) {
		t.Errorf("响应没有回显 is_relation_extraction_enabled：%s", body)
	}
}

// --- 端到端：上传 → ready → 落库的场景元数据（T013）---

// TestNarrativeDocumentProcessesEndToEnd 走真实的 ProcessDocument：
// 解析 → 按场景分块 → embed → 写 PG → 发布，然后核对落库的元数据
// 能把每一块指回原文的正确位置。
//
// ⭐ 前面所有用例都停在纯函数或 handler 边界。只有这一条能证明开关**真的
// 一路传到了分块那一步**——中间任何一环把 doc.IsNarrative 丢掉，
// 上传照样成功、文档照样 ready、检索照样有结果，只是切法悄悄退回按长度切。
func TestNarrativeDocumentProcessesEndToEnd(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-narr", "m3", "u1", true)
	body := "第一章　初见\n" + strings.Repeat("甲的正文。", 30) + "\n" +
		"第二章　别离\n" + strings.Repeat("乙的正文。", 30) + "\n"
	path := filepath.Join(dir, "novel.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: "doc-narr", KnowledgeBaseID: "kb-narr", FileName: "novel.txt",
		FileType: FileTypeTxt, FileSize: len(body), StoragePath: path,
		CreatedBy: "u1", IsNarrative: true}
	if err := repo.createDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, "doc-narr", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	got, err := repo.getDocument(ctx, "doc-narr")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady {
		t.Fatalf("status = %q, error=%q", got.Status, got.ErrorMessage)
	}
	if !got.IsNarrative {
		t.Error("is_narrative 没有从上传一路存到读回")
	}

	rows, err := repo.pgQueries.ListPublishedNarrativeChunks(ctx,
		pggen.ListPublishedNarrativeChunksParams{
			DocumentID: "doc-narr", DocumentVersion: 1, ChunkIndex: -1, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("没有已发布的片段")
	}
	normalized := []rune(body)
	chapters := map[int]bool{}
	for i, row := range rows {
		meta, err := decodeNarrativeMetadata(row.NarrativeMetadata)
		if err != nil {
			t.Fatalf("第 %d 块元数据解码失败：%v", i, err)
		}
		if meta == nil {
			t.Fatalf("第 %d 块没有元数据——开关多半在中途被丢掉了", i)
		}
		if meta.ChapterNumber != nil {
			chapters[*meta.ChapterNumber] = true
		}
		if err := validateNarrativeMetadata(*meta, len([]rune(row.Content))); err != nil {
			t.Errorf("第 %d 块元数据不合法：%v", i, err)
		}
		// ⭐ 真正的验收：区间必须能把原文切回来。
		content := []rune(row.Content)
		for _, seg := range meta.Segments {
			if seg.DocumentStart == nil {
				continue
			}
			want := strings.Join(strings.Fields(string(content[seg.ChunkStart:seg.ChunkEnd])), "")
			gotText := strings.Join(strings.Fields(string(normalized[*seg.DocumentStart:*seg.DocumentEnd])), "")
			if want != gotText {
				t.Errorf("第 %d 块的区间 [%d,%d) 指错了：\n got=%.40q\nwant=%.40q",
					i, *seg.DocumentStart, *seg.DocumentEnd, gotText, want)
			}
		}
	}
	if !chapters[1] || !chapters[2] {
		t.Errorf("落库的章节号不全：%v", chapters)
	}
}

// TestNonNarrativeDocumentStillProcessesIdentically——同一份文本、开关关闭，
// 必须走旧的 txt 路径且**一条元数据都不落**。
// ⚠️ 这是 T013 的回归基线：存量文档全是这条路径。
func TestNonNarrativeDocumentStillProcessesIdentically(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-plain", "m3", "u1", true)
	body := "第一章　初见\n" + strings.Repeat("甲的正文。", 30) + "\n"
	path := filepath.Join(dir, "plain.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-plain", KnowledgeBaseID: "kb-plain",
		FileName: "plain.txt", FileType: FileTypeTxt, FileSize: len(body),
		StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, "doc-plain", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	var withMeta int
	if err := repo.pgdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks WHERE document_id='doc-plain' AND narrative_metadata IS NOT NULL`,
	).Scan(&withMeta); err != nil {
		t.Fatal(err)
	}
	if withMeta != 0 {
		t.Errorf("关闭模式下有 %d 个片段落了叙事元数据", withMeta)
	}
}

// --- 010 T035：上传时就开启抽取 ---

// TestUploadWithExtractionCreatesARunnableIntent——⭐ 上传开关必须真的
// 建出一个**恢复扫描捡得到**的作业意图。
//
// ⚠️ 只把 is_relation_extraction_enabled 存下来是不够的，而这正是这个开关
// 上线前一直被硬拒绝的理由：恢复扫描只捡**已存在**的作业，不会替一份
// 光有开关的文档凭空造一个。少了这一步，文档带着一个"已开启"的开关停在
// 那里，界面显示已开启，而永远不会有任何进度——没有报错，什么都不发生。
func TestUploadWithExtractionCreatesARunnableIntent(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	if _, err := repo.db.ExecContext(ctx, `INSERT INTO documents
		(id, knowledge_base_id, file_name, file_type, file_size, storage_path,
		 status, chunk_count, created_by, is_narrative, is_relation_extraction_enabled)
		VALUES ('doc-up1','kb-x','novel.txt','txt',1,'/tmp/n','pending',0,'u1',1,1)`); err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: "doc-up1", KnowledgeBaseID: "kb-x"}
	if err := repo.createUploadExtractionIntent(ctx, doc, "m-1"); err != nil {
		t.Fatalf("createUploadExtractionIntent: %v", err)
	}

	// 文档必须指向这个作业。⚠️ 不指的话，用户之后手动开启会看到
	// "没有作业"，于是再建一个 run_number=1 的——撞唯一键，用户看到 500。
	var jobID string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT active_relation_job_id FROM documents WHERE id='doc-up1'`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if jobID == "" {
		t.Fatal("上传建了作业却没让文档指向它")
	}

	var state string
	var initComplete bool
	if err := repo.db.QueryRowContext(ctx,
		`SELECT state, initialization_complete FROM relation_extraction_jobs WHERE id=?`,
		jobID).Scan(&state, &initComplete); err != nil {
		t.Fatal(err)
	}
	// ⭐ 必须是 pending 且未初始化：文档这会儿还在排队解析，items 由恢复
	// 扫描在 ready 之后补上。写成 running 会让状态接口报告一个正在跑、
	// 而实际上没有任何 item 的作业。
	if state != jobStatePending || initComplete {
		t.Errorf("state=%q initialization_complete=%v，want pending/false", state, initComplete)
	}

	// ⭐ 最要紧的一格：恢复扫描确实捡得到它。
	jobs, err := repo.queries.ListRecoverableExtractionJobs(ctx,
		gen.ListRecoverableExtractionJobsParams{
			LeaseUntil: sql.NullTime{Time: time.Now(), Valid: true}, ID: "", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, j := range jobs {
		if j.ID == jobID {
			found = true
		}
	}
	if !found {
		t.Error("上传建出来的作业意图不在恢复扫描的范围里——它永远不会开始")
	}
}

// TestUploadWithExtractionGoesThroughTheServicePath——⭐ 上面那一格直接调的是
// repository，这一格走**真实的 UploadDocumentWithOptions**。
//
// ⚠️ 变异测试逼出来的：把 service 里那次 createUploadExtractionIntent 整个
// 删掉，上面那格照样通过——它证明的是"这个函数能建意图"，
// 而不是"上传时真的会调它"。而没人调它的表现正是这个开关最怕的那件事：
// 文档带着"已开启"的开关，永远没有作业。
func TestUploadWithExtractionGoesThroughTheServicePath(t *testing.T) {
	repo := extractionRepo(t)
	ctx := t.Context()
	client := newTestAsynqClient(t)
	seedKB(t, repo, "kb-upsvc", "m3", "u1", true)
	svc := NewService(repo, newFakeProvider(), client, t.TempDir(),
		false, "", 1500*time.Millisecond, false)

	doc, err := svc.UploadDocumentWithOptions(ctx, "kb-upsvc", "u1", user.RoleAdmin,
		"novel.txt", FileTypeTxt, []byte("第一章　甲\n正文。\n"),
		UploadOptions{Narrative: true, RelationExtraction: true, RelationModelID: "m-1"})
	if err != nil {
		t.Fatalf("UploadDocumentWithOptions: %v", err)
	}
	if !doc.IsRelationExtractionEnabled {
		t.Fatal("文档上的开关没落下")
	}

	var jobID string
	var state string
	if err := repo.db.QueryRowContext(ctx,
		`SELECT j.id, j.state FROM documents d
		 JOIN relation_extraction_jobs j ON j.id = d.active_relation_job_id
		 WHERE d.id = ?`, doc.ID).Scan(&jobID, &state); err != nil {
		t.Fatalf("上传勾了抽取，却没有一个被文档指向的作业：%v", err)
	}
	if state != jobStatePending {
		t.Errorf("state = %q, want pending", state)
	}

	// ⭐ 没勾抽取时**一个作业都不能建**——这是"零值等于改动前行为"那条
	// 硬要求在这条路径上的具体形式。
	plain, err := svc.UploadDocumentWithOptions(ctx, "kb-upsvc", "u1", user.RoleAdmin,
		"plain.txt", FileTypeTxt, []byte("正文。\n"), UploadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := repo.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relation_extraction_jobs WHERE document_id=?`, plain.ID).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("没勾抽取却建了 %d 个作业", n)
	}
}
