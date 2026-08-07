package eval

import "testing"

func retrieval(rank int, documentID string) RetrievalResult {
	return RetrievalResult{Rank: rank, DocumentID: documentID}
}

func citation(documentID string) CitationResult {
	return CitationResult{DocumentID: documentID}
}

func TestComputeRAGMetrics_ExpectedDocumentRankedFirst(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-1"}}
	m := computeRAGMetrics(tc, []RetrievalResult{retrieval(1, "doc-1"), retrieval(2, "doc-2")}, nil)

	if !m.RetrievalHit.Evaluated || !m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit = %+v, want evaluated=true value=true", m.RetrievalHit)
	}
	if !m.MRR.Evaluated || m.MRR.Value != 1.0 {
		t.Fatalf("MRR = %+v, want evaluated=true value=1.0", m.MRR)
	}
}

func TestComputeRAGMetrics_ExpectedDocumentRankedThird(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-3"}}
	retrievals := []RetrievalResult{retrieval(1, "doc-1"), retrieval(2, "doc-2"), retrieval(3, "doc-3")}
	m := computeRAGMetrics(tc, retrievals, nil)

	if !m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit.Value = false, want true")
	}
	want := 1.0 / 3.0
	if m.MRR.Value != want {
		t.Fatalf("MRR.Value = %v, want %v", m.MRR.Value, want)
	}
}

func TestComputeRAGMetrics_CompleteMiss(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-missing"}}
	retrievals := []RetrievalResult{retrieval(1, "doc-1"), retrieval(2, "doc-2")}
	m := computeRAGMetrics(tc, retrievals, nil)

	if !m.RetrievalHit.Evaluated || m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit = %+v, want evaluated=true value=false", m.RetrievalHit)
	}
	if !m.MRR.Evaluated || m.MRR.Value != 0 {
		t.Fatalf("MRR = %+v, want evaluated=true value=0", m.MRR)
	}
}

func TestComputeRAGMetrics_MultipleExpectedDocs_HitAtRankOneOnAnyOfThem(t *testing.T) {
	// ExpectedDocumentIDs is a set of acceptable documents, not a priority
	// list — rank 1 matching *any* of them is enough for MRR=1.
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-2", "doc-1"}}
	retrievals := []RetrievalResult{retrieval(1, "doc-1"), retrieval(2, "doc-2")}
	m := computeRAGMetrics(tc, retrievals, nil)

	if !m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit.Value = false, want true")
	}
	if m.MRR.Value != 1.0 {
		t.Fatalf("MRR.Value = %v, want 1.0 (rank-1 retrieval matches an expected document)", m.MRR.Value)
	}
}

func TestComputeRAGMetrics_MultipleExpectedDocs_FirstHitAtRankThree(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-a", "doc-b"}}
	retrievals := []RetrievalResult{
		retrieval(1, "doc-unrelated-1"),
		retrieval(2, "doc-unrelated-2"),
		retrieval(3, "doc-b"),
	}
	m := computeRAGMetrics(tc, retrievals, nil)

	if !m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit.Value = false, want true")
	}
	want := 1.0 / 3.0
	if m.MRR.Value != want {
		t.Fatalf("MRR.Value = %v, want %v (first match against the expected set is at rank 3)", m.MRR.Value, want)
	}
}

func TestComputeRAGMetrics_MRRUnaffectedByExpectedDocumentIDsOrder(t *testing.T) {
	retrievals := []RetrievalResult{retrieval(1, "doc-1"), retrieval(2, "doc-2")}

	forward := computeRAGMetrics(TestCase{ExpectedDocumentIDs: []string{"doc-2", "doc-1"}}, retrievals, nil)
	reversed := computeRAGMetrics(TestCase{ExpectedDocumentIDs: []string{"doc-1", "doc-2"}}, retrievals, nil)

	if forward.MRR.Value != reversed.MRR.Value {
		t.Fatalf("MRR depends on ExpectedDocumentIDs order: forward=%v reversed=%v", forward.MRR.Value, reversed.MRR.Value)
	}
	if forward.MRR.Value != 1.0 {
		t.Fatalf("MRR.Value = %v, want 1.0 regardless of order", forward.MRR.Value)
	}
}

func TestComputeRAGMetrics_MRRUsesLowestRankOnRepeatedDocument(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-1"}}
	// doc-1 appears twice; the lower rank (2) must win, not the later one.
	retrievals := []RetrievalResult{
		retrieval(1, "doc-other"),
		retrieval(2, "doc-1"),
		retrieval(3, "doc-1"),
	}
	m := computeRAGMetrics(tc, retrievals, nil)

	want := 1.0 / 2.0
	if m.MRR.Value != want {
		t.Fatalf("MRR.Value = %v, want %v (must use the lowest rank of the repeated document)", m.MRR.Value, want)
	}
}

func TestComputeRAGMetrics_RetrievalHitAndExpectedDocumentCitedUseSetSemanticsAcrossMultipleExpectedDocs(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-1", "doc-2"}}
	m := computeRAGMetrics(tc, []RetrievalResult{retrieval(1, "doc-2")}, []CitationResult{citation("doc-1")})

	if !m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit.Value = false, want true (doc-2 is in the expected set)")
	}
	if !m.ExpectedDocumentCited.Value {
		t.Fatalf("ExpectedDocumentCited.Value = false, want true (doc-1 is in the expected set)")
	}
}

