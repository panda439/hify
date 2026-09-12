package retrievalbench

import (
	"fmt"
	"strings"
)

// 014：模型无关的双门禁决策，阈值在真实评测前固定（spec「预先固定决策标准」）。
// 与 012 的 DecideRerank/DecideQuality 并存——BGE 的既有结论原样引用，
// 不按这套新标准回算。
const (
	GateDeployment = "deployment_gate"
	GateQuality    = "quality_diagnostic"
)

type RerankGateInput struct {
	Gate       string
	Baseline   MetricReport
	Candidate  MetricReport
	Comparable bool
}

type gateLabels struct{ pass, fail, inconclusive string }

func labelsForGate(gate string) (gateLabels, bool) {
	switch gate {
	case GateDeployment:
		return gateLabels{pass: "ADOPT", fail: "DO_NOT_ADOPT", inconclusive: "INCONCLUSIVE"}, true
	case GateQuality:
		return gateLabels{pass: "QUALITY_PASS", fail: "QUALITY_FAIL", inconclusive: "QUALITY_INCONCLUSIVE"}, true
	}
	return gateLabels{}, false
}

// DecideRerankGate：证据不完整或不可比 → INCONCLUSIVE；证据完整时，
// Recall@10 不低于 baseline、MRR/MAP/NDCG@10 均不低于 baseline 且至少一项
// 提升 → 通过，否则 → 不通过。两个门禁标准相同，只是标签不同。
func DecideRerankGate(in RerankGateInput) RerankDecision {
	labels, ok := labelsForGate(in.Gate)
	if !ok {
		return RerankDecision{Decision: "INCONCLUSIVE", Reasons: []string{fmt.Sprintf("unsupported gate %q", in.Gate)}}
	}
	inconclusive := func(reason string) RerankDecision {
		return RerankDecision{Decision: labels.inconclusive, Reasons: []string{reason}}
	}
	if !in.Comparable {
		return inconclusive("reports are not comparable")
	}
	if mode := normalizeRunMode(in.Candidate.Fingerprint.RunMode); mode != in.Gate {
		return inconclusive(fmt.Sprintf("candidate run_mode %q does not match gate %q", mode, in.Gate))
	}
	// FR-015/FR-016：限速记录必须自洽；限速运行只能回答质量，不能作为部署证据。
	if pacing := in.Candidate.RerankPacing; pacing != nil {
		if err := pacing.validate(); err != nil {
			return inconclusive(err.Error())
		}
		if in.Gate == GateDeployment {
			return inconclusive("paced run cannot be deployment evidence")
		}
	}
	if !in.Candidate.Complete {
		return inconclusive("candidate run is incomplete")
	}
	if in.Candidate.QueryCount != 50 || in.Candidate.FailedQueryCount != 0 {
		return inconclusive("candidate does not contain 50 successful queries")
	}
	if err := ValidateRerankEvidence(in.Candidate); err != nil {
		return inconclusive(err.Error())
	}
	base, baseOK := gateMetricsAt10(in.Baseline)
	candidate, candidateOK := gateMetricsAt10(in.Candidate)
	if !baseOK || !candidateOK {
		return inconclusive("missing @10 aggregate metrics")
	}

	reasons := make([]string, 0, 5)
	for _, m := range []struct {
		name            string
		candidate, base float64
	}{
		{"recall@10", candidate.Recall, base.Recall},
		{"mrr@10", candidate.MRR, base.MRR},
		{"map@10", candidate.MAP, base.MAP},
		{"ndcg@10", candidate.NDCG, base.NDCG},
	} {
		if m.candidate < m.base {
			reasons = append(reasons, fmt.Sprintf("%s %.6f is below baseline %.6f", m.name, m.candidate, m.base))
		}
	}
	if candidate.MRR <= base.MRR && candidate.MAP <= base.MAP && candidate.NDCG <= base.NDCG {
		reasons = append(reasons, "no ranking metric improved over baseline ("+strings.Join([]string{"mrr@10", "map@10", "ndcg@10"}, ", ")+")")
	}
	if len(reasons) > 0 {
		return RerankDecision{Decision: labels.fail, Reasons: reasons}
	}
	return RerankDecision{Decision: labels.pass, Reasons: []string{"all fixed gates passed"}}
}

type gateMetric10 struct{ Recall, MRR, MAP, NDCG float64 }

func gateMetricsAt10(report MetricReport) (gateMetric10, bool) {
	for _, aggregate := range report.Aggregates {
		if aggregate.K == 10 {
			return gateMetric10{aggregate.Recall, aggregate.MRR, aggregate.MAP, aggregate.NDCG}, true
		}
	}
	return gateMetric10{}, false
}
