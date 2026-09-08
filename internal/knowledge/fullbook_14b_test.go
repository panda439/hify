package knowledge

// T042 是唯一允许消耗验收集的门禁。未显式设置 HIFY_FULLBOOK_MODEL 时必须跳过。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"hify/internal/provider"
)

type fullBookModel struct {
	client  *http.Client
	baseURL string
	// rawDir 非空时，**每一次调用一结束就立刻把原始响应写盘**。
	//
	// ⭐ 这是 run2 的教训：原本的归档是跑完之后从 attempts 表统一导出的，
	// 结果测试在 60 分钟墙钟上限处被 SIGINT 打断，导出从未执行——
	// 44 个块的调用记录连同它们的原始响应一起没了（测试库还会被下一次
	// go test 重建，等于二次销毁）。
	//
	// ⚠️ 一次长跑的归档不能依赖"跑完"这个前提：跑得越久越值钱，
	// 而跑得越久被打断的概率也越大。边跑边写才是对的。
	rawDir string
	// seq 给文件名排序，让中断后也能看出调用发生的先后。
	seq int
}

// archive 把一次调用的现场立刻落盘。任何写盘失败都只警告不中断——
// ⚠️ 归档是观测手段，它自己不该有能力让一次真实的抽取跑失败。
func (m *fullBookModel) archive(modelID string, req provider.ChatRequest, res provider.ChatAttemptResult, callErr error) {
	if m.rawDir == "" {
		return
	}
	m.seq++
	prompt := ""
	if len(req.Messages) > 0 {
		prompt = req.Messages[0].Content
	}
	sum := sha256.Sum256([]byte(prompt))
	rec := map[string]any{
		"seq": m.seq, "model": modelID,
		// ⚠️ 只记提示词的摘要和长度，不存正文：语料本身是 gitignore 的，
		// 存一份等于把它复制进版本库。
		"prompt_sha256": hex.EncodeToString(sum[:]),
		"prompt_runes":  len([]rune(prompt)),
		"outcome":       string(res.Outcome),
		"elapsed_ms":    res.ElapsedMs,
		"finish_reason": res.FinishReason,
		"error_code":    res.ErrorCode,
		"usage_known":   res.UsageKnown,
		"input_tokens":  res.InputTokens,
		"output_tokens": res.OutputTokens,
		"content":       res.Message.Content,
		"at":            time.Now().UTC().Format(time.RFC3339),
	}
	if callErr != nil {
		rec["call_error"] = callErr.Error()
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	name := fmt.Sprintf("%04d-%s.json", m.seq, string(res.Outcome))
	if err := os.WriteFile(filepath.Join(m.rawDir, name), append(b, byte('\n')), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "归档写盘失败（不影响本次抽取）: %v\n", err)
	}
}

