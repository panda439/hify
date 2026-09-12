package retrievalbench

import "fmt"

// SanitizeHostedRun returns a copy of a hosted raw run whose query payloads
// contain identifiers only. Scoring uses query IDs, qrels, and rankings, so
// removing Text is lossless for every derived metric and comparison artifact.
func SanitizeHostedRun(run RetrievalRun) (RetrievalRun, error) {
	if run.RerankIdentity == nil || run.RerankIdentity.Source != RerankSourceHostedAPI {
		return RetrievalRun{}, fmt.Errorf("only hosted rerank runs can be sanitized")
	}
	run.Queries = append([]BenchmarkQuery(nil), run.Queries...)
	for i := range run.Queries {
		run.Queries[i].Text = ""
	}
	return run, nil
}
