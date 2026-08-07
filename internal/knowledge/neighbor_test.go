package knowledge

import (
	"reflect"
	"testing"
)

// anchorRC/neighborRC are neighbor_test.go's tiny constructors — parallel
// to hybrid_test.go's rc(), but neighbor.go's rules genuinely depend on
// DocumentID/DocumentVersion/ChunkIndex (unlike rrfFuse, which only cares
// about ID/Score), so these need to set them explicitly.
func anchorRC(id, docID string, version int64, chunkIndex int, score float64) RetrievedChunk {
	return RetrievedChunk{Chunk: Chunk{ID: id, DocumentID: docID, DocumentVersion: version, ChunkIndex: chunkIndex}, Score: score}
}

// neighborRC is what findPublishedNeighborChunks would return for one row
// — Score/NeighborOf are always zero-value here (see
// findPublishedNeighborChunks' doc comment), and DocumentName/PageNumber/
// SectionTitle are the neighbor chunk's OWN Citation metadata, deliberately
// different from any anchor's so tests can catch accidental copying.
func neighborRC(id, docID string, version int64, chunkIndex int, docName string, page *int, section *string) RetrievedChunk {
	return RetrievedChunk{Chunk: Chunk{
		ID: id, DocumentID: docID, DocumentVersion: version, ChunkIndex: chunkIndex,
		DocumentName: docName, PageNumber: page, SectionTitle: section,
	}}
}

func neighborsOf(chunks []RetrievedChunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.NeighborOf
	}
	return out
}

// --- neighborIndexesFor ---

func TestNeighborIndexesForBoundaries(t *testing.T) {
	cases := []struct {
		chunkIndex int
		want       []int
	}{
		{chunkIndex: 0, want: []int{1}},    // 边界：index=0 没有前块，只请求后块
		{chunkIndex: 1, want: []int{0, 2}}, // 正常：前后都请求
		{chunkIndex: 5, want: []int{4, 6}}, // 正常
		{chunkIndex: 100, want: []int{99, 101}},
	}
	for _, tc := range cases {
		got := neighborIndexesFor(tc.chunkIndex)
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("neighborIndexesFor(%d) = %v, want %v (never a negative index)", tc.chunkIndex, got, tc.want)
		}
		for _, idx := range got {
			if idx < 0 {
				t.Fatalf("neighborIndexesFor(%d) produced a negative index %d", tc.chunkIndex, idx)
			}
		}
	}
}

// --- buildNeighborGroups ---

// 同一个 (document_id, document_version) 的多个核心块必须合并进同一组，
// 组内的 index 集合是并集——这是"同一个文档版本只查询一次"的结构性保证。
func TestBuildNeighborGroupsMergesSameDocumentVersionAnchors(t *testing.T) {
	anchors := []RetrievedChunk{
		anchorRC("a1", "doc-1", 3, 5, 0.9), // wants 4, 6
		anchorRC("a2", "doc-1", 3, 7, 0.8), // wants 6, 8 (6 overlaps with a1's want)
		anchorRC("a3", "doc-2", 1, 0, 0.7), // different document -> its own group
		anchorRC("a4", "doc-1", 2, 5, 0.6), // same document, different version -> its own group
	}
	groups := buildNeighborGroups(anchors)
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3 (doc-1/v3, doc-2/v1, doc-1/v2): %+v", len(groups), groups)
	}

	key1 := neighborGroupKey{documentID: "doc-1", documentVersion: 3}
	want1 := map[int]bool{4: true, 6: true, 8: true}
	if !reflect.DeepEqual(groups[key1], want1) {
		t.Fatalf("doc-1/v3 group indexes = %v, want %v (union of a1's and a2's wants, 6 not duplicated)", groups[key1], want1)
	}

	key2 := neighborGroupKey{documentID: "doc-2", documentVersion: 1}
	want2 := map[int]bool{1: true} // chunkIndex=0 -> only next=1
	if !reflect.DeepEqual(groups[key2], want2) {
		t.Fatalf("doc-2/v1 group indexes = %v, want %v", groups[key2], want2)
	}

	key3 := neighborGroupKey{documentID: "doc-1", documentVersion: 2}
	want3 := map[int]bool{4: true, 6: true}
	if !reflect.DeepEqual(groups[key3], want3) {
		t.Fatalf("doc-1/v2 group indexes = %v, want %v (must NOT be merged with doc-1/v3 despite same document_id)", groups[key3], want3)
	}
}

