package knowledge

import "strconv"

// Phase 4: Neighbor Window Retrieval. Hybrid Search (Phase 3, hybrid.go)
// only ever returns chunks that were themselves directly hit by the vector
// or keyword path — an "anchor". If the answer to a question actually
// spans a chunk boundary, the model can end up seeing half a sentence with
// no surrounding explanation. This file adds a second, deliberately
// separate step: for each anchor, best-effort fetch its immediate
// document-order neighbors (chunk_index-1 and chunk_index+1, same
// document_id AND same document_version, published only) and append them
// to the result as a strictly lower-priority second tier, after every
// anchor — never interleaved anchor-by-anchor. See expandWithNeighbors'
// doc comment for exactly why the ordering has to be two full tiers (all
// anchors, then all neighbors) rather than "each anchor followed by its
// own neighbors": conversation/budget.go's selectEvidence consumes its
// input strictly in order, so an interleaved layout would let a
// lower-relevance neighbor chunk consume budget that a lower-RANKED (but
// still core) anchor needed, silently dropping a real Hybrid Search hit
// out of the result — a review fix closed exactly this bug.
//
// This file is pure/DB-free by the same design rule hybrid.go follows:
// expandWithNeighbors takes plain []RetrievedChunk in and returns
// []RetrievedChunk out, so every ordering/dedup/attribution rule is
// testable with zero database dependency (see hybrid_test.go). The DB
// side — grouping anchors by (document_id, document_version) and issuing
// one FindPublishedNeighborChunks call per group — lives in service.go's
// expandWithNeighborWindow, which calls into this file's pure functions
// for the actual assembly.
//
// Anchors never participate in this file's ranking decisions: an anchor's
// position in the input slice is untouched, and Score here is always
// either an anchor's own real relevance score (already computed by
// rrfFuse) or a neighbor's inherited copy of its anchor's score — see
// RetrievedChunk.Score's doc comment for why a neighbor never gets an
// independently measured score of its own. rrfFuse itself never sees
// neighbor chunks; they are assembled strictly after Hybrid Search's own
// ranking is already final.

// neighborIndexesFor returns the chunk_index values a single anchor wants
// as neighbors: chunk_index-1 (skipped when it would go negative — index 0
// has no previous chunk) and chunk_index+1 (always included; the caller's
// FindPublishedNeighborChunks query naturally returns nothing for a
// nonexistent trailing index, which is exactly "the last chunk has no next
// chunk" — no special-casing needed here for that end).
func neighborIndexesFor(chunkIndex int) []int {
	idxs := make([]int, 0, 2)
	if chunkIndex-1 >= 0 {
		idxs = append(idxs, chunkIndex-1)
	}
	idxs = append(idxs, chunkIndex+1)
	return idxs
}

// neighborGroupKey identifies one "same processing attempt" group of
// anchors — see Chunk.DocumentVersion's doc comment for why document_id
// alone is not enough to group by.
type neighborGroupKey struct {
	documentID      string
	documentVersion int64
}

// buildNeighborGroups groups anchors by (document_id, document_version) and
// unions the chunk_index values every anchor in that group needs — this is
// what makes "one query per document version, not one query per anchor per
// direction" possible: service.go's expandWithNeighborWindow issues exactly
// one FindPublishedNeighborChunks call per key in the returned map, with
// that key's full index set as the ANY(...) argument. The number of groups
// can never exceed len(anchors) (worst case: every anchor is its own
// distinct document version), which is itself bounded by topK — the "查询
// 次数有明确上限" requirement this satisfies structurally, not by a
// separate runtime check.
func buildNeighborGroups(anchors []RetrievedChunk) map[neighborGroupKey]map[int]bool {
	groups := make(map[neighborGroupKey]map[int]bool)
	for _, a := range anchors {
		key := neighborGroupKey{documentID: a.DocumentID, documentVersion: a.DocumentVersion}
		set, ok := groups[key]
		if !ok {
			set = make(map[int]bool)
			groups[key] = set
		}
		for _, idx := range neighborIndexesFor(a.ChunkIndex) {
			set[idx] = true
		}
	}
	return groups
}

// neighborLookupKey is expandWithNeighbors' internal index into the flat
// neighbor slice it's handed — (document_id, document_version, chunk_index)
// is exactly what identifies "the chunk at this position in this specific
// processing attempt", the same three-part key the SQL query itself filters
// on. Built as a string rather than a struct so it can key a plain map
// without needing a second comparable-struct type only used here.
func neighborLookupKey(documentID string, documentVersion int64, chunkIndex int) string {
	return documentID + "\x00" + strconv.FormatInt(documentVersion, 10) + "\x00" + strconv.Itoa(chunkIndex)
}

