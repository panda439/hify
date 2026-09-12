package retrievalbench

import (
	"strings"
	"testing"
)

// 014 FR-015/FR-016：Voyage 账户未添加付款方式（3 RPM / 10K TPM）时，只允许
// 30 秒质量诊断使用 benchmark-only 限速。限速记录必须随 raw run 进入 report；
// 限速运行永远不能作为部署门禁证据。

func TestRescoreRunCarriesRerankPacing(t *testing.T) {
	run := RetrievalRun{
		Fingerprint:  Fingerprint{K: []int{1}},
		Queries:      []BenchmarkQuery{{ID: "q1", Text: "问题"}},
		Qrels:        []Qrel{{QueryID: "q1", SourceDocumentID: "d1", Relevance: 1}},
		Results:      []RawQueryResult{{QueryID: "q1", DocumentRanking: []string{"d1"}}},
		Complete:     true,
		QueryCount:   1,
		RerankPacing: &RerankPacing{IntervalMS: 65000, TotalWaitMS: 3185000, Reason: "account rate limit"},
	}
	report, err := RescoreRun(run)
	if err != nil {
		t.Fatal(err)
	}
	if report.RerankPacing == nil || report.RerankPacing.IntervalMS != 65000 || report.RerankPacing.TotalWaitMS != 3185000 {
		t.Fatalf("rescore dropped pacing evidence: %+v", report.RerankPacing)
	}
}

func TestDecideRerankGateRejectsPacedRunAsDeploymentEvidence(t *testing.T) {
	base := gateBaseline()
	paced := &RerankPacing{IntervalMS: 65000, TotalWaitMS: 3185000, Reason: "account rate limit"}

	deployment := hostedGateCandidate("deployment_gate")
	deployment.RerankPacing = paced
	got := DecideRerankGate(RerankGateInput{Gate: "deployment_gate", Baseline: base, Candidate: deployment, Comparable: true})
	if got.Decision != "INCONCLUSIVE" || !strings.Contains(strings.Join(got.Reasons, "; "), "paced") {
		t.Fatalf("paced deployment evidence decision = %+v", got)
	}

	quality := hostedGateCandidate("quality_diagnostic")
	quality.RerankPacing = &RerankPacing{IntervalMS: 65000, TotalWaitMS: 3185000, Reason: "account rate limit"}
	if got := DecideRerankGate(RerankGateInput{Gate: "quality_diagnostic", Baseline: base, Candidate: quality, Comparable: true}); got.Decision != "QUALITY_PASS" {
		t.Fatalf("paced quality evidence decision = %+v, want QUALITY_PASS", got)
	}

	for name, bad := range map[string]*RerankPacing{
		"negative interval": {IntervalMS: -1, Reason: "account rate limit"},
		"negative wait":     {IntervalMS: 65000, TotalWaitMS: -1, Reason: "account rate limit"},
		"missing reason":    {IntervalMS: 65000},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := hostedGateCandidate("quality_diagnostic")
			candidate.RerankPacing = bad
			if got := DecideRerankGate(RerankGateInput{Gate: "quality_diagnostic", Baseline: base, Candidate: candidate, Comparable: true}); got.Decision != "QUALITY_INCONCLUSIVE" {
				t.Fatalf("invalid pacing decision = %+v, want QUALITY_INCONCLUSIVE", got)
			}
		})
	}
}
