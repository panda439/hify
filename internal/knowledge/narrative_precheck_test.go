package knowledge

// T041 模型预检：只使用 ch01/ch02，复用生产抽取/归一 prompt、输入裁剪、解析和
// resolve helper。模型请求有意直接走 Ollama 的 OpenAI 兼容端点，绕过
// provider.ChatOnce 的限流、熔断和账目；因此本文件及 summary.json 不代表完整生产调用链。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

type precheckCall struct {
	Model         string          `json:"model"`
	Chunk         string          `json:"chunk_id"`
	Phase         string          `json:"phase"`
	Attempt       int             `json:"attempt"`
	RequestSHA256 string          `json:"request_sha256"`
	RequestRunes  int             `json:"request_runes"`
	Status        int             `json:"http_status"`
	LatencyMS     int64           `json:"latency_ms"`
	Response      json.RawMessage `json:"response"`
	Error         string          `json:"error,omitempty"`
}

type precheckStats struct {
	Model  string `json:"model"`
	Digest string `json:"digest"`
	Chunks int    `json:"chunks"`
	Calls  struct {
		Extract int `json:"extract"`
		Alias   int `json:"alias"`
	} `json:"calls"`
	JSONValid            int            `json:"json_valid"`
	JSONInvalid          int            `json:"json_invalid"`
	CallFailed           int            `json:"call_failed"`
	RejectReasons        map[string]int `json:"reject_reasons"`
	ResolveOK            int            `json:"resolve_ok"`
	ResolveFailed        int            `json:"resolve_failed"`
	ResolveRejectReasons map[string]int `json:"resolve_reject_reasons"`
	LatencyMS            struct {
		P50 int64 `json:"p50"`
		P95 int64 `json:"p95"`
		Max int64 `json:"max"`
	} `json:"latency_ms"`
	UsageReturned int     `json:"usage_returned"`
	Over60s       int     `json:"over_60s"`
	MentionsAvg   float64 `json:"mentions_avg"`
	RelationsAvg  float64 `json:"relations_avg"`
	Note          string  `json:"note"`
}

type ollamaResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage json.RawMessage `json:"usage"`
}

