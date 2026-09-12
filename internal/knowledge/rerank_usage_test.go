package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"hify/internal/provider"
)

// 014 T006：托管 Rerank 的 token 用量要从 provider 结果一路带到 benchmark
// observer。只要 provider 返回了结果（哪怕随后被 applyRerank 判为不可信而降级），
// 这次调用就已经被计费，用量照实记录；调用本身失败时没有可信用量，记 0。

func TestApplyRerankStepRecordsProviderTokenUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		result       provider.RerankResult
		err          error
		wantApplied  bool
		wantDegraded bool
		wantTokens   int
	}{
		"applied": {
			result:      provider.RerankResult{Scores: []provider.RerankScore{{Index: 0, Score: 0.1}, {Index: 1, Score: 0.9}}, TotalTokens: 777},
			wantApplied: true, wantTokens: 777,
		},
		"validation failure still consumed tokens": {
			result:       provider.RerankResult{Scores: []provider.RerankScore{{Index: 0, Score: 0.9}, {Index: 0, Score: 0.1}}, TotalTokens: 555},
			wantDegraded: true, wantTokens: 555,
		},
		"call error reports no usage": {
			err:          errors.New("simulated timeout"),
			wantDegraded: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc := &service{
				rerankEnabled: true,
				rerankModelID: "rerank-model",
				rerankTimeout: 1500 * time.Millisecond,
				rerankScoreFn: func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
					return tc.result, tc.err
				},
			}
			candidates := []RetrievedChunk{rcContent("c1", 0.9, "片段一"), rcContent("c2", 0.8, "片段二")}
			_, stats := svc.applyRerankStep(context.Background(), "问题", candidates)
			if stats.Applied != tc.wantApplied || stats.Degraded != tc.wantDegraded || stats.TotalTokens != tc.wantTokens {
				t.Fatalf("stats = %+v, want applied=%v degraded=%v tokens=%d", stats, tc.wantApplied, tc.wantDegraded, tc.wantTokens)
			}
		})
	}
}

func TestBenchmarkAdapterCapturesRerankTokenUsage(t *testing.T) {
	f := &benchmarkFakeService{
		docs:   map[string]Document{},
		rerank: rerankStats{Enabled: true, Applied: true, InputCount: 35, DurationMs: 420, TotalTokens: 4321, FailureKind: provider.RerankFailureOther},
	}
	a := NewBenchmarkAdapter(f, "kb-bench", "user", "admin")
	got, err := a.Retrieve(context.Background(), "问题", 10)
	if err != nil {
		t.Fatal(err)
	}
	if got.RerankTotalTokens != 4321 || got.RerankDurationMS != 420 || got.RerankFailureKind != string(provider.RerankFailureOther) {
		t.Fatalf("adapter lost rerank usage observation: %+v", got)
	}
}

func TestApplyRerankStepReportsSafeFailureKindWithoutRawError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want provider.RerankFailureKind
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: provider.RerankFailureTimeout},
		{name: "circuit open", err: provider.ErrRerankResponseInvalid, want: provider.RerankFailureResponseInvalid},
		{name: "other", err: errors.New("secret provider response"), want: provider.RerankFailureOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &service{
				rerankEnabled: true, rerankModelID: "rerank-model", rerankTimeout: time.Second,
				rerankScoreFn: func(context.Context, string, []string) (provider.RerankResult, error) {
					return provider.RerankResult{}, tc.err
				},
			}
			_, stats := svc.applyRerankStep(context.Background(), "query body", []RetrievedChunk{rcContent("c1", .9, "document body"), rcContent("c2", .8, "other body")})
			if stats.FailureKind != tc.want {
				t.Fatalf("failure kind = %q, want %q", stats.FailureKind, tc.want)
			}
		})
	}
}
