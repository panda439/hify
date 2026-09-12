package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"hify/internal/eval/retrievalbench"
	"hify/internal/knowledge"
	"hify/internal/provider"
)

// 014：托管 Rerank 的 precheck、Hify 侧延迟/token 聚合、双门禁 timeout
// 固定、付费制品防覆盖、CLI 入口和"只有 Rerank 指纹不同"。

type hostedRerankProviderFake struct {
	setupProviderServiceFake
	model provider.Model
	prov  provider.Provider
}

func (f *hostedRerankProviderFake) GetModel(_ context.Context, id string) (provider.Model, error) {
	if id != f.model.ID {
		return provider.Model{}, errors.New("model not found")
	}
	return f.model, nil
}

func (f *hostedRerankProviderFake) GetProvider(_ context.Context, id string) (provider.Provider, error) {
	if id != f.prov.ID {
		return provider.Provider{}, errors.New("provider not found")
	}
	return f.prov, nil
}

func voyageProviderFake() *hostedRerankProviderFake {
	return &hostedRerankProviderFake{
		setupProviderServiceFake: setupProviderServiceFake{models: map[string][]provider.Model{}},
		model: provider.Model{
			ID: "voyage-model", ProviderID: "voyage-provider", ModelName: "rerank-3",
			Capability: provider.CapabilityRerank, IsActive: true,
		},
		prov: provider.Provider{
			ID: "voyage-provider", Name: "voyage-ai", AdapterType: provider.AdapterOpenAICompatible,
			BaseURL: "https://api.voyageai.com/v1", AuthType: provider.AuthTypeAPIKey, HasAPIKey: true,
			ExtraConfig: provider.ExtraConfig{RerankFormat: provider.RerankFormatVoyage}, IsActive: true,
		},
	}
}

func TestPrecheckHostedRerankAcceptsConfiguredVoyageModel(t *testing.T) {
	identity, err := precheckHostedRerank(context.Background(), voyageProviderFake(), "voyage-model")
	if err != nil {
		t.Fatal(err)
	}
	if identity.Source != retrievalbench.RerankSourceHostedAPI || identity.ModelName != "rerank-3" || !identity.Ready ||
		identity.EndpointID == "" || identity.Runtime["api_base_url"] != "https://api.voyageai.com/v1" {
		t.Fatalf("unexpected hosted identity: %+v", identity)
	}
	b, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(b)), "key") {
		t.Fatalf("hosted identity must not mention credentials: %s", b)
	}
}

func TestPrecheckHostedRerankRejectsMismatchedConfiguration(t *testing.T) {
	for name, mutate := range map[string]func(*hostedRerankProviderFake){
		"other model name":      func(f *hostedRerankProviderFake) { f.model.ModelName = "rerank-2.5" },
		"not rerank capability": func(f *hostedRerankProviderFake) { f.model.Capability = provider.CapabilityEmbedding },
		"inactive model":        func(f *hostedRerankProviderFake) { f.model.IsActive = false },
		"inactive provider":     func(f *hostedRerankProviderFake) { f.prov.IsActive = false },
		"other base url":        func(f *hostedRerankProviderFake) { f.prov.BaseURL = "https://example.com/v1" },
		"generic rerank format": func(f *hostedRerankProviderFake) { f.prov.ExtraConfig.RerankFormat = "" },
		"api key missing":       func(f *hostedRerankProviderFake) { f.prov.HasAPIKey = false },
		"unauthenticated":       func(f *hostedRerankProviderFake) { f.prov.AuthType = provider.AuthTypeNone },
	} {
		t.Run(name, func(t *testing.T) {
			f := voyageProviderFake()
			mutate(f)
			if _, err := precheckHostedRerank(context.Background(), f, "voyage-model"); err == nil {
				t.Fatal("mismatched hosted configuration must fail precheck")
			}
		})
	}
	if _, err := precheckHostedRerank(context.Background(), voyageProviderFake(), ""); err == nil {
		t.Fatal("empty model id must fail precheck")
	}
	if _, err := precheckHostedRerank(context.Background(), voyageProviderFake(), "unknown"); err == nil {
		t.Fatal("unknown model id must fail precheck")
	}
}

func TestHifyLatencyPercentilesUseNearestRankOverObservedCalls(t *testing.T) {
	durations := []int64{400, 100, 300, 200}
	p50, p95 := hifyLatencyPercentiles(durations)
	if p50 != 300 || p95 != 400 {
		t.Fatalf("p50/p95 = %v/%v, want 300/400", p50, p95)
	}
	if durations[0] != 400 {
		t.Fatal("percentile calculation must not reorder the caller's slice")
	}
	if p50, p95 := hifyLatencyPercentiles(nil); p50 != 0 || p95 != 0 {
		t.Fatalf("empty p50/p95 = %v/%v, want 0/0", p50, p95)
	}
}

