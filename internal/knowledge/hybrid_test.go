package knowledge

import (
	"fmt"
	"reflect"
	"testing"
)

// rc is a tiny test-only constructor — only ID and Score matter for
// rrfFuse's own logic; the rest of Chunk just needs to round-trip
// unchanged through fusion (see TestRRFFusePreservesChunkMetadata).
//
// Content is set to a value unique per id ("content-"+id) rather than left
// empty: since Phase 5, rrfFuse content-dedups its fused candidate list,
// and every rc-built chunk sharing the same "" Content would otherwise
// collapse into a single result and silently break every pre-Phase-5 test
// in this file that expects N distinct chunks back. Tests that actually
// want to exercise content-dedup build chunks with rcContent instead, so
// they control Content explicitly.
func rc(id string, score float64) RetrievedChunk {
	return RetrievedChunk{Chunk: Chunk{ID: id, Content: "content-" + id}, Score: score}
}

// rcContent is rc plus an explicit, caller-controlled Content — used only
// by the Phase 5 content-dedup tests below, where two chunks deliberately
// need to share (or deliberately must NOT share) a normalized Content
// value.
func rcContent(id string, score float64, content string) RetrievedChunk {
	return RetrievedChunk{Chunk: Chunk{ID: id, Content: content}, Score: score}
}

func idsOf(chunks []RetrievedChunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.ID
	}
	return out
}

// fuseTopK 是 001-rag-query-rerank T026 之后的兼容包装：rrfFuse 本身不再
// 接收/截断 topK（截断挪到了 service.go 的 Retrieve 里，重排之后才做，见
// hybrid.go 的新文档注释与 contracts/internal-contracts.md §4）。这个包内
// 测试文件里几十处既有用例都是在验证"融合排序/去重/tie-break"这些和 topK
// 截断本身无关的行为，把它们全部逐个改写成"调用 rrfFuse 再手动截断"两步会
// 让 diff 爆炸且毫无必要——用一个同名参数列表的包装函数收敛所有调用点，
// 只有 TestRRFFuseWithoutTopKMatchesOldTruncatedBehavior 这一条新测试直接
// 调用真正的 rrfFuse(vectorChunks, keywordChunks)，因为它就是在验证"包装
// 函数的截断"和"真实 rrfFuse 输出的前 topK 项"逐字相同（FR-018 的单测级
// 证据，T025）。
func fuseTopK(vectorChunks, keywordChunks []RetrievedChunk, topK int) ([]RetrievedChunk, admissionStats) {
	got, stats := rrfFuse(vectorChunks, keywordChunks)
	if len(got) > topK {
		got = got[:topK]
	}
	return got, stats
}

// fuseIDs is a test-only convenience wrapper: rrfFuse now returns
// (chunks, admissionStats) since Phase 5/8, so a bare idsOf(fuseTopK(...))
// no longer compiles (idsOf takes one argument, not two) — tests that only
// care about the resulting ID order and don't need to assert on admission
// stats use this instead of re-declaring the two-step "call rrfFuse, then
// idsOf the first return value" dance individually.
//
// Every rc()/rcContent()-built chunk in this file's PRE-Phase-8 tests below
// is given a Score >= vectorAdmissionThreshold when passed as a vector
// candidate — see rc's own doc comment update — specifically so those
// tests keep exercising RRF fusion/dedup/ordering in isolation without
// tripping the Phase 8 admission gate that now also runs inside rrfFuse.
// Phase 8's own admission behavior is covered separately, in
// TestRRFFuse*Admission* below and in admission_test.go.
func fuseIDs(vectorChunks, keywordChunks []RetrievedChunk, topK int) []string {
	got, _ := fuseTopK(vectorChunks, keywordChunks, topK)
	return idsOf(got)
}

