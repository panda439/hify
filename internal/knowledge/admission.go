package knowledge

// Phase 8: Evidence Admission. Hybrid Search (Phase 3) ranks candidates
// with Reciprocal Rank Fusion, but RRF's fusionScore is a ranking signal,
// not a relevance signal (see hybrid.go's fusionEntry doc comment) — the
// #1-ranked candidate for a query with zero genuinely relevant chunks is
// still "#1", not "relevant". conversation/budget.go's unified
// ragMinSimilarityScore=0.2 floor filters on RetrievedChunk.Score, which is
// ALREADY max(vector, keyword) — a single number that conflates two
// different measurement scales (pgvector cosine similarity vs pg_trgm
// word-similarity) and, worse, only runs after topK/neighbor-window/
// content-dedup have already spent budget on the wrong candidates.
//
// This file adds a source-aware admission gate that runs INSIDE
// hybrid.go's rrfFuse, on each candidate's own original per-path score(s)
// — never on fusionScore, never on the post-max()'d RetrievedChunk.Score —
// before content-dedup and before topK truncation. See the design doc
// (docs/superpowers/specs/2026-08-08-rag-evidence-admission-design.md) §3
// for why the ordering (admit -> dedup -> topK -> neighbors) is
// load-bearing and §4 for why fusionScore specifically must never be
// compared against these thresholds.
const (
	// vectorAdmissionThreshold is the minimum pgvector cosine similarity a
	// candidate's vector-path signal must clear to admit it as a core
	// block, independent of whether it also has a keyword signal. Fixed by
	// design (§2.3) — not a runtime-configurable knob; see the design doc's
	// "不采用：动态相对分差" section for why a relative/adaptive threshold
	// was rejected in favor of this absolute one.
	vectorAdmissionThreshold = 0.35

	// keywordAdmissionThreshold is the minimum pg_trgm word-similarity a
	// candidate's keyword-path signal must clear to admit it, independent
	// of the vector path. Deliberately a different number than
	// vectorAdmissionThreshold — the two similarity measures are not the
	// same scale (design doc §1's "不是同一种量尺" problem statement), so a
	// single shared threshold would either be too strict for one path or
	// too lax for the other.
	keywordAdmissionThreshold = 0.45
)

// admissionOutcome is admitBySourceSignal's pure verdict for one candidate.
// belowVectorSignal/belowKeywordSignal are independent, can both be true at
// once, and — critically — are only ever true when that path's signal
// actually existed (see admitBySourceSignal's doc comment): a candidate
// that was never hit by a path must never be reported as "below" that
// path's threshold, because it has no real signal to be below, only an
// absent one.
type admissionOutcome struct {
	admitted           bool
	belowVectorSignal  bool
	belowKeywordSignal bool
}

// admitBySourceSignal is the pure, DB-free admission rule (design doc §2.3):
// a candidate is admitted if EITHER path it was actually hit by clears that
// path's own absolute threshold. A missing path (haveVector==false or
// haveKeyword==false) never contributes to the decision either way — its
// zero-value score must never be compared against a threshold, which is
// exactly what haveVector/haveKeyword guard against (a candidate that only
// the keyword path found has vectorScore==0 purely because Go zero-values
// float64, not because it "failed" a 0.35 vector check it was never
// actually subjected to).
func admitBySourceSignal(haveVector bool, vectorScore float64, haveKeyword bool, keywordScore float64) admissionOutcome {
	vectorPasses := haveVector && vectorScore >= vectorAdmissionThreshold
	keywordPasses := haveKeyword && keywordScore >= keywordAdmissionThreshold
	return admissionOutcome{
		admitted:           vectorPasses || keywordPasses,
		belowVectorSignal:  haveVector && !vectorPasses,
		belowKeywordSignal: haveKeyword && !keywordPasses,
	}
}

// admissionStats aggregates one rrfFuse call's admission + content-dedup
// counts — the only thing safe to log or persist about this stage (design
// doc §6 / plan Task 2's "统计口径"): no query text, no chunk content, no
// embeddings, no per-candidate score list, ever.
//
// The four admission-stage fields are NOT mutually exclusive in the way
// their names might suggest:
//   - VectorBelowAdmissionCount and KeywordBelowAdmissionCount CAN overlap
//     with each other (a candidate with both signals present and both below
//     threshold counts toward both) and can each independently overlap with
//     a candidate that was ultimately ADMITTED via its other path (e.g.
//     vector below 0.35 but keyword >= 0.45 still counts toward
//     VectorBelowAdmissionCount even though the candidate passed overall).
//     They answer "how many candidates had a weak signal on this specific
//     path", not "how many candidates were rejected because of this path".
//   - AdmissionRejectedCount is the only true admit/reject total: every
//     candidate counts toward it AT MOST ONCE, only when every path signal
//     it actually had failed to clear that path's threshold (see
//     admitBySourceSignal). It must never be computed by summing the two
//     per-path counts above — that would double-count a candidate with two
//     weak signals and would also wrongly count a candidate that was
//     admitted via one path despite being weak on the other.
type admissionStats struct {
	// CandidateCountBeforeAdmission is how many distinct chunk IDs rrfFuse
	// had fused and sorted before the admission gate ran — the full
	// bounded candidate pool, not yet filtered or content-deduped.
	CandidateCountBeforeAdmission int
	// VectorBelowAdmissionCount counts candidates with a present vector
	// signal that failed to clear vectorAdmissionThreshold. Can overlap
	// with KeywordBelowAdmissionCount; see the type doc comment.
	VectorBelowAdmissionCount int
	// KeywordBelowAdmissionCount counts candidates with a present keyword
	// signal that failed to clear keywordAdmissionThreshold. Can overlap
	// with VectorBelowAdmissionCount; see the type doc comment.
	KeywordBelowAdmissionCount int
	// AdmissionRejectedCount is the true reject total — every existing
	// path signal failed. Counted at most once per candidate; never derive
	// this by summing the two fields above.
	AdmissionRejectedCount int
	// ContentDuplicateCount is dedupExactContentChunks' suppression count,
	// run AFTER admission (design doc §3's "准入必须发生在精确内容去重之
	// 前") over only the admitted candidates — an already-rejected
	// candidate is never considered for, and never counted toward, content
	// dedup.
	ContentDuplicateCount int
}
