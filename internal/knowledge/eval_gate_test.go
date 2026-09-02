package knowledge

// Phase 6: Deterministic Retrieval Eval Gate.
//
// This file drives a real knowledge.Service.Retrieve() call (real MySQL +
// real PostgreSQL/pgvector/pg_trgm, fake embedding provider — no LLM, no
// Judge, no external API) against a fixed, isolated dataset and gates on
// the deterministic metrics defined in internal/eval/retrieval (gate.go):
// Hit@1, Hit@3, MRR, and a content-uniqueness rate.
// It is NOT a new algorithm — Phase 3 (Hybrid Search), Phase 4 (neighbor
// window), Phase 5 (exact content dedup) and Phase 8 (evidence admission)
// are exercised exactly as written; this only adds a repeatable pass/fail
// baseline over them. The dataset was 6 cases as of Phase 6; Phase 8 added
// 3 more (nonempty_kb_irrelevant_query, vector_below_admission,
// admitted_candidate_backfills_topk) covering the admission gate's
// negative and boundary behavior, for 9 total — see each t.Run below for
// what it verifies.
//
// Why this lives here instead of a new cmd/evalrunner subcommand or inside
// internal/eval/retrieval: the dataset needs precise control over chunk embeddings,
// content, chunk_index adjacency and publish state — the same unexported
// seeding helpers (createChunks, publishDocumentVersion,
// seedNeighborChunkBatch, seedChunkWithContent, ...) every other Phase
// 3/4/5 integration test in this file already uses. Building this in a
// separate package would mean either exporting those helpers (widening
// knowledge's public surface for no production reason) or duplicating them
// (a second copy to keep in sync). The pure metric/threshold logic that
// CAN live outside this package already does — see
// internal/eval/retrieval's gate.go doc comment for why that's a sibling
// leaf package rather than a reuse of internal/eval directly (internal/eval
// imports internal/conversation, which imports internal/knowledge — this
// test file importing internal/eval would form an import cycle) and for
// why that split also keeps this gate from depending on cmd/evalrunner's
// LLM-judge machinery, which the task explicitly forbids pulling in here.
//
// Isolation/idempotency (task requirement 6): setupEvalGate uses its own
// testutil DB name ("evalgate"), never "knowledge" — testutil.MySQL/
// Postgres DROP-and-recreate that database fresh on every test run (see
// internal/testutil's package doc), so this dataset can never collide with
// another package's fixture IDs, never accumulates stale rows across runs,
// and never touches the dev database or any other test's data. Nothing
// here needs its own manual cleanup step.
//
// Privacy (task requirement 5): retrievalGateOutcome is the only place
// that ever looks at a returned chunk's Content — it reduces every case
// down to counts (ResultCount/DuplicateContentCount) and a HitRank before
// returning, and separately collects only chunk/document ID, rank, and
// NeighborOf into retrieval.GateHit for the saved report. Neither return
// value nor the report has a field a chunk's Content, this test's query
// text, an embedding vector, or a relevance Score could end up in — Score
// was in an earlier version of GateHit and got removed per Codex's
// first-round Phase 6 review (待修复项 1): the task's allow-list is case
// name/chunk-document ID/rank/NeighborOf/counts/aggregate metrics, and
// Score isn't on it. internal/eval/retrieval/report_test.go has a
// reflection-based test asserting no field in the report's type graph is
// named content/score/query/embedding/fingerprint, so this can't silently
// regress again.
//
// Report path (task requirement 4's "报告是side artifact, 不是唯一强制手
// 段" plus Codex's 待修复项 2): retrievalGateReportPath defaults to
// t.TempDir() — `go test`'s working directory is the package directory
// (internal/knowledge), so a bare relative path like "eval/runs/..." would
// have written into internal/knowledge/eval/runs/, polluting the source
// tree on every plain `go test ./...` run (exactly the bug Codex's review
// caught: git status showed an untracked internal/knowledge/eval/ after a
// real run). The HIFY_RETRIEVAL_GATE_REPORT_PATH env var lets
// `make eval-retrieval-gate` opt into a human-readable report at a
// repo-root-relative path without every other `go test` invocation
// (including plain `go test ./...` in CI) writing anywhere near the
// source tree.
//
// Entry point (task requirement 7): `make eval-retrieval-gate` runs
// `go test -v -race -count=1 -run TestRetrievalGatePhase6 ./internal/knowledge/`
// with HIFY_RETRIEVAL_GATE_REPORT_PATH set to an absolute,
// repo-root-relative eval/runs/ path — a real go test invocation, not a
// new binary, so it inherits testutil's documented
// skip-when-containers-are-down behavior (printed, never silent) instead
// of re-implementing DB bootstrap/skip logic outside the *testing.T-based
// machinery every other integration test already relies on.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hify/internal/eval/retrieval"
	"hify/internal/provider"
	"hify/internal/testutil"
)

// retrievalGateReportPathEnv, when set to a non-empty value, overrides
// where TestRetrievalGatePhase6 saves its human-readable report — see
// this file's top doc comment's "Report path" paragraph for why a plain
// `go test` run must never write into the source tree by default.
const retrievalGateReportPathEnv = "HIFY_RETRIEVAL_GATE_REPORT_PATH"