// T025：rrfFuse 去掉 topK 参数后必须返回"完整已准入已去重"的候选列表，且
// 对同一输入，真正的 rrfFuse(...)[:topK] 必须和旧版"内部截断"版本
// （fuseTopK 包装）逐字相同——截断挪到 Retrieve 里、发生在重排之后，不能
// 改变"关闭重排时输出与改造前一致"这条不变量（FR-018/SC-003）。
func TestRRFFuseWithoutTopKMatchesOldTruncatedBehavior(t *testing.T) {
	// 全部候选都要清过各自路径的准入门槛（vector>=0.35, keyword>=0.45），
	// 否则会被 admission 提前拒绝，混淆"截断丢的"和"准入拒的"。
	vector := []RetrievedChunk{rc("v1", 0.9), rc("v2", 0.8), rc("v3", 0.7), rc("v4", 0.6)}
	keyword := []RetrievedChunk{rc("k1", 0.5), rc("k2", 0.46)}
	const topK = 3

	full, fullStats := rrfFuse(vector, keyword)
	wantTruncated, wantStats := fuseTopK(vector, keyword, topK)

	if len(full) < topK {
		t.Fatalf("full result has %d entries, want at least topK=%d to actually exercise truncation", len(full), topK)
	}
	if got := idsOf(full[:topK]); !reflect.DeepEqual(got, idsOf(wantTruncated)) {
		t.Fatalf("rrfFuse(...)[:topK] = %v, want %v (must equal the old in-function truncation)", got, idsOf(wantTruncated))
	}
	if fullStats != wantStats {
		t.Fatalf("admissionStats changed by removing topK truncation: got %+v, want %+v", fullStats, wantStats)
	}
	if len(full) != len(vector)+len(keyword) {
		t.Fatalf("full result has %d entries, want the complete admitted+deduped pool (%d) — truncation must be gone from rrfFuse itself", len(full), len(vector)+len(keyword))
	}
}

// 1. 同时命中 vector 和 keyword 的 chunk 排名提升.
func TestRRFFusePromotesChunkHitByBothPaths(t *testing.T) {
	// "both" is a weak vector hit (rank 3) but the #1 keyword hit — RRF's
	// additive score should still push it above pure vector-only hits
	// ranked ahead of it in the vector list alone.
	vector := []RetrievedChunk{rc("v1", 0.9), rc("v2", 0.8), rc("both", 0.5)}
	// k2's keyword score must clear keywordAdmissionThreshold (0.45) or
	// Phase 8's admission gate would drop it before this test ever gets to
	// assert on fusion order.
	keyword := []RetrievedChunk{rc("both", 0.7), rc("k2", 0.5)}

	got, _ := fuseTopK(vector, keyword, 10)

	if len(got) != 4 {
		t.Fatalf("got %d results %v, want 4 distinct chunks", len(got), idsOf(got))
	}
	if got[0].ID != "both" {
		t.Fatalf("rank 1 = %s, want both (hit by both paths should be promoted): %v", got[0].ID, idsOf(got))
	}
}

// 2. 两路重复 chunk 正确去重.
func TestRRFFuseDedupesChunkPresentInBothPaths(t *testing.T) {
	vector := []RetrievedChunk{rc("dup", 0.9), rc("v-only", 0.5)}
	// k-only's score must clear keywordAdmissionThreshold (0.45) — see the
	// same Phase 8 admission-gate note above.
	keyword := []RetrievedChunk{rc("dup", 0.6), rc("k-only", 0.5)}

	got, _ := fuseTopK(vector, keyword, 10)

	seen := map[string]int{}
	for _, c := range got {
		seen[c.ID]++
	}
	if seen["dup"] != 1 {
		t.Fatalf("chunk hit by both paths appeared %d times, want exactly 1: %v", seen["dup"], idsOf(got))
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3 distinct chunks (dup, v-only, k-only): %v", len(got), idsOf(got))
	}
}

// 3. vector-only 正常返回.
func TestRRFFuseVectorOnly(t *testing.T) {
	// v3's score must clear vectorAdmissionThreshold (0.35) — see the
	// Phase 8 admission-gate note in TestRRFFusePromotesChunkHitByBothPaths
	// above; it's still the lowest of the three so descending order is
	// unaffected.
	vector := []RetrievedChunk{rc("v1", 0.9), rc("v2", 0.5), rc("v3", 0.4)}

	got, _ := fuseTopK(vector, nil, 10)

	want := []string{"v1", "v2", "v3"}
	if !reflect.DeepEqual(idsOf(got), want) {
		t.Fatalf("vector-only order = %v, want %v (vector rank order preserved with no keyword input)", idsOf(got), want)
	}
	for i, c := range got {
		if c.Score != vector[i].Score {
			t.Fatalf("chunk %s Score = %f, want the original vector cosine score %f (vector-only: no fusion should change it)", c.ID, c.Score, vector[i].Score)
		}
	}
}