func TestSuccessfulRerankDurationsExcludeDegradedCalls(t *testing.T) {
	got := successfulRerankDurations([]knowledge.BenchmarkRetrievalResult{
		{RerankEnabled: true, RerankApplied: true, RerankInputCount: 10, RerankDurationMS: 100},
		{RerankEnabled: true, RerankDegraded: true, RerankInputCount: 10, RerankDurationMS: 1500},
		{RerankEnabled: true, RerankApplied: false, RerankInputCount: 10, RerankDurationMS: 900},
	})
	if len(got) != 1 || got[0] != 100 {
		t.Fatalf("successful durations = %v, want [100]", got)
	}
	if got := successfulRerankDurations(nil); len(got) != 0 {
		t.Fatalf("zero successful calls must have no latency samples: %v", got)
	}
	stats := hostedRerankPhaseStats(2, 2, 0, 2, 20, 2400, successfulRerankDurations([]knowledge.BenchmarkRetrievalResult{{RerankEnabled: true, RerankDegraded: true, RerankInputCount: 10, RerankDurationMS: 1200}}), 0)
	if stats.HifyLatencyStatus != "unavailable" {
		t.Fatalf("zero successful calls must be marked unavailable: %+v", stats)
	}
	b, err := json.Marshal(stats)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "hify_p50_ms") || strings.Contains(string(b), "hify_p95_ms") {
		t.Fatalf("zero successful calls must not serialize p50/p95: %s", b)
	}
}

func TestHostedFailureCountsPersistOnlyFixedKinds(t *testing.T) {
	got := aggregateHostedFailureKinds([]knowledge.BenchmarkRetrievalResult{
		{RerankFailureKind: "timeout"},
		{RerankFailureKind: "http_429"},
		{RerankFailureKind: "circuit_open"},
		{RerankFailureKind: "response_invalid"},
		{RerankFailureKind: "other"},
		{RerankFailureKind: "provider_secret_body"},
		{RerankFailureKind: ""},
	})
	for _, kind := range []string{"timeout", "http_429", "circuit_open", "response_invalid", "other"} {
		if got[kind] == 0 {
			t.Fatalf("missing fixed failure kind %q in %+v", kind, got)
		}
	}
	if len(got) != 5 {
		t.Fatalf("failure counts carried unsafe or unknown kinds: %+v", got)
	}
}

func TestRunQueryErrorMarkerSanitizesOnlyHostedRuns(t *testing.T) {
	err := errors.New("provider response body must not be persisted")
	if got := runQueryErrorMarker(true, err); got != "query_failed" {
		t.Fatalf("hosted error marker = %q, want query_failed", got)
	}
	if got := runQueryErrorMarker(false, err); got != err.Error() {
		t.Fatalf("local sidecar error marker = %q, want original error", got)
	}
}

func TestHostedRerankPhaseStatsKeepsSidecarCountersEmpty(t *testing.T) {
	stats := hostedRerankPhaseStats(50, 50, 50, 0, 1759, 21000, []int64{400, 100, 300, 200}, 123456)
	if stats.HifyOutcome != "all_applied" || stats.HifyInputCount != 1759 || stats.HifyDurationMS != 21000 ||
		stats.HifyTotalTokens != 123456 || stats.HifyP50MS != 300 || stats.HifyP95MS != 400 {
		t.Fatalf("unexpected hosted stats: %+v", stats)
	}
	if stats.RequestCount != 0 || stats.SuccessCount != 0 || stats.StatsBefore != nil || stats.StatsAfter != nil {
		t.Fatalf("hosted stats must not invent sidecar counters: %+v", stats)
	}
	degraded := hostedRerankPhaseStats(50, 50, 0, 50, 1759, 75000, []int64{1500}, 0)
	if degraded.HifyOutcome != "not_all_applied_or_degraded" {
		t.Fatalf("degraded outcome = %q", degraded.HifyOutcome)
	}
}

func TestValidateGateRerankTimeoutPinsBothGates(t *testing.T) {
	for _, ok := range []struct {
		mode    string
		timeout time.Duration
	}{{"deployment_gate", 1500 * time.Millisecond}, {"quality_diagnostic", 30 * time.Second}} {
		if err := validateGateRerankTimeout(ok.mode, ok.timeout); err != nil {
			t.Fatalf("%s %s rejected: %v", ok.mode, ok.timeout, err)
		}
	}
	for _, bad := range []struct {
		mode    string
		timeout time.Duration
	}{{"deployment_gate", 2 * time.Second}, {"quality_diagnostic", 1500 * time.Millisecond}, {"canary", 1500 * time.Millisecond}} {
		if err := validateGateRerankTimeout(bad.mode, bad.timeout); err == nil {
			t.Fatalf("%s %s must be rejected", bad.mode, bad.timeout)
		}
	}
}