// expandWithNeighbors is the pure assembly step: given anchors (Hybrid
// Search's already-ranked, already-topK'd core hits) and neighbors (every
// candidate neighbor chunk fetched for them, from any number of
// findPublishedNeighborChunks calls, in any order — this function does not
// care which group a neighbor came from), produce the final deduped,
// budget-ready slice.
//
// Output layout (fixed by the final-review budget-priority fix — see the
// "review fix" note below for what this replaced): ALL anchors first, in
// their unmodified rrfFuse rank order, followed by ALL neighbor blocks,
// each block ordered previous-then-next and the blocks themselves ordered
// by their owning anchor's rank:
//
//	anchor1, anchor2, anchor3, ...,
//	anchor1.previous, anchor1.next, anchor2.previous, anchor2.next, ...
//
// This is a two-tier priority, not an interleaving: conversation/budget.go's
// selectEvidence fills its rendered-length budget strictly in input order
// and never reorders or backtracks, so whatever tier a chunk sits in here
// directly determines it competes for budget only against chunks of equal
// or higher priority. Every anchor is now tier 1 — a chunk fetched purely
// as someone else's surrounding context (tier 2) can never be considered by
// selectEvidence before EVERY anchor has already had its turn, and
// therefore can never displace a lower-ranked anchor out of a tight budget.
//
// review fix: an earlier version of this function grouped output as
// anchor1, anchor1's neighbors, anchor2, anchor2's neighbors, ... (each
// anchor immediately followed by its own neighbors). That grouping reads
// naturally but is wrong under a real budget: with room for only two
// sources, selectEvidence would keep anchor1 and anchor1's neighbor and
// drop anchor2 entirely — a lower-relevance neighbor chunk pushing a
// higher-ranked CORE hit out of the result, which directly violates "邻接
// 块不能改变核心结果排名" / "邻接块不能把核心块挤出结果" / "预算不足时
// 核心块优先于所有邻接块". The two-tier layout above is what actually
// guarantees that: no neighbor's position in the output can ever be
// numerically earlier than the last anchor's, so a greedy prefix-fill
// budget consumer always exhausts every anchor before it can even look at
// a neighbor.
//
// Rules, in the order they matter:
//  1. Anchor rank is never touched. The anchor tier of the output preserves
//     anchors' relative order exactly as rrfFuse produced it — best-effort
//     neighbor lookup failures elsewhere (service.go) can only ever drop
//     neighbors, never reorder or drop an anchor.
//  2. All anchors precede all neighbors (see the layout above) — this is
//     the load-bearing property the review fix exists for. Within the
//     neighbor tier, each anchor's own neighbor block stays grouped
//     together (previous, then next) and blocks are ordered by their
//     owning anchor's rank, purely for readability/locality; it has no
//     bearing on budget priority since the entire neighbor tier is already
//     strictly lower priority than the entire anchor tier.
//  3. Global dedup by chunk ID via the `used` set, seeded with every
//     anchor ID before any neighbor is considered. This gets two rules at
//     once:
//     - a neighbor chunk that is ALSO separately an anchor is never
//     inserted as anyone's neighbor (used[id] is already true the first
//     time it's looked up) — it only ever appears once, in the anchor
//     tier, keeping its own real Score instead of an inherited one.
//     - when two different anchors would each want the same neighbor
//     chunk (adjacent anchors, or two chunks that happen to share a
//     would-be-neighbor index), the FIRST anchor to claim it — which,
//     because anchors are walked in their already-final rank order, is
//     always the higher-ranked one — wins; the second anchor's lookup for
//     that same chunk ID is skipped.
//  4. A neighbor's Content/DocumentName/PageNumber/SectionTitle/etc. are
//     always its OWN real fields, exactly as findPublishedNeighborChunks
//     returned them — only Score is overwritten (inherited from the owning
//     anchor) and NeighborOf is set to the owning anchor's chunk ID. See
//     RetrievedChunk.Score's doc comment for why Score is the one field
//     that's deliberately borrowed rather than genuine.
//  5. No neighbors for a given anchor (none fetched, none matched, or the
//     neighbor slice is empty/nil entirely) simply contributes nothing —
//     the anchor itself is still emitted. An empty/nil neighbors input
//     makes this function's output identical to anchors itself.
//  6. Output length is always <= len(anchors)*3: at most 2 neighbors
//     (previous, next) can ever be appended per anchor, by construction —
//     nothing in this function can append more than that regardless of how
//     many rows are in neighbors (a mismatched/oversized neighbors slice
//     from a caller bug would just mean most of it is never looked up, not
//     that it leaks into the output).
func expandWithNeighbors(anchors []RetrievedChunk, neighbors []RetrievedChunk) []RetrievedChunk {
	if len(anchors) == 0 {
		return anchors
	}

	neighborByKey := make(map[string]RetrievedChunk, len(neighbors))
	for _, n := range neighbors {
		neighborByKey[neighborLookupKey(n.DocumentID, n.DocumentVersion, n.ChunkIndex)] = n
	}

	used := make(map[string]bool, len(anchors)+len(neighbors))
	for _, a := range anchors {
		used[a.ID] = true
	}

	out := make([]RetrievedChunk, 0, len(anchors)*3)
	// Tier 1: every anchor, unmodified rank order — see this function's
	// doc comment for why this must be a fully separate pass from tier 2,
	// not an interleaved anchor+neighbors loop.
	out = append(out, anchors...)
	// Tier 2: every anchor's neighbor block, in anchor-rank order,
	// previous-then-next within each block. Strictly after every anchor in
	// the output, so a greedy prefix-fill budget consumer can never reach a
	// neighbor before it has already had the chance to include every
	// anchor.
	for _, a := range anchors {
		for _, idx := range neighborIndexesFor(a.ChunkIndex) {
			n, ok := neighborByKey[neighborLookupKey(a.DocumentID, a.DocumentVersion, idx)]
			if !ok {
				continue
			}
			if used[n.ID] {
				continue
			}
			used[n.ID] = true
			n.Score = a.Score
			n.NeighborOf = a.ID
			out = append(out, n)
		}
	}
	return out
}