// retrievalGateReportPath resolves where this run's report gets saved:
// the env var if set, otherwise a path inside t.TempDir() (cleaned up
// automatically after the test, and never anywhere near the source tree
// regardless of the test binary's working directory) — see
// TestRetrievalGateReportPathDefaultsAwayFromSourceTree /
// TestRetrievalGateReportPathHonorsEnvOverride for the regression tests
// covering both branches.
func retrievalGateReportPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(retrievalGateReportPathEnv); p != "" {
		return p
	}
	return filepath.Join(t.TempDir(), "phase6-retrieval-gate-latest.json")
}

func setupEvalGate(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(testutil.MySQL(t, "evalgate"), testutil.Postgres(t, "evalgate"))
}

func docSet(ids ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		s[id] = struct{}{}
	}
	return s
}

// retrievalGateOutcome runs one case's Retrieve() call and reduces the raw
// result into a privacy-safe retrieval.CaseOutcome plus a
// []retrieval.GateHit for the saved report — see this file's top doc
// comment's "Privacy" paragraph. DuplicateContentCount is computed here
// independently of dedupExactContentChunks (a second, separate pass over
// what Retrieve actually returned, using the same normalizeContentForDedup
// key) specifically so a bug in the production dedup call sites shows up
// as a nonzero count here rather than being invisible because this test
// trusted the same code path it's supposed to be checking.
func retrievalGateOutcome(t *testing.T, svc Service, kbIDs []string, query string, topK int, name string, expectedDocIDs map[string]struct{}) (retrieval.CaseOutcome, []retrieval.GateHit) {
	t.Helper()
	return retrievalGateOutcomeWithOptions(t, svc, kbIDs, query, topK, name, expectedDocIDs, RetrieveOptions{})
}

// retrievalGateOutcomeWithOptions 就是 retrievalGateOutcome 多带一个显式的
// RetrieveOptions——002-metadata-filter 新增的两条门禁用例需要它。所有既有用例
// 继续走上面那个传零值的包装函数，因此它们记录下来的结果与该功能上线前逐位相同。
func retrievalGateOutcomeWithOptions(t *testing.T, svc Service, kbIDs []string, query string, topK int, name string, expectedDocIDs map[string]struct{}, opts RetrieveOptions) (retrieval.CaseOutcome, []retrieval.GateHit) {
	t.Helper()
	ctx := context.Background()
	got, err := svc.Retrieve(ctx, kbIDs, query, topK, opts)
	if err != nil {
		t.Fatalf("case %q: Retrieve: %v", name, err)
	}

	outcome := retrieval.CaseOutcome{
		Name:               name,
		ExpectedConfigured: len(expectedDocIDs) > 0,
		ResultCount:        len(got),
	}
	hits := make([]retrieval.GateHit, 0, len(got))
	seenContent := make(map[string]bool, len(got))
	for i, c := range got {
		rank := i + 1
		if outcome.HitRank == 0 {
			if _, ok := expectedDocIDs[c.DocumentID]; ok {
				outcome.HitRank = rank
			}
		}
		if key := normalizeContentForDedup(c.Content); key != "" {
			if seenContent[key] {
				outcome.DuplicateContentCount++
			} else {
				seenContent[key] = true
			}
		}
		hits = append(hits, retrieval.GateHit{
			Rank: rank, ChunkID: c.ID, DocumentID: c.DocumentID,
			KnowledgeBaseID: c.KnowledgeBaseID, NeighborOf: c.NeighborOf,
		})
	}
	return outcome, hits
}

// retrievalGateThresholds is this dataset's fixed pass criteria. Every
// case in this dataset is deliberately constructed so a correctly
// functioning Phase 3/4/5 pipeline hits at rank 1 (see each t.Run's
// comment for the score arithmetic that makes this deterministic, not
// coincidental) and returns zero duplicate content — so a healthy run
// scores a clean 1.0 on all four metrics, and ANY drop below 1.0 on any of
// them is a real regression, not dataset noise. That is what makes
// MinHitAt1=MinHitAt3=MinMRR=MinContentUniqueRate=1.0 the right thresholds
// here (a looser dataset with legitimately-hard cases would need looser
// thresholds; this one doesn't have any).
func retrievalGateThresholds() retrieval.GateThresholds {
	return retrieval.GateThresholds{
		MinHitAt1:            1.0,
		MinHitAt3:            1.0,
		MinMRR:               1.0,
		MinContentUniqueRate: 1.0,
	}
}

