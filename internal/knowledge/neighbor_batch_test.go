package knowledge

import (
	"context"
	"errors"
	"testing"
)

// neighbor_batch_test.go covers Phase 7's core promise for
// expandWithNeighborWindow: on a normal path, no matter how many anchors or
// distinct document versions are involved, the database is asked exactly
// once. This is deliberately a pure, DB-free test — it constructs a bare
// &service{findNeighborBatch: spy} (service's findNeighborBatch field is
// exactly the "minimal internal interface/pure orchestration helper" the
// task's acceptance criteria asked for; see that field's doc comment in
// service.go) and counts how many times the spy is invoked, instead of
// standing up a real Repository/Postgres connection. The equivalent
// against a real database lives in integration_test.go's Phase 7 tests,
// which assert the SAME single-call behavior end to end.

// spyNeighborBatch counts calls and records every requests slice it was
// handed, so a test can assert both "called exactly once" and "called with
// the right coordinates" without a mocking framework.
type spyNeighborBatch struct {
	calls   int
	lastReq []neighborRequest
	result  []RetrievedChunk
	err     error
}

func (s *spyNeighborBatch) fn(ctx context.Context, requests []neighborRequest) ([]RetrievedChunk, error) {
	s.calls++
	s.lastReq = requests
	return s.result, s.err
}

// Multiple anchors spanning multiple documents AND multiple versions of the
// same document must still only trigger one findNeighborBatch call — this
// is the exact N+1 pattern (one call per document-version group) Phase 7
// exists to eliminate.
func TestExpandWithNeighborWindowCallsBatchExactlyOnceAcrossMultipleDocumentVersions(t *testing.T) {
	anchors := []RetrievedChunk{
		anchorRC("a1", "doc-1", 3, 5, 0.9), // doc-1/v3
		anchorRC("a2", "doc-1", 2, 5, 0.8), // doc-1/v2 — same document, different version
		anchorRC("a3", "doc-2", 1, 0, 0.7), // doc-2/v1 — different document entirely
	}
	spy := &spyNeighborBatch{}
	svc := &service{findNeighborBatch: spy.fn}

	if _, _, err := svc.expandWithNeighborWindow(context.Background(), anchors); err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("findNeighborBatch called %d times, want exactly 1 — 3 anchors across 3 distinct document versions must still be ONE batch call, not one per group", spy.calls)
	}
	if len(spy.lastReq) == 0 {
		t.Fatalf("findNeighborBatch called with an empty request set, want the flattened coordinates for all 3 anchors")
	}
}

// A single anchor is still exactly one call — the "batch" behavior isn't
// conditional on there being more than one document version involved.
func TestExpandWithNeighborWindowCallsBatchExactlyOnceForSingleAnchor(t *testing.T) {
	anchors := []RetrievedChunk{anchorRC("a1", "doc-1", 1, 5, 0.9)}
	spy := &spyNeighborBatch{}
	svc := &service{findNeighborBatch: spy.fn}

	if _, _, err := svc.expandWithNeighborWindow(context.Background(), anchors); err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("findNeighborBatch called %d times, want exactly 1", spy.calls)
	}
}

// Empty anchors must short-circuit before ever reaching findNeighborBatch
// — zero anchors means zero neighbor coordinates, and Retrieve's own
// "nothing found" path shouldn't cost a database round trip.
func TestExpandWithNeighborWindowSkipsBatchCallOnEmptyAnchors(t *testing.T) {
	spy := &spyNeighborBatch{}
	svc := &service{findNeighborBatch: spy.fn}

	got, dupCount, err := svc.expandWithNeighborWindow(context.Background(), nil)
	if err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if spy.calls != 0 {
		t.Fatalf("findNeighborBatch called %d times, want 0 — empty anchors must skip the batch call entirely", spy.calls)
	}
	if len(got) != 0 || dupCount != 0 {
		t.Fatalf("got (%v, %d), want (empty, 0)", got, dupCount)
	}
}

