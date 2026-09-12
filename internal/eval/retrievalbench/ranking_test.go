package retrievalbench

import "testing"

func TestFoldChunkRankingDeduplicatesDocumentsByFirstOccurrence(t *testing.T) {
	hits := []ChunkHit{{ChunkID: "c1", DocumentID: "d1", Rank: 1}, {ChunkID: "c2", DocumentID: "d1", Rank: 2}, {ChunkID: "c3", DocumentID: "d2", Rank: 3}, {ChunkID: "c4", DocumentID: "d2", Rank: 4}, {ChunkID: "c5", DocumentID: "d3", Rank: 5}}
	got := FoldChunkRanking(hits, 2)
	if want := []string{"d1", "d2"}; !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestFoldChunkRankingIgnoresMalformedAndKeepsStableOrder(t *testing.T) {
	hits := []ChunkHit{{ChunkID: "c1", DocumentID: "d1", Rank: 9}, {ChunkID: "c2", DocumentID: "", Rank: 2}, {ChunkID: "c3", DocumentID: "d2", Rank: 1}, {ChunkID: "c4", DocumentID: "d3", Rank: 3}}
	got := FoldChunkRanking(hits, 0)
	if want := []string{"d1", "d2", "d3"}; !equalStrings(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