func TestRetrievalGatePhase6(t *testing.T) {
	repo := setupEvalGate(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())

	seedKB(t, repo, "kb-gate-vector", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-keyword", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-dedup", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-neighbor", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-hybrid-dedup", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-empty", "m3", "gate-user", true) // deliberately zero chunks
	// Phase 8: evidence admission negative/boundary cases.
	seedKB(t, repo, "kb-gate-admission-irrelevant", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-admission-vec-reject", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-admission-backfill", "m3", "gate-user", true)
	// 001-rag-query-rerank T038a：SC-001（查询改写）与 SC-002（重排）的受控用例。
	seedKB(t, repo, "kb-gate-rewrite", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-rerank", "m3", "gate-user", true)
	// 002-metadata-filter：SC-001（文档级过滤）与 SC-002（页码过滤）的受控用例。
	seedKB(t, repo, "kb-gate-filter-doc", "m3", "gate-user", true)
	seedKB(t, repo, "kb-gate-filter-page", "m3", "gate-user", true)

	var (
		outcomes []retrieval.CaseOutcome
		cases    []retrieval.GateCaseReport
	)
	record := func(o retrieval.CaseOutcome, hits []retrieval.GateHit) {
		outcomes = append(outcomes, o)
		cases = append(cases, retrieval.GateCaseReport{Name: o.Name, Hits: hits, Outcome: o})
	}

	// 1. 语义向量命中：三条候选里只有一条余弦相似度非零，其余两条正交——
	// 命中必须排第一（fusionScore = 0.65/(60+1) 远高于两条 cos=0 候选）。
	// 查询词刻意不出现在任何一条正文里，确保关键词路径不参与，纯粹验证向量
	// 语义检索。
	t.Run("vector_semantic_hit", func(t *testing.T) {
		seedChunkWithContent(t, repo, "kb-gate-vector", "doc-gate-vector", "gv-hit", []float32{1, 0, 0}, "语义向量命中示例内容：讲解检索排序算法")
		seedChunkWithContent(t, repo, "kb-gate-vector", "doc-gate-vector-other", "gv-decoy-a", []float32{0, 1, 0}, "无关内容：食堂本周菜单更新")
		seedChunkWithContent(t, repo, "kb-gate-vector", "doc-gate-vector-other", "gv-decoy-b", []float32{0, 0, 1}, "无关内容：园区班车时刻表调整")

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-vector"}, "向量语义检索场景查询ZZZNOTOKEN", 3, "vector_semantic_hit", docSet("doc-gate-vector"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1 (gv-hit is the only non-orthogonal vector candidate)", o.HitRank)
		}
		if o.DuplicateContentCount != 0 {
			t.Fatalf("DuplicateContentCount = %d, want 0", o.DuplicateContentCount)
		}
		record(o, hits)
	})

	// 2. 强关键词命中进入 topK：kw-hit 向量分是三者中最弱的（cos=0，向量单路
	// 排名会掉到第 4，topK=3 时被淘汰），但正文含查询里的精确关键词——RRF
	// 融合后 fusionScore = 0.65/(60+4) + 0.35/(60+1) ≈ 0.015894，比向量单路
	// 排名第一的 gk-v1（0.65/(60+1) ≈ 0.010656）还高，必须冲到融合结果第一
	// 位，把向量单路本该进 topK 的 gk-v3 挤出去。
	t.Run("keyword_strong_hit", func(t *testing.T) {
		seedChunkWithContent(t, repo, "kb-gate-keyword", "doc-gate-kw-decoy", "gk-v1", []float32{1, 0, 0}, "无关内容一：项目周报草稿")
		seedChunkWithContent(t, repo, "kb-gate-keyword", "doc-gate-kw-decoy", "gk-v2", []float32{1, 0.1, 0}, "无关内容二：会议室预定安排")
		seedChunkWithContent(t, repo, "kb-gate-keyword", "doc-gate-kw-decoy", "gk-v3", []float32{1, 0.2, 0}, "无关内容三：办公用品采购清单")
		seedChunkWithContent(t, repo, "kb-gate-keyword", "doc-gate-kw-hit", "gk-hit", []float32{0, 1, 0}, "本段正文包含精确关键词GATEKWTOKEN用于命中验证")

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-keyword"}, "GATEKWTOKEN", 3, "keyword_strong_hit", docSet("doc-gate-kw-hit"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1 (strong keyword match must win RRF fusion outright, see this test's comment for the score arithmetic)", o.HitRank)
		}
		for _, h := range hits {
			if h.ChunkID == "gk-v3" {
				t.Fatalf("gk-v3 (weakest vector-only top3 member) should have been displaced out of topK by the keyword hit, but it's still present: %+v", hits)
			}
		}
		if o.DuplicateContentCount != 0 {
			t.Fatalf("DuplicateContentCount = %d, want 0", o.DuplicateContentCount)
		}
		record(o, hits)
	})

	// 3. 不同 ID、完全相同正文只保留排名最高者，唯一候选补位：dup-high 和
	// dup-low 正文完全相同（dup-high 余弦分更高），topK=2 时必须只保留
	// dup-high，让内容不同的 gd-unique 补上被去重腾出的名额——不能是
	// [dup-high, dup-low]。
	t.Run("content_dedup_topk_fill", func(t *testing.T) {
		dupContent := "内容去重验收场景：完全相同的正文，用于验证只保留最高排名者"
		seedChunkWithContent(t, repo, "kb-gate-dedup", "doc-gate-dedup", "gd-dup-high", []float32{1, 0, 0}, dupContent)
		seedChunkWithContent(t, repo, "kb-gate-dedup", "doc-gate-dedup", "gd-dup-low", []float32{1, 0.1, 0}, dupContent)
		seedChunkWithContent(t, repo, "kb-gate-dedup", "doc-gate-dedup", "gd-unique", []float32{1, 0.2, 0}, "内容去重验收场景：内容不同的第三条候选，应当补位")

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-dedup"}, "内容去重场景查询ZZZNOTOKEN", 2, "content_dedup_topk_fill", docSet("doc-gate-dedup"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1", o.HitRank)
		}
		if len(hits) != 2 {
			t.Fatalf("got %d hits, want exactly 2 (topK=2, dedup must free the slot dup-low would have wasted)", len(hits))
		}
		gotIDs := []string{hits[0].ChunkID, hits[1].ChunkID}
		if gotIDs[0] != "gd-dup-high" || gotIDs[1] != "gd-unique" {
			t.Fatalf("got %v, want [gd-dup-high gd-unique] — dedup must keep the higher-ranked duplicate and let the unique candidate fill the freed slot", gotIDs)
		}
		if o.DuplicateContentCount != 0 {
			t.Fatalf("DuplicateContentCount = %d, want 0 (dup-low must never reach the returned result set)", o.DuplicateContentCount)
		}
		record(o, hits)
	})

	// 4. 核心块优先于正文重复的邻接块：anchor 的邻接块 anchor-prev 与另一
	// 个核心命中 core2 正文完全相同——两个核心块（anchor、core2）都必须保
	// 留，重复的邻接块 anchor-prev 必须被丢弃。anchor-prev 的向量与查询正
	// 交（cos=0），只能通过 anchor 的 chunk_index 邻接窗口被捞回，不能凭
	// 向量分单独赢得核心命中名额——同 Phase 5 待修复项 4 的 fixture 设计。
	t.Run("core_over_duplicate_neighbor", func(t *testing.T) {
		sharedContent := "核心块与邻接块共享的重复正文，核心块必须优先保留"
		seedNeighborChunkBatch(t, repo, "kb-gate-neighbor", "doc-gate-nb-anchor", 1, []neighborSeedChunk{
			{ID: "gn-anchor", ChunkIndex: 5, Content: "anchor 自己独有的正文", Vec: []float32{1, 0, 0}},
			{ID: "gn-anchor-prev", ChunkIndex: 4, Content: sharedContent, Vec: []float32{0, 1, 0}},
		}, true)
		seedNeighborChunkBatch(t, repo, "kb-gate-neighbor", "doc-gate-nb-core2", 1, []neighborSeedChunk{
			{ID: "gn-core2", ChunkIndex: 0, Content: sharedContent, Vec: []float32{1, 0, 0}},
		}, true)

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-neighbor"}, "核心邻接去重场景查询", 2, "core_over_duplicate_neighbor", docSet("doc-gate-nb-anchor", "doc-gate-nb-core2"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1", o.HitRank)
		}
		foundAnchor, foundCore2 := false, false
		for _, h := range hits {
			if h.ChunkID == "gn-anchor-prev" {
				t.Fatalf("gn-anchor-prev duplicates gn-core2's content and must have been dropped, got %+v", hits)
			}
			if h.ChunkID == "gn-anchor" {
				foundAnchor = true
			}
			if h.ChunkID == "gn-core2" {
				foundCore2 = true
			}
		}
		if !foundAnchor || !foundCore2 {
			t.Fatalf("want both core hits (gn-anchor, gn-core2) present, got %+v", hits)
		}
		if o.DuplicateContentCount != 0 {
			t.Fatalf("DuplicateContentCount = %d, want 0", o.DuplicateContentCount)
		}
		record(o, hits)
	})

	// 5. 同一 chunk 被向量/关键词同时召回时不重复：gh-both 的正文既含精确
	// 关键词又是最强的向量命中，必须只在最终结果里出现一次。
	t.Run("hybrid_dedup_same_chunk_both_paths", func(t *testing.T) {
		seedChunkWithContent(t, repo, "kb-gate-hybrid-dedup", "doc-gate-hybrid-dedup", "gh-both", []float32{1, 0, 0}, "去重验证关键词GATEHYBRIDTOKEN同时命中向量与关键词两路")
		seedChunkWithContent(t, repo, "kb-gate-hybrid-dedup", "doc-gate-hybrid-dedup-other", "gh-other", []float32{1, 0.5, 0}, "无关内容，仅用于填充候选集")

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-hybrid-dedup"}, "GATEHYBRIDTOKEN", 2, "hybrid_dedup_same_chunk_both_paths", docSet("doc-gate-hybrid-dedup"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1", o.HitRank)
		}
		count := 0
		for _, h := range hits {
			if h.ChunkID == "gh-both" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("gh-both appeared %d times in the final result, want exactly 1 (hit by both vector and keyword paths must fuse into a single entry)", count)
		}
		if o.DuplicateContentCount != 0 {
			t.Fatalf("DuplicateContentCount = %d, want 0", o.DuplicateContentCount)
		}
		record(o, hits)
	})

	// 6. 不相关查询/空结果场景不制造命中：kb-gate-empty 一个 chunk 都没有
	// 种，Retrieve 必须老实返回空结果，不能凭空造出命中。
	t.Run("no_results_negative", func(t *testing.T) {
		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-empty"}, "任意查询文本", 3, "no_results_negative", nil)
		if o.ExpectedConfigured {
			t.Fatalf("ExpectedConfigured = true, want false (this case configures no accepted document)")
		}
		if o.ResultCount != 0 || len(hits) != 0 {
			t.Fatalf("ResultCount = %d, hits = %+v, want 0/empty — an empty knowledge base must never fabricate a hit", o.ResultCount, hits)
		}
		record(o, hits)
	})

	// 7. Phase 8: 非空知识库 + 无关查询，必须返回空——不能因为 KB 非空就
	// 硬凑一个命中出来。用真实 pgvector/pg_trgm 的零信号（正交向量 + 与
	// 查询完全无字符重叠的正文）验证准入层的保守边界。这是一个负样本
	// case：ExpectedConfigured=false，不参与 Hit@1/Hit@3/MRR 平均，但必须
	// 有独立断言（下面的 ResultCount 检查），不能只靠聚合指标筛掉就当作
	// 门禁通过。
	t.Run("nonempty_kb_irrelevant_query", func(t *testing.T) {
		seedChunkWithContent(t, repo, "kb-gate-admission-irrelevant", "doc-gate-admission-irrelevant", "ga-irrelevant",
			[]float32{0, 1, 0}, "内容与关键词完全无关的填充文本：门禁场景七")

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-admission-irrelevant"}, "GATEADMISSIONIRRELEVANTTOKEN", 3, "nonempty_kb_irrelevant_query", nil)
		if o.ExpectedConfigured {
			t.Fatalf("ExpectedConfigured = true, want false (this case configures no accepted document)")
		}
		if o.ResultCount != 0 || len(hits) != 0 {
			t.Fatalf("ResultCount = %d, hits = %+v, want 0/empty — a non-empty KB with zero qualifying admission evidence must never fabricate a hit", o.ResultCount, hits)
		}
		record(o, hits)
	})

	// 8. Phase 8: 仅有低于 0.35 门槛的向量候选，必须返回空——候选存在
	// （不是空知识库），但唯一的候选余弦相似度（0.2）不达标，也没有任何
	// 关键词信号，准入层必须整体拒绝，不能让"唯一候选"这个事实本身放宽
	// 准入。同样是负样本 case，独立断言 ResultCount。
	t.Run("vector_below_admission", func(t *testing.T) {
		seedChunkWithContent(t, repo, "kb-gate-admission-vec-reject", "doc-gate-admission-vec-reject", "ga-vec-below",
			cosineVec(0.2), "内容与关键词完全无关的填充文本：门禁场景八")

		o, hits := retrievalGateOutcome(t, svc, []string{"kb-gate-admission-vec-reject"}, "GATEADMISSIONVECREJECTTOKEN", 3, "vector_below_admission", nil)
		if o.ExpectedConfigured {
			t.Fatalf("ExpectedConfigured = true, want false (this case configures no accepted document)")
		}
		if o.ResultCount != 0 || len(hits) != 0 {
			t.Fatalf("ResultCount = %d, hits = %+v, want 0/empty — the only candidate's cosine similarity (0.2) is below vectorAdmissionThreshold (0.35) with no keyword signal, so admission must reject it even though it's the sole candidate", o.ResultCount, hits)
		}
		record(o, hits)
	})

	// 9. Phase 8: 前部拒绝项不占 topK，后续合格候选补位——ga-rej-top 是
	// fusionScore 最高的候选（向量路径 rank1）但余弦相似度 0.30 低于
	// vectorAdmissionThreshold，必须被拒绝；ga-kw-1st/ga-kw-2nd 是真实强
	// 关键词命中（word_similarity 分别为 1.0/0.8，均高于
	// keywordAdmissionThreshold），用 8 个向量填充块把它们自己微弱的余弦
	// 分挤出 candidateK(topK=2)=8 的向量检索窗口，隔离出"只能靠关键词路径
	// 获得准入资格"的场景。topK=2 时最终必须是 [ga-kw-1st, ga-kw-2nd]，
	// 而不是 [ga-rej-top, ga-kw-1st]（如果准入没有先于 topK 截断生效，会
	// 是后者）。
	t.Run("admitted_candidate_backfills_topk", func(t *testing.T) {
		const kb = "kb-gate-admission-backfill"
		seedChunkWithContent(t, repo, kb, "doc-ga-rej-top", "ga-rej-top",
			cosineVec(0.30), "内容与关键词完全无关的填充文本：门禁场景九拒绝项")
		seedVectorOnlyFillers(t, repo, kb, "doc-ga-filler", 8, 0.29)
		seedChunkWithContent(t, repo, kb, "doc-ga-kw-1st", "ga-kw-1st",
			cosineVec(0.01), "zz GATEADMISSIONBACKFILLTOKEN yy strongest keyword match, gate case nine")
		seedChunkWithContent(t, repo, kb, "doc-ga-kw-2nd", "ga-kw-2nd",
			cosineVec(0.01), "zz GATEADMISSIONBACKFILLTOKE yy second ranked keyword match, gate case nine")

		o, hits := retrievalGateOutcome(t, svc, []string{kb}, "GATEADMISSIONBACKFILLTOKEN", 2, "admitted_candidate_backfills_topk", docSet("doc-ga-kw-1st"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1 (ga-kw-1st is the strongest real keyword match, and ga-rej-top must be rejected before topK truncation)", o.HitRank)
		}
		if len(hits) != 2 {
			t.Fatalf("got %d hits, want exactly 2 — ga-rej-top must be rejected by admission (freeing the slot it would have consumed), letting ga-kw-2nd backfill", len(hits))
		}
		if hits[0].ChunkID != "ga-kw-1st" || hits[1].ChunkID != "ga-kw-2nd" {
			t.Fatalf("got %v, want [ga-kw-1st ga-kw-2nd] — ga-rej-top must never appear in the final result", []string{hits[0].ChunkID, hits[1].ChunkID})
		}
		if o.DuplicateContentCount != 0 {
			t.Fatalf("DuplicateContentCount = %d, want 0", o.DuplicateContentCount)
		}
		record(o, hits)
	})

	// 10/11. SC-001（查询改写）的受控证据。**这两个 case 必须成对看**：
	// 同一个知识库、同一条目标片段、同一条流水线，唯一的变量是送进
	// Retrieve 的 query 字符串——这正是查询改写在生产里做的唯一一件事
	// （改写只改检索输入，不改别的，见 conversation/context.go）。
	//
	// 为什么可以在 knowledge 这一层模拟改写：改写发生在 conversation，
	// 但它对 knowledge 的影响 100% 等价于"换一个 query 字符串"。在这里
	// 用两个字符串跑同一条流水线，比把 conversation 拖进检索门禁更简单，
	// 也不跨层（tasks.md T038a 明确选了这个方案）。
	//
	// 数据是量过的，不是拍脑袋：对目标正文，
	//   word_similarity('它的上限呢',   正文) = 0
	//   word_similarity('分块大小上限', 正文) = 0.5714286
	// 而 keywordAdmissionThreshold = 0.45。所以省略式提问连准入门槛都够
	// 不到，补全后的问题稳稳越过。目标片段的向量与查询向量正交（假嵌入对
	// 任何 query 恒返回 x 轴单位向量，见 newFakeProvider），cos=0 也低于
	// vectorAdmissionThreshold=0.35——两条路都堵死，目标片段只可能靠
	// "改写后的关键词"被捞出来，不存在别的解释。
	//
	// ⚠️ 这组 case 证明的是**机制成立**（改写确实改变了检索结果），不是
	// 真实效果幅度。真实幅度需要真实嵌入模型和足够大的语料，本仓库当前
	// 两者都不具备——原因写在 spec.md 的"度量方式修正"里，任何对外材料
	// 都不得把这里的通过表述成"提升了 N 个百分点"。
	t.Run("rewrite_before_elliptical_query_misses", func(t *testing.T) {
		const kb = "kb-gate-rewrite"
		seedChunkWithContent(t, repo, kb, "doc-gate-rewrite", "grw-target",
			[]float32{0, 1, 0}, "知识库的分块大小上限说明：单个文档分块大小上限为一千字符")

		// expectedDocIDs 故意留空：这个 case 的"正确行为"就是什么都检索不
		// 到，配了期望文档反而会把它算进 Hit@1 拉低门禁指标（和既有的
		// nonempty_kb_irrelevant_query 同样的处理）。
		o, hits := retrievalGateOutcome(t, svc, []string{kb}, "它的上限呢", 3, "rewrite_before_elliptical_query_misses", nil)
		if o.ResultCount != 0 {
			t.Fatalf("ResultCount = %d, want 0 —— 省略式提问 word_similarity=0，两条召回路都够不到准入门槛，不该检索到任何东西", o.ResultCount)
		}
		record(o, hits)
	})

	t.Run("rewrite_after_standalone_query_hits", func(t *testing.T) {
		const kb = "kb-gate-rewrite"
		// 不再 seed：复用上一个 case 种下的同一条 grw-target，确保两个
		// case 之间**只有 query 字符串这一个变量**。
		o, hits := retrievalGateOutcome(t, svc, []string{kb}, "分块大小上限", 3, "rewrite_after_standalone_query_hits", docSet("doc-gate-rewrite"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1 —— 补全后的问题 word_similarity=0.571 > 0.45，必须命中同一条 grw-target", o.HitRank)
		}
		if len(hits) != 1 || hits[0].ChunkID != "grw-target" {
			t.Fatalf("got %v, want [grw-target]", hits)
		}
		record(o, hits)
	})

	// 12. SC-002（重排）的受控证据：目标片段在融合排名里排在 topK 之外，
	// 重排把它提进 topK 且升到第一。用注入的固定打分假实现，不依赖任何
	// 真实 rerank 服务——同时也就保证了这个 case 是确定性的。
	//
	// 构造方式：5 条候选都靠关键词路进来（正文都含同一个 token），靠正文
	// 长度差异拉开 word_similarity，使 grr-target 稳定排在第 4；topK=3 时
	// 它本来必然被截断掉。重排给它最高分，它必须变成第 1 名。
	t.Run("rerank_promotes_out_of_topk_candidate", func(t *testing.T) {
		const kb = "kb-gate-rerank"
		seedChunkWithContent(t, repo, kb, "doc-grr-1", "grr-noise-1", cosineVec(0.01), "GATERERANKTOKEN 门禁重排场景噪声一")
		seedChunkWithContent(t, repo, kb, "doc-grr-2", "grr-noise-2", cosineVec(0.01), "GATERERANKTOKEN 门禁重排场景噪声二")
		seedChunkWithContent(t, repo, kb, "doc-grr-3", "grr-noise-3", cosineVec(0.01), "GATERERANKTOKEN 门禁重排场景噪声三")
		seedChunkWithContent(t, repo, kb, "doc-grr-target", "grr-target", cosineVec(0.01), "GATERERANKTOKEN 门禁重排场景目标片段，正文更长因此关键词相似度更低，融合排名必然靠后")

		// 先确认"不重排时它确实进不了 topK"——否则这个 case 就算通过也
		// 什么都没证明（目标片段本来就在 topK 里的话，重排提不提升无从谈起）。
		baseline, err := svc.Retrieve(context.Background(), []string{kb}, "GATERERANKTOKEN", 3, RetrieveOptions{})
		if err != nil {
			t.Fatalf("baseline Retrieve: %v", err)
		}
		for _, c := range baseline {
			if c.ID == "grr-target" {
				t.Fatalf("前置条件不成立：不重排时 grr-target 已经在 topK 里了，这个 case 证明不了任何东西")
			}
		}

		// 固定打分：目标片段最高，其余按送入顺序递减——确定性，可复现。
		rerankSvc := newTestServiceWithRerank(repo, fp, t.TempDir(),
			func(_ context.Context, _ string, documents []string) (provider.RerankResult, error) {
				scores := make([]provider.RerankScore, len(documents))
				for i, d := range documents {
					score := 0.1 - float64(i)*0.01
					if strings.Contains(d, "目标片段") {
						score = 0.99
					}
					scores[i] = provider.RerankScore{Index: i, Score: score}
				}
				return provider.RerankResult{Scores: scores}, nil
			})

		o, hits := retrievalGateOutcome(t, rerankSvc, []string{kb}, "GATERERANKTOKEN", 3, "rerank_promotes_out_of_topk_candidate", docSet("doc-grr-target"))
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1 —— 重排必须把融合排名 topK 之外的 grr-target 提到第一位", o.HitRank)
		}
		if hits[0].ChunkID != "grr-target" {
			t.Fatalf("hits[0] = %s, want grr-target", hits[0].ChunkID)
		}
		record(o, hits)
	})

	// 13. SC-001（文档级过滤）的受控证据：同一个问题，不带过滤时两份文档的
	// 片段都在候选里；限定到 A 之后 B 的片段一条都不能出现。
	//
	// 两条 chunk 的余弦分数刻意让**诱饵更高**（0.99 > 0.90），于是"限定到 A
	// 之后 A 的片段排第一"不可能是碰巧——不带过滤时排第一的是 B。
	t.Run("filter_scopes_to_document", func(t *testing.T) {
		const kb = "kb-gate-filter-doc"
		seedChunkWithContent(t, repo, kb, "doc-gate-filter-target", "gfd-target", cosineVec(0.90), "门禁文档过滤场景：目标文档的正文")
		seedChunkWithContent(t, repo, kb, "doc-gate-filter-decoy", "gfd-decoy", cosineVec(0.99), "门禁文档过滤场景：诱饵文档的正文，分数更高")

		// 前置条件：不带过滤时诱饵排第一，目标排第二。否则这个 case 证明不了
		// "过滤真的缩小了范围"。
		baseline, err := svc.Retrieve(context.Background(), []string{kb}, "门禁文档过滤场景", 3, RetrieveOptions{})
		if err != nil {
			t.Fatalf("baseline Retrieve: %v", err)
		}
		if len(baseline) != 2 || baseline[0].ID != "gfd-decoy" {
			t.Fatalf("前置条件不成立：不带过滤时应为 [gfd-decoy gfd-target]，got %v", ids(baseline))
		}

		filterSvc := newTestServiceWithFilter(repo, fp, t.TempDir())
		o, hits := retrievalGateOutcomeWithOptions(t, filterSvc, []string{kb}, "门禁文档过滤场景", 3,
			"filter_scopes_to_document", docSet("doc-gate-filter-target"),
			RetrieveOptions{Filter: RetrieveFilter{DocumentIDs: []string{"doc-gate-filter-target"}}})
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1 —— 限定到目标文档后它必须是第一条", o.HitRank)
		}
		if len(hits) != 1 || hits[0].ChunkID != "gfd-target" {
			t.Fatalf("got %v, want 只有 [gfd-target]（诱饵文档的片段必须被过滤掉）", hits)
		}
		record(o, hits)
	})

	// 14. SC-002（页码过滤）的受控证据：目标片段在第 12 页，页码范围含 12 时
	// 命中、不含时不命中。两条 chunk 的 chunk_index 刻意不相邻（0 与 9），
	// 避免邻接窗口把范围外那条作为上下文补全带回来——那是正确行为
	// （FR-011 的豁免），但会让这个 case 的断言失去针对性。
	t.Run("filter_scopes_to_page_range", func(t *testing.T) {
		const kb = "kb-gate-filter-page"
		p2, p12 := 2, 12
		seedNeighborChunkBatch(t, repo, kb, "doc-gate-filter-paged", 1, []neighborSeedChunk{
			{ID: "gfp-early", ChunkIndex: 0, Content: "门禁页码过滤场景：第二页的正文", Vec: cosineVec(0.99), PageNumber: &p2},
			{ID: "gfp-target", ChunkIndex: 9, Content: "门禁页码过滤场景：第十二页的正文", Vec: cosineVec(0.90), PageNumber: &p12},
		}, true)

		filterSvc := newTestServiceWithFilter(repo, fp, t.TempDir())

		// 范围不含第 12 页：目标片段必须不出现。
		outOfRange, err := filterSvc.Retrieve(context.Background(), []string{kb}, "门禁页码过滤场景", 3,
			RetrieveOptions{Filter: RetrieveFilter{PageMin: intPtr(1), PageMax: intPtr(5)}})
		if err != nil {
			t.Fatalf("Retrieve（[1,5]）: %v", err)
		}
		for _, c := range outOfRange {
			if c.ID == "gfp-target" {
				t.Fatalf("页码范围 [1,5] 不该召回第 12 页的 gfp-target，got %v", ids(outOfRange))
			}
		}

		// 范围含第 12 页：命中，且第 2 页那条被挡在外面。
		o, hits := retrievalGateOutcomeWithOptions(t, filterSvc, []string{kb}, "门禁页码过滤场景", 3,
			"filter_scopes_to_page_range", docSet("doc-gate-filter-paged"),
			RetrieveOptions{Filter: RetrieveFilter{PageMin: intPtr(10), PageMax: intPtr(15)}})
		if o.HitRank != 1 {
			t.Fatalf("HitRank = %d, want 1", o.HitRank)
		}
		if len(hits) != 1 || hits[0].ChunkID != "gfp-target" {
			t.Fatalf("got %v, want 只有 [gfp-target]（第 2 页那条必须被页码过滤挡住）", hits)
		}
		record(o, hits)
	})

	// --- 门禁：把全部 case 的结果聚合成 Hit@1/Hit@3/MRR/ContentUniqueRate，
	// 用 EvaluateRetrievalGate 做出 pass/fail 判定，判定失败就是真的 go
	// test 失败（非零退出码），不是只生成一份报告等人去看。
	metrics := retrieval.AggregateMetrics(outcomes)
	pass, reasons := retrieval.EvaluateGate(metrics, retrievalGateThresholds())

	report := retrieval.GateReport{Cases: cases, Metrics: metrics, Pass: pass, Reasons: reasons}
	reportPath := retrievalGateReportPath(t)
	if err := retrieval.SaveReport(report, reportPath); err != nil {
		t.Fatalf("save retrieval gate report: %v", err)
	}

	if !pass {
		t.Fatalf("retrieval gate FAILED: %v (see %s for the full per-case breakdown)", reasons, reportPath)
	}
}

