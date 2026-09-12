package retrievalbench

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// 014：托管 Rerank（Voyage）的证据校验、列表价估算和模型无关门禁。
// 012 的 DecideRerank/DecideQuality 保持原样，BGE 既有结论不按新标准回算。

func hostedGateCandidate(mode string) MetricReport {
	return MetricReport{
		Complete: true, QueryCount: 50,
		Fingerprint: Fingerprint{RunMode: mode},
		Aggregates:  []AggregateMetrics{{K: 10, Recall: .98, MRR: .70, MAP: .60, NDCG: .80}},
		RerankIdentity: &RerankModelIdentity{
			Source: RerankSourceHostedAPI, ModelName: VoyageRerankModelName,
			EndpointID: "sha256:abc", Ready: true,
		},
		RerankStats: &RerankPhaseStats{
			HifyEnabledCount: 50, HifyAppliedCount: 50, HifyOutcome: "all_applied",
			HifyInputCount: 1759, HifyP50MS: 420, HifyP95MS: 900, HifyLatencyStatus: "available", HifyTotalTokens: 123456,
			HifyFailureCounts: map[string]int{"timeout": 0, "http_429": 0, "circuit_open": 0, "response_invalid": 0, "other": 0},
		},
	}
}

func gateBaseline() MetricReport {
	return MetricReport{
		Complete: true, QueryCount: 50,
		Aggregates: []AggregateMetrics{{K: 10, Recall: .976, MRR: .626746, MAP: .55, NDCG: .696447}},
	}
}

func TestValidateRerankEvidenceAcceptsCompleteHostedRun(t *testing.T) {
	if err := ValidateRerankEvidence(hostedGateCandidate("deployment_gate")); err != nil {
		t.Fatalf("complete hosted evidence rejected: %v", err)
	}
}

func TestValidateRerankEvidenceRejectsIncompleteHostedEvidence(t *testing.T) {
	for name, mutate := range map[string]func(*MetricReport){
		"other hosted model":  func(r *MetricReport) { r.RerankIdentity.ModelName = "rerank-2.5" },
		"not ready":           func(r *MetricReport) { r.RerankIdentity.Ready = false },
		"missing endpoint":    func(r *MetricReport) { r.RerankIdentity.EndpointID = "" },
		"missing token usage": func(r *MetricReport) { r.RerankStats.HifyTotalTokens = 0 },
		"missing input count": func(r *MetricReport) { r.RerankStats.HifyInputCount = 0 },
		"one degraded query": func(r *MetricReport) {
			r.RerankStats.HifyAppliedCount = 49
			r.RerankStats.HifyDegradedCount = 1
			r.RerankStats.HifyOutcome = "not_all_applied_or_degraded"
		},
		"non-finite p95": func(r *MetricReport) { r.RerankStats.HifyP95MS = math.NaN() },
		"infinite p50":   func(r *MetricReport) { r.RerankStats.HifyP50MS = math.Inf(1) },
		"p95 below p50":  func(r *MetricReport) { r.RerankStats.HifyP95MS = 1 },
		"unknown failure kind": func(r *MetricReport) {
			r.RerankStats.HifyFailureCounts = map[string]int{"timeout": 1, "secret_body": 1}
		},
		"negative failure count": func(r *MetricReport) {
			r.RerankStats.HifyFailureCounts = map[string]int{"timeout": -1}
		},
		"sidecar counters set": func(r *MetricReport) {
			r.RerankStats.RequestCount, r.RerankStats.SuccessCount = 50, 50
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := hostedGateCandidate("deployment_gate")
			mutate(&report)
			if err := ValidateRerankEvidence(report); err == nil {
				t.Fatal("incomplete hosted evidence must be rejected")
			}
		})
	}
}

func TestEstimateListPriceUSD(t *testing.T) {
	got, err := EstimateListPriceUSD(1_234_567, VoyageRerank3PricePerMillionUSD)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-0.06172835) > 1e-12 {
		t.Fatalf("list price = %.10f, want 0.06172835", got)
	}
	if zero, err := EstimateListPriceUSD(0, VoyageRerank3PricePerMillionUSD); err != nil || zero != 0 {
		t.Fatalf("zero tokens = %v, %v; want 0, nil", zero, err)
	}
	for name, in := range map[string]struct {
		tokens int
		price  float64
	}{
		"negative tokens": {-1, .05},
		"nan price":       {1, math.NaN()},
		"inf price":       {1, math.Inf(1)},
		"negative price":  {1, -.01},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := EstimateListPriceUSD(in.tokens, in.price); err == nil {
				t.Fatal("invalid price input must be rejected")
			}
		})
	}
}

