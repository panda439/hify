package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"hify/internal/platform/apperr"
)

// 003-retrieval-playground：试检索端点的集成测试。
//
// 这些用例走**真实的 gin handler + 真实 Service + 真实 PostgreSQL**，
// 而不是直接调 Service.Retrieve——本期新增的全部代码都在 handler/dto 这一层，
// 绕过它就等于什么都没测。

// doRetrieve 把一次 POST /knowledge-bases/:id/retrieve 打进 handler，
// 返回状态码与原始响应体。
func doRetrieve(t *testing.T, svc Service, kbID, body string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := NewHandler(svc)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: kbID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	// 生产路径上 httperr.Wrap 负责把 error 转成响应体；这里直接调 handler 并
	// 自己断言返回的 error，能更精确地区分"哪个哨兵错误"，而不是只看状态码。
	if err := h.Retrieve(c); err != nil {
		var appErr *apperr.AppError
		if errors.As(err, &appErr) {
			return statusForKind(appErr.Kind), appErr.Code
		}
		return http.StatusInternalServerError, err.Error()
	}
	return rec.Code, rec.Body.String()
}

// statusForKind 只覆盖本文件用得到的两种，避免为测试引入对 httperr 内部映射的依赖。
func statusForKind(kind apperr.Kind) int {
	switch kind {
	case apperr.KindNotFound:
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}

func decodeRetrieve(t *testing.T, body string) retrieveResponse {
	t.Helper()
	var resp retrieveResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n%s", err, body)
	}
	return resp
}

// seedProbeKB 用测试自己的名字派生出唯一的 KB / 文档 / chunk ID。
// 同一次 go test run 里测试库不会在用例之间清空，所以每个用例必须用自己的
// 一套 ID——这也是本包既有集成用例的惯例（kb-search / kb-dedup-e2e ……）。
// 返回 (kbID, docA, docB)。
func seedProbeKB(t *testing.T, repo *Repository) (string, string, string) {
	t.Helper()
	// 用测试名的短哈希而不是测试名本身：knowledge_bases.id 是 CHAR(36)
	// （见 internal/db/CLAUDE.md 的建表约定），测试名拼上前缀会超长。
	h := fnv.New32a()
	_, _ = h.Write([]byte(t.Name()))
	suffix := strconv.FormatUint(uint64(h.Sum32()), 36)
	kbID := "kb-probe-" + suffix
	docA, docB := "doc-probe-a-"+suffix, "doc-probe-b-"+suffix

	seedKB(t, repo, kbID, "m3", "u1", true)
	p12 := 12
	seedNeighborChunkBatch(t, repo, kbID, docA, 1, []neighborSeedChunk{
		{ID: "pb-a1-" + suffix, ChunkIndex: 0, Content: "文档A：PROBETOKEN 出现在这里", Vec: []float32{1, 0, 0},
			DocumentName: "手册A.pdf", PageNumber: &p12},
	}, true)
	seedNeighborChunkBatch(t, repo, kbID, docB, 1, []neighborSeedChunk{
		{ID: "pb-b1-" + suffix, ChunkIndex: 0, Content: "文档B：PROBETOKEN 也在这里", Vec: []float32{1, 0.1, 0},
			DocumentName: "手册B.txt"},
	}, true)
	return kbID, docA, docB
}