// 4. keyword-only 正常返回.
func TestRRFFuseKeywordOnly(t *testing.T) {
	// k2's score must clear keywordAdmissionThreshold (0.45) — see the
	// Phase 8 admission-gate note above; still the lower of the two so
	// descending order is unaffected.
	keyword := []RetrievedChunk{rc("k1", 0.8), rc("k2", 0.5)}

	got, _ := fuseTopK(nil, keyword, 10)

	want := []string{"k1", "k2"}
	if !reflect.DeepEqual(idsOf(got), want) {
		t.Fatalf("keyword-only order = %v, want %v", idsOf(got), want)
	}
	for i, c := range got {
		if c.Score != keyword[i].Score {
			t.Fatalf("chunk %s Score = %f, want the original keyword word-similarity score %f", c.ID, c.Score, keyword[i].Score)
		}
	}
}

// 5. 相同融合分数时排序稳定，最终按 chunk ID 升序.
//
// vectorWeight/(rrfK+r) and keywordWeight/(rrfK+s) are two independent
// binary64 divisions — mathematically equal at infinitely many (r, s)
// pairs (e.g. r=57, s=3 both equal the exact rational 1/180), but that
// doesn't guarantee IEEE754 produces bit-identical float64 results,
// because 0.65 and 0.35 are themselves not exactly representable in
// binary, so their rounding errors don't generally cancel out across
// different divisors. (r, s) = (70, 10) is a pair verified (by brute
// force, see the phase report) to actually round to the exact same
// float64 in Go — that's the genuine tie this test exercises, not just a
// mathematical coincidence that floating point then quietly breaks.
func TestRRFFuseTiedFusionScoreBreaksByIDWhenScoreAlsoTies(t *testing.T) {
	const vectorRank = 70
	const keywordRank = 10
	if got, want := vectorWeight/(rrfK+vectorRank), keywordWeight/(rrfK+keywordRank); got != want {
		t.Fatalf("precondition failed: vectorWeight/(rrfK+%d) = %v != keywordWeight/(rrfK+%d) = %v — pick a different verified-equal (rank, rank) pair", vectorRank, got, keywordRank, want)
	}

	vector := make([]RetrievedChunk, vectorRank)
	for i := range vector {
		vector[i] = rc(fmt.Sprintf("vfiller%02d", i), 0.99)
	}
	vector[vectorRank-1] = rc("zzz-vector-hit", 0.5)

	keyword := make([]RetrievedChunk, keywordRank)
	for i := range keyword {
		keyword[i] = rc(fmt.Sprintf("kfiller%02d", i), 0.99)
	}
	keyword[keywordRank-1] = rc("aaa-keyword-hit", 0.5)

	got, _ := fuseTopK(vector, keyword, len(vector)+len(keyword))

	var rankOfA, rankOfZ = -1, -1
	for i, c := range got {
		switch c.ID {
		case "aaa-keyword-hit":
			rankOfA = i
		case "zzz-vector-hit":
			rankOfZ = i
		}
	}
	if rankOfA == -1 || rankOfZ == -1 {
		t.Fatalf("both tied chunks must appear in the fused output: got %v", idsOf(got))
	}
	if rankOfA >= rankOfZ {
		t.Fatalf("aaa-keyword-hit (rank %d) should sort before zzz-vector-hit (rank %d) on equal fusionScore+Score: ID ascending was not applied", rankOfA, rankOfZ)
	}
}

// 6. 最终结果不超过 topK.
func TestRRFFuseTruncatesToTopK(t *testing.T) {
	vector := []RetrievedChunk{rc("v1", 0.9), rc("v2", 0.8), rc("v3", 0.7), rc("v4", 0.6)}
	keyword := []RetrievedChunk{rc("k1", 0.5), rc("k2", 0.4)}

	got, _ := fuseTopK(vector, keyword, 3)

	if len(got) != 3 {
		t.Fatalf("got %d results, want exactly topK=3: %v", len(got), idsOf(got))
	}
}

