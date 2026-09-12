package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hify/internal/eval/retrievalbench"
	"hify/internal/provider"
)

func TestParseCommandRequiresSubcommandAndAcceptsScore(t *testing.T) {
	if _, err := parseArgs([]string{"retrievalbench"}); err == nil {
		t.Fatal("expected subcommand error")
	}
	cfg, err := parseArgs([]string{"retrievalbench", "score", "--input", "run.json", "--output", "report.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.command != "score" || cfg.input != "run.json" || cfg.output != "report.json" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestParseSetupRerankAcceptsFixedServiceArguments(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "setup-rerank", "--base-url", "http://127.0.0.1:8090", "--user-id", "user-1", "--model", "BAAI/bge-reranker-v2-m3", "--output", "eval/runs/miracl-zh-rerank-setup.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.command != "setup-rerank" || cfg.rerankBaseURL != "http://127.0.0.1:8090" || cfg.rerankUserID != "user-1" || cfg.rerankModel != "BAAI/bge-reranker-v2-m3" {
		t.Fatalf("unexpected rerank setup config: %+v", cfg)
	}
}

func TestParseCompareAcceptsRerankExperimentMode(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "compare", "--baseline", "base.json", "--candidate", "candidate.json", "--rerank-experiment"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.rerankExperiment {
		t.Fatalf("rerank experiment mode was not parsed: %+v", cfg)
	}
}

func TestParseCompareAcceptsQualityExperimentMode(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "compare", "--baseline", "base.json", "--candidate", "candidate.json", "--quality-experiment"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.qualityExperiment {
		t.Fatalf("quality experiment mode was not parsed: %+v", cfg)
	}
}

func TestParseDecisionRequiresRerankReports(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "decision", "--baseline", "base.json", "--candidate", "candidate.json", "--output", "decision.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.command != "decision" || cfg.baseline != "base.json" || cfg.candidate != "candidate.json" || cfg.output != "decision.json" {
		t.Fatalf("unexpected decision config: %+v", cfg)
	}
}

func TestParseDiagnosticAcceptsDatasetAndThreshold(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "diagnostic", "--input", "candidate.json", "--dataset", "dataset", "--output", "diagnostic.json", "--threshold", "0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dataset != "dataset" || cfg.diagnosticThreshold != 0.5 {
		t.Fatalf("unexpected diagnostic config: %+v", cfg)
	}
}

func TestParseRunAcceptsQualityDiagnosticMode(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "run", "--run-mode", "quality_diagnostic", "--input", "dataset", "--output", "quality-run.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.runMode != "quality_diagnostic" {
		t.Fatalf("unexpected quality run mode: %+v", cfg)
	}
}

func TestQualityDiagnosticUsesThirtySecondTimeoutWithoutChangingDeploymentGate(t *testing.T) {
	if got := benchmarkRerankTimeout("quality_diagnostic", 1500*time.Millisecond); got != 30*time.Second {
		t.Fatalf("quality timeout = %s, want 30s", got)
	}
	if got := benchmarkRerankTimeout("deployment_gate", 1500*time.Millisecond); got != 1500*time.Millisecond {
		t.Fatalf("deployment timeout = %s, want configured 1.5s", got)
	}
}

func TestQualityDecisionRequiresIndependentReportPaths(t *testing.T) {
	if err := qualityDecision(commandConfig{}); err == nil {
		t.Fatal("quality-decision must require baseline, candidate and output")
	}
}

func TestSetupRerankRejectsAlternateModelBeforeNetwork(t *testing.T) {
	err := setupRerank(commandConfig{rerankBaseURL: "http://127.0.0.1:1", rerankModel: "other", rerankUserID: "user-1", output: "checkpoint.json"})
	if err == nil || !strings.Contains(err.Error(), "fixed") {
		t.Fatalf("error = %v, want fixed-model rejection", err)
	}
}

func TestDecisionRejectsMissingRequiredPaths(t *testing.T) {
	if err := decision(commandConfig{}); err == nil {
		t.Fatal("decision must require baseline, candidate and output")
	}
}