// --- expandWithNeighbors ---

// 1. 一个核心块补齐前后邻接块.
func TestExpandWithNeighborsFillsPreviousAndNext(t *testing.T) {
	anchor := anchorRC("a1", "doc-1", 1, 5, 0.9)
	prev := neighborRC("n-prev", "doc-1", 1, 4, "handbook.pdf", nil, nil)
	next := neighborRC("n-next", "doc-1", 1, 6, "handbook.pdf", nil, nil)

	got := expandWithNeighbors([]RetrievedChunk{anchor}, []RetrievedChunk{next, prev}) // order in neighbors slice must not matter

	want := []string{"a1", "n-prev", "n-next"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (anchor first, then previous, then next)", got, want)
	}
}

// 2. chunk_index=0 只补后块.
func TestExpandWithNeighborsChunkIndexZeroOnlyGetsNext(t *testing.T) {
	anchor := anchorRC("a1", "doc-1", 1, 0, 0.9)
	next := neighborRC("n-next", "doc-1", 1, 1, "handbook.pdf", nil, nil)

	got := expandWithNeighbors([]RetrievedChunk{anchor}, []RetrievedChunk{next})

	want := []string{"a1", "n-next"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (no previous chunk for index 0)", got, want)
	}
}

// 3. 最后一块只补前块（neighbors 集合里天然没有 next，因为文档里不存在）.
func TestExpandWithNeighborsLastChunkOnlyGetsPrevious(t *testing.T) {
	anchor := anchorRC("a1", "doc-1", 1, 9, 0.9) // last chunk of a 10-chunk document (indexes 0..9)
	prev := neighborRC("n-prev", "doc-1", 1, 8, "handbook.pdf", nil, nil)
	// no chunk at index 10 in the neighbors slice — it doesn't exist in the document

	got := expandWithNeighbors([]RetrievedChunk{anchor}, []RetrievedChunk{prev})

	want := []string{"a1", "n-prev"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (no next chunk past the document's last index)", got, want)
	}
}

// 4. 邻接块缺号时正常跳过（既没有 prev 也没有 next 被找到）.
func TestExpandWithNeighborsSkipsMissingIndexes(t *testing.T) {
	anchor := anchorRC("a1", "doc-1", 1, 5, 0.9)
	// neighbors slice is empty entirely — simulates both index 4 and 6 missing (deleted/never existed).

	got := expandWithNeighbors([]RetrievedChunk{anchor}, nil)

	want := []string{"a1"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (anchor alone when no neighbor chunk exists)", got, want)
	}
}

// 5. 多个核心块的排名不改变.
func TestExpandWithNeighborsPreservesAnchorRank(t *testing.T) {
	anchors := []RetrievedChunk{
		anchorRC("a3", "doc-1", 1, 20, 0.5), // deliberately out of ChunkIndex order — anchor order is RANK order, not document order
		anchorRC("a1", "doc-1", 1, 5, 0.9),
		anchorRC("a2", "doc-2", 1, 0, 0.7),
	}
	got := expandWithNeighbors(anchors, nil)
	want := []string{"a3", "a1", "a2"} // exactly the input order — rrfFuse already decided this
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (anchor rank must never be reordered by neighbor expansion)", got, want)
	}
}