// 7. RetrievedChunk.Score 不能直接使用未归一化的原始 RRF 小数——fusionScore
// values are ~1/60 ≈ 0.016 at best; if Score ever leaked the fusion value
// instead of the original relevance, every one of these chunks would fall
// below budget.go's ragMinSimilarityScore=0.2 floor and get silently
// dropped from the model's context. Assert Score stays in the original
// relevance domain the caller supplied.
func TestRRFFuseScoreIsRelevanceNotRawFusionScore(t *testing.T) {
	vector := []RetrievedChunk{rc("v1", 0.93)}
	keyword := []RetrievedChunk{rc("k1", 0.81)}

	got, _ := fuseTopK(vector, keyword, 10)

	byID := map[string]RetrievedChunk{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if byID["v1"].Score != 0.93 {
		t.Fatalf("v1.Score = %f, want the original vector relevance 0.93 (not an RRF fraction)", byID["v1"].Score)
	}
	if byID["k1"].Score != 0.81 {
		t.Fatalf("k1.Score = %f, want the original keyword relevance 0.81 (not an RRF fraction)", byID["k1"].Score)
	}
	// A raw single-path RRF contribution at best rank is
	// max(vectorWeight, keywordWeight)/(rrfK+1) ≈ 0.65/61 ≈ 0.0107 — every
	// Score above must stay far above that, confirming it was never
	// overwritten by a fusion fraction.
	const maxPlausibleRawRRFValue = 0.02
	for _, c := range got {
		if c.Score < maxPlausibleRawRRFValue {
			t.Fatalf("chunk %s Score = %f looks like a raw RRF fraction, not a 0..1 relevance score", c.ID, c.Score)
		}
	}
}

// hit-by-both-paths: Score must be the MAX of the two original relevance
// numbers, not their sum, not the fusion score.
func TestRRFFuseScoreForBothPathsHitIsMaxNotSum(t *testing.T) {
	vector := []RetrievedChunk{rc("both", 0.4)}
	keyword := []RetrievedChunk{rc("both", 0.9)}

	got, _ := fuseTopK(vector, keyword, 10)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %v", len(got), idsOf(got))
	}
	if got[0].Score != 0.9 {
		t.Fatalf("both.Score = %f, want max(0.4, 0.9) = 0.9", got[0].Score)
	}
}

// rrfFuse itself treats each input slice's POSITION as that path's RRF
// rank, so literally shuffling vector/keyword before calling rrfFuse would
// change what the input means (a chunk moved to a different index now
// has a different rank), not just how it's expressed — there's no
// meaningful "same rank data, different input order" to feed rrfFuse
// directly. What this test actually establishes is narrower but still
// real: rrfFuse builds an internal map (entries) before converting it
// back to a slice, and Go randomizes map iteration order per run — this
// confirms that internal randomness never leaks into the final order
// (the fusionScore/Score/ID sort fully re-establishes it every time).
// See TestVectorCandidateBuildOrderDoesNotAffectFinalHybridResult below
// for the test that actually covers "upstream candidate order changes,
// final result doesn't" — that's a property of
// sortVectorCandidatesByScoreThenID (hybrid.go), which runs BEFORE
// rrfFuse, not of rrfFuse itself.
func TestRRFFuseInternalMapIterationDoesNotLeakIntoOutputOrder(t *testing.T) {
	vector := []RetrievedChunk{rc("v1", 0.9), rc("v2", 0.7), rc("both", 0.5), rc("v3", 0.3)}
	keyword := []RetrievedChunk{rc("both", 0.6), rc("k1", 0.4), rc("k2", 0.2)}

	first := fuseIDs(vector, keyword, 10)
	for i := 0; i < 20; i++ {
		got := fuseIDs(vector, keyword, 10)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: order changed across repeated calls with identical input: got %v, want %v", i, got, first)
		}
	}
}

// sortVectorCandidatesByScoreThenID: 多个向量候选 Score 完全相同时，必须
// 按 chunk ID 升序稳定排序，不管输入进来的顺序是什么样的——这是
// service.go 在合并多个 embedding model 分组的结果时依赖的不变量（分组
// 来自一个 map，Go 每次运行的遍历顺序都不同）。
func TestSortVectorCandidatesByScoreThenIDStableOnTiedScores(t *testing.T) {
	orderings := [][]RetrievedChunk{
		{rc("c3", 0.5), rc("c1", 0.5), rc("c2", 0.5)},
		{rc("c1", 0.5), rc("c2", 0.5), rc("c3", 0.5)},
		{rc("c2", 0.5), rc("c3", 0.5), rc("c1", 0.5)},
	}
	want := []string{"c1", "c2", "c3"}
	for i, candidates := range orderings {
		cp := append([]RetrievedChunk(nil), candidates...)
		sortVectorCandidatesByScoreThenID(cp)
		if got := idsOf(cp); !reflect.DeepEqual(got, want) {
			t.Fatalf("input order %d: got %v, want %v (equal Score must sort by ID ascending regardless of input order)", i, got, want)
		}
	}
}