func TestBenchmarkFingerprintCapturesCompatibilityAndRedactsService(t *testing.T) {
	ds := retrievalbench.PreparedDataset{Manifest: retrievalbench.DatasetManifest{
		Dataset: "miracl", Revision: "v1.0", ConfigSHA256: "selection-sha",
		QueryIDs: []string{"q1"}, DocumentIDs: []string{"d1"},
		QrelsSHA256: "qrels-sha", CorpusSHA256: "corpus-sha",
	}}
	fp := buildBenchmarkFingerprint(ds, "bge-m3:567m", "digest", "http://127.0.0.1:11434", "{\"size\":500}", "{\"top_k\":10}")
	if fp.SelectionConfigSHA256 != "selection-sha" || fp.ChunkConfig == "" || fp.RetrievalConfig == "" || fp.ServiceEndpointID == "" {
		t.Fatalf("compatibility fingerprint omitted actual configuration: %+v", fp)
	}
	b, err := json.Marshal(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "127.0.0.1") || strings.Contains(string(b), "11434") {
		t.Fatalf("fingerprint leaked service address: %s", b)
	}
	fp.RunMode = "quality_diagnostic"
	b, err = json.Marshal(fp)
	if err != nil || !strings.Contains(string(b), `"run_mode":"quality_diagnostic"`) {
		t.Fatalf("quality run mode missing from fingerprint: %s", b)
	}
}

func TestRerankPrecheckValidatesFixedIdentityAndContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rerank" {
			_, _ = w.Write([]byte(`{"results":[{"index":1,"relevance_score":0.2},{"index":0,"relevance_score":0.8}]}`))
			return
		}
		if r.URL.Path != "/health" {
			t.Fatalf("path = %s, want /health or /rerank", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ready":true,"model":"BAAI/bge-reranker-v2-m3","revision":"953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e","digest":"d9e3e081faff1eefb84019509b2f5558fd74c1a05a2c7db22f74174fcedb5286","license":"Apache-2.0","runtime":{"python":"3.12.2","torch":"2","transformers":"4"}}`))
	}))
	defer server.Close()
	health, err := precheckRerankService(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if health.Model != "BAAI/bge-reranker-v2-m3" || health.Revision != "953dc6f6f85a1b2dbfca4c34a2796e7dde08d41e" || health.Digest == "" || !health.Ready {
		t.Fatalf("unexpected health: %+v", health)
	}
}