// 6. 邻接块本身也是核心块时不提前、不重复：它只能以自己的核心排名出现一次.
func TestExpandWithNeighborsNeighborThatIsAlsoAnAnchorAppearsOnlyOnceAtItsOwnRank(t *testing.T) {
	// a1 (rank 1, index 5) wants a neighbor at index 6.
	// a2 (rank 2, index 6) is itself a core hit — it must NOT be inserted
	// early as a1's "next" neighbor; it must appear exactly once, at rank 2,
	// keeping its own real Score (0.8), not a1's inherited Score (0.9).
	a1 := anchorRC("a1", "doc-1", 1, 5, 0.9)
	a2 := anchorRC("a2", "doc-1", 1, 6, 0.8)
	// The neighbor row for index 6 would legitimately be findable (a2 IS
	// chunk index 6 in this document/version) — simulate it being present
	// in the raw neighbors slice, exactly as a real DB query would return it.
	wouldBeNeighbor := neighborRC("a2", "doc-1", 1, 6, "handbook.pdf", nil, nil)

	got := expandWithNeighbors([]RetrievedChunk{a1, a2}, []RetrievedChunk{wouldBeNeighbor})

	want := []string{"a1", "a2"} // NOT [a1, a2(as neighbor), a2(as anchor)] and NOT [a1, a2, a2]
	if gotIDs := idsOf(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v (a2 must appear exactly once, at its own anchor rank)", gotIDs, want)
	}
	for _, c := range got {
		if c.ID == "a2" {
			if c.NeighborOf != "" {
				t.Fatalf("a2.NeighborOf = %q, want \"\" (a2 is an anchor, never a neighbor)", c.NeighborOf)
			}
			if c.Score != 0.8 {
				t.Fatalf("a2.Score = %f, want its own real anchor Score 0.8, not a1's inherited 0.9", c.Score)
			}
		}
	}
}

// 7. 两个核心块共享同一个邻接块时只出现一次，归属排名更高的核心块.
func TestExpandWithNeighborsSharedNeighborAttributedToHigherRankedAnchor(t *testing.T) {
	// a1 (rank 1, index 5) wants next=6. a2 (rank 2, index 7) wants prev=6.
	// Both want the same chunk (index 6) — it must appear exactly once,
	// attributed to a1 (the higher-ranked anchor, since anchors are walked
	// in rank order and a1 claims it first).
	a1 := anchorRC("a1", "doc-1", 1, 5, 0.9)
	a2 := anchorRC("a2", "doc-1", 1, 7, 0.6)
	shared := neighborRC("n-shared", "doc-1", 1, 6, "handbook.pdf", nil, nil)

	got := expandWithNeighbors([]RetrievedChunk{a1, a2}, []RetrievedChunk{shared})

	count := 0
	var attributedScore float64
	var neighborOf string
	for _, c := range got {
		if c.ID == "n-shared" {
			count++
			attributedScore = c.Score
			neighborOf = c.NeighborOf
		}
	}
	if count != 1 {
		t.Fatalf("n-shared appeared %d times, want exactly 1: %v", count, idsOf(got))
	}
	if neighborOf != "a1" {
		t.Fatalf("n-shared.NeighborOf = %q, want %q (the higher-ranked anchor)", neighborOf, "a1")
	}
	if attributedScore != 0.9 {
		t.Fatalf("n-shared.Score = %f, want a1's Score 0.9 (the anchor it was attributed to), not a2's 0.6", attributedScore)
	}
	// Budget-priority fix: ALL anchors precede ALL neighbors — a2 (a real
	// core hit) must come right after a1, not have a1's neighbor sitting
	// between them. n-shared still belongs to a1 (NeighborOf/Score checked
	// above), it just doesn't get to occupy an input position ahead of a2
	// anymore.
	want := []string{"a1", "a2", "n-shared"}
	if gotIDs := idsOf(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v (every anchor must precede every neighbor)", gotIDs, want)
	}
}

// 8. 邻接块继承正确的核心 Score.
func TestExpandWithNeighborsInheritsOwningAnchorScore(t *testing.T) {
	a1 := anchorRC("a1", "doc-1", 1, 5, 0.73)
	next := neighborRC("n-next", "doc-1", 1, 6, "handbook.pdf", nil, nil)

	got := expandWithNeighbors([]RetrievedChunk{a1}, []RetrievedChunk{next})

	for _, c := range got {
		if c.ID == "n-next" && c.Score != 0.73 {
			t.Fatalf("n-next.Score = %f, want the owning anchor's Score 0.73", c.Score)
		}
	}
}

