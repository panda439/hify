package knowledge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"hify/internal/provider"
)

// T027：先写失败测试，再在 rerank.go 里实现 applyRerank（T028）。覆盖
// contracts/internal-contracts.md §4 的四条要求：
//   1. 分数降序生效；
//   2. 分数相同按重排前原始位置（originalIndex）升序——确定性 tie-break，
//      宪法第 V 条硬要求；
//   3. RetrievedChunk.Score / Citation 相关元数据 / NeighborOf 均不被
//      rerank 分数改写——rerank 分数只活在包内未导出结构里（rerankedCandidate），
//      绝不写回 RetrievedChunk（FR-008，data-model.md §2.2 的硬约束）；
//   4. 响应校验不通过（provider 层已经在 5 条规则上报错，这里额外覆盖
//      "分数条数对不上候选数"这种调用方自己传错的情况）时返回 false 且候选
//      顺序原样返回，调用方据此整体丢弃、保持融合排序。

func TestApplyRerankOrdersByScoreDescending(t *testing.T) {
	candidates := []RetrievedChunk{rc("a", 0.5), rc("b", 0.5), rc("c", 0.5)}
	scores := []provider.RerankScore{
		{Index: 0, Score: 0.1}, // a
		{Index: 1, Score: 0.9}, // b
		{Index: 2, Score: 0.5}, // c
	}

	got, ok := applyRerank(candidates, scores)
	if !ok {
		t.Fatal("applyRerank returned ok=false, want true for a fully valid response")
	}
	want := []string{"b", "c", "a"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v (descending rerank score)", got, want)
	}
}

func TestApplyRerankTiedScoreBreaksByOriginalIndexAscending(t *testing.T) {
	// 三个候选拿到完全相同的 rerank 分数——必须回退到"重排前的原始位置
	// （也就是送进 rerank 请求时 documents 数组里的下标）升序"，而不是任意
	// 顺序或 ID 顺序（这里 ID 故意和原始顺序相反，用来排除"其实是按 ID 排
	// 序"的假阳性）。
	candidates := []RetrievedChunk{rc("zzz-first", 0.9), rc("mmm-second", 0.8), rc("aaa-third", 0.7)}
	scores := []provider.RerankScore{
		{Index: 0, Score: 0.42},
		{Index: 1, Score: 0.42},
		{Index: 2, Score: 0.42},
	}

	got, ok := applyRerank(candidates, scores)
	if !ok {
		t.Fatal("applyRerank returned ok=false, want true")
	}
	want := []string{"zzz-first", "mmm-second", "aaa-third"}
	if got := idsOf(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("tied-score order = %v, want %v (original index ascending)", got, want)
	}
}

func TestApplyRerankNeverOverwritesScoreOrMetadata(t *testing.T) {
	page := 3
	section := "2.1 概述"
	candidates := []RetrievedChunk{
		{
			Chunk:      Chunk{ID: "core", DocumentName: "手册.pdf", PageNumber: &page, SectionTitle: &section},
			Score:      0.37, // 向量/关键词较大值，rerank 绝不能改写这个字段
			NeighborOf: "",
		},
		{
			Chunk:      Chunk{ID: "neighbor", DocumentName: "手册.pdf"},
			Score:      0.20,
			NeighborOf: "core",
		},
	}
	scores := []provider.RerankScore{
		{Index: 0, Score: 0.99},
		{Index: 1, Score: 0.11},
	}

	got, ok := applyRerank(candidates, scores)
	if !ok {
		t.Fatal("applyRerank returned ok=false, want true")
	}
	byID := map[string]RetrievedChunk{}
	for _, c := range got {
		byID[c.ID] = c
	}
	if byID["core"].Score != 0.37 {
		t.Fatalf("core.Score = %v, want unchanged 0.37 (rerank score must never overwrite RetrievedChunk.Score, FR-008)", byID["core"].Score)
	}
	if byID["neighbor"].Score != 0.20 {
		t.Fatalf("neighbor.Score = %v, want unchanged 0.20", byID["neighbor"].Score)
	}
	if byID["neighbor"].NeighborOf != "core" {
		t.Fatalf("neighbor.NeighborOf = %q, want unchanged %q", byID["neighbor"].NeighborOf, "core")
	}
	if byID["core"].DocumentName != "手册.pdf" || byID["core"].PageNumber == nil || *byID["core"].PageNumber != page {
		t.Fatalf("core citation metadata was altered: %+v", byID["core"])
	}
}