// 不同相关度的候选绝不能被 ID 兜底规则重排——ID 只在 Score 相等时才生效。
func TestSortVectorCandidatesByScoreThenIDNeverReordersDifferentScores(t *testing.T) {
	// "zzz" has the highest Score despite the largest ID, "aaa" has the
	// lowest Score despite the smallest ID — if the tie-break rule were
	// ever applied unconditionally, this would come out ID-ascending
	// (aaa, mmm, zzz) instead of Score-descending (zzz, mmm, aaa).
	candidates := []RetrievedChunk{rc("aaa", 0.1), rc("zzz", 0.9), rc("mmm", 0.5)}
	sortVectorCandidatesByScoreThenID(candidates)
	want := []string{"zzz", "mmm", "aaa"}
	if got := idsOf(candidates); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v — differing Score must never be overridden by the ID tie-break", got, want)
	}
}

// 改变模型分组/候选追加顺序（模拟 kbsByModel 的 map 遍历顺序在不同运行间
// 变化），送进 rrfFuse 之前先跑 sortVectorCandidatesByScoreThenID，最终
// hybrid 结果必须完全一致——这是 service.go 真实调用序列
// (concat -> sortVectorCandidatesByScoreThenID -> rrfFuse) 的端到端覆盖，
// 不只是 rrfFuse 自己的内部稳定性。
func TestVectorCandidateBuildOrderDoesNotAffectFinalHybridResult(t *testing.T) {
	// Two "model groups" worth of candidates, some with tied Score across
	// groups (c-tie-a/c-tie-b both 0.5) — exactly the scenario a map
	// iteration reorder would otherwise expose.
	groupA := []RetrievedChunk{rc("g-a1", 0.8), rc("c-tie-a", 0.5)}
	groupB := []RetrievedChunk{rc("g-b1", 0.6), rc("c-tie-b", 0.5)}
	keyword := []RetrievedChunk{rc("k1", 0.7)}

	buildOrders := [][]RetrievedChunk{
		append(append([]RetrievedChunk{}, groupA...), groupB...),
		append(append([]RetrievedChunk{}, groupB...), groupA...),
		{groupA[1], groupB[0], groupA[0], groupB[1]}, // fully interleaved, arbitrary
	}

	var want []string
	for i, order := range buildOrders {
		cp := append([]RetrievedChunk(nil), order...)
		sortVectorCandidatesByScoreThenID(cp)
		got := fuseIDs(cp, keyword, 10)
		if i == 0 {
			want = got
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("build order %d: got %v, want %v (same underlying candidates, different concatenation order, must fuse identically)", i, got, want)
		}
	}
}

// Chunk metadata (everything besides ID/Score) must round-trip through
// fusion untouched — rrfFuse only ever overwrites Score, never any other
// field, so Citation-relevant metadata (DocumentName/PageNumber/
// SectionTitle, checked at the repository/integration level in
// integration_test.go) can't be silently dropped by the fusion step
// itself.
func TestRRFFusePreservesChunkMetadata(t *testing.T) {
	docName := "handbook.pdf"
	page := 7
	section := "3.2 Refunds"
	c := RetrievedChunk{
		Chunk: Chunk{
			ID:           "meta1",
			DocumentName: docName,
			PageNumber:   &page,
			PageEnd:      &page,
			SectionTitle: &section,
		},
		Score: 0.77,
	}

	got, _ := fuseTopK([]RetrievedChunk{c}, nil, 10)
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].DocumentName != docName {
		t.Fatalf("DocumentName = %q, want %q", got[0].DocumentName, docName)
	}
	if got[0].PageNumber == nil || *got[0].PageNumber != page {
		t.Fatalf("PageNumber = %v, want %d", got[0].PageNumber, page)
	}
	if got[0].SectionTitle == nil || *got[0].SectionTitle != section {
		t.Fatalf("SectionTitle = %v, want %q", got[0].SectionTitle, section)
	}
}

// --- Phase 5: rrfFuse content-dedup ---

// 四. A、A、B、C（A 的两条记录内容完全相同、ID 不同）+ topK=3，去重后应
// 该得到 A、B、C，而不是把去重前排名靠前的两条 A 都截进结果、把 C 挤出去。
func TestRRFFuseContentDedupLetsUniqueLowerRankedCandidateFillTopKSlot(t *testing.T) {
	vector := []RetrievedChunk{
		rcContent("a-high", 0.95, "重复的正文内容"),
		rcContent("a-dup", 0.90, "重复的正文内容"), // 和 a-high 内容相同，融合排名仅次于它
		rcContent("b", 0.80, "内容B"),
		rcContent("c", 0.70, "内容C"),
	}

	got, _ := fuseTopK(vector, nil, 3)

	want := []string{"a-high", "b", "c"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v — content-dedup must run BEFORE topK truncation so c can fill the slot a-dup would otherwise have wasted", got, want)
	}
}

