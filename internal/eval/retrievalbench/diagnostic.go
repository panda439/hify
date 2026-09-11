package retrievalbench

// QueryDiagnostic records contextual signals for a low-scoring query. These
// fields are diagnostic only; they never alter qrels or formal metrics.
type QueryDiagnostic struct {
	QueryID                 string  `json:"query_id"`
	Recall                  float64 `json:"recall"`
	RelevantPassageCount    int     `json:"relevant_passage_count"`
	RelevantReturnedCount   int     `json:"relevant_returned_count"`
	ReturnedUnjudgedCount   int     `json:"returned_unjudged_count"`
	ReturnedTitleMatchCount int     `json:"returned_title_match_count"`
}

func DiagnoseLowScoreQueries(dataset PreparedDataset, run RetrievalRun, threshold float64) []QueryDiagnostic {
	docByID := make(map[string]BenchmarkDocument, len(dataset.Documents))
	for _, document := range dataset.Documents {
		docByID[document.SourceDocumentID] = document
	}
	qrelsByQuery := make(map[string]map[string]int)
	relevantTitlesByQuery := make(map[string]map[string]struct{})
	for _, qrel := range dataset.Qrels {
		if qrelsByQuery[qrel.QueryID] == nil {
			qrelsByQuery[qrel.QueryID] = make(map[string]int)
			relevantTitlesByQuery[qrel.QueryID] = make(map[string]struct{})
		}
		qrelsByQuery[qrel.QueryID][qrel.SourceDocumentID] = qrel.Relevance
		if qrel.Relevance > 0 {
			if title := docByID[qrel.SourceDocumentID].Title; title != "" {
				relevantTitlesByQuery[qrel.QueryID][title] = struct{}{}
			}
		}
	}
	resultByQuery := make(map[string]RawQueryResult, len(run.Results))
	for _, result := range run.Results {
		resultByQuery[result.QueryID] = result
	}
	out := make([]QueryDiagnostic, 0)
	for _, query := range dataset.Queries {
		qrels := qrelsByQuery[query.ID]
		relevantTotal := 0
		for _, relevance := range qrels {
			if relevance > 0 {
				relevantTotal++
			}
		}
		result := resultByQuery[query.ID]
		diagnostic := QueryDiagnostic{QueryID: query.ID, RelevantPassageCount: relevantTotal}
		seen := make(map[string]struct{})
		for _, documentID := range result.DocumentRanking {
			if _, duplicate := seen[documentID]; duplicate {
				continue
			}
			seen[documentID] = struct{}{}
			if relevance, judged := qrels[documentID]; !judged {
				diagnostic.ReturnedUnjudgedCount++
			} else if relevance > 0 {
				diagnostic.RelevantReturnedCount++
			}
			if title := docByID[documentID].Title; title != "" {
				if _, match := relevantTitlesByQuery[query.ID][title]; match {
					diagnostic.ReturnedTitleMatchCount++
				}
			}
		}
		if relevantTotal > 0 {
			diagnostic.Recall = float64(diagnostic.RelevantReturnedCount) / float64(relevantTotal)
		}
		if diagnostic.Recall < threshold {
			out = append(out, diagnostic)
		}
	}
	return out
}