func TestNewVoyageRerankCostSeparatesListPriceFromFreeAllowance(t *testing.T) {
	cost, err := NewVoyageRerankCost(1_234_567, "2026-09-11T10:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if cost.ModelName != VoyageRerankModelName || cost.APIBaseURL != VoyageAPIBaseURL ||
		cost.PriceSourceURL != VoyagePriceSourceURL || cost.PriceSnapshotDate != VoyagePriceSnapshotDate ||
		cost.FreeTokensPerAccount != VoyageRerank3FreeTokensPerAccount || cost.TotalTokens != 1_234_567 ||
		cost.EvaluatedAt == "" || cost.TokenCountBasis == "" || cost.BillingNote == "" {
		t.Fatalf("cost evidence is incomplete: %+v", cost)
	}
	if math.Abs(cost.ListPriceUSD-0.06172835) > 1e-12 {
		t.Fatalf("list price = %.10f, want 0.06172835", cost.ListPriceUSD)
	}
	b, err := json.Marshal(cost)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(b))
	for _, forbidden := range []string{"api_key", "authorization", "bearer"} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("cost evidence must not carry credentials (%q): %s", forbidden, b)
		}
	}
	if _, err := NewVoyageRerankCost(-1, "2026-09-11T10:00:00Z"); err == nil {
		t.Fatal("negative tokens must be rejected")
	}
	if _, err := NewVoyageRerankCost(1, ""); err == nil {
		t.Fatal("evaluation time is required")
	}
}

func TestDecideRerankGateDeploymentOutcomes(t *testing.T) {
	base := gateBaseline()
	cases := []struct {
		name       string
		mutate     func(*MetricReport)
		comparable bool
		want       string
		reason     string
	}{
		{"all gates pass", nil, true, "ADOPT", ""},
		{"every metric equals baseline", func(r *MetricReport) { r.Aggregates[0] = base.Aggregates[0] }, true, "DO_NOT_ADOPT", "no ranking metric improved"},
		{"only map improves with recall equal", func(r *MetricReport) {
			r.Aggregates[0] = base.Aggregates[0]
			r.Aggregates[0].MAP = .56
		}, true, "ADOPT", ""},
		{"map declines while mrr and ndcg improve", func(r *MetricReport) { r.Aggregates[0].MAP = .54 }, true, "DO_NOT_ADOPT", "map@10"},
		{"recall declines", func(r *MetricReport) { r.Aggregates[0].Recall = .97 }, true, "DO_NOT_ADOPT", "recall@10"},
		{"one degraded query", func(r *MetricReport) {
			r.Complete = false
			r.RerankStats.HifyAppliedCount = 49
			r.RerankStats.HifyDegradedCount = 1
			r.RerankStats.HifyOutcome = "not_all_applied_or_degraded"
		}, true, "INCONCLUSIVE", ""},
		{"incompatible reports", nil, false, "INCONCLUSIVE", "not comparable"},
		{"quality run under deployment gate", func(r *MetricReport) { r.Fingerprint.RunMode = "quality_diagnostic" }, true, "INCONCLUSIVE", "run_mode"},
		{"failed query", func(r *MetricReport) { r.FailedQueryCount = 1 }, true, "INCONCLUSIVE", ""},
		{"not fifty queries", func(r *MetricReport) {
			r.QueryCount = 49
			r.RerankStats.HifyEnabledCount = 49
			r.RerankStats.HifyAppliedCount = 49
		}, true, "INCONCLUSIVE", ""},
		{"missing @10", func(r *MetricReport) { r.Aggregates[0].K = 5 }, true, "INCONCLUSIVE", "@10"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := hostedGateCandidate("deployment_gate")
			if tc.mutate != nil {
				tc.mutate(&candidate)
			}
			got := DecideRerankGate(RerankGateInput{Gate: "deployment_gate", Baseline: base, Candidate: candidate, Comparable: tc.comparable})
			if got.Decision != tc.want {
				t.Fatalf("decision = %+v, want %s", got, tc.want)
			}
			if len(got.Reasons) == 0 {
				t.Fatalf("decision must carry stable reasons: %+v", got)
			}
			if tc.reason != "" && !strings.Contains(strings.Join(got.Reasons, "; "), tc.reason) {
				t.Fatalf("reasons = %v, want to mention %q", got.Reasons, tc.reason)
			}
		})
	}
}

