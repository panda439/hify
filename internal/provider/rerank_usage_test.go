package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/sony/gobreaker"
)

// 014 T005：托管 Rerank 的真实消耗只能来自 Voyage 响应的 usage.total_tokens，
// adapter 必须如实带出；负数说明响应不可信，整体报错。通用 top_n/results
// 格式不在 014 范围内，保持"未上报"（零值），行为不变。

func TestVoyageRerankResponseDecodesUsageTotalTokens(t *testing.T) {
	client := newTestVoyageRerankClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"index": 1, "relevance_score": 0.2},
				{"index": 0, "relevance_score": 0.8},
			},
			"model": "rerank-3",
			"usage": map[string]any{"total_tokens": 321},
		})
	})

	got, err := client.Rerank(context.Background(), RerankRequest{
		Model: "rerank-3", Query: "q", Documents: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if got.TotalTokens != 321 {
		t.Fatalf("TotalTokens = %d, want 321", got.TotalTokens)
	}
}

func TestClassifyRerankFailureUsesOnlyFixedSafeKinds(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want RerankFailureKind
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: RerankFailureTimeout},
		{name: "http 429", err: &adapterError{status: http.StatusTooManyRequests, cause: errors.New("private response body")}, want: RerankFailureHTTP429},
		{name: "circuit open", err: gobreaker.ErrOpenState, want: RerankFailureCircuitOpen},
		{name: "response invalid", err: ErrRerankResponseInvalid, want: RerankFailureResponseInvalid},
		{name: "other", err: errors.New("private response body"), want: RerankFailureOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyRerankFailure(tc.err); got != tc.want {
				t.Fatalf("kind = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVoyageRerankResponseRejectsNegativeUsage(t *testing.T) {
	client := newTestVoyageRerankClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"index": 0, "relevance_score": 0.8}},
			"usage": map[string]any{"total_tokens": -1},
		})
	})

	if _, err := client.Rerank(context.Background(), RerankRequest{
		Model: "rerank-3", Query: "q", Documents: []string{"a"},
	}); err == nil {
		t.Fatal("negative usage.total_tokens must make the response untrusted")
	}
}

func TestGenericRerankResponseKeepsTotalTokensUnreported(t *testing.T) {
	client := newTestRerankClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "relevance_score": 0.8}},
			"usage":   map[string]any{"total_tokens": 99},
		})
	})

	got, err := client.Rerank(context.Background(), RerankRequest{
		Model: "bge", Query: "q", Documents: []string{"a"},
	})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if got.TotalTokens != 0 {
		t.Fatalf("generic rerank TotalTokens = %d, want 0 (unchanged contract)", got.TotalTokens)
	}
}