func TestFetchRerankStatsReadsCountersLatencyAndResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			t.Fatalf("path = %s, want /stats", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"request_count":50,"success_count":48,"failure_count":2,"degraded_count":2,"candidate_count_total":190,"cold_start_ms":123,"steady_latency_ms":[10,20],"current_rss_bytes":100,"peak_rss_bytes":200,"swap_used_bytes":300}`))
	}))
	defer server.Close()
	got, err := fetchRerankStats(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.RequestCount != 50 || got.SuccessCount != 48 || got.DegradedCount != 2 || got.ColdStartMS != 123 || len(got.SteadyLatencyMS) != 2 || got.PeakRSSBytes != 200 {
		t.Fatalf("unexpected stats: %+v", got)
	}
}

func TestRerankStatsDeltaTracksRequestsAndNeverCarriesPromptData(t *testing.T) {
	before := retrievalbench.RerankStatsSnapshot{RequestCount: 2, SuccessCount: 1, CandidateCountTotal: 4, SwapUsedBytes: 100, SteadyLatencyMS: []float64{10}}
	after := retrievalbench.RerankStatsSnapshot{RequestCount: 52, SuccessCount: 51, CandidateCountTotal: 204, ColdStartMS: 123, SwapUsedBytes: 1000, SteadyLatencyMS: []float64{10, 20, 30, 40}}
	delta := DiffRerankStats(before, after)
	if delta.RequestCount != 50 || delta.SuccessCount != 50 || delta.CandidateCountTotal != 200 {
		t.Fatalf("unexpected stats delta: %+v", delta)
	}
	if delta.ColdStartMS != 123 || delta.SteadyP50MS != 30 || delta.SteadyP95MS != 40 || delta.SwapDeltaBytes == 0 {
		t.Fatalf("unexpected latency/resource delta: %+v", delta)
	}
	b, _ := json.Marshal(delta)
	if strings.Contains(string(b), "query") || strings.Contains(string(b), "document") {
		t.Fatalf("stats delta leaked prompt data: %s", b)
	}
}

func TestRerankPhaseStatsCountsUnservedQueriesAsDegraded(t *testing.T) {
	before := retrievalbench.RerankStatsSnapshot{}
	after := retrievalbench.RerankStatsSnapshot{RequestCount: 5, SuccessCount: 1, FailureCount: 0}
	stats := rerankPhaseStats(before, after, 50, 50, 0, 50, 0, 0)
	if stats.HifyOutcome != "not_all_applied_or_degraded" {
		t.Fatalf("hify outcome = %q, want not_all_applied_or_degraded", stats.HifyOutcome)
	}
}

func TestRerankPhaseStatsNeverInfersHifyAppliedFromSidecarSuccess(t *testing.T) {
	before := retrievalbench.RerankStatsSnapshot{}
	after := retrievalbench.RerankStatsSnapshot{RequestCount: 50, SuccessCount: 50, FailureCount: 0}
	stats := rerankPhaseStats(before, after, 50, 50, 0, 50, 0, 0)
	if stats.HifyAppliedCount != 0 || stats.HifyDegradedCount != 50 {
		t.Fatalf("unexpected Hify observation counts: %+v", stats)
	}
	if stats.HifyOutcome == "all_applied" {
		t.Fatal("sidecar success must not imply all Hify reranks were applied")
	}
}

func TestRerankStatsSettledRequiresSidecarAccounting(t *testing.T) {
	if rerankStatsSettled(retrievalbench.RerankStatsSnapshot{RequestCount: 5, SuccessCount: 1, FailureCount: 0}) {
		t.Fatal("in-flight sidecar requests must not be treated as settled")
	}
	if !rerankStatsSettled(retrievalbench.RerankStatsSnapshot{RequestCount: 5, SuccessCount: 4, FailureCount: 1}) {
		t.Fatal("fully accounted sidecar requests must be settled")
	}
}

type setupProviderServiceFake struct {
	providers        []provider.Provider
	models           map[string][]provider.Model
	createdProviders int
	createdModels    int
}

func (f *setupProviderServiceFake) CreateProvider(_ context.Context, in provider.CreateProviderInput) (provider.Provider, error) {
	f.createdProviders++
	p := provider.Provider{ID: "provider-1", Name: in.Name, BaseURL: in.BaseURL, AuthType: in.AuthType, IsActive: true}
	f.providers = append(f.providers, p)
	return p, nil
}
func (f *setupProviderServiceFake) ListProviders(_ context.Context, _, _ int) ([]provider.Provider, int, error) {
	return f.providers, len(f.providers), nil
}
func (f *setupProviderServiceFake) GetProvider(_ context.Context, _ string) (provider.Provider, error) {
	return provider.Provider{}, nil
}
func (f *setupProviderServiceFake) UpdateProvider(_ context.Context, _ string, _ provider.UpdateProviderInput) (provider.Provider, error) {
	return provider.Provider{}, nil
}
func (f *setupProviderServiceFake) TestConnection(_ context.Context, _ string) error { return nil }
func (f *setupProviderServiceFake) AddModel(_ context.Context, providerID string, in provider.CreateModelInput) (provider.Model, error) {
	f.createdModels++
	m := provider.Model{ID: "model-1", ProviderID: providerID, ModelName: in.ModelName, Capability: in.Capability, IsActive: true}
	f.models[providerID] = append(f.models[providerID], m)
	return m, nil
}
func (f *setupProviderServiceFake) GetModel(_ context.Context, _ string) (provider.Model, error) {
	return provider.Model{}, nil
}
func (f *setupProviderServiceFake) ListModels(_ context.Context, providerID string) ([]provider.Model, error) {
	return f.models[providerID], nil
}
func (f *setupProviderServiceFake) ListModelsByCapability(_ context.Context, _ string) ([]provider.Model, error) {
	return nil, nil
}
func (f *setupProviderServiceFake) UpdateModel(_ context.Context, _ string, _ provider.UpdateModelInput) (provider.Model, error) {
	return provider.Model{}, nil
}
func (f *setupProviderServiceFake) ResolveClient(_ context.Context, _ string) (provider.Client, error) {
	return nil, nil
}
func (f *setupProviderServiceFake) ChatOnce(_ context.Context, _ string, _ provider.ChatRequest, _ time.Duration) (provider.ChatAttemptResult, error) {
	return provider.ChatAttemptResult{}, nil
}

func TestEnsureRerankProviderModelIsIdempotent(t *testing.T) {
	f := &setupProviderServiceFake{models: map[string][]provider.Model{}}
	a, err := ensureRerankProviderModel(context.Background(), f, "http://127.0.0.1:8090", "user-1", "BAAI/bge-reranker-v2-m3")
	if err != nil {
		t.Fatal(err)
	}
	b, err := ensureRerankProviderModel(context.Background(), f, "http://127.0.0.1:8090", "user-1", "BAAI/bge-reranker-v2-m3")
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID || f.createdProviders != 1 || f.createdModels != 1 {
		t.Fatalf("setup was not idempotent: a=%+v b=%+v providers=%d models=%d", a, b, f.createdProviders, f.createdModels)
	}
}