func TestRetrieveHandlerWithoutFilterReturnsBothDocuments(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	kbID, _, _ := seedProbeKB(t, repo)

	code, body := doRetrieve(t, svc, kbID, `{"query":"PROBETOKEN","top_k":5}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	resp := decodeRetrieve(t, body)
	if resp.FilterApplied {
		t.Fatal("没传任何过滤条件时 filter_applied 必须是 false")
	}
	if resp.HitCount != 2 {
		t.Fatalf("want 2 条命中（A、B 各一），got %d：%s", resp.HitCount, body)
	}
	// 文档名必须回传，前端要显示它而不是 ID（FR-009）。
	for _, c := range resp.Chunks {
		if c.DocumentName == "" {
			t.Fatalf("片段 %s 没有 document_name，前端只能显示 ID", c.ID)
		}
	}
}

func TestRetrieveHandlerFilterByDocument(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	kbID, docA, _ := seedProbeKB(t, repo)

	code, body := doRetrieve(t, svc, kbID,
		`{"query":"PROBETOKEN","top_k":5,"document_ids":["`+docA+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	resp := decodeRetrieve(t, body)
	if !resp.FilterApplied {
		t.Fatal("传了 document_ids，filter_applied 必须是 true")
	}
	if len(resp.Chunks) == 0 {
		t.Fatalf("限定到 doc-probe-a 后结果为空：%s", body)
	}
	for _, c := range resp.Chunks {
		if c.DocumentID != docA {
			t.Fatalf("结果里混入了 %s 的片段：%s", c.DocumentID, body)
		}
	}
}

func TestRetrieveHandlerFilterByPageRange(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	kbID, _, docB := seedProbeKB(t, repo)

	// [10,15] 含第 12 页：A 命中；B 是 txt、没有页码，必须被挡住。
	code, body := doRetrieve(t, svc, kbID,
		`{"query":"PROBETOKEN","top_k":5,"page_min":10,"page_max":15}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	resp := decodeRetrieve(t, body)
	for _, c := range resp.Chunks {
		if c.DocumentID == docB {
			t.Fatalf("无页码的 txt 片段不该通过页码过滤：%s", body)
		}
	}
	if resp.HitCount != 1 {
		t.Fatalf("want 1 条命中（第 12 页的 A），got %d：%s", resp.HitCount, body)
	}
	// page_number 必须如实回传，前端要显示它。
	if resp.Chunks[0].PageNumber == nil || *resp.Chunks[0].PageNumber != 12 {
		t.Fatalf("page_number 应为 12，got %v", resp.Chunks[0].PageNumber)
	}
}

// TestRetrieveHandlerSerializesMissingPageAsNull：没有页码的片段必须序列化成
// JSON null，而不是 0——0 是一个被编造出来的页码（页码 1-indexed）。
func TestRetrieveHandlerSerializesMissingPageAsNull(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	kbID, _, docB := seedProbeKB(t, repo)

	code, body := doRetrieve(t, svc, kbID,
		`{"query":"PROBETOKEN","top_k":5,"document_ids":["`+docB+`"]}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	if !strings.Contains(body, `"page_number":null`) {
		t.Fatalf("无页码的片段必须序列化成 null（不是 0）：%s", body)
	}
}

// TestRetrieveHandlerPropagatesFilterErrors —— FR-006：002 的三个过滤错误
// 必须原样传达，不得被吞掉或降级成空结果。
func TestRetrieveHandlerPropagatesFilterErrors(t *testing.T) {
	repo := setupIntegration(t)
	kbID, docA, _ := seedProbeKB(t, repo)

	tooMany := make([]string, maxFilterDocumentIDs+1)
	for i := range tooMany {
		tooMany[i] = `"d"`
	}

	for _, tc := range []struct {
		name     string
		filterOn bool
		body     string
		wantCode string
	}{
		{"文档数超上限", true,
			`{"query":"q","document_ids":[` + strings.Join(tooMany, ",") + `]}`,
			ErrTooManyFilterDocuments.Code},
		{"页码起止颠倒", true,
			`{"query":"q","page_min":9,"page_max":2}`, ErrInvalidPageRange.Code},
		{"页码为零", true,
			`{"query":"q","page_min":0}`, ErrInvalidPageRange.Code},
		{"开关未启用但传了过滤条件", false,
			`{"query":"q","document_ids":["` + docA + `"]}`, ErrMetadataFilterDisabled.Code},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var svc Service
			if tc.filterOn {
				svc = newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
			} else {
				svc = newTestService(repo, newFakeProvider(), t.TempDir())
			}
			code, got := doRetrieve(t, svc, kbID, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("状态码 = %d, want 400；返回 %s", code, got)
			}
			if got != tc.wantCode {
				t.Fatalf("错误码 = %s, want %s —— 过滤错误必须原样传达，不能被吞掉或降级成空结果", got, tc.wantCode)
			}
		})
	}
}

// TestRetrieveHandlerEmptyFilterUnaffectedByToggle：开关关闭时，**空**过滤器
// 必须照常工作（002 的 FR-006/FR-013）。面板在默认配置下至少要能做无过滤检索。
func TestRetrieveHandlerEmptyFilterUnaffectedByToggle(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir()) // 开关关闭
	kbID, _, _ := seedProbeKB(t, repo)

	code, body := doRetrieve(t, svc, kbID, `{"query":"PROBETOKEN","top_k":5}`)
	if code != http.StatusOK {
		t.Fatalf("开关关闭 + 空过滤器必须正常返回，got %d：%s", code, body)
	}
	if decodeRetrieve(t, body).HitCount == 0 {
		t.Fatalf("want 有命中，got 空：%s", body)
	}
}

func TestRetrieveHandlerRejectsBadRequests(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	kbID, _, _ := seedProbeKB(t, repo)

	for _, tc := range []struct{ name, body string }{
		{"问题缺失", `{"top_k":5}`},
		{"问题为空串", `{"query":""}`},
		{"问题只有空白", `{"query":"   "}`},
		{"请求体不是合法 JSON", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, got := doRetrieve(t, svc, kbID, tc.body)
			if code != http.StatusBadRequest || got != ErrInvalidRequest.Code {
				t.Fatalf("want 400 %s，got %d %s", ErrInvalidRequest.Code, code, got)
			}
		})
	}
}

// TestRetrieveHandlerUnknownKnowledgeBaseIs404：对不存在的知识库探测必须是
// 404，而不是空结果。Service.Retrieve 自己会把未知 KB 当成"不贡献候选"
// （那对 conversation 是对的——一个被删的知识库不该让整轮对话失败），
// 但对试检索面板来说，空结果会被用户读成"没匹配到"，掩盖掉真正的问题。
func TestRetrieveHandlerUnknownKnowledgeBaseIs404(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	seedProbeKB(t, repo)

	code, got := doRetrieve(t, svc, "kb-does-not-exist", `{"query":"PROBETOKEN"}`)
	if code != http.StatusNotFound || got != ErrNotFound.Code {
		t.Fatalf("want 404 %s，got %d %s", ErrNotFound.Code, code, got)
	}
}