// 9. 邻接块 NeighborOf 指向正确核心 chunk ID.
func TestExpandWithNeighborsSetsNeighborOfToOwningAnchorID(t *testing.T) {
	a1 := anchorRC("a1", "doc-1", 1, 5, 0.9)
	prev := neighborRC("n-prev", "doc-1", 1, 4, "handbook.pdf", nil, nil)
	next := neighborRC("n-next", "doc-1", 1, 6, "handbook.pdf", nil, nil)

	got := expandWithNeighbors([]RetrievedChunk{a1}, []RetrievedChunk{prev, next})

	wantNeighborOf := []string{"", "a1", "a1"} // a1 itself, then its two neighbors, in output order
	if gotNeighborOf := neighborsOf(got); !reflect.DeepEqual(gotNeighborOf, wantNeighborOf) {
		t.Fatalf("NeighborOf sequence = %v, want %v", gotNeighborOf, wantNeighborOf)
	}
}

// 10. Citation 元数据来自邻接块自己，不能复制核心块的页码、章节或文档名.
func TestExpandWithNeighborsKeepsNeighborsOwnCitationMetadata(t *testing.T) {
	anchorPage := 5
	anchorSection := "2.1 概述"
	a1 := RetrievedChunk{
		Chunk: Chunk{ID: "a1", DocumentID: "doc-1", DocumentVersion: 1, ChunkIndex: 5,
			DocumentName: "anchor-doc.pdf", PageNumber: &anchorPage, SectionTitle: &anchorSection},
		Score: 0.9,
	}
	neighborPage := 6
	neighborSection := "2.2 细节"
	next := neighborRC("n-next", "doc-1", 1, 6, "neighbor-doc.pdf", &neighborPage, &neighborSection)

	got := expandWithNeighbors([]RetrievedChunk{a1}, []RetrievedChunk{next})

	var n RetrievedChunk
	for _, c := range got {
		if c.ID == "n-next" {
			n = c
		}
	}
	if n.DocumentName != "neighbor-doc.pdf" {
		t.Fatalf("n-next.DocumentName = %q, want its own %q, not the anchor's %q", n.DocumentName, "neighbor-doc.pdf", "anchor-doc.pdf")
	}
	if n.PageNumber == nil || *n.PageNumber != neighborPage {
		t.Fatalf("n-next.PageNumber = %v, want its own %d, not the anchor's %d", n.PageNumber, neighborPage, anchorPage)
	}
	if n.SectionTitle == nil || *n.SectionTitle != neighborSection {
		t.Fatalf("n-next.SectionTitle = %v, want its own %q, not the anchor's %q", n.SectionTitle, neighborSection, anchorSection)
	}
}

// 预算优先级修复的核心回归：三个核心块各自都有前后邻接块时，输出必须是
// 两个完整分层——全部核心块（保持排名）在前，全部邻接块（按所属核心块
// 排名分组，组内 previous/next）在后，绝不是每个核心块紧跟自己的邻接块
// 再轮到下一个核心块。这是本轮修复要保证的确切布局，直接照抄需求里的
// 示例顺序。
func TestExpandWithNeighborsAllAnchorsPrecedeAllNeighbors(t *testing.T) {
	a1 := anchorRC("anchor1", "doc-1", 1, 5, 0.9)
	a2 := anchorRC("anchor2", "doc-2", 1, 5, 0.8)
	a3 := anchorRC("anchor3", "doc-3", 1, 5, 0.7)

	neighbors := []RetrievedChunk{
		// 故意打乱顺序喂进去——expandWithNeighbors 不能依赖 neighbors 切片
		// 本身的顺序,只能依赖 anchors 的排名顺序来决定邻接块分层内部的位置。
		neighborRC("anchor3.next", "doc-3", 1, 6, "d3.pdf", nil, nil),
		neighborRC("anchor1.previous", "doc-1", 1, 4, "d1.pdf", nil, nil),
		neighborRC("anchor2.next", "doc-2", 1, 6, "d2.pdf", nil, nil),
		neighborRC("anchor1.next", "doc-1", 1, 6, "d1.pdf", nil, nil),
		neighborRC("anchor2.previous", "doc-2", 1, 4, "d2.pdf", nil, nil),
		neighborRC("anchor3.previous", "doc-3", 1, 4, "d3.pdf", nil, nil),
	}

	got := expandWithNeighbors([]RetrievedChunk{a1, a2, a3}, neighbors)

	want := []string{
		"anchor1", "anchor2", "anchor3",
		"anchor1.previous", "anchor1.next",
		"anchor2.previous", "anchor2.next",
		"anchor3.previous", "anchor3.next",
	}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (all anchors first in rank order, then every anchor's own previous/next block in anchor-rank order)", got, want)
	}
}

