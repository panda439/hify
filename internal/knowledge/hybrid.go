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

	// Phase 8 (admission.go): per-path raw signal, tracked independently of
	// relevance/fusionScore above. haveVector/haveKeyword distinguish "this
	// path never hit this chunk" from "this path hit this chunk with a
	// score of exactly 0" — admitBySourceSignal must never treat the
	// former as the latter (see its doc comment). vectorScore/keywordScore
	// are each that path's OWN max raw score across every occurrence in
	// that path's candidate list — never the other path's score, never
	// fusionScore, never the max()'d relevance field above.
	haveVector   bool
	vectorScore  float64
	haveKeyword  bool
	keywordScore float64
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
//
// Phase 8 (admission.go): after sorting, a source-aware admission gate
// rejects any candidate whose per-path raw signal(s) never cleared that
// path's absolute threshold — see admission.go's doc comment. This runs
// BEFORE Phase 5's content-dedup below, not after: see the design doc §3
// for why running dedup first could let a same-content pair's
// higher-ranked-but-unqualified half survive at the expense of a
// lower-ranked-but-qualified duplicate.
//
// Phase 5 (dedup.go): exact-content dedup runs on the ADMITTED candidates
// (Phase 8 gate above), themselves already the FULL fused, deduped-by-ID,
// fully-sorted list minus rejects — BEFORE truncating to topK, not after.
// This ordering is load-bearing: the admitted list here is already a
// bounded "expanded candidate" set (at most
// len(vectorChunks)+len(keywordChunks), itself bounded by 2*candidateK ≤
// 200 — see candidateK's doc comment), so deduping first and truncating
// second is what lets a unique, merely lower-fusionScore chunk fill the
// topK slot a higher-ranked same-content duplicate would otherwise have
// wasted (see the phase report's "A、A、B、C + topK=3 → A、B、C" acceptance
// scenario). Deduping AFTER truncation would get this wrong — it could
// truncate away the very candidate that should have filled the freed slot.
// Because entries here are already sorted highest-fusionScore-first,
// dedupExactContentChunks' first-occurrence-wins rule also gives "a
// duplicate keeps only its highest-ranked occurrence" for free — see that
// function's doc comment.
//
// The second return value is admissionStats (Phase 8) — a safe,
// content-free aggregate of both the admission gate's and this dedup
// pass's counts. service.go's Retrieve logs it via a safe, content-free
// structured debug line.
func rrfFuse(vectorChunks, keywordChunks []RetrievedChunk, topK int) ([]RetrievedChunk, admissionStats) {
	entries := make(map[string]*fusionEntry)

	addPath := func(chunks []RetrievedChunk, weight float64, isVector bool) {
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
			// Phase 8: track this path's own raw signal separately from
			// relevance above — see fusionEntry's doc comment.
			if isVector {
				if !e.haveVector || c.Score > e.vectorScore {
					e.vectorScore = c.Score
					e.haveVector = true
				}
			} else {
				if !e.haveKeyword || c.Score > e.keywordScore {
					e.keywordScore = c.Score
					e.haveKeyword = true
				}
			}
		}
	}
	addPath(vectorChunks, vectorWeight, true)
	addPath(keywordChunks, keywordWeight, false)

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

	// Phase 8: source-aware evidence admission runs on the full sorted
	// candidate list, on each entry's own per-path raw signal (never
	// fusionScore, never the post-max() relevance/Score field) — see
	// admission.go's doc comment and the design doc §3 for why this must
	// happen BEFORE content-dedup and topK truncation: a high-ranked but
	// unqualified duplicate must be rejected first so a lower-ranked but
	// qualified duplicate can survive content-dedup instead of being
	// discarded in favor of the (later-rejected) higher-ranked one.
	stats := admissionStats{CandidateCountBeforeAdmission: len(out)}
	admitted := make([]RetrievedChunk, 0, len(out))
	for _, c := range out {
		e := entries[c.ID]
		verdict := admitBySourceSignal(e.haveVector, e.vectorScore, e.haveKeyword, e.keywordScore)
		if verdict.belowVectorSignal {
			stats.VectorBelowAdmissionCount++
		}
		if verdict.belowKeywordSignal {
			stats.KeywordBelowAdmissionCount++
		}
		if !verdict.admitted {
			stats.AdmissionRejectedCount++
			continue
		}
		admitted = append(admitted, c)
	}

	// Phase 5: content-dedup the admitted candidate list BEFORE truncating
	// to topK — see this function's doc comment above for why the order of
	// these two steps matters. Phase 8 additionally requires admission to
	// run before this, not after — see the admission block above.
	admitted, contentDuplicateCount := dedupExactContentChunks(admitted)
	stats.ContentDuplicateCount = contentDuplicateCount

	if len(admitted) > topK {
		admitted = admitted[:topK]
	}
	return admitted, stats
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