// TestRetrieveHandlerMarksNeighborChunks —— US4/FR-011：邻接块必须能被前端认出来，
// 否则"页码范围外的片段出现在结果里"看起来就是个 bug。
func TestRetrieveHandlerMarksNeighborChunks(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	seedKB(t, repo, "kb-probe-nb", "m3", "u1", true)
	p1, p2 := 1, 2
	seedNeighborChunkBatch(t, repo, "kb-probe-nb", "doc-probe-nb", 1, []neighborSeedChunk{
		{ID: "pbn-prev", ChunkIndex: 0, Content: "第一页：答案的前半句", Vec: []float32{0, 1, 0},
			DocumentName: "手册.pdf", PageNumber: &p1},
		{ID: "pbn-anchor", ChunkIndex: 1, Content: "第二页：命中的那一句", Vec: []float32{1, 0, 0},
			DocumentName: "手册.pdf", PageNumber: &p2},
	}, true)

	code, body := doRetrieve(t, svc, "kb-probe-nb",
		`{"query":"查询","top_k":5,"page_min":2,"page_max":2}`)
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	resp := decodeRetrieve(t, body)

	var anchor, neighbor *chunkResult
	for i := range resp.Chunks {
		switch resp.Chunks[i].ID {
		case "pbn-anchor":
			anchor = &resp.Chunks[i]
		case "pbn-prev":
			neighbor = &resp.Chunks[i]
		}
	}
	if anchor == nil || anchor.IsNeighbor {
		t.Fatalf("pbn-anchor 应作为命中返回（is_neighbor=false）：%s", body)
	}
	if neighbor == nil {
		t.Fatalf("第 1 页的邻接块应当出现（邻接块豁免页码过滤，002 FR-011）：%s", body)
	}
	if !neighbor.IsNeighbor || neighbor.NeighborOf != "pbn-anchor" {
		t.Fatalf("pbn-prev 必须标记成 pbn-anchor 的邻接块，got is_neighbor=%v neighbor_of=%q",
			neighbor.IsNeighbor, neighbor.NeighborOf)
	}
	if resp.HitCount != 1 || resp.NeighborCount != 1 {
		t.Fatalf("want hit_count=1 neighbor_count=1，got %d/%d", resp.HitCount, resp.NeighborCount)
	}
}

// --- 007-document-processing-notice：文档列表端点的提示字段（契约 §1） ---

// doListDocuments 走真实的 ListDocuments handler，返回状态码与响应体。
func doListDocuments(t *testing.T, svc Service, kbID string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	h := NewHandler(svc)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: kbID}}
	c.Request = httptest.NewRequest(http.MethodGet, "/?limit=50", nil)

	if err := h.ListDocuments(c); err != nil {
		t.Fatalf("ListDocuments handler: %v", err)
	}
	return rec.Code, rec.Body.String()
}

// TestListDocumentsEndpointCarriesUnextractedPages 走**真实的 HTTP handler**
// 断言契约 §1 的形状。
//
// ⚠️ 为什么不满足于服务层的用例：用户看到提示的路径是
// handler → dto → JSON → 前端，中间任何一环把字段掉了，服务层的断言都照样绿。
// 尤其 toDocumentResponse 是手写的字段拷贝，漏一行不会有任何编译错误。
//
// 同时锁定 C2：没有提示时序列化成 **null**，不是 `[]`——给一个事实两种表示，
// 每个下游都得处理两种，或者更常见地，只处理一种然后在另一种上悄悄出错。
func TestListDocumentsEndpointCarriesUnextractedPages(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	path := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"",
		"pagethree body text standing alone.",
	))
	seedDocForProcessing(t, repo, "kb-http-notice", "doc-http-notice", "contract.pdf", FileTypePDF, path)
	if err := svc.ProcessDocument(ctx, "doc-http-notice", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	code, body := doListDocuments(t, svc, "kb-http-notice")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	if !strings.Contains(body, `"unextracted_pages":[2]`) {
		t.Fatalf("响应体里没有带回 unextracted_pages:[2]——"+
			"handler/dto 这一段把字段掉了，服务层的断言看不到这个。响应：%s", body)
	}

	// 对照：没有缺页的文档必须序列化成 null，而不是 []。
	txtPath := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(txtPath, []byte("一段普通正文。\n\n另一段正文。"), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDocForProcessing(t, repo, "kb-http-nonotice", "doc-http-nonotice", "plain.txt", FileTypeTxt, txtPath)
	if err := svc.ProcessDocument(ctx, "doc-http-nonotice", 1); err != nil {
		t.Fatalf("ProcessDocument(txt): %v", err)
	}
	code, body = doListDocuments(t, svc, "kb-http-nonotice")
	if code != http.StatusOK {
		t.Fatalf("状态码 %d，响应 %s", code, body)
	}
	if !strings.Contains(body, `"unextracted_pages":null`) {
		t.Fatalf("无提示时应当序列化成 null 而不是 []（契约 C2）。响应：%s", body)
	}
}