func TestApplyRerankRejectsScoreCountMismatch(t *testing.T) {
	candidates := []RetrievedChunk{rc("a", 0.5), rc("b", 0.5)}
	scores := []provider.RerankScore{{Index: 0, Score: 0.9}} // 只有一条，候选有两个

	got, ok := applyRerank(candidates, scores)
	if ok {
		t.Fatal("applyRerank returned ok=true, want false for a score-count mismatch")
	}
	if !reflect.DeepEqual(idsOf(got), idsOf(candidates)) {
		t.Fatalf("order = %v, want original order preserved on validation failure (%v)", idsOf(got), idsOf(candidates))
	}
}

func TestApplyRerankRejectsOutOfRangeIndex(t *testing.T) {
	candidates := []RetrievedChunk{rc("a", 0.5), rc("b", 0.5)}
	scores := []provider.RerankScore{{Index: 0, Score: 0.9}, {Index: 5, Score: 0.1}}

	got, ok := applyRerank(candidates, scores)
	if ok {
		t.Fatal("applyRerank returned ok=true, want false for an out-of-range index")
	}
	if !reflect.DeepEqual(idsOf(got), idsOf(candidates)) {
		t.Fatalf("order = %v, want original order preserved", idsOf(got))
	}
}

func TestApplyRerankRejectsDuplicateIndex(t *testing.T) {
	candidates := []RetrievedChunk{rc("a", 0.5), rc("b", 0.5)}
	scores := []provider.RerankScore{{Index: 0, Score: 0.9}, {Index: 0, Score: 0.1}}

	got, ok := applyRerank(candidates, scores)
	if ok {
		t.Fatal("applyRerank returned ok=true, want false for a duplicate index")
	}
	if !reflect.DeepEqual(idsOf(got), idsOf(candidates)) {
		t.Fatalf("order = %v, want original order preserved", idsOf(got))
	}
}

func TestApplyRerankRejectsMissingIndex(t *testing.T) {
	candidates := []RetrievedChunk{rc("a", 0.5), rc("b", 0.5), rc("c", 0.5)}
	// index 1 从未出现——长度对得上（凑巧两条），但覆盖不完整。
	scores := []provider.RerankScore{{Index: 0, Score: 0.9}, {Index: 0, Score: 0.1}, {Index: 2, Score: 0.2}}

	got, ok := applyRerank(candidates, scores)
	if ok {
		t.Fatal("applyRerank returned ok=true, want false for missing index coverage")
	}
	if !reflect.DeepEqual(idsOf(got), idsOf(candidates)) {
		t.Fatalf("order = %v, want original order preserved", idsOf(got))
	}
}

func TestApplyRerankDeterministicAcrossRepeatedCalls(t *testing.T) {
	candidates := []RetrievedChunk{rc("a", 0.5), rc("b", 0.5), rc("c", 0.5), rc("d", 0.5)}
	scores := []provider.RerankScore{
		{Index: 0, Score: 0.3},
		{Index: 1, Score: 0.3},
		{Index: 2, Score: 0.9},
		{Index: 3, Score: 0.1},
	}
	first, _ := applyRerank(candidates, scores)
	firstIDs := idsOf(first)
	for i := 0; i < 20; i++ {
		got, _ := applyRerank(candidates, scores)
		if !reflect.DeepEqual(idsOf(got), firstIDs) {
			t.Fatalf("run %d: order changed across repeated calls: got %v, want %v", i, idsOf(got), firstIDs)
		}
	}
}