func TestComputeRAGMetrics_ExpectedDocumentIDsUnset(t *testing.T) {
	tc := TestCase{RequireCitation: true}
	m := computeRAGMetrics(tc, []RetrievalResult{retrieval(1, "doc-1")}, []CitationResult{citation("doc-1")})

	if m.RetrievalHit.Evaluated {
		t.Fatalf("RetrievalHit.Evaluated = true, want false when ExpectedDocumentIDs unset")
	}
	if m.MRR.Evaluated {
		t.Fatalf("MRR.Evaluated = true, want false when ExpectedDocumentIDs unset")
	}
	if m.ExpectedDocumentCited.Evaluated {
		t.Fatalf("ExpectedDocumentCited.Evaluated = true, want false when ExpectedDocumentIDs unset")
	}
}

func TestComputeRAGMetrics_RequireCitationSatisfied(t *testing.T) {
	tc := TestCase{RequireCitation: true}
	m := computeRAGMetrics(tc, nil, []CitationResult{citation("doc-1")})

	if !m.CitationRequirementMet.Evaluated || !m.CitationRequirementMet.Value {
		t.Fatalf("CitationRequirementMet = %+v, want evaluated=true value=true", m.CitationRequirementMet)
	}
}

func TestComputeRAGMetrics_RequireCitationUnsatisfied(t *testing.T) {
	tc := TestCase{RequireCitation: true}
	m := computeRAGMetrics(tc, nil, nil)

	if !m.CitationRequirementMet.Evaluated || m.CitationRequirementMet.Value {
		t.Fatalf("CitationRequirementMet = %+v, want evaluated=true value=false", m.CitationRequirementMet)
	}
}

func TestComputeRAGMetrics_CitationPointsAtNonExpectedDocument(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-expected"}}
	m := computeRAGMetrics(tc, []RetrievalResult{retrieval(1, "doc-expected")}, []CitationResult{citation("doc-other")})

	if !m.ExpectedDocumentCited.Evaluated || m.ExpectedDocumentCited.Value {
		t.Fatalf("ExpectedDocumentCited = %+v, want evaluated=true value=false", m.ExpectedDocumentCited)
	}
}

func TestComputeRAGMetrics_RecallAtKHitWithinCutoff(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-3"}}
	retrievals := []RetrievalResult{retrieval(1, "doc-1"), retrieval(2, "doc-2"), retrieval(3, "doc-3")}
	m := computeRAGMetrics(tc, retrievals, nil)

	if !m.RecallAt1.Evaluated || m.RecallAt1.Value {
		t.Fatalf("RecallAt1 = %+v, want evaluated=true value=false (match is at rank 3, outside top-1)", m.RecallAt1)
	}
	if !m.RecallAt3.Evaluated || !m.RecallAt3.Value {
		t.Fatalf("RecallAt3 = %+v, want evaluated=true value=true (match is at rank 3, within top-3)", m.RecallAt3)
	}
}

func TestComputeRAGMetrics_RecallAtKMissOutsideCutoff(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-5"}}
	retrievals := []RetrievalResult{
		retrieval(1, "doc-1"), retrieval(2, "doc-2"), retrieval(3, "doc-3"),
		retrieval(4, "doc-4"), retrieval(5, "doc-5"),
	}
	m := computeRAGMetrics(tc, retrievals, nil)

	if m.RecallAt1.Value || m.RecallAt3.Value {
		t.Fatalf("RecallAt1/RecallAt3 = %+v/%+v, want both false (match is at rank 5, outside both cutoffs)", m.RecallAt1, m.RecallAt3)
	}
	// RetrievalHit still true — it looks at the whole retrieved set, not
	// just top-K, which is exactly the distinction RecallAt1/3 add.
	if !m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit.Value = false, want true (RetrievalHit is not rank-bounded)")
	}
}

func TestComputeRAGMetrics_RecallAtKUnevaluatedWithoutExpectedDocumentIDs(t *testing.T) {
	tc := TestCase{RequireCitation: true}
	m := computeRAGMetrics(tc, []RetrievalResult{retrieval(1, "doc-1")}, nil)

	if m.RecallAt1.Evaluated || m.RecallAt3.Evaluated {
		t.Fatalf("RecallAt1/RecallAt3 = %+v/%+v, want Evaluated=false when ExpectedDocumentIDs unset", m.RecallAt1, m.RecallAt3)
	}
}

func TestComputeRAGMetrics_RetrievalEmpty(t *testing.T) {
	tc := TestCase{ExpectedDocumentIDs: []string{"doc-1"}}
	m := computeRAGMetrics(tc, nil, nil)

	if !m.RetrievalHit.Evaluated || m.RetrievalHit.Value {
		t.Fatalf("RetrievalHit = %+v, want evaluated=true value=false on empty retrieval", m.RetrievalHit)
	}
	if !m.MRR.Evaluated || m.MRR.Value != 0 {
		t.Fatalf("MRR = %+v, want evaluated=true value=0 on empty retrieval", m.MRR)
	}
}
