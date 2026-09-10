package retrievalbench

import "testing"

func TestMetricReportCarriesStageCountsAndLocalCostStatus(t *testing.T) {
	r := MetricReport{Stages: BenchmarkStages{
		Prepare:       PhaseStats{ElapsedMS: 1, DocumentCount: 800},
		Embedding:     PhaseStats{ElapsedMS: 2, DocumentCount: 800, ChunkCount: 822},
		Ingest:        PhaseStats{ElapsedMS: 3, DocumentCount: 800, ChunkCount: 822},
		Query:         PhaseStats{ElapsedMS: 4, QueryCount: 50},
		EmbeddingCost: "not_applicable",
	}}
	if r.Stages.EmbeddingCost != "not_applicable" || r.Stages.Query.QueryCount != 50 || r.Stages.Embedding.ChunkCount != 822 {
		t.Fatalf("stage evidence missing: %+v", r.Stages)
	}
}
