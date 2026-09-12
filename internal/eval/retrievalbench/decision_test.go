package retrievalbench

import "testing"

func TestDecideRerankReturnsThreeStableOutcomes(t *testing.T) {
	base := testReport()
	base.Aggregates = []AggregateMetrics{{K: 10, Recall: .976, MRR: .626746, NDCG: .696447}}
	candidate := base
	candidate.Complete = true
	candidate.Aggregates = []AggregateMetrics{{K: 10, Recall: .976, MRR: .7, NDCG: .8}}
	candidate.RerankIdentity = &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "r", License: "Apache-2.0", Ready: true}
	candidate.RerankStats = &RerankPhaseStats{RequestCount: 50, SuccessCount: 50, HifyEnabledCount: 50, HifyAppliedCount: 50, HifyOutcome: "all_applied"}
	candidate.QueryCount = 50

	decision := DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: true})
	if decision.Decision != "ADOPT" {
		t.Fatalf("decision = %+v, want ADOPT", decision)
	}

	candidate.Aggregates[0].Recall = .9
	decision = DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: true})
	if decision.Decision != "DO_NOT_ADOPT" {
		t.Fatalf("decision = %+v, want DO_NOT_ADOPT", decision)
	}

	candidate.Complete = false
	decision = DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: false})
	if decision.Decision != "INCONCLUSIVE" {
		t.Fatalf("decision = %+v, want INCONCLUSIVE", decision)
	}
}

func TestValidateRerankEvidenceRejectsCounterInconsistencyAndNonFiniteLatency(t *testing.T) {
	report := MetricReport{
		Complete: true, QueryCount: 50,
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "r", License: "Apache-2.0", Ready: true},
		RerankStats:    &RerankPhaseStats{RequestCount: 50, SuccessCount: 49, FailureCount: 1, HifyEnabledCount: 50, HifyAppliedCount: 50, HifyOutcome: "all_applied", SteadyP50MS: 1, SteadyP95MS: 2},
	}
	if err := ValidateRerankEvidence(report); err != nil {
		t.Fatal(err)
	}
	report.RerankStats.SuccessCount = 48
	if err := ValidateRerankEvidence(report); err == nil {
		t.Fatal("expected request counter inconsistency")
	}
}

func TestDecideRerankRejectsEqualThresholdsRecallDeclineAndFailure(t *testing.T) {
	base := MetricReport{Complete: true, QueryCount: 50, Aggregates: []AggregateMetrics{{K: 10, Recall: .98, MRR: .7, NDCG: .8}}}
	valid := func() MetricReport {
		return MetricReport{
			Complete: true, QueryCount: 50,
			Aggregates:     []AggregateMetrics{{K: 10, Recall: .98, MRR: .7, NDCG: .8}},
			RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
			RerankStats:    &RerankPhaseStats{RequestCount: 50, SuccessCount: 50, HifyEnabledCount: 50, HifyAppliedCount: 50, HifyOutcome: "all_applied"},
		}
	}
	for name, mutate := range map[string]func(*MetricReport){
		"equal fixed thresholds": func(r *MetricReport) {
			r.Aggregates[0].MRR = RerankMRR10Threshold
			r.Aggregates[0].NDCG = RerankNDCG10Threshold
		},
		"recall decline": func(r *MetricReport) { r.Aggregates[0].Recall = .97 },
		"failure": func(r *MetricReport) {
			r.RerankStats.SuccessCount = 49
			r.RerankStats.FailureCount = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid()
			mutate(&candidate)
			got := DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: true})
			if got.Decision != "DO_NOT_ADOPT" {
				t.Fatalf("decision = %+v, want DO_NOT_ADOPT", got)
			}
		})
	}
}

func TestDecideRerankReturnsInconclusiveForMissingEvidenceOrIncompatibleReports(t *testing.T) {
	base := testReport()
	candidate := testReport()
	candidate.Complete = true
	candidate.Aggregates = []AggregateMetrics{{K: 10, Recall: .98, MRR: .7, NDCG: .8}}
	if got := DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: true}); got.Decision != "INCONCLUSIVE" {
		t.Fatalf("missing evidence decision = %+v", got)
	}
	if got := DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: false}); got.Decision != "INCONCLUSIVE" {
		t.Fatalf("incompatible decision = %+v", got)
	}
}

func TestDecideRerankRejectsSidecarSuccessWhenHifyAllQueriesDegraded(t *testing.T) {
	base := MetricReport{Complete: true, QueryCount: 50, Aggregates: []AggregateMetrics{{K: 10, Recall: .9, MRR: .5, NDCG: .5}}}
	candidate := MetricReport{
		Complete: true, QueryCount: 50,
		Aggregates:     []AggregateMetrics{{K: 10, Recall: .99, MRR: .8, NDCG: .8}},
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
		RerankStats: &RerankPhaseStats{
			RequestCount: 50, SuccessCount: 50, HifyAppliedCount: 0, HifyDegradedCount: 50,
			HifyOutcome: "not_all_applied_or_degraded", SteadyP50MS: 7, SteadyP95MS: 11,
		},
	}
	decision := DecideRerank(RerankDecisionInput{Baseline: base, Candidate: candidate, Comparable: true})
	if decision.Decision == "ADOPT" {
		t.Fatalf("sidecar success with Hify degradation must not be adoptable: %+v", decision)
	}
}