// A generic (non-context) failure from findNeighborBatch must be
// best-effort: Retrieve still gets every anchor back, unmodified, with a
// nil error — exactly the same contract Phase 4's per-group loop offered,
// just now covering the whole batch instead of one group at a time.
func TestExpandWithNeighborWindowDegradesToAnchorsOnlyOnGenericBatchError(t *testing.T) {
	anchors := []RetrievedChunk{
		anchorRC("a1", "doc-1", 1, 5, 0.9),
		anchorRC("a2", "doc-2", 1, 0, 0.8),
	}
	spy := &spyNeighborBatch{err: errors.New("boom: connection reset")}
	svc := &service{findNeighborBatch: spy.fn}

	got, dupCount, err := svc.expandWithNeighborWindow(context.Background(), anchors)
	if err != nil {
		t.Fatalf("expandWithNeighborWindow: %v, want nil — a generic batch failure must be best-effort, not propagate as an error", err)
	}
	if dupCount != 0 {
		t.Fatalf("neighborDuplicateCount = %d, want 0 — no neighbors were fetched, nothing to dedup", dupCount)
	}
	want := idsOf(anchors)
	if got := idsOf(got); !equalIDs(got, want) {
		t.Fatalf("got %v, want %v — every anchor must still be present after a best-effort neighbor batch failure", got, want)
	}
	if spy.calls != 1 {
		t.Fatalf("findNeighborBatch called %d times, want exactly 1 (one attempt, no retry loop)", spy.calls)
	}
}

// context.Canceled/DeadlineExceeded from findNeighborBatch must NOT be
// swallowed as best-effort — it has to propagate all the way out of
// expandWithNeighborWindow (and therefore out of Retrieve), same contract
// classifyRetrieveErr already enforces for the vector/keyword paths.
func TestExpandWithNeighborWindowPropagatesContextCanceledFromBatchCall(t *testing.T) {
	anchors := []RetrievedChunk{anchorRC("a1", "doc-1", 1, 5, 0.9)}
	spy := &spyNeighborBatch{err: context.Canceled}
	svc := &service{findNeighborBatch: spy.fn}

	_, _, err := svc.expandWithNeighborWindow(context.Background(), anchors)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got err=%v, want context.Canceled to propagate instead of being treated as a best-effort miss", err)
	}
	if spy.calls != 1 {
		t.Fatalf("findNeighborBatch called %d times, want exactly 1", spy.calls)
	}
}

