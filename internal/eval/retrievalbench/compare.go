package retrievalbench

import (
	"encoding/json"
	"fmt"
	"reflect"
)

func CompareReports(base, candidate MetricReport, diagnosticOverride bool) (ComparisonReport, error) {
	fields := []string{}
	if base.Fingerprint.Dataset != candidate.Fingerprint.Dataset {
		fields = append(fields, "dataset")
	}
	if base.Fingerprint.Revision != candidate.Fingerprint.Revision {
		fields = append(fields, "revision")
	}
	if base.Fingerprint.QrelsSHA256 != candidate.Fingerprint.QrelsSHA256 {
		fields = append(fields, "qrels_sha256")
	}
	if base.Fingerprint.CorpusSHA256 != candidate.Fingerprint.CorpusSHA256 {
		fields = append(fields, "corpus_sha256")
	}
	if base.Fingerprint.EmbeddingModel != candidate.Fingerprint.EmbeddingModel {
		fields = append(fields, "embedding_model")
	}
	if base.Fingerprint.EmbeddingDigest != candidate.Fingerprint.EmbeddingDigest {
		fields = append(fields, "embedding_digest")
	}
	if base.Fingerprint.EmbeddingDimension != candidate.Fingerprint.EmbeddingDimension {
		fields = append(fields, "embedding_dimension")
	}
	if base.Fingerprint.SelectionConfigSHA256 != candidate.Fingerprint.SelectionConfigSHA256 {
		fields = append(fields, "selection_config_sha256")
	}
	if base.Fingerprint.ChunkConfig != candidate.Fingerprint.ChunkConfig {
		fields = append(fields, "chunk_config")
	}
	if base.Fingerprint.RetrievalConfig != candidate.Fingerprint.RetrievalConfig {
		fields = append(fields, "retrieval_config")
	}
	if base.Fingerprint.ServiceEndpointID != candidate.Fingerprint.ServiceEndpointID {
		fields = append(fields, "service_endpoint_id")
	}
	if base.Fingerprint.RerankEnabled != candidate.Fingerprint.RerankEnabled ||
		base.Fingerprint.RerankModelName != candidate.Fingerprint.RerankModelName ||
		base.Fingerprint.RerankModelDigest != candidate.Fingerprint.RerankModelDigest ||
		base.Fingerprint.RerankCandidateLimit != candidate.Fingerprint.RerankCandidateLimit ||
		base.Fingerprint.RerankTimeoutMS != candidate.Fingerprint.RerankTimeoutMS {
		fields = append(fields, "rerank_config")
	}
	if !reflect.DeepEqual(base.Fingerprint.QueryIDs, candidate.Fingerprint.QueryIDs) {
		fields = append(fields, "query_ids")
	}
	if !reflect.DeepEqual(base.Fingerprint.DocumentIDs, candidate.Fingerprint.DocumentIDs) {
		fields = append(fields, "document_ids")
	}
	if base.Fingerprint.MetricVersion != candidate.Fingerprint.MetricVersion {
		fields = append(fields, "metric_version")
	}
	if normalizeRunMode(base.Fingerprint.RunMode) != normalizeRunMode(candidate.Fingerprint.RunMode) {
		fields = append(fields, "run_mode")
	}
	if fmt.Sprint(base.Fingerprint.K) != fmt.Sprint(candidate.Fingerprint.K) {
		fields = append(fields, "k")
	}
	if len(fields) > 0 && !diagnosticOverride {
		return ComparisonReport{}, fmt.Errorf("incompatible reports: %v", fields)
	}
	if len(fields) > 0 {
		return ComparisonReport{Status: "NON_COMPARABLE", NonComparable: true}, nil
	}
	out := ComparisonReport{Status: "IDENTICAL"}
	for i := 0; i < len(base.Aggregates) && i < len(candidate.Aggregates); i++ {
		a, b := base.Aggregates[i], candidate.Aggregates[i]
		d := AggregateMetrics{K: a.K, Recall: b.Recall - a.Recall, Precision: b.Precision - a.Precision, MRR: b.MRR - a.MRR, MAP: b.MAP - a.MAP, NDCG: b.NDCG - a.NDCG}
		out.Deltas = append(out.Deltas, d)
		if d.Recall != 0 || d.MRR != 0 || d.NDCG != 0 {
			out.Status = "CHANGED"
		}
	}
	classificationK, err := queryClassificationK(base, candidate)
	if err != nil {
		return ComparisonReport{}, err
	}
	bm := map[string]QueryMetrics{}
	for _, m := range base.Queries {
		if m.K == classificationK {
			bm[m.QueryID] = m
		}
	}
	cm := map[string]QueryMetrics{}
	for _, m := range candidate.Queries {
		if m.K == classificationK {
			cm[m.QueryID] = m
		}
	}
	seen := make(map[string]bool, len(bm))
	for _, metric := range base.Queries {
		if metric.K != classificationK || seen[metric.QueryID] {
			continue
		}
		seen[metric.QueryID] = true
		a := bm[metric.QueryID]
		m, ok := cm[a.QueryID]
		if !ok {
			return ComparisonReport{}, fmt.Errorf("missing candidate query metric %q at K=%d", a.QueryID, classificationK)
		}
		score := (m.Recall - a.Recall) + (m.MRR - a.MRR) + (m.NDCG - a.NDCG)
		if score > 0 {
			out.ImprovedQueries = appendUnique(out.ImprovedQueries, a.QueryID)
		} else if score < 0 {
			out.RegressedQueries = appendUnique(out.RegressedQueries, a.QueryID)
		} else {
			out.UnchangedQueries = appendUnique(out.UnchangedQueries, a.QueryID)
		}
	}
	return out, nil
}