// --- retrievalGateReportPath regression tests (Codex's first-round Phase
// 6 review, 待修复项 2): a plain `go test` run must never write a report
// into the source tree, and `make eval-retrieval-gate`'s env-var override
// must be honored when set. These are pure, DB-free, and run unconditionally
// (no testutil skip) — no reason for them to depend on the containers
// being up.

func TestRetrievalGateReportPathDefaultsAwayFromSourceTree(t *testing.T) {
	t.Setenv(retrievalGateReportPathEnv, "") // explicit unset, in case a parent env leaked one in
	got := retrievalGateReportPath(t)

	if !filepath.IsAbs(got) {
		t.Fatalf("default report path %q is not absolute", got)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got, cwd) {
		t.Fatalf("default report path %q must not live under the test's working directory %q — this is exactly the bug Codex's Phase 6 review caught (go test's cwd is the package directory, so a bare relative path like \"eval/runs/...\" resolves inside internal/knowledge/ and pollutes the source tree)", got, cwd)
	}
}

func TestRetrievalGateReportPathHonorsEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-report.json")
	t.Setenv(retrievalGateReportPathEnv, want)
	if got := retrievalGateReportPath(t); got != want {
		t.Fatalf("report path = %q, want %q (env override must take priority over the default)", got, want)
	}
}