// 去重必须保留原本融合排名更高的那一条重复内容，而不是任意一条或最后一条。
func TestRRFFuseContentDedupKeepsHigherFusionRankedDuplicate(t *testing.T) {
	vector := []RetrievedChunk{rcContent("low-rank-dup", 0.99, "重复内容")}
	keyword := []RetrievedChunk{rcContent("high-rank-dup", 0.5, "重复内容")}
	// high-rank-dup is keyword rank 1 (weight 0.35/(60+1)), low-rank-dup is
	// vector rank 1 (weight 0.65/(60+1)) — vector's higher weight at the
	// same rank makes low-rank-dup's fusionScore the larger one, so despite
	// the variable names this exercises "whichever ends up ranked first by
	// fusionScore survives", not literally input order.
	got, _ := fuseTopK(vector, keyword, 10)
	if len(got) != 1 {
		t.Fatalf("got %d results %v, want exactly 1 (same normalized content must collapse to one)", len(got), idsOf(got))
	}
	if got[0].ID != "low-rank-dup" {
		t.Fatalf("kept ID = %s, want low-rank-dup (the higher-fusionScore duplicate, per vectorWeight > keywordWeight at equal rank)", got[0].ID)
	}
}

// 内容去重发生在 fusionScore/Score/ID 排序之后，必须保持结果整体确定性
// （多次调用完全一致），不能因为 Go map 遍历顺序而抖动。
func TestRRFFuseContentDedupIsDeterministicAcrossRuns(t *testing.T) {
	vector := []RetrievedChunk{
		rcContent("d1", 0.9, "重复A"),
		rcContent("d2", 0.8, "重复A"),
		rcContent("d3", 0.7, "重复A"),
		rcContent("u1", 0.6, "唯一内容"),
	}
	first := fuseIDs(vector, nil, 10)
	for i := 0; i < 10; i++ {
		got := fuseIDs(vector, nil, 10)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: got %v, want %v (content-dedup result must be stable across repeated calls)", i, got, first)
		}
	}
	if !reflect.DeepEqual(first, []string{"d1", "u1"}) {
		t.Fatalf("got %v, want [d1 u1]", first)
	}
}

// 待修复项 3（审核修复）: rrfFuse 的第二个返回值是核心候选阶段被内容去重
// 抑制掉的条数，必须对得上真实丢弃的重复数量，不受 topK 截断影响（去重发
// 生在截断之前，所以即便被去重后剩下的候选还会被 topK 截断，抑制计数只
// 反映"内容去重丢了几条"，不包含"topK 截断又丢了几条"）。
func TestRRFFuseReturnsCoreDuplicateCount(t *testing.T) {
	vector := []RetrievedChunk{
		rcContent("a-high", 0.95, "重复内容A"),
		rcContent("a-dup1", 0.90, "重复内容A"),
		rcContent("a-dup2", 0.85, "重复内容A"),
		rcContent("b", 0.80, "内容B"),
	}
	_, stats := fuseTopK(vector, nil, 10)
	if stats.ContentDuplicateCount != 2 {
		t.Fatalf("ContentDuplicateCount = %d, want 2 (a-dup1 and a-dup2 both suppressed, a-high survives, b is unique)", stats.ContentDuplicateCount)
	}

	// 无重复内容时计数为 0。
	noDup := []RetrievedChunk{rcContent("x", 0.9, "内容X"), rcContent("y", 0.8, "内容Y")}
	_, zeroStats := fuseTopK(noDup, nil, 10)
	if zeroStats.ContentDuplicateCount != 0 {
		t.Fatalf("ContentDuplicateCount = %d, want 0 (no duplicate content)", zeroStats.ContentDuplicateCount)
	}
}

func TestCandidateKBounds(t *testing.T) {
	cases := []struct {
		topK int
		want int
	}{
		{topK: 1, want: 4},
		{topK: 5, want: 20},
		{topK: 30, want: 100}, // 30*4=120, clamped to 100
		{topK: 50, want: 100}, // maxTopK, 50*4=200, clamped to 100
		{topK: 0, want: 0},    // defensive: callers always pass a clamped topK, but the formula itself shouldn't panic or go negative
	}
	for _, tc := range cases {
		if got := candidateK(tc.topK); got != tc.want {
			t.Fatalf("candidateK(%d) = %d, want %d", tc.topK, got, tc.want)
		}
	}
}