// 11. 输入顺序固定时输出完全确定.
func TestExpandWithNeighborsIsDeterministicAcrossRuns(t *testing.T) {
	anchors := []RetrievedChunk{
		anchorRC("a1", "doc-1", 1, 5, 0.9),
		anchorRC("a2", "doc-2", 1, 0, 0.7),
	}
	neighbors := []RetrievedChunk{
		neighborRC("n1", "doc-1", 1, 4, "d1.pdf", nil, nil),
		neighborRC("n2", "doc-1", 1, 6, "d1.pdf", nil, nil),
		neighborRC("n3", "doc-2", 1, 1, "d2.pdf", nil, nil),
	}

	first := idsOf(expandWithNeighbors(anchors, neighbors))
	for i := 0; i < 20; i++ {
		got := idsOf(expandWithNeighbors(anchors, neighbors))
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: output order changed across repeated calls with identical input: got %v, want %v", i, got, first)
		}
	}
}

// 12. 扩展结果不超过 anchorCount*3.
func TestExpandWithNeighborsNeverExceedsAnchorCountTimesThree(t *testing.T) {
	const anchorCount = 5
	anchors := make([]RetrievedChunk, anchorCount)
	var neighbors []RetrievedChunk
	for i := 0; i < anchorCount; i++ {
		docID := "doc-multi"
		version := int64(1)
		idx := 100 + i*10 // spaced far apart (and away from 0) so no anchor's neighbors collide with another's or hit the chunk_index=0 boundary
		anchors[i] = anchorRC(idsForIndex(i), docID, version, idx, 1.0-float64(i)*0.1)
		neighbors = append(neighbors,
			neighborRC(idsForIndex(i)+"-prev", docID, version, idx-1, "d.pdf", nil, nil),
			neighborRC(idsForIndex(i)+"-next", docID, version, idx+1, "d.pdf", nil, nil),
		)
	}

	got := expandWithNeighbors(anchors, neighbors)
	if len(got) > anchorCount*3 {
		t.Fatalf("got %d results, want at most anchorCount*3=%d", len(got), anchorCount*3)
	}
	if len(got) != anchorCount*3 {
		t.Fatalf("got %d results, want exactly %d (every anchor here has both a distinct prev and next available)", len(got), anchorCount*3)
	}
}

func idsForIndex(i int) string {
	return "anchor-" + string(rune('A'+i))
}

// 13. 空核心结果返回空.
func TestExpandWithNeighborsEmptyAnchorsReturnsEmpty(t *testing.T) {
	got := expandWithNeighbors(nil, []RetrievedChunk{neighborRC("n1", "doc-1", 1, 0, "d.pdf", nil, nil)})
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (no anchors means nothing to expand)", got)
	}
}

// 14. 空邻接结果保持核心结果不变.
func TestExpandWithNeighborsEmptyNeighborsKeepsAnchorsUnchanged(t *testing.T) {
	anchors := []RetrievedChunk{
		anchorRC("a1", "doc-1", 1, 5, 0.9),
		anchorRC("a2", "doc-2", 1, 3, 0.7),
	}
	got := expandWithNeighbors(anchors, nil)
	if !reflect.DeepEqual(got, anchors) {
		t.Fatalf("got %+v, want anchors unchanged %+v", got, anchors)
	}
	gotEmpty := expandWithNeighbors(anchors, []RetrievedChunk{})
	if !reflect.DeepEqual(gotEmpty, anchors) {
		t.Fatalf("got %+v (with empty non-nil neighbors slice), want anchors unchanged %+v", gotEmpty, anchors)
	}
}
