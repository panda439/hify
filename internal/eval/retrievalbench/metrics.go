package retrievalbench

import (
	"fmt"
	"math"
	"sort"
)

var DefaultKs = []int{1, 3, 5, 10}

func ScoreQuery(query BenchmarkQuery, ranking []string, qrels []Qrel, failed bool, ks []int) ([]QueryMetrics, error) {
	if query.ID == "" {
		return nil, fmt.Errorf("query id is required")
	}
	if len(ks) == 0 {
		ks = DefaultKs
	}
	rels := make(map[string]int)
	for _, q := range qrels {
		if q.QueryID != query.ID {
			continue
		}
		if old, ok := rels[q.SourceDocumentID]; ok && old != q.Relevance {
			return nil, fmt.Errorf("conflicting qrel %s/%s", q.QueryID, q.SourceDocumentID)
		}
		rels[q.SourceDocumentID] = q.Relevance
	}
	relevant := 0
	for _, rel := range rels {
		if rel > 0 {
			relevant++
		}
	}
	out := make([]QueryMetrics, 0, len(ks))
	for _, k := range ks {
		if k <= 0 {
			return nil, fmt.Errorf("k must be positive")
		}
		m := QueryMetrics{QueryID: query.ID, K: k, RelevantDocuments: relevant}
		if failed {
			out = append(out, m)
			continue
		}
		n := k
		if len(ranking) < n {
			n = len(ranking)
		}
		m.ReturnedDocuments = n
		for i := 0; i < n; i++ {
			if rels[ranking[i]] > 0 {
				m.TruePositives++
				if m.MRR == 0 {
					m.MRR = 1.0 / float64(i+1)
				}
				m.AP += float64(m.TruePositives) / float64(i+1)
			}
		}
		if relevant > 0 {
			m.Recall = float64(m.TruePositives) / float64(relevant)
			denom := relevant
			if denom > k {
				denom = k
			}
			m.AP /= float64(denom)
		}
		m.Precision = float64(m.TruePositives) / float64(k)
		m.NDCG = ndcg(ranking[:n], rels, k)
		out = append(out, m)
	}
	return out, nil
}

func ndcg(ranking []string, rels map[string]int, k int) float64 {
	dcg := 0.0
	for i, id := range ranking {
		if i >= k {
			break
		}
		if rel := rels[id]; rel > 0 {
			dcg += float64(rel) / math.Log2(float64(i+2))
		}
	}
	ideal := make([]int, 0, len(rels))
	for _, rel := range rels {
		if rel > 0 {
			ideal = append(ideal, rel)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(ideal)))
	idcg := 0.0
	for i, rel := range ideal {
		if i >= k {
			break
		}
		idcg += float64(rel) / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

func ScoreRun(queries []BenchmarkQuery, results []RawQueryResult, qrels []Qrel, ks []int) (MetricReport, error) {
	byID := make(map[string]RawQueryResult, len(results))
	for _, r := range results {
		if _, ok := byID[r.QueryID]; ok {
			return MetricReport{}, fmt.Errorf("duplicate result for query %s", r.QueryID)
		}
		if r.DocumentRanking == nil && len(r.ChunkHits) > 0 {
			r.DocumentRanking = FoldChunkRanking(r.ChunkHits, 0)
		}
		byID[r.QueryID] = r
	}
	all := make([]QueryMetrics, 0, len(queries)*len(ks))
	failed := 0
	for _, q := range queries {
		r, ok := byID[q.ID]
		if !ok {
			failed++
			ms, err := ScoreQuery(q, nil, qrels, true, ks)
			if err != nil {
				return MetricReport{}, err
			}
			all = append(all, ms...)
			continue
		}
		isFailed := r.Error != ""
		if isFailed {
			failed++
		}
		ms, err := ScoreQuery(q, r.DocumentRanking, qrels, isFailed, ks)
		if err != nil {
			return MetricReport{}, err
		}
		all = append(all, ms...)
	}
	if len(ks) == 0 {
		ks = DefaultKs
	}
	aggs := make([]AggregateMetrics, 0, len(ks))
	for _, k := range ks {
		a := AggregateMetrics{K: k}
		count := 0
		for _, m := range all {
			if m.K != k {
				continue
			}
			count++
			a.Recall += m.Recall
			a.Precision += m.Precision
			a.MRR += m.MRR
			a.MAP += m.AP
			a.NDCG += m.NDCG
		}
		if count > 0 {
			f := float64(count)
			a.Recall /= f
			a.Precision /= f
			a.MRR /= f
			a.MAP /= f
			a.NDCG /= f
		}
		aggs = append(aggs, a)
	}
	return MetricReport{Queries: all, Aggregates: aggs, QueryCount: len(queries), FailedQueryCount: failed, Complete: failed == 0}, nil
}
