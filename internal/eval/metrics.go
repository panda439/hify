package eval

// computeRAGMetrics derives the deterministic retrieval/citation metrics
// for one case from what actually happened during the run (retrievals,
// citations) and what the test case expected. Every field's Evaluated flag
// is gated on the corresponding TestCase input being configured — a case
// that never sets ExpectedDocumentIDs gets RetrievalHit/MRR/
// ExpectedDocumentCited all left at Evaluated:false, not a fabricated
// false/zero that would look like an actual miss.
func computeRAGMetrics(tc TestCase, retrievals []RetrievalResult, citations []CitationResult) RAGMetrics {
	var m RAGMetrics

	if len(tc.ExpectedDocumentIDs) > 0 {
		expected := make(map[string]struct{}, len(tc.ExpectedDocumentIDs))
		for _, id := range tc.ExpectedDocumentIDs {
			expected[id] = struct{}{}
		}

		m.RetrievalHit = BoolMetric{Evaluated: true, Value: anyDocumentIn(retrievalDocumentIDs(retrievals), expected)}
		m.MRR = FloatMetric{Evaluated: true, Value: reciprocalRank(retrievals, expected)}
		m.RecallAt1 = BoolMetric{Evaluated: true, Value: recallAtK(retrievals, expected, 1)}
		m.RecallAt3 = BoolMetric{Evaluated: true, Value: recallAtK(retrievals, expected, 3)}
		m.ExpectedDocumentCited = BoolMetric{Evaluated: true, Value: anyDocumentIn(citationDocumentIDs(citations), expected)}
	}

	if tc.RequireCitation {
		m.CitationRequirementMet = BoolMetric{Evaluated: true, Value: len(citations) > 0}
	}

	return m
}

func retrievalDocumentIDs(retrievals []RetrievalResult) []string {
	ids := make([]string, len(retrievals))
	for i, r := range retrievals {
		ids[i] = r.DocumentID
	}
	return ids
}

func citationDocumentIDs(citations []CitationResult) []string {
	ids := make([]string, len(citations))
	for i, c := range citations {
		ids[i] = c.DocumentID
	}
	return ids
}

func anyDocumentIn(documentIDs []string, expected map[string]struct{}) bool {
	for _, id := range documentIDs {
		if _, ok := expected[id]; ok {
			return true
		}
	}
	return false
}

// reciprocalRank returns 1/rank of the lowest-rank retrieval whose
// DocumentID is in expected — standard MRR against a set of acceptable
// documents, not a specific "priority" one. expected is a set (order
// carries no meaning: TestCase.ExpectedDocumentIDs can be listed in any
// order without changing the result). retrievals is expected in the same
// order EventRetrieval delivered it (i.e. Rank ascending), so the first
// matching element encountered is always the lowest-rank one, which also
// gives the "same document repeated, use the first/lowest rank" behavior
// for free. Returns 0 if no retrieval matches any expected document.
func reciprocalRank(retrievals []RetrievalResult, expected map[string]struct{}) float64 {
	for _, r := range retrievals {
		if _, ok := expected[r.DocumentID]; ok {
			return 1 / float64(r.Rank)
		}
	}
	return 0
}

// recallAtK reports whether any retrieval within the top-k ranks (Rank <=
// k, 1-based) matches the expected document set — see RAGMetrics'
// RecallAt1/RecallAt3 doc comment for why this is Hit@K against an
// acceptable-document set rather than fraction-of-relevant-retrieved
// recall. retrievals need not be sorted by Rank for this to be correct
// (every element is checked against the k cutoff independently), unlike
// reciprocalRank which relies on ascending order to short-circuit on the
// first match.
func recallAtK(retrievals []RetrievalResult, expected map[string]struct{}, k int) bool {
	for _, r := range retrievals {
		if r.Rank > k {
			continue
		}
		if _, ok := expected[r.DocumentID]; ok {
			return true
		}
	}
	return false
}