// --- 001-rag-query-rerank US3：T036，隐私断言（FR-017）---
//
// applyRerankStep（service.go）在两条降级路径上都会 slog.Warn 一行——
// "rerank call failed" 和 "rerank response failed validation"。两个用例都
// 真的用 slog.NewJSONHandler 接管 slog.Default() 捕获实际写出的日志，显式
// strings.Contains 找 query 原文和候选正文的敏感标记串，找到就判失败——不
// 是弱化成"字段数量对不对"。applyRerankStep 本身零 DB 依赖（rerankScoreFn
// 是可替换的方法值字段），所以这里直接 &service{...} 构造，不需要
// setupIntegration。

func captureSlogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestApplyRerankStepPrivacyCallErrorNeverLogsQueryOrChunkContent(t *testing.T) {
	const sensitiveQuery = "SECRET_QUERY_call_error_不该出现在日志里_123"
	const sensitiveContent1 = "SECRET_CHUNK_CONTENT_片段正文一_不该出现在日志里_456"
	const sensitiveContent2 = "SECRET_CHUNK_CONTENT_片段正文二_不该出现在日志里_789"

	buf := captureSlogOutput(t)

	svc := &service{
		rerankEnabled: true,
		rerankModelID: "rerank-model",
		rerankTimeout: 1500 * time.Millisecond,
		rerankScoreFn: func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
			return provider.RerankResult{}, errors.New("simulated rerank provider failure")
		},
	}
	candidates := []RetrievedChunk{
		rcContent("c1", 0.9, sensitiveContent1),
		rcContent("c2", 0.8, sensitiveContent2),
	}

	_, stats := svc.applyRerankStep(context.Background(), sensitiveQuery, candidates)
	if !stats.Degraded {
		t.Fatalf("expected Degraded=true for a rerank call error, got %+v", stats)
	}

	logged := buf.String()
	if strings.Contains(logged, sensitiveQuery) {
		t.Fatalf("rerank privacy log leaked the query text:\n%s", logged)
	}
	if strings.Contains(logged, sensitiveContent1) || strings.Contains(logged, sensitiveContent2) {
		t.Fatalf("rerank privacy log leaked chunk content:\n%s", logged)
	}
	if !strings.Contains(logged, "input_count") {
		t.Fatalf("expected the structural input_count field in log output, got:\n%s", logged)
	}
}

func TestApplyRerankStepPrivacyResponseValidationFailureNeverLogsQueryOrChunkContent(t *testing.T) {
	const sensitiveQuery = "SECRET_QUERY_validation_失败_不该出现在日志里_321"
	const sensitiveContent1 = "SECRET_CHUNK_CONTENT_片段正文三_不该出现在日志里_654"
	const sensitiveContent2 = "SECRET_CHUNK_CONTENT_片段正文四_不该出现在日志里_987"

	buf := captureSlogOutput(t)

	svc := &service{
		rerankEnabled: true,
		rerankModelID: "rerank-model",
		rerankTimeout: 1500 * time.Millisecond,
		rerankScoreFn: func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
			// 两条候选的分数都打在 index 0 上——重复且未覆盖 index
			// 1，applyRerank 的响应校验会拒绝（见 rerank.go）。
			return provider.RerankResult{Scores: []provider.RerankScore{
				{Index: 0, Score: 0.9},
				{Index: 0, Score: 0.1},
			}}, nil
		},
	}
	candidates := []RetrievedChunk{
		rcContent("c1", 0.9, sensitiveContent1),
		rcContent("c2", 0.8, sensitiveContent2),
	}

	_, stats := svc.applyRerankStep(context.Background(), sensitiveQuery, candidates)
	if !stats.Degraded {
		t.Fatalf("expected Degraded=true for a rerank response validation failure, got %+v", stats)
	}

	logged := buf.String()
	if strings.Contains(logged, sensitiveQuery) {
		t.Fatalf("rerank privacy log leaked the query text:\n%s", logged)
	}
	if strings.Contains(logged, sensitiveContent1) || strings.Contains(logged, sensitiveContent2) {
		t.Fatalf("rerank privacy log leaked chunk content:\n%s", logged)
	}
	if !strings.Contains(logged, "input_count") {
		t.Fatalf("expected the structural input_count field in log output, got:\n%s", logged)
	}
}