func (m *fullBookModel) ChatOnce(ctx context.Context, modelID string, req provider.ChatRequest, timeout time.Duration) (res provider.ChatAttemptResult, err error) {
	// ⭐ 单一出口归档：无论走哪条返回路径（超时、传输失败、正常完成），
	// 这一次调用都会被写盘。漏掉任何一条，比率就是错的。
	defer func() { m.archive(modelID, req, res, err) }()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := json.Marshal(struct {
		Model       string             `json:"model"`
		Messages    []provider.Message `json:"messages"`
		Temperature float64            `json:"temperature"`
		MaxTokens   int                `json:"max_tokens"`
	}{modelID, req.Messages, 0, req.MaxTokens})
	if err != nil {
		return provider.ChatAttemptResult{}, err
	}
	start := time.Now()
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.baseURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return provider.ChatAttemptResult{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(r)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return provider.ChatAttemptResult{Outcome: provider.AttemptUnknown, Dispatched: true, ElapsedMs: elapsed, ErrorCode: "http_error", Cause: err}, nil
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return provider.ChatAttemptResult{Outcome: provider.AttemptUnknown, Dispatched: true, ElapsedMs: elapsed, ErrorCode: "read_error", Cause: readErr}, nil
	}
	if resp.StatusCode/100 != 2 {
		return provider.ChatAttemptResult{Outcome: provider.AttemptFailed, Dispatched: true, ElapsedMs: elapsed, ErrorCode: fmt.Sprintf("http_%d", resp.StatusCode), Message: provider.Message{Content: string(raw)}}, nil
	}
	var out struct {
		Choices []struct {
			Message provider.Message `json:"message"`
			Finish  string           `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return provider.ChatAttemptResult{Outcome: provider.AttemptFailed, Dispatched: true, ElapsedMs: elapsed, ErrorCode: "invalid_response", Message: provider.Message{Content: string(raw)}}, nil
	}
	msg := out.Choices[0].Message
	result := provider.ChatAttemptResult{Outcome: provider.AttemptCompleted, Dispatched: true, ElapsedMs: elapsed, Message: msg, FinishReason: out.Choices[0].Finish, UsageKnown: out.Usage != nil}
	if out.Usage != nil {
		result.InputTokens, result.OutputTokens = out.Usage.Prompt, out.Usage.Completion
	}
	return result, nil
}

func TestFullBook14B(t *testing.T) {
	modelID := strings.TrimSpace(os.Getenv("HIFY_FULLBOOK_MODEL"))
	if modelID == "" {
		t.Skip("HIFY_FULLBOOK_MODEL 未设置，跳过全书真实模型门禁")
	}
	if modelID != "qwen2.5:14b" {
		t.Fatalf("T042 只允许 qwen2.5:14b，实际 %q", modelID)
	}
	started := time.Now()
	ctx := t.Context()
	root := fullBookRoot(t)
	outDir := filepath.Join(root, "specs/010-narrative-scene-chunking-and-relation-extraction/evidence/fullbook-14b")
	if err := os.MkdirAll(filepath.Join(outDir, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	var corpus strings.Builder
	for i := 1; i <= 9; i++ {
		b, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("eval/corpus/aq-010/ch%02d.txt", i)))
		if err != nil {
			t.Fatal(err)
		}
		corpus.Write(b)
		if !bytes.HasSuffix(b, []byte("\n")) {
			corpus.WriteByte('\n')
		}
	}
	repo := setupIntegration(t)
	seedKB(t, repo, "kb-fullbook-14b", "m3", "u1", true)
	// ⚠️ seedKB 的默认 chunk_size 是 5（给微型夹具用的），全书 26002 rune
	// 按 5 切会切出 5000 多块、撞上 maxChunksPerDocument=2000 被拒——那是
	// 夹具的问题，不是产品限制。这里必须显式设成生产默认口径 500，
	// 与前三轮预检完全一致，否则块集合不同、数字也就不可比。
	if _, err := repo.db.ExecContext(t.Context(),
		"UPDATE knowledge_bases SET chunk_size=500, chunk_overlap=0 WHERE id='kb-fullbook-14b'"); err != nil {
		t.Fatal(err)
	}
	svc := NewService(repo, newFakeProvider(), newTestAsynqClient(t), t.TempDir(), false, "", time.Second, false, modelID)
	doc, err := svc.UploadDocumentWithOptions(ctx, "kb-fullbook-14b", "u1", "member", "aq-zhengzhuan.txt", FileTypeTxt, []byte(corpus.String()), UploadOptions{Narrative: true, RelationExtraction: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, doc.ID, doc.Version); err != nil {
		t.Fatal(err)
	}
	var jobID string
	if err := repo.db.QueryRowContext(ctx, `SELECT active_relation_job_id FROM documents WHERE id=?`, doc.ID).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	runner := runnerFor(t, repo, &fullBookModel{client: &http.Client{}, baseURL: envOr("HIFY_OLLAMA_BASE_URL", "http://127.0.0.1:11434"), rawDir: filepath.Join(outDir, "raw")})
	// ⚠️ 只有实验才设这个；不设就是契约里的 32。
	if v := strings.TrimSpace(os.Getenv("HIFY_FULLBOOK_ALIAS_CANDIDATES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			t.Fatalf("HIFY_FULLBOOK_ALIAS_CANDIDATES 不是正整数: %q", v)
		}
		runner.aliasCandidateLimit = n
		t.Logf("实验：归一候选窗口设为 %d（契约默认 %d）", n, maxAliasCandidates)
	}
	res, runErr := runner.runJob(ctx, jobID)
	writeFullBookArtifacts(t, repo, jobID, doc.ID, res, outDir, time.Since(started), modelID)
	if runErr != nil && runErr != ErrExtractionCallBudgetExhausted && runErr != ErrExtractionActiveTimeExhausted {
		t.Fatalf("runJob: %v", runErr)
	}
}

func writeFullBookArtifacts(t *testing.T, repo *Repository, jobID, docID string, res runJobResult, outDir string, wall time.Duration, model string) {
	t.Helper()
	ctx := t.Context()
	type attempt struct {
		ID, ItemID, ChunkID, Phase, State, ErrorCode string
		Raw                                          json.RawMessage
		UsageKnown                                   bool
		In, Out                                      sql.NullInt64
		Elapsed                                      sql.NullInt64
	}
	rows, err := repo.db.QueryContext(ctx, `SELECT a.id,a.item_id,i.chunk_id,a.phase,a.state,COALESCE(a.error_code,''),a.raw_response,a.usage_known,a.input_tokens,a.output_tokens,a.elapsed_ms FROM relation_extraction_attempts a JOIN relation_extraction_items i ON i.id=a.item_id WHERE a.job_id=? ORDER BY a.created_at,a.id`, jobID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []attempt
	states, phases, other := map[string]int{}, map[string]int{}, map[string]int{}
	known, inSum, outSum := 0, 0, 0
	for rows.Next() {
		var a attempt
		var raw sql.NullString
		var uk int
		if err := rows.Scan(&a.ID, &a.ItemID, &a.ChunkID, &a.Phase, &a.State, &a.ErrorCode, &raw, &uk, &a.In, &a.Out, &a.Elapsed); err != nil {
			t.Fatal(err)
		}
		a.UsageKnown = uk != 0
		if raw.Valid {
			a.Raw = json.RawMessage(raw.String)
		}
		all = append(all, a)
		states[a.State]++
		phases[a.Phase]++
		if a.UsageKnown && a.In.Valid && a.Out.Valid {
			known++
			inSum += int(a.In.Int64)
			outSum += int(a.Out.Int64)
		}
		if a.ErrorCode != "" && a.ErrorCode != "invalid_output" && a.ErrorCode != "extract_invalid" {
			other[a.ErrorCode]++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		b, _ := json.MarshalIndent(a, "", "  ")
		name := fmt.Sprintf("%s-%s-%s.json", a.ChunkID, a.Phase, a.ID)
		if err := os.WriteFile(filepath.Join(outDir, "raw", name), append(b, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	var total, succeeded, failed, reserved, confirmed, unknown, retry int
	var active int64
	var state, stop sql.NullString
	if err := repo.db.QueryRowContext(ctx, `SELECT total_items,succeeded_items,failed_items,reserved_calls,confirmed_dispatches,unknown_attempts,active_ms_used,retry_rounds,state,stop_reason FROM relation_extraction_jobs WHERE id=?`, jobID).Scan(&total, &succeeded, &failed, &reserved, &confirmed, &unknown, &active, &retry, &state, &stop); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(outDir, "ledger.json"), map[string]any{"job_id": jobID, "model": model, "total": total, "succeeded": succeeded, "failed_items": failed, "reserved_calls": reserved, "confirmed_dispatches": confirmed, "unknown_attempts": unknown, "active_ms_used": active, "retry_rounds": retry, "state": state.String, "stop_reason": stop.String, "wall_ms": wall.Milliseconds(), "attempts": len(all), "attempts_by_state": states, "attempts_by_phase": phases, "usage_known": known, "usage_unknown": len(all) - known, "input_tokens_known": inSum, "output_tokens_known": outSum, "run_result": res})
	denom := len(all)
	if denom == 0 {
		denom = 1
	}
	writeJSON(t, filepath.Join(outDir, "quality.json"), map[string]any{"legal_rate": float64(states["completed"]) / float64(denom), "verifiable_quote_rate": nil, "ambiguous_positions": res.AmbiguousPositions, "ambiguous_evidence": res.AmbiguousEvidence, "other_error_codes": other})
	writeJSON(t, filepath.Join(outDir, "results.json"), map[string]any{"characters": count(t, repo, `SELECT COUNT(*) FROM narrative_characters WHERE job_id=?`, jobID), "relations": count(t, repo, `SELECT COUNT(*) FROM narrative_relations WHERE job_id=?`, jobID), "evidence": count(t, repo, `SELECT COUNT(*) FROM narrative_relation_evidence WHERE job_id=?`, jobID)})
	readme := fmt.Sprintf("# T042 full book\\n\\n- model: %s\\n- wall clock ms: %d\\n- active ms: %d\\n- cost_kind: not_applicable (local model; no monetary amount)\\n- extractPromptVersion: %s\\n- extractionSchemaVersion: %d\\n- direct Ollama OpenAI-compatible HTTP was used; provider.ChatOnce was bypassed\\n- prompt, schema, validation, and eval annotations were unchanged\\n- no accuracy, precision, or recall: human truth is not available\\n", model, wall.Milliseconds(), active, extractPromptVersion, extractionSchemaVersion)
	if err := os.WriteFile(filepath.Join(outDir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
func count(t *testing.T, repo *Repository, q, id string) int {
	t.Helper()
	var n int
	if err := repo.db.QueryRowContext(t.Context(), q, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func envOr(k, v string) string {
	if x := strings.TrimSpace(os.Getenv(k)); x != "" {
		return x
	}
	return v
}
func fullBookRoot(t *testing.T) string {
	t.Helper()
	cwd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(cwd, "../.."))
}

var _ = sort.Strings
