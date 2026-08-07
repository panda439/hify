package knowledge

import "strings"

// Phase 5: Exact Content Deduplication. Hybrid Search (Phase 3) and Neighbor
// Window Retrieval (Phase 4) can both independently surface the exact same
// underlying text more than once — two different chunk rows (different ID,
// possibly different document, possibly a reprocessed duplicate upload)
// whose Content is byte-for-byte the same after only the most conservative
// whitespace normalization. Sending the model the same paragraph twice
// wastes RAG budget and looks like a bug to anyone reading the citations.
//
// This is deliberately NOT semantic/fuzzy dedup: no embedding-distance
// threshold, no LLM judgment call, no near-duplicate heuristic. It only
// ever collapses content that is exactly equal once CRLF is unified to LF,
// each line's trailing whitespace is trimmed, and the whole string's
// leading/trailing whitespace is trimmed. Case, punctuation, and internal
// (mid-line) indentation are left untouched on purpose — two chunks that
// differ only in a capital letter or an extra internal space are NOT
// duplicates and must never be silently merged. See the "四 大小写、标点、
// 内部缩进差异不能误判" acceptance scenario this guards.
//
// A lone CR (not part of a CRLF pair) is deliberately NOT folded into LF —
// review fix: an earlier version of this file also replaced a bare "\r"
// with "\n", which is normalization scope creep beyond what the phase's
// "只统一 CRLF" requirement allows. "a\rb" and "a\nb" must stay distinct.
//
// A normalized key of "" (content that is empty, or entirely whitespace)
// deliberately never participates in dedup at all — review fix: an
// earlier version let every empty/whitespace-only chunk collapse into the
// first one seen, which is wrong (empty content carries no information to
// definitively call the SAME piece of information as another empty-content
// chunk, and it's a degenerate edge case, not the "exact content repeated"
// scenario this feature exists for). See dedupExactContentChunks' doc
// comment for exactly how this is implemented.
//
// This file is pure/DB-free, the same design rule hybrid.go and neighbor.go
// already follow: dedupExactContentChunks takes plain []RetrievedChunk in
// and returns []RetrievedChunk out, so every rule here is testable with
// zero database dependency (see dedup_test.go). It is called from two
// different places in the retrieval pipeline, each with its own ordering
// guarantee that makes the exact same "keep the first occurrence" rule do
// the right thing without this file needing to know which caller it is:
//
//   - hybrid.go's rrfFuse, on the FULL fused (not yet topK-truncated)
//     candidate list, sorted highest-fusionScore-first — see rrfFuse's
//     updated doc comment for why deduping BEFORE truncating to topK is
//     what lets a later, uniquely-worded candidate fill the slot a
//     higher-ranked duplicate would otherwise have wasted.
//   - neighbor.go's expandWithNeighbors, on its two-tier output (every
//     anchor, in rank order, then every neighbor block) — see that
//     function's doc comment for why this makes "core beats a duplicate
//     neighbor" and "earlier neighbor beats a later duplicate neighbor"
//     both fall out of the exact same ordering rule for free.
//
// Both call sites also need a SAFE suppression count for structured debug
// logging (service.go's Retrieve) — "how many duplicates did dedup drop",
// never the content/query/fingerprint that would make that log line a
// privacy leak. dedupExactContentChunks' second return value is exactly
// that count; see its doc comment for what does and does not count toward
// it.
func normalizeContentForDedup(content string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// dedupExactContentChunks removes exact-content duplicates from an
// already-ordered slice (the caller's ordering IS the priority order —
// this function never re-sorts anything) by normalized-content key,
// keeping the FIRST occurrence of each distinct key and dropping every
// later one. It never merges two duplicate entries' fields into one — a
// kept chunk is returned byte-for-byte as the caller passed it in:
// Content (the original, un-normalized text — normalization is only ever
// used to compute the dedup key, never written back), Score, every
// Citation-relevant field (DocumentName/PageNumber/SectionTitle),
// DocumentVersion, and NeighborOf are all left exactly as they already
// were. A dropped duplicate simply does not appear in the output; nothing
// about the entry that IS kept changes because a duplicate of it existed.
//
// Empty content (normalizeContentForDedup returns "" — the chunk's Content
// was empty or entirely whitespace) is a deliberate carve-out: such a chunk
// is ALWAYS kept and NEVER recorded in the seen-keys set, so it can never
// suppress, and can never itself be suppressed by, another empty-content
// chunk. It also never contributes to the returned suppression count. This
// is not merely "don't crash on empty input" — a genuinely empty-content
// chunk existing in the database is itself a data anomaly outside this
// feature's scope; treating two of them as "the same real content
// repeated" would be actively wrong.
//
// The second return value is how many chunks were dropped as duplicates —
// safe to log (it's a count, never content) via service.go's Retrieve,
// which aggregates this across both call sites into
// core_duplicate_count/neighbor_duplicate_count. It only counts genuine
// non-empty-content duplicates; see the empty-content carve-out above.
//
// An empty or nil input returns itself unchanged with a 0 count (nothing to
// dedup); every other input returns a newly allocated slice, the same
// non-mutating convention rrfFuse and expandWithNeighbors already follow.
func dedupExactContentChunks(chunks []RetrievedChunk) ([]RetrievedChunk, int) {
	if len(chunks) == 0 {
		return chunks, 0
	}
	seen := make(map[string]bool, len(chunks))
	out := make([]RetrievedChunk, 0, len(chunks))
	suppressed := 0
	for _, c := range chunks {
		key := normalizeContentForDedup(c.Content)
		if key == "" {
			// Empty/whitespace-only content never participates in dedup —
			// always kept, never tracked in seen, never counted.
			out = append(out, c)
			continue
		}
		if seen[key] {
			suppressed++
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out, suppressed
}
