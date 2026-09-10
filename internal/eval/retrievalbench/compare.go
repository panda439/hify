package retrievalbench

import (
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
	if !reflect.DeepEqual(base.Fingerprint.QueryIDs, candidate.Fingerprint.QueryIDs) {
		fields = append(fields, "query_ids")
	}
	if !reflect.DeepEqual(base.Fingerprint.DocumentIDs, candidate.Fingerprint.DocumentIDs) {
		fields = append(fields, "document_ids")
	}
	if base.Fingerprint.MetricVersion != candidate.Fingerprint.MetricVersion {
		fields = append(fields, "metric_version")
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
	bm := map[string]QueryMetrics{}
	for _, m := range base.Queries {
		bm[m.QueryID+fmt.Sprint(m.K)] = m
	}
	for _, m := range candidate.Queries {
		a, ok := bm[m.QueryID+fmt.Sprint(m.K)]
		if !ok {
			continue
		}
		score := (m.Recall - a.Recall) + (m.MRR - a.MRR) + (m.NDCG - a.NDCG)
		if score > 0 {
			out.ImprovedQueries = appendUnique(out.ImprovedQueries, m.QueryID)
		} else if score < 0 {
			out.RegressedQueries = appendUnique(out.RegressedQueries, m.QueryID)
		} else {
			out.UnchangedQueries = appendUnique(out.UnchangedQueries, m.QueryID)
		}
	}
	return out, nil
}
func appendUnique(xs []string, s string) []string {
	for _, x := range xs {
		if x == s {
			return xs
		}
	}
	return append(xs, s)
}