func TestNarrativePrecheck(t *testing.T) {
	if os.Getenv("HIFY_PRECHECK_RECOMPUTE") == "1" {
		root := precheckRepoRoot(t)
		precheckRecompute(t, root, filepath.Join(root, "specs/010-narrative-scene-chunking-and-relation-extraction/evidence/precheck"))
		return
	}
	models := strings.Split(strings.TrimSpace(os.Getenv("HIFY_PRECHECK_MODELS")), ",")
	if strings.TrimSpace(os.Getenv("HIFY_PRECHECK_MODELS")) == "" {
		t.Skip("HIFY_PRECHECK_MODELS 未设置，跳过本地模型预检")
	}
	root := precheckRepoRoot(t)
	outDir := filepath.Join(root, "specs/010-narrative-scene-chunking-and-relation-extraction/evidence/precheck")
	if err := os.MkdirAll(filepath.Join(outDir, "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	tags := struct {
		Models []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"models"`
	}{}
	if err := precheckJSON(t, "GET", "http://127.0.0.1:11434/api/tags", nil, &tags); err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for _, m := range tags.Models {
		digests[m.Name] = m.Digest
	}
	all := make([]precheckStats, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		t.Run(model, func(t *testing.T) { all = append(all, precheckModel(t, root, outDir, model, digests[model])) })
	}
	report := struct {
		Models []precheckStats `json:"models"`
	}{Models: all}
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "summary.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func precheckModel(t *testing.T, root, outDir, model, digest string) precheckStats {
	chapters := []string{"ch01.txt", "ch02.txt"}
	var pieces []struct {
		id, content string
		meta        narrativeMetadata
	}
	for _, name := range chapters {
		body, err := os.ReadFile(filepath.Join(root, "eval/corpus/aq-010", name))
		if err != nil {
			t.Fatal(err)
		}
		for i, p := range chunkNarrative(string(body), 500, 0) {
			id := strings.TrimSuffix(name, ".txt") + "-" + fmt.Sprintf("%03d", i+1)
			meta := narrativeMetadata{}
			if p.Narrative != nil {
				meta = *p.Narrative
			}
			pieces = append(pieces, struct {
				id, content string
				meta        narrativeMetadata
			}{id, p.Content, meta})
		}
	}
	stats := precheckStats{Model: model, Digest: digest, Chunks: len(pieces), RejectReasons: map[string]int{}, ResolveRejectReasons: map[string]int{}, Note: "直接 POST Ollama /v1/chat/completions；有意绕过 provider.ChatOnce（限流/熔断/账目），不代表完整生产调用路径。"}
	for _, reason := range []string{"围栏", "缺字段", "未知字段", "重复key", "截断", "其他"} {
		stats.RejectReasons[reason] = 0
	}
	for _, reason := range []string{"引用找不到", "跨分隔符", "occurrence越界"} {
		stats.ResolveRejectReasons[reason] = 0
	}
	var latencies []int64
	var mentionTotal, relationTotal int
	for _, p := range pieces {
		chunk := extractionChunkView{ChunkID: p.id, DocumentVersion: 1, Content: p.content, Meta: p.meta}
		instruction := buildExtractInstruction()
		rendered, err := fitExtractionInput(instruction, p.content)
		if err != nil {
			t.Fatal(err)
		}
		raw, meta := precheckHTTP(t, model, p.id, "extract", rendered, outDir)
		stats.Calls.Extract++
		latencies = append(latencies, meta.LatencyMS)
		if meta.Usage {
			stats.UsageReturned++
		}
		if meta.LatencyMS > 60000 {
			stats.Over60s++
		}
		var resp ollamaResponse
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &resp)
		}
		content := ""
		if len(resp.Choices) > 0 {
			content = resp.Choices[0].Message.Content
		}
		if meta.CallFailed {
			stats.CallFailed++
			continue
		}
		parsed, err := parseExtractionResponse([]byte(content))
		if err != nil {
			stats.JSONInvalid++
			stats.RejectReasons[precheckReject(content, responseFinish(resp), err)]++
			continue
		}
		stats.JSONValid++
		resolved, err := resolveExtraction(chunk, parsed)
		if err != nil {
			stats.ResolveFailed++
			stats.ResolveRejectReasons[precheckResolveReject(err)]++
			continue
		}
		stats.ResolveOK++
		mentionTotal += len(resolved.Mentions)
		relationTotal += len(resolved.Relations)
		in := aliasInput{Chunk: chunk, Mentions: resolved.Mentions, Proposals: resolved.AliasProposals}
		if !needsAliasPhase(in) {
			continue
		}
		aliasInstruction := buildAliasInstruction(in)
		candidates := renderAliasCandidates(in.Candidates)
		aliasRendered, _, err := fitAliasInput(aliasInstruction, p.content, candidates)
		if err != nil {
			t.Fatal(err)
		}
		aliasRaw, aliasMeta := precheckHTTP(t, model, p.id, "alias", aliasRendered, outDir)
		stats.Calls.Alias++
		latencies = append(latencies, aliasMeta.LatencyMS)
		if aliasMeta.Usage {
			stats.UsageReturned++
		}
		if aliasMeta.LatencyMS > 60000 {
			stats.Over60s++
		}
		var ar ollamaResponse
		_ = json.Unmarshal(aliasRaw, &ar)
		ac := ""
		if len(ar.Choices) > 0 {
			ac = ar.Choices[0].Message.Content
		}
		ap, err := parseAliasResponse([]byte(ac))
		if err != nil {
			stats.RejectReasons[precheckReject(ac, responseFinish(ar), err)]++
			continue
		}
		if _, err := resolveAliasDecisions(in, ap); err != nil {
			stats.ResolveRejectReasons[precheckResolveReject(err)]++
		}
	}
	stats.LatencyMS.P50, stats.LatencyMS.P95, stats.LatencyMS.Max = precheckPercentiles(latencies)
	n := float64(len(pieces))
	if n > 0 {
		stats.MentionsAvg = float64(mentionTotal) / n
		stats.RelationsAvg = float64(relationTotal) / n
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	_ = data
	return stats
}

func precheckRecompute(t *testing.T, root, outDir string) {
	entries, err := os.ReadDir(filepath.Join(outDir, "raw"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Models []precheckStats `json:"models"`
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		report.Models = append(report.Models, recomputeModel(t, root, outDir, entry.Name()))
	}
	sort.Slice(report.Models, func(i, j int) bool { return report.Models[i].Model < report.Models[j].Model })
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "summary.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func recomputeModel(t *testing.T, root, outDir, model string) precheckStats {
	stats := precheckStats{Model: model, Chunks: 13, RejectReasons: map[string]int{}, ResolveRejectReasons: map[string]int{}, Note: "从 raw 离线重算；未发起模型请求。json_valid/json_invalid 分母为拿到响应的次数，call_failed 单独统计，calls 为发起调用数。"}
	for _, reason := range []string{"围栏", "缺字段", "未知字段", "重复key", "截断", "其他"} {
		stats.RejectReasons[reason] = 0
	}
	for _, reason := range []string{"引用找不到", "跨分隔符", "occurrence越界", "其他"} {
		stats.ResolveRejectReasons[reason] = 0
	}
	files, err := filepath.Glob(filepath.Join(outDir, "raw", model, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var latencies []int64
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var call precheckCall
		if err := json.Unmarshal(b, &call); err != nil {
			t.Fatal(err)
		}
		stats.Calls.Extract++
		latencies = append(latencies, call.LatencyMS)
		if call.LatencyMS > 60000 {
			stats.Over60s++
		}
		if call.Error != "" || call.Status == 0 || string(call.Response) == "null" || len(call.Response) == 0 {
			stats.CallFailed++
			continue
		}
		var resp ollamaResponse
		if err := json.Unmarshal(call.Response, &resp); err != nil {
			t.Fatal(err)
		}
		if len(resp.Usage) > 0 && string(resp.Usage) != "null" {
			stats.UsageReturned++
		}
		content := ""
		if len(resp.Choices) > 0 {
			content = resp.Choices[0].Message.Content
		}
		parsed, parseErr := parseExtractionResponse([]byte(content))
		if parseErr != nil {
			stats.JSONInvalid++
			stats.RejectReasons[precheckReject(content, responseFinish(resp), parseErr)]++
			continue
		}
		stats.JSONValid++
		// Recompute resolve figures using the same production helper and the raw request's chunk id.
		chunk := recomputeChunk(t, root, call.Chunk)
		resolved, resolveErr := resolveExtraction(chunk, parsed)
		if resolveErr != nil {
			stats.ResolveFailed++
			stats.ResolveRejectReasons[precheckResolveReject(resolveErr)]++
			continue
		}
		stats.ResolveOK++
		stats.MentionsAvg += float64(len(resolved.Mentions))
		stats.RelationsAvg += float64(len(resolved.Relations))
	}
	stats.LatencyMS.P50, stats.LatencyMS.P95, stats.LatencyMS.Max = precheckPercentiles(latencies)
	denom := float64(stats.Chunks)
	if denom > 0 {
		stats.MentionsAvg /= denom
		stats.RelationsAvg /= denom
	}
	return stats
}

func recomputeChunk(t *testing.T, root, id string) extractionChunkView {
	parts := strings.Split(id, "-")
	name := parts[0] + ".txt"
	idx := 0
	if len(parts) > 1 {
		_, _ = fmt.Sscanf(parts[1], "%d", &idx)
	}
	body, err := os.ReadFile(filepath.Join(root, "eval/corpus/aq-010", name))
	if err != nil {
		t.Fatal(err)
	}
	pieces := chunkNarrative(string(body), 500, 0)
	if idx < 1 || idx > len(pieces) {
		t.Fatalf("raw chunk id %q 不在 corpus 分块内", id)
	}
	p := pieces[idx-1]
	meta := narrativeMetadata{}
	if p.Narrative != nil {
		meta = *p.Narrative
	}
	return extractionChunkView{ChunkID: id, DocumentVersion: 1, Content: p.Content, Meta: meta}
}

type precheckHTTPMeta struct {
	LatencyMS  int64
	Usage      bool
	CallFailed bool
}

func precheckHTTP(t *testing.T, model, chunk, phase, prompt, outDir string) ([]byte, precheckHTTPMeta) {
	h := sha256.Sum256([]byte(prompt))
	reqBody, _ := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": prompt}}, "temperature": 0, "max_tokens": maxOutputTokens})
	ctx, cancel := context.WithTimeout(context.Background(), extractionCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1:11434/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	meta := precheckHTTPMeta{LatencyMS: elapsed.Milliseconds()}
	status := 0
	var body []byte
	if resp != nil {
		status = resp.StatusCode
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	call := precheckCall{Model: model, Chunk: chunk, Phase: phase, Attempt: 1, RequestSHA256: hex.EncodeToString(h[:]), RequestRunes: len([]rune(prompt)), Status: status, LatencyMS: meta.LatencyMS}
	if err != nil {
		meta.CallFailed = true
		call.Error = err.Error()
	} else {
		call.Response = json.RawMessage(body)
		var o ollamaResponse
		if json.Unmarshal(body, &o) == nil && len(o.Usage) > 0 && string(o.Usage) != "null" {
			meta.Usage = true
		}
	}
	b, _ := json.MarshalIndent(call, "", "  ")
	dir := filepath.Join(outDir, "raw", model)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, chunk+"-"+phase+"-1.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err != nil {
		return nil, meta
	}
	return body, meta
}

func precheckJSON(t *testing.T, method, url string, body io.Reader, dst any) error {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("Ollama %s 返回 HTTP %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}
func precheckRepoRoot(t *testing.T) string {
	_, file, _, ok := runtimeCaller()
	if !ok {
		t.Fatal("无法定位测试文件")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}
func runtimeCaller() (uintptr, string, int, bool)  { return caller(1) }
func caller(skip int) (uintptr, string, int, bool) { return runtime.Caller(skip + 1) }
func responseFinish(resp ollamaResponse) string {
	if len(resp.Choices) == 0 {
		return ""
	}
	return resp.Choices[0].FinishReason
}

func precheckReject(content, finish string, err error) string {
	if strings.HasPrefix(strings.TrimSpace(content), "```") {
		return "围栏"
	}
	if finish == "length" {
		return "截断"
	}
	if err == nil {
		return "其他"
	}
	return "其他"
}
func precheckResolveReject(err error) string {
	if errors.Is(err, errQuoteNotCitable) {
		return "跨分隔符"
	}
	if errors.Is(err, errQuoteNotFound) {
		if strings.Contains(err.Error(), "occurrence") {
			return "occurrence越界"
		}
		return "引用找不到"
	}
	return "其他"
}
func precheckPercentiles(a []int64) (int64, int64, int64) {
	if len(a) == 0 {
		return 0, 0, 0
	}
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	pick := func(p float64) int64 { i := int(p*float64(len(a)-1) + 0.5); return a[i] }
	return pick(.5), pick(.95), a[len(a)-1]
}
