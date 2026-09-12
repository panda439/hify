package retrievalbench

import (
	"fmt"
	"math"
)

const (
	RerankRecall10Threshold = 0.976
	RerankMRR10Threshold    = 0.626746
	RerankNDCG10Threshold   = 0.696447
)

type RerankDecisionInput struct {
	Baseline   MetricReport
	Candidate  MetricReport
	Comparable bool
}

type RerankDecision struct {
	Decision string   `json:"decision"`
	Reasons  []string `json:"reasons"`
}

func DecideRerank(input RerankDecisionInput) RerankDecision {
	if !input.Comparable {
		return RerankDecision{Decision: "INCONCLUSIVE", Reasons: []string{"reports are not comparable"}}
	}
	if !input.Candidate.Complete {
		return RerankDecision{Decision: "INCONCLUSIVE", Reasons: []string{"candidate run is incomplete"}}
	}
	if err := ValidateRerankEvidence(input.Candidate); err != nil {
		return RerankDecision{Decision: "INCONCLUSIVE", Reasons: []string{err.Error()}}
	}
	base, baseOK := metricAt10(input.Baseline)
	candidate, candidateOK := metricAt10(input.Candidate)
	if !baseOK || !candidateOK {
		return RerankDecision{Decision: "INCONCLUSIVE", Reasons: []string{"missing @10 aggregate metrics"}}
	}
	reasons := make([]string, 0, 4)
	if candidate.MRR <= RerankMRR10Threshold || candidate.MRR <= base.MRR {
		reasons = append(reasons, fmt.Sprintf("mrr@10 %.6f does not exceed %.6f", candidate.MRR, RerankMRR10Threshold))
	}
	if candidate.NDCG <= RerankNDCG10Threshold || candidate.NDCG <= base.NDCG {
		reasons = append(reasons, fmt.Sprintf("ndcg@10 %.6f does not exceed %.6f", candidate.NDCG, RerankNDCG10Threshold))
	}
	if candidate.Recall < RerankRecall10Threshold || candidate.Recall < base.Recall {
		reasons = append(reasons, fmt.Sprintf("recall@10 %.6f is below %.6f or baseline", candidate.Recall, RerankRecall10Threshold))
	}
	if input.Candidate.QueryCount != 50 || input.Candidate.RerankStats == nil || input.Candidate.RerankStats.HifyEnabledCount != 50 || input.Candidate.RerankStats.HifyAppliedCount != 50 || input.Candidate.RerankStats.HifyDegradedCount != 0 {
		reasons = append(reasons, "candidate does not have 50 successful non-degraded rerank requests")
	}
	if len(reasons) > 0 {
		return RerankDecision{Decision: "DO_NOT_ADOPT", Reasons: reasons}
	}
	return RerankDecision{Decision: "ADOPT", Reasons: []string{"all fixed adoption gates passed"}}
}

type metric10 struct{ Recall, MRR, NDCG float64 }

func metricAt10(report MetricReport) (metric10, bool) {
	for _, aggregate := range report.Aggregates {
		if aggregate.K == 10 {
			return metric10{aggregate.Recall, aggregate.MRR, aggregate.NDCG}, true
		}
	}
	return metric10{}, false
}

func ValidateRerankEvidence(report MetricReport) error {
	if report.RerankIdentity == nil || !report.RerankIdentity.Ready {
		return fmt.Errorf("rerank model identity is not ready")
	}
	// 014：托管 API 没有 sidecar 计数，走独立的证据规则；本地 sidecar 保持 012 原样。
	if report.RerankIdentity.Source == RerankSourceHostedAPI {
		return validateHostedRerankEvidence(report)
	}
	if report.RerankIdentity.ModelName != "BAAI/bge-reranker-v2-m3" || report.RerankIdentity.License != "Apache-2.0" {
		return fmt.Errorf("rerank model identity does not match fixed model")
	}
	if report.RerankStats == nil {
		return fmt.Errorf("rerank phase stats are missing")
	}
	s := report.RerankStats
	if s.RequestCount != s.SuccessCount+s.FailureCount {
		return fmt.Errorf("rerank request counters are inconsistent")
	}
	if s.SuccessCount < 0 || s.FailureCount < 0 || s.DegradedCount < 0 || s.DegradedCount > s.RequestCount {
		return fmt.Errorf("rerank counters are invalid")
	}
	if s.HifyEnabledCount < 0 || s.HifyAppliedCount < 0 || s.HifyDegradedCount < 0 || s.HifyAppliedCount > s.HifyEnabledCount || s.HifyDegradedCount > s.HifyEnabledCount {
		return fmt.Errorf("hify rerank counters are invalid")
	}
	if report.QueryCount != s.HifyEnabledCount || s.HifyAppliedCount != report.QueryCount || s.HifyDegradedCount != 0 {
		return fmt.Errorf("hify rerank outcome is incomplete")
	}
	if s.HifyOutcome != "all_applied" {
		return fmt.Errorf("hify rerank outcome is incomplete")
	}
	if math.IsNaN(s.SteadyP50MS) || math.IsInf(s.SteadyP50MS, 0) || math.IsNaN(s.SteadyP95MS) || math.IsInf(s.SteadyP95MS, 0) {
		return fmt.Errorf("rerank latency is not finite")
	}
	return nil
}
