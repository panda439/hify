package knowledge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"hify/internal/platform/jwt"
)

// extraction_ops_test.go 走真实 HTTP，守抽取的控制操作（010 T029/T030）。
//
// ⭐ 重心在两件事上：
//  1. **幂等键的粒度是"这一次用户动作"**。追加额度是累加的——按状态判重
//     （"现在是 paused，那就恢复"）会让一次网络重试多加一份额度，
//     而两个响应都显示成功;
//  2. **路径上的 kb 不是授权依据**。知道文档 ID 的人换一个自己有权限的 kb
//     放进路径，两边的 ID 都合法，任何一层单独看都没有问题。

const opsSecret = "extraction-ops-secret"

type opsFixture struct {
	svc    Service
	repo   *Repository
	server *httptest.Server
	token  string
	kbID   string
	docID  string
	jobID  string
}

func setupOpsFixture(t *testing.T, kbID string, chapters int) *opsFixture {
	t.Helper()
	repo := setupIntegration(t)
	svc := startTestService(t, repo, "qwen2.5:14b")
	seedKB(t, repo, kbID, "m3", "u1", true)
	if _, err := repo.db.ExecContext(t.Context(),
		"UPDATE knowledge_bases SET chunk_size=200,chunk_overlap=0 WHERE id=?", kbID); err != nil {
		t.Fatal(err)
	}
	doc, err := svc.UploadDocumentWithOptions(t.Context(), kbID, "u1", "member",
		"novel.txt", FileTypeTxt, narrativeUpload(chapters),
		UploadOptions{Narrative: true, RelationExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(t.Context(), doc.ID, doc.Version); err != nil {
		t.Fatal(err)
	}
	ready, err := svc.GetDocument(t.Context(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	RegisterRoutes(router.Group("/api/v1"), NewHandler(svc), opsSecret)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	token, err := jwt.Issue(opsSecret, jwt.Claims{UserID: "u1", Role: "member"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return &opsFixture{svc: svc, repo: repo, server: server, token: token,
		kbID: kbID, docID: doc.ID, jobID: ready.ActiveRelationJobID}
}

func (f *opsFixture) call(t *testing.T, method, path string, body any) (int, extractionStatusResponse, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), method, f.server.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var st extractionStatusResponse
	_ = json.Unmarshal(raw, &st) // 错误响应不是这个结构，交给调用方看 raw
	return res.StatusCode, st, raw
}

func (f *opsFixture) extractionPath(suffix string) string {
	return "/api/v1/knowledge-bases/" + f.kbID + "/documents/" + f.docID + "/extraction" + suffix
}

// TestExtractionStatusReportsUnknownAsNull：还没初始化完成时 total_items
// 必须是 null。⚠️ 0/0 的进度条看起来像"跑完了，什么都没有"。
func TestExtractionStatusReportsUnknownAsNull(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-status", 3)
	code, st, raw := f.call(t, http.MethodGet, f.extractionPath(""), nil)
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, raw)
	}
	if !st.Enabled || st.JobID == "" {
		t.Fatalf("状态没反映出已开启：%s", raw)
	}
	if st.TotalItems == nil || *st.TotalItems != 3 {
		t.Errorf("total_items=%v，语料是 3 章", st.TotalItems)
	}
	if st.CostAmount != nil {
		t.Errorf("本地模型没有金钱计费，cost_amount 必须是 null，得到 %v", *st.CostAmount)
	}
	if st.CostKind != costKindNotApplicable {
		t.Errorf("cost_kind=%q", st.CostKind)
	}
	if st.RemainingChunks == nil || *st.RemainingChunks != 3 {
		t.Errorf("remaining_chunks=%v", st.RemainingChunks)
	}

	// 初始化未完成的作业：总数未知，报 null 而不是 0。
	if _, err := f.repo.db.ExecContext(t.Context(),
		`UPDATE relation_extraction_jobs SET initialization_complete=0 WHERE id=?`, f.jobID); err != nil {
		t.Fatal(err)
	}
	_, st, raw = f.call(t, http.MethodGet, f.extractionPath(""), nil)
	if st.TotalItems != nil {
		t.Errorf("初始化没完成却报了 total_items=%d：%s", *st.TotalItems, raw)
	}
}

// TestPauseThenResumeIsIdempotentPerKey 是这一组里最重要的用例：
// 同一个键重放不能把额度再加一遍。
func TestPauseThenResumeIsIdempotentPerKey(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-idem", 2)

	code, st, raw := f.call(t, http.MethodPost, f.extractionPath("/pause"),
		map[string]any{"idempotency_key": "op-pause-1"})
	if code != http.StatusOK || st.State != jobStatePaused {
		t.Fatalf("pause: code=%d state=%q body=%s", code, st.State, raw)
	}
	if st.StopReason != stopReasonUserPaused {
		t.Errorf("stop_reason=%q", st.StopReason)
	}

	resume := map[string]any{"idempotency_key": "op-resume-1", "additional_calls": 500}
	code, st, raw = f.call(t, http.MethodPost, f.extractionPath("/resume"), resume)
	if code != http.StatusAccepted || st.State != jobStateRunning {
		t.Fatalf("resume: code=%d state=%q body=%s", code, st.State, raw)
	}
	afterFirst := st.RemainingCalls

	// 同键重放：额度**不能**再加一次。
	code, st, raw = f.call(t, http.MethodPost, f.extractionPath("/resume"), resume)
	if code != http.StatusAccepted {
		t.Fatalf("重放 resume: code=%d body=%s", code, raw)
	}
	if st.RemainingCalls != afterFirst {
		t.Errorf("同一个键重放又加了额度：%d -> %d", afterFirst, st.RemainingCalls)
	}

	// 同键不同体：409，不能静默按新的来。
	code, _, raw = f.call(t, http.MethodPost, f.extractionPath("/resume"),
		map[string]any{"idempotency_key": "op-resume-1", "additional_calls": 999})
	if code != http.StatusConflict {
		t.Fatalf("同键异体 code=%d body=%s", code, raw)
	}
}

// TestResumeRequiresPausedAndBudget：状态不对和没额度是两种不同的拒绝，
// 用户要做的事完全不同。
func TestResumeRequiresPausedAndBudget(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-resume", 2)

	// running 的作业不能 resume。
	code, _, raw := f.call(t, http.MethodPost, f.extractionPath("/resume"),
		map[string]any{"idempotency_key": "r1"})
	if code != http.StatusConflict {
		t.Fatalf("running 状态下 resume code=%d body=%s", code, raw)
	}

	// 暂停之后，额度耗尽且不追加 → 另一种 409。
	if code, _, raw = f.call(t, http.MethodPost, f.extractionPath("/pause"),
		map[string]any{"idempotency_key": "p1"}); code != http.StatusOK {
		t.Fatalf("pause code=%d body=%s", code, raw)
	}
	if _, err := f.repo.db.ExecContext(t.Context(),
		`UPDATE relation_extraction_jobs SET reserved_calls = call_limit WHERE id=?`, f.jobID); err != nil {
		t.Fatal(err)
	}
	code, _, raw = f.call(t, http.MethodPost, f.extractionPath("/resume"),
		map[string]any{"idempotency_key": "r2"})
	if code != http.StatusConflict {
		t.Fatalf("没额度 resume code=%d body=%s", code, raw)
	}
	// 追加额度之后同一个动作就能过。
	code, st, raw := f.call(t, http.MethodPost, f.extractionPath("/resume"),
		map[string]any{"idempotency_key": "r3", "additional_calls": 10})
	if code != http.StatusAccepted || st.State != jobStateRunning {
		t.Fatalf("追加额度后 resume code=%d state=%q body=%s", code, st.State, raw)
	}
}

// TestDisableThenEnableDoesNotAutoRun：enable 不自动恢复一个暂停的作业。
// ⚠️ 自动恢复的话，"我明明停了它"和"它又开始花钱"会同时成立。
func TestDisableThenEnableDoesNotAutoRun(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-enable", 2)

	code, st, raw := f.call(t, http.MethodPost, f.extractionPath("/disable"),
		map[string]any{"idempotency_key": "d1"})
	if code != http.StatusOK || st.Enabled {
		t.Fatalf("disable code=%d enabled=%v body=%s", code, st.Enabled, raw)
	}
	if st.State != jobStatePaused || st.StopReason != stopReasonUserDisabled {
		t.Errorf("disable 之后 state=%q stop_reason=%q", st.State, st.StopReason)
	}
	// 关掉之后不能直接 resume（契约：必须先 enable）。
	if code, _, raw = f.call(t, http.MethodPost, f.extractionPath("/resume"),
		map[string]any{"idempotency_key": "d-r1"}); code != http.StatusConflict {
		t.Fatalf("disabled 状态下 resume code=%d body=%s", code, raw)
	}

	code, st, raw = f.call(t, http.MethodPost, f.extractionPath("/enable"),
		map[string]any{"idempotency_key": "e1"})
	if code != http.StatusAccepted || !st.Enabled {
		t.Fatalf("enable code=%d enabled=%v body=%s", code, st.Enabled, raw)
	}
	if st.State != jobStatePaused {
		t.Errorf("enable 把暂停的作业自动跑起来了：state=%q", st.State)
	}
	// enable 之后 resume 才被允许。
	if code, st, raw = f.call(t, http.MethodPost, f.extractionPath("/resume"),
		map[string]any{"idempotency_key": "e-r1", "additional_calls": 5}); code != http.StatusAccepted {
		t.Fatalf("enable 后 resume code=%d body=%s", code, raw)
	}
	if st.State != jobStateRunning {
		t.Errorf("resume 之后 state=%q", st.State)
	}
}

// TestRestartSupersedesTheOldRunAndKeepsItsLedger：旧 run 的账目必须留着。
// ⚠️ 抹掉的话，"这本书一共花了多少"会系统性偏小。
func TestRestartSupersedesTheOldRunAndKeepsItsLedger(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-restart", 2)
	// 给旧 run 造一点账目。
	if _, err := f.repo.db.ExecContext(t.Context(),
		`UPDATE relation_extraction_jobs SET confirmed_dispatches=7, active_ms_used=1234 WHERE id=?`,
		f.jobID); err != nil {
		t.Fatal(err)
	}

	code, st, raw := f.call(t, http.MethodPost, f.extractionPath("/restart"),
		map[string]any{"idempotency_key": "rs1", "model_id": "qwen2.5:7b"})
	if code != http.StatusAccepted {
		t.Fatalf("restart code=%d body=%s", code, raw)
	}
	if st.JobID == f.jobID {
		t.Fatal("restart 没有开出新的 run")
	}
	if st.RunNumber != 2 {
		t.Errorf("run_number=%d, want 2", st.RunNumber)
	}
	if st.ModelID != "qwen2.5:7b" {
		t.Errorf("新 run 没有用请求里的模型：%q", st.ModelID)
	}
	// 新 run 的账目是干净的，旧 run 的账目原样保留。
	if st.ConfirmedCalls != 0 {
		t.Errorf("新 run 继承了旧账目：confirmed_calls=%d", st.ConfirmedCalls)
	}
	var oldState string
	var oldCalls int
	if err := f.repo.db.QueryRowContext(t.Context(),
		`SELECT state, confirmed_dispatches FROM relation_extraction_jobs WHERE id=?`,
		f.jobID).Scan(&oldState, &oldCalls); err != nil {
		t.Fatal(err)
	}
	if oldState != jobStateSuperseded {
		t.Errorf("旧 run state=%q", oldState)
	}
	if oldCalls != 7 {
		t.Errorf("旧 run 的账目被动了：confirmed_dispatches=%d", oldCalls)
	}

	// 同一个键重放不再开新 run。
	_, again, raw := f.call(t, http.MethodPost, f.extractionPath("/restart"),
		map[string]any{"idempotency_key": "rs1", "model_id": "qwen2.5:7b"})
	if again.JobID != st.JobID {
		t.Errorf("重放 restart 又开了一个 run：%s -> %s (%s)", st.JobID, again.JobID, raw)
	}
}

// TestPathKnowledgeBaseIsNotAuthorization：路径上的 kb 与文档实际归属不符
// 时必须 404，不能拿路径 kb 当授权依据。
func TestPathKnowledgeBaseIsNotAuthorization(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-auth", 1)
	// 另一个用户自己的知识库——他对它有全部权限。
	seedKB(t, f.repo, "kb-ops-auth-other", "m3", "u1", true)

	path := "/api/v1/knowledge-bases/kb-ops-auth-other/documents/" + f.docID + "/extraction"
	code, _, raw := f.call(t, http.MethodGet, path, nil)
	if code != http.StatusNotFound {
		t.Fatalf("换个 kb 就能读别人文档的状态：code=%d body=%s", code, raw)
	}
	code, _, raw = f.call(t, http.MethodPost, path+"/pause",
		map[string]any{"idempotency_key": "x1"})
	if code != http.StatusNotFound {
		t.Fatalf("换个 kb 就能暂停别人的作业：code=%d body=%s", code, raw)
	}
}

// TestOperationKeyGuards：键必填、不超长、不能用系统保留前缀。
func TestOperationKeyGuards(t *testing.T) {
	f := setupOpsFixture(t, "kb-ops-keys", 1)
	for _, tc := range []struct {
		name string
		key  string
		want int
	}{
		{"空键", "", http.StatusBadRequest},
		{"保留前缀", autoOperationKeyPrefix + "1", http.StatusBadRequest},
		{"超长", string(bytes.Repeat([]byte("k"), maxIdempotencyKeyLen+1)), http.StatusBadRequest},
		{"正常", "ok-1", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, raw := f.call(t, http.MethodPost, f.extractionPath("/pause"),
				map[string]any{"idempotency_key": tc.key})
			if code != tc.want {
				t.Fatalf("code=%d want %d body=%s", code, tc.want, raw)
			}
		})
	}
}
