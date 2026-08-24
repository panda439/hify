package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// T019：先写失败测试，再在 openai_compat.go 里实现 openAICompatClient.Rerank
// （T021）。这里覆盖 contracts/rerank-http-api.md 的两半契约：
//   1. 请求体编码——return_documents 恒为 false、top_n 恒等于送入的候选数；
//   2. 响应校验——5 条"不可信"规则命中任意一条都必须整体报错，不能返回
//      部分结果（调用方 knowledge.applyRerank 才是"整体丢弃保持原顺序"的
//      降级点，这里只负责如实报告"这个响应不可信"）。

func newTestRerankClient(t *testing.T, handler http.HandlerFunc) *openAICompatClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return newOpenAICompatClient(srv.URL, "test-key", nil, srv.Client())
}

func TestRerankRequestEncoding(t *testing.T) {
	var captured map[string]any
	client := newTestRerankClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Fatalf("path = %s, want /rerank", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want Bearer test-key", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.9},
				{"index": 1, "relevance_score": 0.1},
			},
		})
	})

	_, err := client.Rerank(context.Background(), RerankRequest{
		Model:     "bge-reranker-v2-m3",
		Query:     "问题",
		Documents: []string{"片段1", "片段2"},
	})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}

	if got, want := captured["model"], "bge-reranker-v2-m3"; got != want {
		t.Fatalf("model = %v, want %v", got, want)
	}
	if got, want := captured["return_documents"], false; got != want {
		t.Fatalf("return_documents = %v, want %v — must never echo片段正文回传，省带宽也减少日志泄漏面", got, want)
	}
	// JSON 数字统一解成 float64。
	if got, want := captured["top_n"], float64(2); got != want {
		t.Fatalf("top_n = %v, want %v (len(documents))", got, want)
	}
	docs, ok := captured["documents"].([]any)
	if !ok || len(docs) != 2 || docs[0] != "片段1" || docs[1] != "片段2" {
		t.Fatalf("documents = %v, want [片段1 片段2] in original order", captured["documents"])
	}
}

func TestRerankResponseDecoding(t *testing.T) {
	client := newTestRerankClient(t, func(w http.ResponseWriter, r *http.Request) {
		// results 的顺序不可依赖——响应里 index=1 排在 index=0 之前，调用方
		// 必须按 Index 回填，不能假定 Scores[i] 对应 Documents[i]。
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 1, "relevance_score": 0.71, "document": "忽略这个多余字段"},
				{"index": 0, "relevance_score": 0.94},
			},
			"usage": map[string]any{"total_tokens": 123}, // 允许携带、Hify 忽略
		})
	})

	got, err := client.Rerank(context.Background(), RerankRequest{
		Model: "m", Query: "q", Documents: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	byIndex := map[int]float64{}
	for _, s := range got.Scores {
		byIndex[s.Index] = s.Score
	}
	if byIndex[0] != 0.94 || byIndex[1] != 0.71 {
		t.Fatalf("scores by index = %v, want {0:0.94 1:0.71}", byIndex)
	}
}

// --- contracts/rerank-http-api.md 的 5 条"不可信"校验 ---

func rerankServerReturning(t *testing.T, body string) *openAICompatClient {
	t.Helper()
	return newTestRerankClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	})
}

func TestRerankResponseUntrustedWhenEmpty(t *testing.T) {
	client := rerankServerReturning(t, `{"results": []}`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: empty results for a non-empty request")
	}
}

func TestRerankResponseUntrustedWhenLengthMismatched(t *testing.T) {
	client := rerankServerReturning(t, `{"results": [{"index": 0, "relevance_score": 0.5}]}`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: results length != documents length")
	}
}

func TestRerankResponseUntrustedWhenIndexOutOfRange(t *testing.T) {
	client := rerankServerReturning(t, `{"results": [{"index": 0, "relevance_score": 0.5}, {"index": 5, "relevance_score": 0.4}]}`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: index out of [0, len(documents)) range")
	}
}

func TestRerankResponseUntrustedWhenIndexDuplicated(t *testing.T) {
	client := rerankServerReturning(t, `{"results": [{"index": 0, "relevance_score": 0.5}, {"index": 0, "relevance_score": 0.9}]}`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: duplicate index")
	}
}

func TestRerankResponseUntrustedWhenIndexMissing(t *testing.T) {
	// documents 长度为 2（index 0..1 必须被完整覆盖），但两条结果都给了
	// index 0——长度对得上，但 index 1 缺失。
	client := rerankServerReturning(t, `{"results": [{"index": 0, "relevance_score": 0.5}, {"index": 0, "relevance_score": 0.9}]}`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: index 1 never covered")
	}
}

func TestRerankResponseUntrustedWhenUnparsable(t *testing.T) {
	client := rerankServerReturning(t, `not json`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: unparsable JSON")
	}
}

func TestRerankResponseUntrustedWhenScoreNonNumeric(t *testing.T) {
	client := rerankServerReturning(t, `{"results": [{"index": 0, "relevance_score": "high"}, {"index": 1, "relevance_score": 0.4}]}`)
	if _, err := client.Rerank(context.Background(), RerankRequest{Documents: []string{"a", "b"}}); err == nil {
		t.Fatal("want error: relevance_score is not numeric")
	}
}
