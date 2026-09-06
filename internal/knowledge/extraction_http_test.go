package knowledge

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"net/http/httptest"

	"hify/internal/platform/apperr"
	"hify/internal/user"
)

// extraction_http_test.go 守抽取操作接口（010 T029）。
//
// ⭐ 这一层最容易被忽略的两件事：
//   - **路径归属**：handler 必须核对 :docId 真的属于 :id 那个知识库，
//     不能拿请求里的 kbId 直接当授权依据。否则知道文档 ID 的人可以借一个
//     自己有权限的知识库去操作别人的文档，而每一步鉴权看起来都做了。
//   - **幂等键**：同键重放返回原结果、同键异 body 返回 409。
//     不管的话，用户手抖点两次"重新开始"就会开出两个 run 同时花钱。

func doExtraction(t *testing.T, svc Service, method, kbID, docID, action, body, userID, role string) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandler(svc)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: kbID}, {Key: "docId", Value: docID}}
	c.Request = httptest.NewRequest(method, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("user_id", userID)
	c.Set("role", role)

	var err error
	switch action {
	case "":
		err = h.GetExtractionStatus(c)
	case "enable":
		err = h.EnableExtraction(c)
	case "disable":
		err = h.DisableExtraction(c)
	case "pause":
		err = h.PauseExtraction(c)
	case "resume":
		err = h.ResumeExtraction(c)
	case "restart":
		err = h.RestartExtraction(c)
	default:
		t.Fatalf("unknown action %q", action)
	}
	if err != nil {
		var appErr *apperr.AppError
		if errorsAs(err, &appErr) {
			// ⚠️ 不用 retrieve_handler_test.go 里那个 statusForKind：
			// 它只区分 not_found，其余一律 400——那对它自己的用例够用，
			// 但这里要验的正是 403 与 409，用它会让每条断言都对着 400 比。
			return fullStatusForKind(appErr.Kind), appErr.Code
		}
		return http.StatusInternalServerError, err.Error()
	}
	return rec.Code, rec.Body.String()
}

// fullStatusForKind 是与生产 httperr 一致的完整映射。
func fullStatusForKind(kind apperr.Kind) int {
	switch kind {
	case apperr.KindNotFound:
		return http.StatusNotFound
	case apperr.KindConflict:
		return http.StatusConflict
	case apperr.KindForbidden:
		return http.StatusForbidden
	case apperr.KindUnauthorized:
		return http.StatusUnauthorized
	case apperr.KindRateLimited:
		return http.StatusTooManyRequests
	default:
		return http.StatusBadRequest
	}
}

func errorsAs(err error, target **apperr.AppError) bool {
	for err != nil {
		if e, ok := err.(*apperr.AppError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// extractionFixture 建一份 ready 的叙事文档并返回 kb/doc id。
func extractionFixture(t *testing.T, repo *Repository, docID string) (string, Service) {
	t.Helper()
	seedNarrativeDocument(t, repo, docID, 3)
	if _, err := repo.db.ExecContext(t.Context(),
		`INSERT IGNORE INTO knowledge_bases
		 (id, name, embedding_model_id, chunk_size, chunk_overlap, is_active, created_by)
		 VALUES ('kb-x','小说库','m3',500,50,1,'u1')`); err != nil {
		t.Fatal(err)
	}
	return "kb-x", newTestService(repo, newFakeProvider(), t.TempDir())
}

// TestExtractionRequiresWritePermission——写操作只能创建者或 admin。
func TestExtractionRequiresWritePermission(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h1")

	code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h1", "enable",
		`{"model_id":"m3","idempotency_key":"k1"}`, "someone-else", user.RoleMember)
	if code != http.StatusForbidden {
		t.Errorf("非创建者 code = %d (%s), want 403", code, body)
	}
	// admin 可以。
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h1", "enable",
		`{"model_id":"m3","idempotency_key":"k1"}`, "someone-else", user.RoleAdmin); code >= 400 {
		t.Errorf("admin 被拒：%d %s", code, body)
	}
}

// TestExtractionChecksPathOwnership——⭐ :docId 必须真的属于 :id。
//
// ⚠️ 不核对的话，知道文档 ID 的人可以借一个自己有权限的知识库去操作
// 别人的文档，而每一步鉴权看起来都做了——权限查的是那个"借来的"知识库。
func TestExtractionChecksPathOwnership(t *testing.T) {
	repo := extractionRepo(t)
	_, svc := extractionFixture(t, repo, "doc-h2")
	if _, err := repo.db.ExecContext(t.Context(),
		`INSERT IGNORE INTO knowledge_bases
		 (id, name, embedding_model_id, chunk_size, chunk_overlap, is_active, created_by)
		 VALUES ('kb-other','别人的库','m3',500,50,1,'u1')`); err != nil {
		t.Fatal(err)
	}
	code, body := doExtraction(t, svc, http.MethodPost, "kb-other", "doc-h2", "enable",
		`{"model_id":"m3","idempotency_key":"k1"}`, "u1", user.RoleMember)
	if code != http.StatusNotFound {
		t.Errorf("拿别的知识库去操作这份文档：code = %d (%s), want 404", code, body)
	}
}

// TestExtractionRejectsBadInput——400 的几种。
func TestExtractionRejectsBadInput(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h3")
	for name, tc := range map[string]struct{ body, action string }{
		"缺少幂等键":      {`{"model_id":"m3"}`, "enable"},
		"幂等键过长":      {`{"model_id":"m3","idempotency_key":"` + strings.Repeat("x", 129) + `"}`, "enable"},
		"enable 缺模型": {`{"idempotency_key":"k1"}`, "enable"},
		"不是合法 JSON":  {`{`, "enable"},
		// ⚠️ 用户不得使用 upload: 前缀——那是系统给"上传时就开启"保留的键，
		// 用户占用它会让系统那次自动创建被当成重放而静默跳过。
		"占用保留前缀": {`{"model_id":"m3","idempotency_key":"upload:1"}`, "enable"},
	} {
		code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h3", tc.action,
			tc.body, "u1", user.RoleMember)
		if code != http.StatusBadRequest {
			t.Errorf("「%s」code = %d (%s), want 400", name, code, body)
		}
	}
}