func TestDecideQualityReturnsPassFailAndInconclusive(t *testing.T) {
	base := MetricReport{
		QueryCount: 50, Complete: true,
		Aggregates: []AggregateMetrics{{K: 10, Recall: .976, MRR: .626746, NDCG: .696447}},
	}
	quality := func() MetricReport {
		return MetricReport{
			QueryCount: 50, Complete: true,
			Fingerprint:    Fingerprint{RunMode: "quality_diagnostic"},
			Aggregates:     []AggregateMetrics{{K: 10, Recall: .976, MRR: .7, NDCG: .8}},
			RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
			RerankStats:    &RerankPhaseStats{RequestCount: 50, SuccessCount: 50, HifyEnabledCount: 50, HifyAppliedCount: 50, HifyDegradedCount: 0, HifyOutcome: "all_applied", SteadyP50MS: 7, SteadyP95MS: 11},
		}
	}
	decision := DecideQuality(QualityDecisionInput{Baseline: base, Candidate: quality(), Comparable: true})
	if decision.Decision != "QUALITY_PASS" {
		t.Fatalf("quality pass decision = %+v", decision)
	}
	fail := quality()
	fail.Aggregates[0].MRR = base.Aggregates[0].MRR
	decision = DecideQuality(QualityDecisionInput{Baseline: base, Candidate: fail, Comparable: true})
	if decision.Decision != "QUALITY_FAIL" {
		t.Fatalf("quality fail decision = %+v", decision)
	}
	incomplete := quality()
	incomplete.Complete = false
	incomplete.RerankStats.HifyAppliedCount = 0
	incomplete.RerankStats.HifyDegradedCount = 50
	decision = DecideQuality(QualityDecisionInput{Baseline: base, Candidate: incomplete, Comparable: true})
	if decision.Decision != "QUALITY_INCONCLUSIVE" {
		t.Fatalf("quality incomplete decision = %+v", decision)
	}
}

func TestDecideQualityRequiresAllHifyReranksAndNoQueryFailures(t *testing.T) {
	base := MetricReport{QueryCount: 50, Complete: true, Aggregates: []AggregateMetrics{{K: 10, Recall: .9, MRR: .5, NDCG: .5}}}
	candidate := MetricReport{
		QueryCount: 50, Complete: true, FailedQueryCount: 1,
		Fingerprint:    Fingerprint{RunMode: "quality_diagnostic"},
		Aggregates:     []AggregateMetrics{{K: 10, Recall: .99, MRR: .8, NDCG: .8}},
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
		RerankStats:    &RerankPhaseStats{RequestCount: 50, SuccessCount: 50, HifyEnabledCount: 50, HifyAppliedCount: 49, HifyDegradedCount: 1, HifyOutcome: "not_all_applied_or_degraded", SteadyP50MS: 7, SteadyP95MS: 11},
	}
	decision := DecideQuality(QualityDecisionInput{Baseline: base, Candidate: candidate, Comparable: true})
	if decision.Decision != "QUALITY_INCONCLUSIVE" {
		t.Fatalf("incomplete quality evidence decision = %+v", decision)
	}
}

func TestDecideQualityRejectsIncompleteSidecarEvidence(t *testing.T) {
	base := MetricReport{QueryCount: 50, Complete: true, Aggregates: []AggregateMetrics{{K: 10, Recall: .9, MRR: .5, NDCG: .5}}}
	candidate := MetricReport{
		QueryCount: 50, Complete: true,
		Fingerprint:    Fingerprint{RunMode: "quality_diagnostic"},
		Aggregates:     []AggregateMetrics{{K: 10, Recall: .99, MRR: .8, NDCG: .8}},
		RerankIdentity: &RerankModelIdentity{ModelName: "BAAI/bge-reranker-v2-m3", Revision: "rev", License: "Apache-2.0", Ready: true},
		RerankStats: &RerankPhaseStats{
			RequestCount: 49, SuccessCount: 49, FailureCount: 0, DegradedCount: 0,
			HifyEnabledCount: 50, HifyAppliedCount: 50, HifyDegradedCount: 0, HifyOutcome: "all_applied",
			SteadyP50MS: 7, SteadyP95MS: 11,
		},
	}
	decision := DecideQuality(QualityDecisionInput{Baseline: base, Candidate: candidate, Comparable: true})
	if decision.Decision != "QUALITY_INCONCLUSIVE" {
		t.Fatalf("incomplete sidecar evidence decision = %+v", decision)
	}
}