func TestDecideRerankGateQualityLabels(t *testing.T) {
	base := gateBaseline()
	decide := func(gate string, candidate MetricReport) string {
		return DecideRerankGate(RerankGateInput{Gate: gate, Baseline: base, Candidate: candidate, Comparable: true}).Decision
	}
	if got := decide("quality_diagnostic", hostedGateCandidate("quality_diagnostic")); got != "QUALITY_PASS" {
		t.Fatalf("pass = %s", got)
	}
	fail := hostedGateCandidate("quality_diagnostic")
	fail.Aggregates[0].NDCG = .69
	if got := decide("quality_diagnostic", fail); got != "QUALITY_FAIL" {
		t.Fatalf("fail = %s", got)
	}
	incomplete := hostedGateCandidate("quality_diagnostic")
	incomplete.RerankStats.HifyTotalTokens = 0
	if got := decide("quality_diagnostic", incomplete); got != "QUALITY_INCONCLUSIVE" {
		t.Fatalf("incomplete = %s", got)
	}
	if got := decide("quality_diagnostic", hostedGateCandidate("deployment_gate")); got != "QUALITY_INCONCLUSIVE" {
		t.Fatalf("deployment run under quality gate = %s", got)
	}
	unknown := DecideRerankGate(RerankGateInput{Gate: "canary", Baseline: base, Candidate: hostedGateCandidate("canary"), Comparable: true})
	if unknown.Decision != "INCONCLUSIVE" || !strings.Contains(strings.Join(unknown.Reasons, "; "), "gate") {
		t.Fatalf("unknown gate = %+v", unknown)
	}
}

func TestDecideRerankGateIsModelAgnosticForLocalSidecarEvidence(t *testing.T) {
	candidate := MetricReport{
		Complete: true, QueryCount: 50,
		Fingerprint:    Fingerprint{RunMode: "quality_diagnostic"},
		Aggregates:     []AggregateMetrics{{K: 10, Recall: .99, MRR: .75, MAP: .65, NDCG: .78}},
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
		RerankStats: &RerankPhaseStats{
			RequestCount: 50, SuccessCount: 50, HifyEnabledCount: 50, HifyAppliedCount: 50,
			HifyOutcome: "all_applied", SteadyP50MS: 7, SteadyP95MS: 11,
		},
	}
	got := DecideRerankGate(RerankGateInput{Gate: "quality_diagnostic", Baseline: gateBaseline(), Candidate: candidate, Comparable: true})
	if got.Decision != "QUALITY_PASS" {
		t.Fatalf("local sidecar evidence decision = %+v", got)
	}
}

func TestCompareRerankExperimentAcceptsHostedModelButRejectsNonRerankDrift(t *testing.T) {
	base := testReport()
	candidate := testReport()
	candidate.Fingerprint.RerankEnabled = true
	candidate.Fingerprint.RerankModelName = VoyageRerankModelName
	candidate.Fingerprint.RerankModelDigest = "sha256:abc"
	candidate.Fingerprint.RerankCandidateLimit = 50
	candidate.Fingerprint.RerankTimeoutMS = 1500
	if _, err := CompareRerankExperiment(base, candidate); err != nil {
		t.Fatalf("hosted rerank-only difference must be comparable: %v", err)
	}
	candidate.Fingerprint.ChunkConfig = "drifted-chunk-config"
	if _, err := CompareRerankExperiment(base, candidate); err == nil {
		t.Fatal("non-rerank fingerprint drift must be rejected")
	}
}

func TestRescoreRunCarriesHostedCostEvidence(t *testing.T) {
	run := RetrievalRun{
		Fingerprint: Fingerprint{K: []int{1}},
		Queries:     []BenchmarkQuery{{ID: "q1", Text: "问题"}},
		Qrels:       []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}},
		Results:     []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d1"}}},
		Complete:    true, QueryCount: 1,
		HostedRerankCost: &HostedRerankCost{ModelName: VoyageRerankModelName, TotalTokens: 10},
	}
	report, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if report.HostedRerankCost == nil || report.HostedRerankCost.TotalTokens != 10 {
		t.Fatalf("rescore dropped hosted cost evidence: %+v", report.HostedRerankCost)
	}
}