// TestIdempotentReplayReturnsTheSameRun——⭐ 同键重放不开第二个 run。
// ⚠️ 用户手抖点两次"重新开始"就会开出两个 run 同时花钱。
func TestIdempotentReplayReturnsTheSameRun(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h4")
	req := `{"model_id":"m3","idempotency_key":"same-key"}`

	code1, body1 := doExtraction(t, svc, http.MethodPost, kb, "doc-h4", "enable", req, "u1", user.RoleMember)
	if code1 >= 400 {
		t.Fatalf("首次 enable 失败：%d %s", code1, body1)
	}
	code2, body2 := doExtraction(t, svc, http.MethodPost, kb, "doc-h4", "enable", req, "u1", user.RoleMember)
	if code2 >= 400 {
		t.Fatalf("重放失败：%d %s", code2, body2)
	}
	var a, b struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal([]byte(body1), &a)
	_ = json.Unmarshal([]byte(body2), &b)
	if a.JobID == "" || a.JobID != b.JobID {
		t.Errorf("重放开出了不同的 run：%q vs %q", a.JobID, b.JobID)
	}
	if n := countRows(t, repo,
		`SELECT COUNT(*) FROM relation_extraction_jobs WHERE document_id='doc-h4'`); n != 1 {
		t.Errorf("作业 %d 个, want 1", n)
	}
}

// TestSameKeyDifferentBodyIsConflict——⭐ 同键异 body 返回 409。
// ⚠️ 放行的话，第二次请求的参数（比如换了模型）会被静默忽略，
// 用户以为换了模型，实际还在用第一次那个。
func TestSameKeyDifferentBodyIsConflict(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h5")
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h5", "enable",
		`{"model_id":"m3","idempotency_key":"k"}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("首次失败：%d %s", code, body)
	}
	code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h5", "enable",
		`{"model_id":"m-other","idempotency_key":"k"}`, "u1", user.RoleMember)
	if code != http.StatusConflict {
		t.Errorf("同键异 body：code = %d (%s), want 409", code, body)
	}
}

// TestStatusReportsUnknownAsNull——⭐ 未知值返回 null，不填零。
//
// ⚠️ 初始化还没完成时 total_items 填 0，前端会显示"0/0 已完成"——
// 一个看起来已经跑完的进度条，而实际上一条都还没开始。
func TestStatusReportsUnknownAsNull(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h6")
	// 还没开启：状态里没有 job。
	code, body := doExtraction(t, svc, http.MethodGet, kb, "doc-h6", "", "", "u1", user.RoleMember)
	if code != http.StatusOK {
		t.Fatalf("GET 状态失败：%d %s", code, body)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("状态不是 JSON：%v (%s)", err, body)
	}
	if st["enabled"] != false {
		t.Errorf("未开启却报 enabled=%v", st["enabled"])
	}
	for _, k := range []string{"total_items", "succeeded_items", "cost_amount"} {
		v, ok := st[k]
		if !ok {
			t.Errorf("状态里缺字段 %q", k)
			continue
		}
		if v != nil {
			t.Errorf("%s = %v，未知值必须是 null 而不是 0——否则前端会显示一个已经跑完的进度条", k, v)
		}
	}
}

// TestResumeOnDisabledIsConflict——disabled 必须先 enable 再 resume。
func TestResumeOnDisabledIsConflict(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h7")
	code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h7", "resume",
		`{"idempotency_key":"k"}`, "u1", user.RoleMember)
	if code != http.StatusConflict {
		t.Errorf("对未开启的文档 resume：code = %d (%s), want 409", code, body)
	}
}

// TestEnableOnPausedDoesNotAutoRun——⭐ enable 一个已暂停的作业只恢复可见开关，
// **不自动运行**。⚠️ 自动跑起来等于系统替用户推翻了一次显式的暂停。
func TestEnableOnPausedDoesNotAutoRun(t *testing.T) {
	repo := extractionRepo(t)
	kb, svc := extractionFixture(t, repo, "doc-h8")
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h8", "enable",
		`{"model_id":"m3","idempotency_key":"k1"}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("enable 失败：%d %s", code, body)
	}
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h8", "pause",
		`{"idempotency_key":"k2"}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("pause 失败：%d %s", code, body)
	}
	if code, body := doExtraction(t, svc, http.MethodPost, kb, "doc-h8", "enable",
		`{"model_id":"m3","idempotency_key":"k3"}`, "u1", user.RoleMember); code >= 400 {
		t.Fatalf("再 enable 失败：%d %s", code, body)
	}
	var state string
	if err := repo.db.QueryRowContext(t.Context(),
		`SELECT state FROM relation_extraction_jobs WHERE document_id='doc-h8'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != jobStatePaused {
		t.Errorf("enable 把暂停的作业自动跑起来了：state = %q", state)
	}
}