func TestEnsureFreshOutputRefusesToOverwritePaidRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miracl-zh-voyage-deployment.json")
	if err := ensureFreshOutput(path); err != nil {
		t.Fatalf("absent output rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureFreshOutput(path); err == nil {
		t.Fatal("existing paid run output must not be overwritten")
	}
}

func TestBenchmarkRerankSourceAcceptsOnlyKnownSources(t *testing.T) {
	for in, want := range map[string]string{
		"":              retrievalbench.RerankSourceLocalSidecar,
		"local_sidecar": retrievalbench.RerankSourceLocalSidecar,
		"hosted_api":    retrievalbench.RerankSourceHostedAPI,
	} {
		got, err := benchmarkRerankSource(in)
		if err != nil || got != want {
			t.Fatalf("source %q = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := benchmarkRerankSource("voyage"); err == nil {
		t.Fatal("unknown rerank source must be rejected")
	}
}

func TestParseGateDecisionAndHostedRunArguments(t *testing.T) {
	cfg, err := parseArgs([]string{"retrievalbench", "gate-decision", "--gate", "quality_diagnostic", "--baseline", "b.json", "--candidate", "c.json", "--output", "d.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.command != "gate-decision" || cfg.gate != "quality_diagnostic" || cfg.baseline != "b.json" || cfg.candidate != "c.json" || cfg.output != "d.json" {
		t.Fatalf("unexpected gate-decision config: %+v", cfg)
	}
	cfg, err = parseArgs([]string{"retrievalbench", "run", "--rerank-source", "hosted_api", "--input", "dataset", "--output", "run.json"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.rerankSource != "hosted_api" {
		t.Fatalf("rerank source not parsed: %+v", cfg)
	}
	if err := gateDecision(commandConfig{}); err == nil {
		t.Fatal("gate-decision must require gate, baseline, candidate and output")
	}
	if err := gateDecision(commandConfig{gate: "canary", baseline: "b.json", candidate: "c.json", output: "d.json"}); err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("unknown gate error = %v", err)
	}
}

func TestHostedRerankFingerprintOnlyChangesRerankFields(t *testing.T) {
	ds := retrievalbench.PreparedDataset{Manifest: retrievalbench.DatasetManifest{
		Dataset: "miracl", Revision: "v1.0", ConfigSHA256: "selection-sha",
		QueryIDs: []string{"q1"}, DocumentIDs: []string{"d1"},
		QrelsSHA256: "qrels-sha", CorpusSHA256: "corpus-sha",
	}}
	chunk := `{"chunk_size":500,"chunk_overlap":50}`
	base := buildBenchmarkFingerprint(ds, "bge-m3:567m", "digest", "http://127.0.0.1:11434", chunk, `{"top_k":10,"rerank_enabled":false,"metadata_filter_enabled":false}`)
	candidate := buildBenchmarkFingerprint(ds, "bge-m3:567m", "digest", "http://127.0.0.1:11434", chunk, `{"top_k":10,"rerank_enabled":true,"metadata_filter_enabled":false}`)
	applyRerankFingerprint(&candidate, "rerank-3", "sha256:abc", "deployment_gate", 1500*time.Millisecond)
	if !candidate.RerankEnabled || candidate.RerankModelName != "rerank-3" || candidate.RerankModelDigest != "sha256:abc" ||
		candidate.RerankCandidateLimit != 50 || candidate.RerankTimeoutMS != 1500 || candidate.RunMode != "deployment_gate" {
		t.Fatalf("unexpected rerank fingerprint: %+v", candidate)
	}
	queries := []retrievalbench.QueryMetrics{{QueryID: "q1", K: 10}}
	baseReport := retrievalbench.MetricReport{Fingerprint: base, Queries: queries}
	if _, err := retrievalbench.CompareRerankExperiment(baseReport, retrievalbench.MetricReport{Fingerprint: candidate, Queries: queries}); err != nil {
		t.Fatalf("rerank-only fingerprint difference must be comparable: %v", err)
	}
	candidate.EmbeddingDigest = "other-digest"
	if _, err := retrievalbench.CompareRerankExperiment(baseReport, retrievalbench.MetricReport{Fingerprint: candidate, Queries: queries}); err == nil {
		t.Fatal("embedding drift must be rejected")
	}
}
