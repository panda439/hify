package retrievalbench

import "fmt"

type QualityDecisionInput struct {
	Baseline   MetricReport
	Candidate  MetricReport
	Comparable bool
}

type QualityDecision struct {
	Decision string   `json:"decision"`
	Reasons  []string `json:"reasons"`
}

// DecideQuality evaluates model quality only after the quality diagnostic has
// proved that every query used Hify's reranker path. It deliberately does not
// return ADOPT: deployment suitability remains the separate 1.5s decision.
func DecideQuality(input QualityDecisionInput) QualityDecision {
	if !input.Comparable {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{"reports are not comparable"}}
	}
	if input.Candidate.Fingerprint.RunMode != "quality_diagnostic" {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{"candidate is not a quality diagnostic run"}}
	}
	if !input.Candidate.Complete {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{"quality diagnostic run is incomplete"}}
	}
	if input.Candidate.QueryCount != 50 || input.Candidate.FailedQueryCount != 0 {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{"quality diagnostic does not contain 50 successful queries"}}
	}
	if err := ValidateRerankEvidence(input.Candidate); err != nil {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{err.Error()}}
	}
	stats := input.Candidate.RerankStats
	if stats.RequestCount != 50 || stats.SuccessCount != 50 || stats.FailureCount != 0 || stats.DegradedCount != 0 {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{"quality diagnostic sidecar evidence is incomplete"}}
	}
	base, baseOK := metricAt10(input.Baseline)
	candidate, candidateOK := metricAt10(input.Candidate)
	if !baseOK || !candidateOK {
		return QualityDecision{Decision: "QUALITY_INCONCLUSIVE", Reasons: []string{"missing @10 aggregate metrics"}}
	}
	reasons := make([]string, 0, 3)
	if candidate.MRR <= base.MRR {
		reasons = append(reasons, fmt.Sprintf("mrr@10 %.6f does not exceed baseline %.6f", candidate.MRR, base.MRR))
	}
	if candidate.NDCG <= base.NDCG {
		reasons = append(reasons, fmt.Sprintf("ndcg@10 %.6f does not exceed baseline %.6f", candidate.NDCG, base.NDCG))
	}
	if candidate.Recall < base.Recall {
		reasons = append(reasons, fmt.Sprintf("recall@10 %.6f is below baseline %.6f", candidate.Recall, base.Recall))
	}
	if len(reasons) != 0 {
		return QualityDecision{Decision: "QUALITY_FAIL", Reasons: reasons}
	}
	return QualityDecision{Decision: "QUALITY_PASS", Reasons: []string{"all quality gates passed"}}
}