// queryClassificationK selects the decision's single query-level metric
// bucket. Quality and MIRACL reports include K=10; small unit fixtures may
// only contain another K, so use the highest K shared by both reports there.
func queryClassificationK(base, candidate MetricReport) (int, error) {
	baseKs := map[int]bool{}
	candidateKs := map[int]bool{}
	for _, m := range base.Queries {
		baseKs[m.K] = true
	}
	for _, m := range candidate.Queries {
		candidateKs[m.K] = true
	}
	if baseKs[10] && candidateKs[10] {
		return 10, nil
	}
	selected := -1
	for k := range baseKs {
		if candidateKs[k] && k > selected {
			selected = k
		}
	}
	if selected < 0 {
		return 0, fmt.Errorf("query classification requires a shared metric K")
	}
	return selected, nil
}

// CompareQualityExperiment allows the explicit quality_diagnostic run mode to
// be compared with the historical deployment-gate baseline while retaining
// every other single-variable compatibility check.
func CompareQualityExperiment(base, candidate MetricReport) (ComparisonReport, error) {
	if candidate.Fingerprint.RunMode != "quality_diagnostic" {
		return ComparisonReport{}, fmt.Errorf("candidate run_mode must be quality_diagnostic")
	}
	candidate.Fingerprint.RunMode = "deployment_gate"
	out, err := CompareRerankExperiment(base, candidate)
	if err != nil {
		return ComparisonReport{}, err
	}
	out.ExperimentVariable = "rerank_quality_diagnostic"
	return out, nil
}

func normalizeRunMode(value string) string {
	if value == "" {
		return "deployment_gate"
	}
	return value
}

// CompareRerankExperiment is the only comparison path that permits rerank
// configuration to differ. All other compatibility fields still go through
// the strict ordinary comparison.
func CompareRerankExperiment(base, candidate MetricReport) (ComparisonReport, error) {
	base.Fingerprint.RerankEnabled = false
	base.Fingerprint.RerankModelName = ""
	base.Fingerprint.RerankModelDigest = ""
	base.Fingerprint.RerankCandidateLimit = 0
	base.Fingerprint.RerankTimeoutMS = 0
	candidate.Fingerprint.RerankEnabled = false
	candidate.Fingerprint.RerankModelName = ""
	candidate.Fingerprint.RerankModelDigest = ""
	candidate.Fingerprint.RerankCandidateLimit = 0
	candidate.Fingerprint.RerankTimeoutMS = 0
	base.Fingerprint.RetrievalConfig = withoutRerankRetrievalConfig(base.Fingerprint.RetrievalConfig)
	candidate.Fingerprint.RetrievalConfig = withoutRerankRetrievalConfig(candidate.Fingerprint.RetrievalConfig)
	out, err := CompareReports(base, candidate, false)
	if err != nil {
		return ComparisonReport{}, err
	}
	out.ExperimentVariable = "rerank"
	return out, nil
}

func withoutRerankRetrievalConfig(value string) string {
	var config map[string]any
	if err := json.Unmarshal([]byte(value), &config); err != nil {
		return value
	}
	delete(config, "rerank_enabled")
	encoded, err := json.Marshal(config)
	if err != nil {
		return value
	}
	return string(encoded)
}
func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}
