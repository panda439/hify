package knowledge

import "sort"

// Phase 3: Hybrid Search fuses two independently-ranked candidate lists
// (vector similarity, keyword/trigram similarity) with Reciprocal Rank
// Fusion. This file is deliberately DB-free — rrfFuse takes plain
// []RetrievedChunk (already ranked by their own path) and returns the
// final merged, deduped, globally-topK'd list. That's what makes it
// testable with zero database dependency (see hybrid_test.go).
const (
	// rrfK is RRF's smoothing constant — the standard "60" from the
	// original Cormack/Clarke/Buettcher paper, also the most common
	// default in production hybrid-search systems (Elastic, Weaviate,
	// etc). Larger values flatten the rank->score curve (rank 1 and rank
	// 20 end up closer together); smaller values make rank 1 dominate
	// much more sharply. Not exposed as a runtime knob — see the report's
	// "禁止范围" note: this phase does not add dynamic weight
	// configuration.
	rrfK = 60.0

	// vectorWeight/keywordWeight bias the fused score toward vector
	// results without ever letting keyword-only hits get crowded out
	// entirely — a chunk that ONLY the keyword path found still gets a
	// full keywordWeight/(rrfK+rank) contribution and can outrank a weak
	// vector hit. 0.65/0.35 favors semantic recall (this is a RAG
	// pipeline, not a search engine) while still giving exact keyword
	// matches real influence on the final order.
	vectorWeight  = 0.65
	keywordWeight = 0.35
)

// candidateK bounds how many rows each path (vector, keyword) fetches
// before fusion — topK alone would leave RRF almost no room to reorder
// anything (two topK=5 lists have at most 10 distinct chunks to fuse from,
// most of which get truncated right back out). min(max(topK*4, topK), 100)
// gives fusion a meaningfully wider pool while still bounding worst-case
// query cost — a large topK (already clamped to maxTopK=50 by clampTopK)
// can't blow candidateK past 100 regardless of multiplier.
func candidateK(topK int) int {
	c := topK * 4
	if c < topK {
		c = topK
	}
	if c > 100 {
		c = 100
	}
	return c
}

// fusionEntry is rrfFuse's internal bookkeeping per distinct chunk ID —
// fusionScore never leaves this file. It exists purely to decide final
// ORDER; it is not a relevance score (an isolated RRF value is a small,
// unnormalized fraction — see RetrievedChunk's doc comment) and must never
// be written into RetrievedChunk.Score or compared against
// conversation/budget.go's ragMinSimilarityScore floor.
type fusionEntry struct {
	chunk       RetrievedChunk
	haveChunk   bool
	fusionScore float64
	relevance   float64
	haveScore   bool
}

// rrfFuse merges vector-path and keyword-path candidates (each already
// ordered best-first by its own path) into one deduped, globally-ranked,
// topK-truncated slice.
//
// Per-chunk relevance (RetrievedChunk.Score) is kept semantically separate
// from ranking (fusionScore, internal only):
//   - vector-only hit: Score = cosine similarity from the vector path.
//   - keyword-only hit: Score = word-similarity from the keyword path.
//   - hit in both paths: Score = max(vector score, keyword score) — the
//     stronger of the two signals a human would actually trust, not an
//     RRF fraction (~0.01-0.02) that would trip conversation/budget.go's
//     ragMinSimilarityScore floor and silently filter out every hybrid
//     result (see the phase report's "Score 语义不能被破坏" section).
//
// Final order: fusionScore desc, then Score desc, then chunk ID asc — the
// third key is what makes the output deterministic even when two chunks
// land on the exact same fusionScore and Score, and it's also why
// processing order of the two input slices never affects the result: every
// comparison eventually bottoms out at ID, which doesn't depend on input
// order at all.
func rrfFuse(vectorChunks, keywordChunks []RetrievedChunk, topK int) []RetrievedChunk {
	entries := make(map[string]*fusionEntry)

	addPath := func(chunks []RetrievedChunk, weight float64) {
		for i, c := range chunks {
			rank := i + 1 // 1-based: RRF's own convention, and avoids a rank=0 divide-by-rrfK edge case
			e, ok := entries[c.ID]
			if !ok {
				e = &fusionEntry{}
				entries[c.ID] = e
			}
			e.fusionScore += weight / (rrfK + float64(rank))
			if !e.haveChunk {
				e.chunk = c
				e.haveChunk = true
			}
			if !e.haveScore || c.Score > e.relevance {
				e.relevance = c.Score
				e.haveScore = true
			}
		}
	}
	addPath(vectorChunks, vectorWeight)
	addPath(keywordChunks, keywordWeight)

	out := make([]RetrievedChunk, 0, len(entries))
	for _, e := range entries {
		rc := e.chunk
		rc.Score = e.relevance
		out = append(out, rc)
	}

	sort.Slice(out, func(i, j int) bool {
		si, sj := entries[out[i].ID].fusionScore, entries[out[j].ID].fusionScore
		if si != sj {
			return si > sj
		}
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})

	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

// sortVectorCandidatesByScoreThenID orders a slice of vector-path
// RetrievedChunk in place: Score descending, chunk ID ascending as a
// stable tie-break for equal Score only — it must never reorder two
// candidates whose Score actually differs. service.go's Retrieve calls
// this once, after concatenating every embedding-model group's results,
// because that concatenation order depends on kbsByModel's map iteration
// order (Go randomizes map iteration per run) — without this, two
// candidates with the exact same Score (duplicate/near-duplicate
// embeddings are not rare) could land at different slice positions
// across runs. rrfFuse then reads slice position as RRF rank, so an
// unstable order here would make the final fused rank unstable too, the
// same failure mode the id ASC tie-break in SearchVectorChunks's SQL
// ORDER BY (pgqueries/chunks.sql) fixes at the single-query level — this
// function is the Go-level fix for the cross-model-group merge that SQL
// alone can't reach.
func sortVectorCandidatesByScoreThenID(candidates []RetrievedChunk) {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		return candidates[i].ID < candidates[j].ID
	})
}