// A successful batch call's results still flow through expandWithNeighbors
// exactly as before — this is a smoke test that the plumbing (requests in,
// neighbors out, assembled result) is wired correctly, not a re-test of
// expandWithNeighbors' own assembly rules (covered exhaustively in
// neighbor_test.go).
func TestExpandWithNeighborWindowAssemblesSuccessfulBatchResult(t *testing.T) {
	anchor := anchorRC("a1", "doc-1", 1, 5, 0.9)
	prev := neighborRC("n-prev", "doc-1", 1, 4, "handbook.pdf", nil, nil)
	next := neighborRC("n-next", "doc-1", 1, 6, "handbook.pdf", nil, nil)

	spy := &spyNeighborBatch{result: []RetrievedChunk{prev, next}}
	svc := &service{findNeighborBatch: spy.fn}

	got, dupCount, err := svc.expandWithNeighborWindow(context.Background(), []RetrievedChunk{anchor})
	if err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if dupCount != 0 {
		t.Fatalf("neighborDuplicateCount = %d, want 0", dupCount)
	}
	want := []string{"a1", "n-prev", "n-next"}
	if got := idsOf(got); !equalIDs(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if spy.calls != 1 {
		t.Fatalf("findNeighborBatch called %d times, want exactly 1", spy.calls)
	}
}

// --- Phase 8: admission's effect on neighbor batch calls ---
//
// These drive the real orchestration sequence (rrfFuse -> anchors ->
// expandWithNeighborWindow) rather than hand-building an anchors slice —
// admission itself only exists inside rrfFuse (hybrid.go), so a test that
// skipped rrfFuse and hand-built anchors couldn't prove "a REJECTED
// candidate's coordinates never reach findNeighborBatch", only that
// whatever's already in the anchors slice gets batched correctly (which
// the tests above already cover).

// All candidates rejected by admission -> rrfFuse returns zero anchors ->
// expandWithNeighborWindow must short-circuit before ever calling
// findNeighborBatch, exactly like the hand-built-empty-anchors case above,
// but reached through the real admission path this time.
func TestExpandWithNeighborWindowSkipsBatchCallWhenAdmissionRejectsEveryCandidate(t *testing.T) {
	vectorCandidates := []RetrievedChunk{
		rc("weak-1", 0.1), // below vectorAdmissionThreshold, no keyword signal
		rc("weak-2", 0.2), // below vectorAdmissionThreshold, no keyword signal
	}
	anchors, admission := fuseTopK(vectorCandidates, nil, 10)
	if len(anchors) != 0 {
		t.Fatalf("test setup invalid: anchors = %v, want empty (both candidates should be rejected by admission)", idsOf(anchors))
	}
	if admission.AdmissionRejectedCount != 2 {
		t.Fatalf("test setup invalid: AdmissionRejectedCount = %d, want 2", admission.AdmissionRejectedCount)
	}

	spy := &spyNeighborBatch{}
	svc := &service{findNeighborBatch: spy.fn}

	got, dupCount, err := svc.expandWithNeighborWindow(context.Background(), anchors)
	if err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if spy.calls != 0 {
		t.Fatalf("findNeighborBatch called %d times, want 0 — every candidate was rejected by admission, there must be no core blocks left to look up neighbors for", spy.calls)
	}
	if len(got) != 0 || dupCount != 0 {
		t.Fatalf("got (%v, %d), want (empty, 0)", got, dupCount)
	}
}

// A mix of rejected and admitted candidates: the batch request set handed
// to findNeighborBatch must only ever contain the admitted anchor's
// coordinates — a rejected candidate's document/chunk-index coordinates
// must never leak into the neighbor lookup, even when it was ranked ahead
// of the admitted one.
func TestExpandWithNeighborWindowBatchRequestOnlyContainsAdmittedAnchorCoordinates(t *testing.T) {
	rejected := RetrievedChunk{Chunk: Chunk{ID: "rejected", DocumentID: "doc-rejected", DocumentVersion: 1, ChunkIndex: 9, Content: "content-rejected"}, Score: 0.1}
	admitted := RetrievedChunk{Chunk: Chunk{ID: "admitted", DocumentID: "doc-admitted", DocumentVersion: 1, ChunkIndex: 5, Content: "content-admitted"}, Score: 0.9}

	anchors, admission := fuseTopK([]RetrievedChunk{rejected, admitted}, nil, 10)
	if got := idsOf(anchors); !equalIDs(got, []string{"admitted"}) {
		t.Fatalf("test setup invalid: anchors = %v, want exactly [admitted]", got)
	}
	if admission.AdmissionRejectedCount != 1 {
		t.Fatalf("test setup invalid: AdmissionRejectedCount = %d, want 1", admission.AdmissionRejectedCount)
	}

	spy := &spyNeighborBatch{}
	svc := &service{findNeighborBatch: spy.fn}

	if _, _, err := svc.expandWithNeighborWindow(context.Background(), anchors); err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("findNeighborBatch called %d times, want exactly 1", spy.calls)
	}
	for _, req := range spy.lastReq {
		if req.documentID == "doc-rejected" {
			t.Fatalf("batch request %v contains a coordinate from the REJECTED candidate's document (doc-rejected) — a rejected candidate's document/index must never reach the neighbor batch query", spy.lastReq)
		}
	}
}

// equalIDs is a tiny order-sensitive helper local to this file — idsOf
// already exists (hybrid_test.go) and returns []string, this just avoids
// importing reflect for a single equality check per test above.
func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
