package knowledge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	pgvector "github.com/pgvector/pgvector-go"

	"hify/internal/platform"
	"hify/internal/platform/apperr"
	"hify/internal/provider"
	"hify/internal/testutil"
	"hify/internal/user"
)

// 链路 2/3/4 的集成测试：跑在 docker-compose 起的真实 MySQL + pgvector 上
// （make db-up），每次运行由 testutil 重建独立的 hify_test_knowledge 库，
// 绝不碰开发库 hify。数据库不可达时整组跳过（skip），不算失败。
//
// 这些用例验证的是单测覆盖不了的两类契约：
//   - pgvector 检索 SQL 的真实行为（<=> 排序、维度过滤、topK 下推）；
//   - MySQL 和 PG 之间无事务、靠代码约定维持的跨库一致性（删除顺序）。

func setupIntegration(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(testutil.MySQL(t, "knowledge"), testutil.Postgres(t, "knowledge"))
}

// --- fakes：knowledge 依赖 provider 仅通过 Service 接口，这是天然的缝隙 ---

// fakeEmbedClient 的 embed 返回确定性向量，取代真实 LLM 供应商。
type fakeEmbedClient struct {
	provider.Client
	embed func(input []string) (provider.EmbedResult, error)
}

func (f *fakeEmbedClient) Embed(ctx context.Context, req provider.EmbedRequest) (provider.EmbedResult, error) {
	return f.embed(req.Input)
}

// Rerank：001-rag-query-rerank（T024）给 provider.Client 接口新增的方法，
// 补一个空实现——fakeEmbedClient 只测嵌入路径，重排相关的集成测试
// （T031+）另有自己可编程的 fake，见 rerank_test.go/新增的
// integration_test.go 用例。
func (f *fakeEmbedClient) Rerank(ctx context.Context, req provider.RerankRequest) (provider.RerankResult, error) {
	return provider.RerankResult{}, nil
}

type fakeProviderService struct {
	provider.Service
	models map[string]provider.Model
	embed  func(modelName string, input []string) (provider.EmbedResult, error)
}

func (f *fakeProviderService) GetModel(ctx context.Context, id string) (provider.Model, error) {
	m, ok := f.models[id]
	if !ok {
		return provider.Model{}, provider.ErrModelNotFound
	}
	return m, nil
}

func (f *fakeProviderService) ResolveClient(ctx context.Context, providerID string) (provider.Client, error) {
	return &fakeEmbedClient{embed: func(input []string) (provider.EmbedResult, error) {
		return f.embed(providerID, input)
	}}, nil
}

// vecForModel: m3 是 3 维模型、m2 是 2 维模型；查询向量固定指向 x 轴，
// 让余弦相似度可以手算验证。
func newFakeProvider() *fakeProviderService {
	return &fakeProviderService{
		models: map[string]provider.Model{
			"m3": {ID: "m3", ProviderID: "p3", ModelName: "embed-3d", Capability: provider.CapabilityEmbedding},
			"m2": {ID: "m2", ProviderID: "p2", ModelName: "embed-2d", Capability: provider.CapabilityEmbedding},
		},
		embed: func(providerID string, input []string) (provider.EmbedResult, error) {
			dim := 3
			if providerID == "p2" {
				dim = 2
			}
			vecs := make([][]float32, len(input))
			for i := range input {
				v := make([]float32, dim)
				v[0] = 1 // 查询向量恒为 x 轴单位向量
				vecs[i] = v
			}
			return provider.EmbedResult{Embeddings: vecs, Dimension: dim}, nil
		},
	}
}

// newTestServiceWithFilter 与 newTestService 唯一的区别是打开
// 002-metadata-filter 的开关。单独一个构造器而不是给 newTestService 加参数，
// 是为了让既有几十个用例保持一字未改——它们全部传空过滤器，开关对其无影响。
func newTestServiceWithFilter(repo *Repository, fp *fakeProviderService, storageDir string) Service {
	return NewService(repo, fp, nil, storageDir, false, "", 1500*time.Millisecond, true)
}

func newTestService(repo *Repository, fp *fakeProviderService, storageDir string) Service {
	// asynq client 传 nil：这些用例不走 UploadDocument/RetryDocument 的入队
	// 路径——真正需要入队（比如验证 RetryDocument 重新排队）的用例改用
	// newTestAsynqClient 构造一个连真实 Redis 的 client。
	return NewService(repo, fp, nil, storageDir, false, "", 1500*time.Millisecond, false)
}

// newTestAsynqClient connects to the docker-compose Redis instance —
// needed only by tests that exercise RetryDocument's enqueue call. DB 15
// keeps it out of the way of whatever a developer's own dev server is
// using on the default DB. Skips (not fails) when Redis is unreachable,
// the same convention testutil.MySQL/Postgres use.
func newTestAsynqClient(t *testing.T) *asynq.Client {
	t.Helper()
	cfg := platform.RedisConfig{Addr: "127.0.0.1:6380", DB: 15}
	rdb := platform.NewRedisClient(cfg)
	defer rdb.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("跳过需要 asynq/redis 的用例（先 make db-up 起容器）: %v", err)
	}
	client := platform.NewAsynqClient(cfg)
	t.Cleanup(func() { client.Close() })
	return client
}

func seedKB(t *testing.T, repo *Repository, id, modelID, createdBy string, active bool) {
	t.Helper()
	ctx := context.Background()
	err := repo.createKnowledgeBase(ctx, KnowledgeBase{
		ID: id, Name: "kb-" + id, EmbeddingModelID: modelID,
		ChunkSize: 5, ChunkOverlap: 0, CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("seed kb %s: %v", id, err)
	}
	// 域规则：KB 创建即 active（CreateKnowledgeBase SQL 不写 is_active），
	// 停用只能走 update 路径——和真实用户操作一致。
	if !active {
		if err := repo.updateKnowledgeBase(ctx, KnowledgeBase{
			ID: id, Name: "kb-" + id, IsActive: false,
		}); err != nil {
			t.Fatalf("deactivate kb %s: %v", id, err)
		}
	}
}

// seedChunkVersion is the version every seedChunk call writes under —
// createChunks always inserts unpublished now (see repository.go), so
// seeding has to publish too, or these chunks would be invisible to
// searchChunks/countChunksByKnowledgeBase and every pre-existing test
// using this helper would break. Publishing is idempotent per (document,
// version), so multiple seedChunk calls against the same docID are safe.
const seedChunkVersion = int64(1)

func seedChunk(t *testing.T, repo *Repository, kbID, docID, chunkID string, vec []float32) {
	t.Helper()
	ctx := context.Background()
	err := repo.createChunks(ctx, []Chunk{{
		ID: chunkID, KnowledgeBaseID: kbID, DocumentID: docID, ChunkIndex: 0,
		Content: "content-" + chunkID, ContentLength: 1,
		Embedding: vec, EmbeddingDimension: len(vec),
	}}, seedChunkVersion)
	if err != nil {
		t.Fatalf("seed chunk %s: %v", chunkID, err)
	}
	if err := repo.publishDocumentVersion(ctx, docID, seedChunkVersion); err != nil {
		t.Fatalf("publish seeded chunk %s: %v", chunkID, err)
	}
}

// countAllChunksForDocument bypasses the is_published filter that
// countChunksByKnowledgeBase applies — used to assert that a failed
// ProcessDocument attempt left literally zero rows behind, not just zero
// published ones.
func countAllChunksForDocument(t *testing.T, repo *Repository, documentID string) int {
	t.Helper()
	var n int
	if err := repo.pgdb.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM chunks WHERE document_id = $1", documentID).Scan(&n); err != nil {
		t.Fatalf("count all chunks for document %s: %v", documentID, err)
	}
	return n
}

// --- 链路 3：pgvector 检索 ---

func TestIntegrationSearchVectorChunksOrderingAndDimensionFilter(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	kb := "kb-search"
	seedChunk(t, repo, kb, "doc-s", "c-exact", []float32{1, 0, 0}) // cos = 1.0
	seedChunk(t, repo, kb, "doc-s", "c-mid", []float32{1, 1, 0})   // cos ≈ 0.7071
	seedChunk(t, repo, kb, "doc-s", "c-ortho", []float32{0, 1, 0}) // cos = 0
	seedChunk(t, repo, kb, "doc-s", "c-2d", []float32{1, 0})       // 异维度，必须被过滤

	got, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchChunks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3 (2-dim chunk must be filtered out)", len(got))
	}
	wantOrder := []string{"c-exact", "c-mid", "c-ortho"}
	wantScore := []float64{1.0, math.Sqrt2 / 2, 0.0}
	for i := range got {
		if got[i].ID != wantOrder[i] {
			t.Fatalf("rank %d = %s, want %s (full: %v)", i, got[i].ID, wantOrder[i], ids(got))
		}
		if math.Abs(got[i].Score-wantScore[i]) > 1e-6 {
			t.Fatalf("score[%d] = %f, want %f", i, got[i].Score, wantScore[i])
		}
	}

	// topK 下推：LIMIT 生效且取的是最相似的。
	top1, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 1, RetrieveFilter{})
	if err != nil || len(top1) != 1 || top1[0].ID != "c-exact" {
		t.Fatalf("topK=1 = %v (err %v), want [c-exact]", ids(top1), err)
	}

	// knowledge_base_id 过滤：别的 KB 查不到这些 chunk。
	other, err := repo.searchVectorChunks(ctx, []string{"kb-nonexistent"}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil || len(other) != 0 {
		t.Fatalf("other-KB search = %v (err %v), want empty", ids(other), err)
	}
}

func TestIntegrationRetrieveMergesAcrossModelsAndSkipsInactive(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-r3", "m3", "u1", true)
	seedKB(t, repo, "kb-r2", "m2", "u1", true)
	seedKB(t, repo, "kb-off", "m3", "u1", false) // inactive，必须被跳过
	seedChunk(t, repo, "kb-r3", "doc-r", "r3-hit", []float32{1, 0, 0})
	// r3-weak's vector is [1,2,0] (cos ~= 0.447 against the [1,0,0] query),
	// not the much weaker [1,4,0] (cos ~= 0.243) this test used
	// pre-Phase-8 — Phase 8's vectorAdmissionThreshold=0.35 admission gate
	// would otherwise reject it outright, breaking this test's actual
	// point (cross-model-group merge with a genuinely-admitted-but-weaker
	// hit ranked last), not exercising admission itself — see
	// admission_test.go/eval_gate_test.go for the dedicated admission
	// rejection cases.
	seedChunk(t, repo, "kb-r3", "doc-r", "r3-weak", []float32{1, 2, 0})
	seedChunk(t, repo, "kb-r2", "doc-r", "r2-hit", []float32{1, 0})
	seedChunk(t, repo, "kb-off", "doc-r", "off-hit", []float32{1, 0, 0})

	got, err := svc.Retrieve(ctx, []string{"kb-r3", "kb-r2", "kb-off", "kb-ghost"}, "查询", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	// 两个模型组各自检索后全局按分数合并：两个满分在前，弱命中垫底；
	// inactive 和不存在的 KB 都不产生结果也不报错。
	if len(got) != 3 {
		t.Fatalf("got %d results %v, want 3", len(got), ids(got))
	}
	if got[0].Score < 0.999 || got[1].Score < 0.999 {
		t.Fatalf("top-2 should both be perfect hits: %+v", scores(got))
	}
	for _, c := range got {
		if c.KnowledgeBaseID == "kb-off" {
			t.Fatalf("inactive KB leaked into results: %v", ids(got))
		}
	}
	if got[2].ID != "r3-weak" {
		t.Fatalf("rank 3 = %s, want r3-weak", got[2].ID)
	}

	// 空入参与空查询：直接返回空，不打 DB。
	if r, err := svc.Retrieve(ctx, nil, "q", 5, RetrieveOptions{}); err != nil || r != nil {
		t.Fatalf("Retrieve(nil kbs) = %v, %v; want nil, nil", r, err)
	}
	if r, err := svc.Retrieve(ctx, []string{"kb-r3"}, "", 5, RetrieveOptions{}); err != nil || r != nil {
		t.Fatalf("Retrieve(empty query) = %v, %v; want nil, nil", r, err)
	}
}

// --- Phase 5: exact content dedup, driven through the real public
// Service.Retrieve entry point (setupIntegration — needs both MySQL and
// Postgres, same requirement TestIntegrationRetrieveMergesAcrossModelsAndSkipsInactive
// above already has). In an environment with only Postgres up, like this
// sandbox, these SKIP — exactly like every other setupIntegration test in
// this file already does, and for the identical, already-documented
// reason (see docs/eval-phase4-neighbor-window-report.md's "未能亲自验证
// 的部分" section). The PG-only tests directly above already give real
// execution proof of the same rrfFuse/expandWithNeighbors dedup behavior
// against a genuine Postgres instance; these two additionally confirm the
// full public entry point (Retrieve, including its MySQL knowledge_base
// lookup and Hybrid Search embedding path) wires it all together
// end-to-end, and need Codex's full docker environment to actually run.

// 一 + 四（Service.Retrieve 全链路版本）: 两条内容完全相同、ID 不同的
// chunk（一条余弦相似度更高）经过真实 Retrieve() 调用（embed -> 向量检索
// -> RRF 融合 -> 内容去重 -> 邻接扩展）后，topK 太小放不下两条重复内容时
// 只保留分数更高的一条，让内容不同的第三条候选补位。
func TestIntegrationRetrieveDedupsExactDuplicateContentEndToEnd(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-dedup-e2e", "m3", "u1", true)
	dupContent := "端到端重复内容验证：完全相同的正文"
	seedChunkWithContent(t, repo, "kb-dedup-e2e", "doc-dedup-e2e", "dup-high", []float32{1, 0, 0}, dupContent)  // cos = 1.0
	seedChunkWithContent(t, repo, "kb-dedup-e2e", "doc-dedup-e2e", "dup-low", []float32{1, 0.1, 0}, dupContent) // cos < 1.0, same content
	seedChunkWithContent(t, repo, "kb-dedup-e2e", "doc-dedup-e2e", "unique", []float32{1, 0.2, 0}, "内容不同的第三条候选")

	got, err := svc.Retrieve(ctx, []string{"kb-dedup-e2e"}, "查询", 2, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	want := []string{"dup-high", "unique"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v — Retrieve must content-dedup before truncating to topK, keeping dup-high and letting unique fill the freed slot", gotIDs, want)
	}
}

// 五（Service.Retrieve 全链路版本）: 一个核心命中块的邻接窗口块正文和另一
// 个核心命中块完全相同时，最终 Retrieve() 结果必须保留两个核心块，丢弃那
// 条重复的邻接块——核心块优先于邻接块，全链路（含真实 embed/向量检索/RRF/
// 邻接扩展）下同样成立。
func TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-dedup-nb-e2e", "m3", "u1", true)
	sharedContent := "核心块 core2 与 anchor 的邻接块共享的正文"

	seedNeighborChunkBatch(t, repo, "kb-dedup-nb-e2e", "doc-anchor-e2e", 1, []neighborSeedChunk{
		{ID: "anchor", ChunkIndex: 5, Content: "anchor 自己独有的正文", Vec: []float32{1, 0, 0}}, // cos = 1.0
		// anchor-prev's vector is deliberately orthogonal to the query
		// (cos = 0), NOT [1,0,0] — review fix (待修复项 4): with an equally
		// perfect cosine score, real vector search would rank anchor-prev
		// as a CORE hit in its own right (tied with anchor/core2, broken by
		// ID), so with topK=2 it could win a core slot ahead of core2
		// instead of only ever being discovered through anchor's neighbor
		// window lookup — which is not what this test is supposed to be
		// exercising. A weak cosine score here still lets anchor-prev be
		// fetched as anchor's chunk_index=4 neighbor (neighbor lookup goes
		// by chunk_index adjacency, not vector score), while guaranteeing
		// it can never out-rank a real core hit for a topK anchor slot.
		{ID: "anchor-prev", ChunkIndex: 4, Content: sharedContent, Vec: []float32{0, 1, 0}}, // cos = 0.0
	}, true)
	seedNeighborChunkBatch(t, repo, "kb-dedup-nb-e2e", "doc-core2-e2e", 1, []neighborSeedChunk{
		{ID: "core2", ChunkIndex: 0, Content: sharedContent, Vec: []float32{1, 0, 0}},
	}, true)

	got, err := svc.Retrieve(ctx, []string{"kb-dedup-nb-e2e"}, "查询", 2, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, c := range got {
		if c.ID == "anchor-prev" {
			t.Fatalf("got %v — anchor-prev duplicates core2's content and must be dropped, core2 (a real core hit) must win", ids(got))
		}
	}
	foundAnchor, foundCore2 := false, false
	for _, c := range got {
		if c.ID == "anchor" {
			foundAnchor = true
		}
		if c.ID == "core2" {
			foundCore2 = true
		}
	}
	if !foundAnchor || !foundCore2 {
		t.Fatalf("got %v, want both core hits (anchor, core2) present — neighbor dedup must never drop a core hit", ids(got))
	}
}

// --- 链路 2：文档入库流水线（parse → chunk → embed → PG 落库 → 状态机） ---

func TestIntegrationProcessDocumentHappyPath(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-proc", "m3", "u1", true)
	path := filepath.Join(dir, "doc.txt")
	// ChunkSize=5、overlap=0 → 恰好 3 个 chunk。
	if err := os.WriteFile(path, []byte("aaaaabbbbbccc"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: "doc-ok", KnowledgeBaseID: "kb-proc", FileName: "doc.txt",
		FileType: FileTypeTxt, FileSize: 13, StoragePath: path, CreatedBy: "u1"}
	if err := repo.createDocument(ctx, doc); err != nil {
		t.Fatalf("createDocument: %v", err)
	}

	if err := svc.ProcessDocument(ctx, "doc-ok", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	got, err := repo.getDocument(ctx, "doc-ok")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 3 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/3 (err=%q)", got.Status, got.ChunkCount, got.ErrorMessage)
	}
	n, err := repo.countChunksByKnowledgeBase(ctx, "kb-proc")
	if err != nil || n != 3 {
		t.Fatalf("PG chunk count = %d (err %v), want 3", n, err)
	}
}

func TestIntegrationProcessDocumentEmbedCountMismatchFails(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	// 故意让 embed 返回数量比分块少 1，模拟供应商静默丢向量。
	fp.embed = func(providerID string, input []string) (provider.EmbedResult, error) {
		vecs := make([][]float32, len(input)-1)
		for i := range vecs {
			vecs[i] = []float32{1, 0, 0}
		}
		return provider.EmbedResult{Embeddings: vecs, Dimension: 3}, nil
	}
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-mis", "m3", "u1", true)
	path := filepath.Join(dir, "mis.txt")
	if err := os.WriteFile(path, []byte("aaaaabbbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-mis", KnowledgeBaseID: "kb-mis",
		FileName: "mis.txt", FileType: FileTypeTxt, FileSize: 10, StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-mis", 1); err == nil {
		t.Fatal("expected error on embedding count mismatch")
	}
	got, _ := repo.getDocument(ctx, "doc-mis")
	if got.Status != StatusFailed || !strings.Contains(got.ErrorMessage, "不一致") {
		t.Fatalf("status=%s err=%q, want failed with mismatch message", got.Status, got.ErrorMessage)
	}
	if n, _ := repo.countChunksByKnowledgeBase(ctx, "kb-mis"); n != 0 {
		t.Fatalf("failed document must not leave chunks behind, found %d", n)
	}
}

func TestIntegrationProcessDocumentMissingFileFails(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-gone", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-gone", KnowledgeBaseID: "kb-gone",
		FileName: "gone.txt", FileType: FileTypeTxt, FileSize: 1,
		StoragePath: "/nonexistent/gone.txt", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-gone", 1); err == nil {
		t.Fatal("expected error for missing file")
	}
	got, _ := repo.getDocument(ctx, "doc-gone")
	if got.Status != StatusFailed || got.ErrorMessage == "" {
		t.Fatalf("status=%s err=%q, want failed with message (not stuck at processing)", got.Status, got.ErrorMessage)
	}
}

// --- 链路 4：跨库删除一致性 ---

func TestIntegrationDeleteDocumentCrossStore(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-del", "m3", "owner-1", true)
	path := filepath.Join(dir, "del.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-del", KnowledgeBaseID: "kb-del",
		FileName: "del.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: path, CreatedBy: "owner-1"}); err != nil {
		t.Fatal(err)
	}
	seedChunk(t, repo, "kb-del", "doc-del", "del-c1", []float32{1, 0, 0})
	seedChunk(t, repo, "kb-del", "doc-del", "del-c2", []float32{0, 1, 0})

	// 非 owner 非 admin：拒绝，且两库数据原封不动。
	if err := svc.DeleteDocument(ctx, "doc-del", "someone-else", "user"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner delete err = %v, want ErrForbidden", err)
	}
	if n, _ := repo.countChunksByKnowledgeBase(ctx, "kb-del"); n != 2 {
		t.Fatalf("forbidden delete must not touch chunks, count = %d", n)
	}

	// owner 删除：MySQL 行、PG chunks、磁盘文件全部消失。
	if err := svc.DeleteDocument(ctx, "doc-del", "owner-1", "user"); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := repo.getDocument(ctx, "doc-del"); !errors.Is(err, ErrDocumentNotFound) {
		t.Fatalf("document still readable after delete: %v", err)
	}
	if n, _ := repo.countChunksByKnowledgeBase(ctx, "kb-del"); n != 0 {
		t.Fatalf("orphan chunks left in PG after delete: %d", n)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still on disk after delete: %v", err)
	}
	// 删除后立即检索：不再返回该文档的任何 chunk（链路 4 的终点标准）。
	got, err := svc.Retrieve(ctx, []string{"kb-del"}, "查询", 10, RetrieveOptions{})
	if err != nil || len(got) != 0 {
		t.Fatalf("retrieval after delete = %v (err %v), want empty", ids(got), err)
	}
}

func TestIntegrationDeleteDocumentAdminBypassesOwnership(t *testing.T) {
	repo := setupIntegration(t)
	dir := t.TempDir()
	svc := newTestService(repo, newFakeProvider(), dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-adm", "m3", "owner-2", true)
	path := filepath.Join(dir, "adm.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-adm", KnowledgeBaseID: "kb-adm",
		FileName: "adm.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: path, CreatedBy: "owner-2"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.DeleteDocument(ctx, "doc-adm", "not-the-owner", user.RoleAdmin); err != nil {
		t.Fatalf("admin delete should bypass ownership: %v", err)
	}
}

// --- 幂等性 + 可恢复性改造新增用例 ---

func TestIntegrationProcessDocumentConcurrentDeliveryNoDuplicateChunks(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-dup", "m3", "u1", true)
	path := filepath.Join(dir, "dup.txt")
	// ChunkSize=5、overlap=0 → 恰好 3 个 chunk。
	if err := os.WriteFile(path, []byte("aaaaabbbbbccc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-dup", KnowledgeBaseID: "kb-dup",
		FileName: "dup.txt", FileType: FileTypeTxt, FileSize: 13, StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	// 两次并发到达同一 (documentID, version) —— 只有一次能赢得认领 CAS，
	// 另一次必须安全退出，不产生重复 chunks。
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = svc.ProcessDocument(ctx, "doc-dup", 1)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent ProcessDocument[%d] returned error: %v", i, err)
		}
	}

	got, err := repo.getDocument(ctx, "doc-dup")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 3 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/3", got.Status, got.ChunkCount)
	}
	n, err := repo.countChunksByKnowledgeBase(ctx, "kb-dup")
	if err != nil || n != 3 {
		t.Fatalf("PG chunk count = %d (err %v), want 3 (no duplicates from concurrent delivery)", n, err)
	}
}

func TestIntegrationProcessDocumentReadyDocumentReprocessIsNoop(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-ready", "m3", "u1", true)
	path := filepath.Join(dir, "ready.txt")
	if err := os.WriteFile(path, []byte("aaaaabbbbbccc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-ready", KnowledgeBaseID: "kb-ready",
		FileName: "ready.txt", FileType: FileTypeTxt, FileSize: 13, StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, "doc-ready", 1); err != nil {
		t.Fatalf("first ProcessDocument: %v", err)
	}

	// 篡改源文件——如果第二次调用真的重新处理了，结果会不一样。用它来
	// 证明第二次调用是彻底的 no-op，不是"恰好结果相同"。
	if err := os.WriteFile(path, []byte("totally different content now, way more text than before"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, "doc-ready", 1); err != nil {
		t.Fatalf("second ProcessDocument (should be a no-op): %v", err)
	}

	got, err := repo.getDocument(ctx, "doc-ready")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 3 {
		t.Fatalf("doc status=%s chunkCount=%d, want unchanged ready/3", got.Status, got.ChunkCount)
	}
	n, err := repo.countChunksByKnowledgeBase(ctx, "kb-ready")
	if err != nil || n != 3 {
		t.Fatalf("PG chunk count = %d (err %v), want unchanged 3", n, err)
	}
}

func TestIntegrationDeleteDuringProcessingLeavesNoSearchableOrphan(t *testing.T) {
	repo := setupIntegration(t)
	dir := t.TempDir()
	svc := newTestService(repo, newFakeProvider(), dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-race", "m3", "owner-3", true)
	path := filepath.Join(dir, "race.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-race", KnowledgeBaseID: "kb-race",
		FileName: "race.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: path, CreatedBy: "owner-3"}); err != nil {
		t.Fatal(err)
	}

	// 模拟一个 worker 已经认领了这个文档……
	claimed, err := repo.claimDocumentProcessing(ctx, "doc-race", 1, time.Now().Add(leaseDuration))
	if err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v, want true/nil", claimed, err)
	}

	// ……与此同时用户把它删除了（DeleteDocument 不检查 status，任何状态
	// 下都能删——这正是需要靠发布网关兜底的并发场景）。
	if err := svc.DeleteDocument(ctx, "doc-race", "owner-3", "user"); err != nil {
		t.Fatalf("DeleteDocument during processing: %v", err)
	}

	// worker 对此一无所知，跑完自己的活儿，把结果写成未发布的 chunks。
	if err := repo.createChunks(ctx, []Chunk{{
		ID: "race-c1", KnowledgeBaseID: "kb-race", DocumentID: "doc-race", ChunkIndex: 0,
		Content: "orphan content", ContentLength: 1, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3,
	}}, 1); err != nil {
		t.Fatalf("createChunks after concurrent delete: %v", err)
	}

	// processing -> publishing 网关必须拒绝：文档行已经不存在，CAS 影响 0 行。
	publishing, err := repo.markDocumentPublishing(ctx, "doc-race", 1, time.Now().Add(leaseDuration), nil, nil)
	if err != nil {
		t.Fatalf("markDocumentPublishing: %v", err)
	}
	if publishing {
		t.Fatal("markDocumentPublishing succeeded against a deleted document — should have been fenced")
	}

	// 即使 chunk 物理写入了，因为从未发布，检索永远看不到它。
	got, err := repo.searchVectorChunks(ctx, []string{"kb-race"}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil || len(got) != 0 {
		t.Fatalf("searchChunks = %v (err %v), want empty (unpublished chunk must not be searchable)", ids(got), err)
	}

	// ProcessDocument 在发布网关被拒绝时会做的清理动作。
	if err := repo.deleteChunksByDocumentVersion(ctx, "doc-race", 1); err != nil {
		t.Fatalf("deleteChunksByDocumentVersion: %v", err)
	}
	if n := countAllChunksForDocument(t, repo, "doc-race"); n != 0 {
		t.Fatalf("chunk count after cleanup = %d, want 0", n)
	}
}

func TestIntegrationProcessDocumentPartialEmbedBatchFailureNoPartialPublish(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	callCount := 0
	// 第二批 embed 调用失败，模拟供应商在批处理中途报错。
	fp.embed = func(providerID string, input []string) (provider.EmbedResult, error) {
		callCount++
		if callCount == 2 {
			return provider.EmbedResult{}, errors.New("simulated batch failure")
		}
		vecs := make([][]float32, len(input))
		for i := range vecs {
			vecs[i] = []float32{1, 0, 0}
		}
		return provider.EmbedResult{Embeddings: vecs, Dimension: 3}, nil
	}
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-batch", "m3", "u1", true) // ChunkSize=5、overlap=0
	path := filepath.Join(dir, "batch.txt")
	// 200 字符 / ChunkSize=5 → 40 个 chunk → embedBatchSize=32 下切成 2 批
	// （32 + 8），第二批命中上面的故意失败。
	content := strings.Repeat("a", 200)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-batch", KnowledgeBaseID: "kb-batch",
		FileName: "batch.txt", FileType: FileTypeTxt, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-batch", 1); err == nil {
		t.Fatal("expected error from second embed batch failure")
	}
	if callCount != 2 {
		t.Fatalf("embed called %d times, want exactly 2 (batches of 32+8)", callCount)
	}

	got, _ := repo.getDocument(ctx, "doc-batch")
	if got.Status != StatusFailed {
		t.Fatalf("doc status=%s, want failed", got.Status)
	}
	if n := countAllChunksForDocument(t, repo, "doc-batch"); n != 0 {
		t.Fatalf("chunks physically written despite batch failure: %d, want 0 (no partial publish)", n)
	}
}

func TestIntegrationChunkCountHardCapRejectsOversizedDocument(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	fp.embed = func(providerID string, input []string) (provider.EmbedResult, error) {
		t.Fatal("embed must not be called when chunk count exceeds maxChunksPerDocument")
		return provider.EmbedResult{}, nil
	}
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-cap", "m3", "u1", true) // ChunkSize=5、overlap=0
	path := filepath.Join(dir, "huge.txt")
	// (maxChunksPerDocument+2)*5 字符 → maxChunksPerDocument+2 个 chunk。
	content := strings.Repeat("a", (maxChunksPerDocument+2)*5)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-cap", KnowledgeBaseID: "kb-cap",
		FileName: "huge.txt", FileType: FileTypeTxt, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-cap", 1); !errors.Is(err, ErrTooManyChunks) {
		t.Fatalf("ProcessDocument err = %v, want ErrTooManyChunks", err)
	}
	got, _ := repo.getDocument(ctx, "doc-cap")
	if got.Status != StatusFailed || !strings.Contains(got.ErrorMessage, "上限") {
		t.Fatalf("status=%s err=%q, want failed with cap message", got.Status, got.ErrorMessage)
	}
}

func TestIntegrationRetrieveClampsExcessiveTopK(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-topk", "m3", "u1", true)
	const seeded = maxTopK + 10
	for i := 0; i < seeded; i++ {
		seedChunk(t, repo, "kb-topk", "doc-topk", fmt.Sprintf("c-%d", i), []float32{1, 0, 0})
	}

	got, err := svc.Retrieve(ctx, []string{"kb-topk"}, "查询", 999999, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != maxTopK {
		t.Fatalf("got %d results, want exactly maxTopK=%d (topK must be clamped)", len(got), maxTopK)
	}
}

func TestIntegrationCASFailureDoesNotOverwriteNewerState(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-cas", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-cas", KnowledgeBaseID: "kb-cas",
		FileName: "cas.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	claimed, err := repo.claimDocumentProcessing(ctx, "doc-cas", 1, time.Now().Add(leaseDuration))
	if err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v, want true/nil", claimed, err)
	}

	// reconciliation 判定这次尝试卡死（租约过期），认领并把 version 前进到 2。
	// expiredBefore 传一个比 claim 时种下的租约更晚的时间，模拟"这一刻看来
	// 确实已经过期"。
	reclaimed, err := repo.reclaimStaleProcessingDocument(ctx, "doc-cas", 1, 2, time.Now().Add(2*leaseDuration))
	if err != nil || !reclaimed {
		t.Fatalf("reclaim setup = %v, %v, want true/nil", reclaimed, err)
	}

	// 原来（已经过期）的 worker 这时才跑完 Embedding，试图用旧 version=1
	// 转 publishing——第一个会撞上 fencing 的 CAS。
	publishing, err := repo.markDocumentPublishing(ctx, "doc-cas", 1, time.Now().Add(leaseDuration), nil, nil)
	if err != nil {
		t.Fatalf("markDocumentPublishing: %v", err)
	}
	if publishing {
		t.Fatal("markDocumentPublishing succeeded with a stale version — must not overwrite reconciliation's newer state")
	}

	got, err := repo.getDocument(ctx, "doc-cas")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || got.Version != 2 {
		t.Fatalf("doc status=%s version=%d, want pending/2 (reconciliation's state, untouched by the stale worker)", got.Status, got.Version)
	}
}

func TestIntegrationRetryDocumentOnlyAllowedFromPendingOrFailed(t *testing.T) {
	repo := setupIntegration(t)
	// RetryDocument's success path enqueues a task, unlike everything else
	// in this file — needs a real asynq client, not newTestService's nil.
	svc := NewService(repo, newFakeProvider(), newTestAsynqClient(t), t.TempDir(), false, "", 1500*time.Millisecond, false)
	ctx := context.Background()

	seedKB(t, repo, "kb-retry", "m3", "owner-4", true)

	// ready 状态不可重试。
	if err := repo.createDocument(ctx, Document{ID: "doc-retry-ready", KnowledgeBaseID: "kb-retry",
		FileName: "r.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "owner-4"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-retry-ready", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if ok, err := repo.markDocumentPublishing(ctx, "doc-retry-ready", 1, time.Now().Add(leaseDuration), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}
	if ok, err := repo.markDocumentReady(ctx, "doc-retry-ready", 1, 0); err != nil || !ok {
		t.Fatalf("markDocumentReady setup = %v, %v", ok, err)
	}
	if _, err := svc.RetryDocument(ctx, "doc-retry-ready", "owner-4", "user"); !errors.Is(err, ErrDocumentNotRetryable) {
		t.Fatalf("RetryDocument on ready doc err = %v, want ErrDocumentNotRetryable", err)
	}

	// processing 状态不可重试。
	if err := repo.createDocument(ctx, Document{ID: "doc-retry-processing", KnowledgeBaseID: "kb-retry",
		FileName: "r.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "owner-4"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-retry-processing", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if _, err := svc.RetryDocument(ctx, "doc-retry-processing", "owner-4", "user"); !errors.Is(err, ErrDocumentNotRetryable) {
		t.Fatalf("RetryDocument on processing doc err = %v, want ErrDocumentNotRetryable", err)
	}

	// failed 状态可以重试，version 前进。
	if err := repo.createDocument(ctx, Document{ID: "doc-retry-failed", KnowledgeBaseID: "kb-retry",
		FileName: "r.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "owner-4"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-retry-failed", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if ok, err := repo.markDocumentFailed(ctx, "doc-retry-failed", 1, "手动测试失败"); err != nil || !ok {
		t.Fatalf("markDocumentFailed setup = %v, %v", ok, err)
	}
	doc, err := svc.RetryDocument(ctx, "doc-retry-failed", "owner-4", "user")
	if err != nil {
		t.Fatalf("RetryDocument on failed doc: %v", err)
	}
	if doc.Status != StatusPending || doc.Version != 2 {
		t.Fatalf("retried doc status=%s version=%d, want pending/2", doc.Status, doc.Version)
	}

	// 非 owner 非 admin 不能重试，即使状态允许。
	if err := repo.createDocument(ctx, Document{ID: "doc-retry-forbidden", KnowledgeBaseID: "kb-retry",
		FileName: "r.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "owner-4"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-retry-forbidden", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if ok, err := repo.markDocumentFailed(ctx, "doc-retry-forbidden", 1, "手动测试失败"); err != nil || !ok {
		t.Fatalf("markDocumentFailed setup = %v, %v", ok, err)
	}
	if _, err := svc.RetryDocument(ctx, "doc-retry-forbidden", "someone-else", "user"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("RetryDocument by non-owner err = %v, want ErrForbidden", err)
	}
}

// --- 发布一致性 / 租约 / Embedding 维度校验三个审查问题的新增用例 ---

// gatedEmbedFunc builds a fakeProviderService.embed closure that blocks on
// the pauseAtCall-th call until the test sends on release — this lets a
// test deterministically pause a multi-batch ProcessDocument run
// mid-flight and act concurrently (start reconciliation, expire a lease,
// etc.) via channel happens-before ordering instead of guessing with
// sleeps. entered is closed the instant the paused call is entered, so
// the test knows the preceding batches (and their lease renewals) have
// already completed.
func gatedEmbedFunc(dim int, pauseAtCall int32, entered, release chan struct{}) (func(providerID string, input []string) (provider.EmbedResult, error), *int32) {
	var callCount int32
	fn := func(providerID string, input []string) (provider.EmbedResult, error) {
		n := atomic.AddInt32(&callCount, 1)
		if n == pauseAtCall {
			close(entered)
			<-release
		}
		vecs := make([][]float32, len(input))
		for i := range vecs {
			vecs[i] = make([]float32, dim)
			vecs[i][0] = 1
		}
		return provider.EmbedResult{Embeddings: vecs, Dimension: dim}, nil
	}
	return fn, &callCount
}

// --- 问题一：发布状态机（processing -> publishing -> ready）恢复 ---

func TestIntegrationReconcileRecoversPublishNeverAttempted(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-pubfail", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-pubfail", KnowledgeBaseID: "kb-pubfail",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	// 构造和真实 ProcessDocument 完全一致的中间态：claim -> 写入未发布
	// chunks -> processing -> publishing，租约直接种成已过期（模拟 PG 发布
	// 从未成功、worker 也没能再往下推进，等 reconciliation 接手）。
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-pubfail", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if err := repo.createChunks(ctx, []Chunk{
		{ID: "pf-c1", KnowledgeBaseID: "kb-pubfail", DocumentID: "doc-pubfail", ChunkIndex: 0,
			Content: "c1", ContentLength: 2, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3},
		{ID: "pf-c2", KnowledgeBaseID: "kb-pubfail", DocumentID: "doc-pubfail", ChunkIndex: 1,
			Content: "c2", ContentLength: 2, Embedding: []float32{0, 1, 0}, EmbeddingDimension: 3},
	}, 1); err != nil {
		t.Fatalf("createChunks setup: %v", err)
	}
	if ok, err := repo.markDocumentPublishing(ctx, "doc-pubfail", 1, time.Now().Add(-time.Minute), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// 此时文档处于 publishing，chunks 未发布，检索不到。
	if got, err := repo.searchVectorChunks(ctx, []string{"kb-pubfail"}, []float32{1, 0, 0}, 10, RetrieveFilter{}); err != nil || len(got) != 0 {
		t.Fatalf("pre-recovery searchChunks = %v (err %v), want empty", ids(got), err)
	}

	n, err := svc.ReconcileStuckDocuments(ctx)
	if err != nil {
		t.Fatalf("ReconcileStuckDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled count = %d, want 1", n)
	}

	got, err := repo.getDocument(ctx, "doc-pubfail")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 2 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/2", got.Status, got.ChunkCount)
	}

	found, err := repo.searchVectorChunks(ctx, []string{"kb-pubfail"}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil || len(found) != 2 {
		t.Fatalf("post-recovery searchChunks = %v (err %v), want 2 chunks", ids(found), err)
	}
	if n := countAllChunksForDocument(t, repo, "doc-pubfail"); n != 2 {
		t.Fatalf("total chunk rows = %d, want 2 (no duplicates)", n)
	}
}

func TestIntegrationReconcileRecoversPublishSucceededBeforeReadyCrash(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-pubcrash", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-pubcrash", KnowledgeBaseID: "kb-pubcrash",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-pubcrash", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if err := repo.createChunks(ctx, []Chunk{{
		ID: "pc-c1", KnowledgeBaseID: "kb-pubcrash", DocumentID: "doc-pubcrash", ChunkIndex: 0,
		Content: "c1", ContentLength: 2, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3,
	}}, 1); err != nil {
		t.Fatalf("createChunks setup: %v", err)
	}
	if ok, err := repo.markDocumentPublishing(ctx, "doc-pubcrash", 1, time.Now().Add(-time.Minute), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// 模拟"PG 发布已经成功，但 worker 在 CAS ready 之前崩溃"：这里先真的
	// 调一次 publishDocumentVersion，让 chunks 已经处于已发布状态，MySQL
	// 侧却还停在 publishing。
	if err := repo.publishDocumentVersion(ctx, "doc-pubcrash", 1); err != nil {
		t.Fatalf("simulate pre-crash publish: %v", err)
	}
	if got, err := repo.searchVectorChunks(ctx, []string{"kb-pubcrash"}, []float32{1, 0, 0}, 10, RetrieveFilter{}); err != nil || len(got) != 1 {
		t.Fatalf("chunk should already be published: %v (err %v)", ids(got), err)
	}

	// reconciliation 重跑同一段幂等发布逻辑：DELETE/UPDATE 对已经是目标状态
	// 的行是 no-op，然后正常完成 publishing -> ready 的 CAS。
	n, err := svc.ReconcileStuckDocuments(ctx)
	if err != nil {
		t.Fatalf("ReconcileStuckDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled count = %d, want 1", n)
	}

	got, err := repo.getDocument(ctx, "doc-pubcrash")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 1 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/1", got.Status, got.ChunkCount)
	}
	if n := countAllChunksForDocument(t, repo, "doc-pubcrash"); n != 1 {
		t.Fatalf("total chunk rows = %d, want 1 (idempotent republish must not duplicate)", n)
	}
}

func TestIntegrationPublishPermanentFailureReturnsErrorNotReady(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-pubdead", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-pubdead", KnowledgeBaseID: "kb-pubdead",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-pubdead", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if err := repo.createChunks(ctx, []Chunk{{
		ID: "pd-c1", KnowledgeBaseID: "kb-pubdead", DocumentID: "doc-pubdead", ChunkIndex: 0,
		Content: "c1", ContentLength: 2, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3,
	}}, 1); err != nil {
		t.Fatalf("createChunks setup: %v", err)
	}
	if ok, err := repo.markDocumentPublishing(ctx, "doc-pubdead", 1, time.Now().Add(leaseDuration), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// publishAndComplete 是 ProcessDocument 和 ReconcileStuckDocuments 共用
	// 的发布恢复逻辑（见 service.go）。构造一个真实场景下会让 PG 发布反复
	// 失败的精确前置状态很难不引入脆弱的时序/故障注入手段，这里改用白盒
	// 方式直接调用同包内的 unexported 方法，配上已取消的 context 让它必然
	// 失败——验证的是同一份生产代码：错误必须原样返回，不能被吞掉伪装成
	// 功；文档必须还停留在 publishing，不能被误标 ready。
	svc := &service{repo: repo, providerSvc: newFakeProvider(), storageDir: t.TempDir()}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := svc.publishAndComplete(cancelCtx, "doc-pubdead", 1); err == nil {
		t.Fatal("publishAndComplete with a dead context returned nil, want a non-nil error")
	}

	got, err := repo.getDocument(ctx, "doc-pubdead")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPublishing {
		t.Fatalf("doc status=%s, want still publishing (must not be faked to ready)", got.Status)
	}
}

func TestIntegrationReconcileOnlyRecoversPublishingAfterLeaseExpires(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-publease", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-publease", KnowledgeBaseID: "kb-publease",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-publease", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if err := repo.createChunks(ctx, []Chunk{{
		ID: "pl-c1", KnowledgeBaseID: "kb-publease", DocumentID: "doc-publease", ChunkIndex: 0,
		Content: "c1", ContentLength: 2, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3,
	}}, 1); err != nil {
		t.Fatalf("createChunks setup: %v", err)
	}

	// 租约还没过期：reconciliation 不应该碰它。
	if ok, err := repo.markDocumentPublishing(ctx, "doc-publease", 1, time.Now().Add(leaseDuration), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}
	if n, err := svc.ReconcileStuckDocuments(ctx); err != nil || n != 0 {
		t.Fatalf("ReconcileStuckDocuments (lease still valid) = %d, %v, want 0 reclaimed", n, err)
	}
	if got, err := repo.getDocument(ctx, "doc-publease"); err != nil || got.Status != StatusPublishing {
		t.Fatalf("doc should still be publishing before lease expiry: status=%s (err %v)", got.Status, err)
	}

	// 租约过期后：reconciliation 完成幂等发布，进 ready。用 renewDocumentLease
	// （不是再调一次 markDocumentPublishing——此时状态已经是 publishing，
	// 那条 CAS 的前置状态是 processing，不会匹配）把租约直接种成过期。
	if ok, err := repo.renewDocumentLease(ctx, "doc-publease", 1, StatusPublishing, time.Now().Add(-time.Minute)); err != nil || !ok {
		t.Fatalf("expire lease setup = %v, %v", ok, err)
	}
	n, err := svc.ReconcileStuckDocuments(ctx)
	if err != nil {
		t.Fatalf("ReconcileStuckDocuments (lease expired): %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d documents, want 1", n)
	}

	got, err := repo.getDocument(ctx, "doc-publease")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 1 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/1", got.Status, got.ChunkCount)
	}
	if found, err := repo.searchVectorChunks(ctx, []string{"kb-publease"}, []float32{1, 0, 0}, 10, RetrieveFilter{}); err != nil || len(found) != 1 {
		t.Fatalf("searchChunks after recovery = %v (err %v), want 1", ids(found), err)
	}
}

// --- 问题二：processing/publishing 心跳租约 ---

func TestIntegrationHeartbeatingWorkerNotReclaimedByReconciliation(t *testing.T) {
	repo := setupIntegration(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	embedFn, callCount := gatedEmbedFunc(3, 2, entered, release)
	fp := newFakeProvider()
	fp.embed = embedFn
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-heartbeat", "m3", "u1", true)
	path := filepath.Join(dir, "hb.txt")
	// ChunkSize=5（seedKB）、200 字符 -> 40 个 chunk -> embedBatchSize=32 下
	// 切成 2 批（32+8），在第 2 批上暂停。
	content := strings.Repeat("a", 200)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-heartbeat", KnowledgeBaseID: "kb-heartbeat",
		FileName: "hb.txt", FileType: FileTypeTxt, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan error, 1)
	go func() { resultCh <- svc.ProcessDocument(ctx, "doc-heartbeat", 1) }()

	<-entered // 第 1 批已完成并续过租，第 2 批被卡住

	// 这就是问题二要证明的核心机制：reconciliation 现在看的是"租约有没有
	// 过期"，不是"claim 之后过了多久"。第一批完成后已经续过一次租，即使
	// 从 claim 起算的真实耗时已经超过旧的 15 分钟阈值语义，只要租约仍在
	// 有效期内，reconciliation 就不能碰它——真的等 15 分钟不现实，这里通
	// 过直接验证"刚续过租的 worker 不会被回收"来证明机制本身是正确的。
	n, err := svc.ReconcileStuckDocuments(ctx)
	if err != nil {
		t.Fatalf("ReconcileStuckDocuments: %v", err)
	}
	if n != 0 {
		t.Fatalf("reconciled %d documents, want 0 (heartbeating worker must not be reclaimed)", n)
	}
	got, err := repo.getDocument(ctx, "doc-heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusProcessing || got.Version != 1 {
		t.Fatalf("doc status=%s version=%d, want unchanged processing/1", got.Status, got.Version)
	}

	close(release)
	if err := <-resultCh; err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if got := atomic.LoadInt32(callCount); got != 2 {
		t.Fatalf("embed called %d times, want 2", got)
	}
	got, err = repo.getDocument(ctx, "doc-heartbeat")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 40 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/40", got.Status, got.ChunkCount)
	}
}

func TestIntegrationExpiredLeaseReclaimedAndStaleWorkerStops(t *testing.T) {
	// 临时调小租约时长，让真实的 lease 过期可以在毫秒级别内真实发生，不
	// 用 mock 时间——leaseDuration 是 var 就是为了让测试能这样做。
	origLease := leaseDuration
	leaseDuration = 100 * time.Millisecond
	t.Cleanup(func() { leaseDuration = origLease })

	repo := setupIntegration(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	embedFn, callCount := gatedEmbedFunc(3, 2, entered, release)
	fp := newFakeProvider()
	fp.embed = embedFn
	dir := t.TempDir()
	svc := NewService(repo, fp, newTestAsynqClient(t), dir, false, "", 1500*time.Millisecond, false) // reconciliation 的回收要真的入队
	ctx := context.Background()

	seedKB(t, repo, "kb-expire", "m3", "u1", true)
	path := filepath.Join(dir, "exp.txt")
	content := strings.Repeat("a", 200)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-expire", KnowledgeBaseID: "kb-expire",
		FileName: "exp.txt", FileType: FileTypeTxt, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan error, 1)
	go func() { resultCh <- svc.ProcessDocument(ctx, "doc-expire", 1) }()

	<-entered // 第 1 批完成，续租到 now+100ms，第 2 批被卡住

	time.Sleep(200 * time.Millisecond) // 真实等 100ms 的租约过期

	// 需求 5：processing 租约真正过期，reconciliation 用 CAS 提升 version
	// 并重新入队。
	n, err := svc.ReconcileStuckDocuments(ctx)
	if err != nil {
		t.Fatalf("ReconcileStuckDocuments: %v", err)
	}
	if n != 1 {
		t.Fatalf("reconciled %d documents, want 1 (expired lease must be reclaimed)", n)
	}
	got, err := repo.getDocument(ctx, "doc-expire")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || got.Version != 2 {
		t.Fatalf("doc status=%s version=%d, want pending/2 after reclaim", got.Status, got.Version)
	}

	// 需求 7：放行被卡住的旧 worker——它下一次续租（针对已经过期的
	// version=1）必须失败，必须立刻停手，不能继续写 chunks 或发布。
	close(release)
	if err := <-resultCh; err != nil {
		t.Fatalf("stale worker's ProcessDocument returned an error, want nil (fenced, not failed): %v", err)
	}
	if got := atomic.LoadInt32(callCount); got != 2 {
		t.Fatalf("stale worker made %d embed calls, want exactly 2 (must stop after being fenced, not retry)", got)
	}
	if n := countAllChunksForDocument(t, repo, "doc-expire"); n != 0 {
		t.Fatalf("stale worker left %d chunk rows behind, want 0 (must never publish old-version chunks)", n)
	}
}

// --- 租约回收 TOCTOU 修复：最终 CAS 必须重新校验扫描时刻的过期条件 ---

func TestIntegrationReclaimProcessingRejectsRenewedLeaseUsingStaleScanSnapshot(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-toctou-proc", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-toctou-proc", KnowledgeBaseID: "kb-toctou-proc",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	// 1. 创建 processing 文档，租约已经过期。
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-toctou-proc", 1, time.Now().Add(-time.Minute)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}

	// 2. 用和 reconciliation 完全相同的查询拿到这份"过期快照"，记下这一刻
	// 的时间戳——这就是后面 reconciliation 会传给 CAS 的 expiredBefore。
	scanTime := time.Now()
	expired, err := repo.listLeaseExpiredProcessingDocuments(ctx, scanTime)
	if err != nil {
		t.Fatalf("listLeaseExpiredProcessingDocuments: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "doc-toctou-proc" {
		t.Fatalf("scan returned %d docs, want exactly doc-toctou-proc", len(expired))
	}
	expiredBefore := scanTime

	// 3. 查询完成之后，活跃 worker 成功把租约续到了未来。
	newLease := time.Now().Add(leaseDuration).Truncate(time.Millisecond)
	if renewed, err := repo.renewDocumentLease(ctx, "doc-toctou-proc", 1, StatusProcessing, newLease); err != nil || !renewed {
		t.Fatalf("worker renew = %v, %v, want true/nil", renewed, err)
	}

	// 4. reconciliation 仍然拿着旧的 expiredBefore 执行 reclaim CAS。
	reclaimed, err := repo.reclaimStaleProcessingDocument(ctx, "doc-toctou-proc", 1, 2, expiredBefore)
	if err != nil {
		t.Fatalf("reclaimStaleProcessingDocument: %v", err)
	}
	if reclaimed {
		t.Fatal("reclaim succeeded against a document renewed after the scan — TOCTOU window not closed")
	}

	got, err := repo.getDocument(ctx, "doc-toctou-proc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusProcessing || got.Version != 1 {
		t.Fatalf("doc status=%s version=%d, want unchanged processing/1", got.Status, got.Version)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(newLease) {
		t.Fatalf("lease = %v, want unchanged at the worker's renewed value %v", got.LeaseExpiresAt, newLease)
	}
}

func TestIntegrationReclaimProcessingConcurrentOnlyOneWins(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-toctou-proc2", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-toctou-proc2", KnowledgeBaseID: "kb-toctou-proc2",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-toctou-proc2", 1, time.Now().Add(-time.Minute)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}

	// 两个"恢复者"（模拟两个 Hify 实例的 reconciliation）用完全相同的
	// version 和 expiredBefore 并发执行同一条 CAS。
	expiredBefore := time.Now()
	var wg sync.WaitGroup
	results := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = repo.reclaimStaleProcessingDocument(ctx, "doc-toctou-proc2", 1, 2, expiredBefore)
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("reclaim[%d] error: %v", i, errs[i])
		}
		if results[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (concurrent CAS must not double-reclaim)", winners)
	}

	got, err := repo.getDocument(ctx, "doc-toctou-proc2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPending || got.Version != 2 {
		t.Fatalf("doc status=%s version=%d, want pending/2 (version must advance exactly once)", got.Status, got.Version)
	}
}

func TestIntegrationClaimPublishingRecoveryRejectsRenewedLeaseUsingStaleScanSnapshot(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-toctou-pub", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-toctou-pub", KnowledgeBaseID: "kb-toctou-pub",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-toctou-pub", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if err := repo.createChunks(ctx, []Chunk{{
		ID: "tp-c1", KnowledgeBaseID: "kb-toctou-pub", DocumentID: "doc-toctou-pub", ChunkIndex: 0,
		Content: "c1", ContentLength: 2, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3,
	}}, 1); err != nil {
		t.Fatalf("createChunks setup: %v", err)
	}
	// 1. 文档进入 publishing，租约直接种成已过期。
	if ok, err := repo.markDocumentPublishing(ctx, "doc-toctou-pub", 1, time.Now().Add(-time.Minute), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// 2. reconciliation 完成扫描，拿到过期快照。
	scanTime := time.Now()
	expired, err := repo.listLeaseExpiredPublishingDocuments(ctx, scanTime)
	if err != nil {
		t.Fatalf("listLeaseExpiredPublishingDocuments: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != "doc-toctou-pub" {
		t.Fatalf("scan returned %d docs, want exactly doc-toctou-pub", len(expired))
	}
	expiredBefore := scanTime

	// 3. 扫描之后，原 worker 自己续租成功（比如它其实还在正常发布中）。
	newLease := time.Now().Add(leaseDuration).Truncate(time.Millisecond)
	if renewed, err := repo.renewDocumentLease(ctx, "doc-toctou-pub", 1, StatusPublishing, newLease); err != nil || !renewed {
		t.Fatalf("worker renew = %v, %v, want true/nil", renewed, err)
	}

	// 4. reconciliation 拿着旧的 expiredBefore 尝试取得恢复权。
	claimed, err := repo.claimExpiredPublishingRecovery(ctx, "doc-toctou-pub", 1, expiredBefore, time.Now().Add(leaseDuration))
	if err != nil {
		t.Fatalf("claimExpiredPublishingRecovery: %v", err)
	}
	if claimed {
		t.Fatal("recovery claim succeeded against a document renewed after the scan — TOCTOU window not closed")
	}

	// 5. 恢复权没拿到，真实 ReconcileStuckDocuments 里就不会再调
	// publishAndComplete——这里直接断言副作用：文档仍是 publishing，worker
	// 续的新租约没被覆盖，chunks 仍未发布（不可检索）。
	got, err := repo.getDocument(ctx, "doc-toctou-pub")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPublishing {
		t.Fatalf("doc status=%s, want unchanged publishing", got.Status)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(newLease) {
		t.Fatalf("lease = %v, want unchanged at the worker's renewed value %v", got.LeaseExpiresAt, newLease)
	}
	if found, err := repo.searchVectorChunks(ctx, []string{"kb-toctou-pub"}, []float32{1, 0, 0}, 10, RetrieveFilter{}); err != nil || len(found) != 0 {
		t.Fatalf("searchChunks = %v (err %v), want empty (must not have been published)", ids(found), err)
	}
}

func TestIntegrationClaimPublishingRecoveryConcurrentOnlyOneWinsThenPublishes(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-toctou-pub2", "m3", "u1", true)
	if err := repo.createDocument(ctx, Document{ID: "doc-toctou-pub2", KnowledgeBaseID: "kb-toctou-pub2",
		FileName: "f.txt", FileType: FileTypeTxt, FileSize: 1, StoragePath: "/dev/null", CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}
	if claimed, err := repo.claimDocumentProcessing(ctx, "doc-toctou-pub2", 1, time.Now().Add(leaseDuration)); err != nil || !claimed {
		t.Fatalf("claim setup = %v, %v", claimed, err)
	}
	if err := repo.createChunks(ctx, []Chunk{{
		ID: "tp2-c1", KnowledgeBaseID: "kb-toctou-pub2", DocumentID: "doc-toctou-pub2", ChunkIndex: 0,
		Content: "c1", ContentLength: 2, Embedding: []float32{1, 0, 0}, EmbeddingDimension: 3,
	}}, 1); err != nil {
		t.Fatalf("createChunks setup: %v", err)
	}
	if ok, err := repo.markDocumentPublishing(ctx, "doc-toctou-pub2", 1, time.Now().Add(-time.Minute), nil, nil); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// 两个"恢复者"并发抢同一份恢复权。
	expiredBefore := time.Now()
	var wg sync.WaitGroup
	claims := make([]bool, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			claims[i], errs[i] = repo.claimExpiredPublishingRecovery(ctx, "doc-toctou-pub2", 1, expiredBefore, time.Now().Add(leaseDuration))
		}(i)
	}
	wg.Wait()

	winners := 0
	for i := range claims {
		if errs[i] != nil {
			t.Fatalf("claim[%d] error: %v", i, errs[i])
		}
		if claims[i] {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (concurrent recovery claim must not double-win)", winners)
	}

	// 获胜者执行 publishAndComplete——同包内直接构造 *service 调用
	// unexported 方法，和 TestIntegrationPublishPermanentFailureReturnsErrorNotReady
	// 用的是同一种白盒方式。
	svc := &service{repo: repo, providerSvc: newFakeProvider(), storageDir: t.TempDir()}
	if err := svc.publishAndComplete(ctx, "doc-toctou-pub2", 1); err != nil {
		t.Fatalf("publishAndComplete: %v", err)
	}

	got, err := repo.getDocument(ctx, "doc-toctou-pub2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 1 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/1", got.Status, got.ChunkCount)
	}
	if got.LeaseExpiresAt != nil {
		t.Fatalf("lease = %v, want cleared", got.LeaseExpiresAt)
	}
	if n := countAllChunksForDocument(t, repo, "doc-toctou-pub2"); n != 1 {
		t.Fatalf("total chunk rows = %d, want 1 (no duplicates)", n)
	}
	found, err := repo.searchVectorChunks(ctx, []string{"kb-toctou-pub2"}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil || len(found) != 1 {
		t.Fatalf("searchChunks = %v (err %v), want 1 result (published and searchable)", ids(found), err)
	}
}

// --- 问题三：Embedding 批间维度一致性校验 ---

func TestIntegrationProcessDocumentDimensionMismatchAcrossBatchesFails(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	callCount := 0
	// 第二批返回和第一批不同的维度，模拟供应商在批处理中途行为不一致。
	fp.embed = func(providerID string, input []string) (provider.EmbedResult, error) {
		callCount++
		dim := 3
		if callCount == 2 {
			dim = 5
		}
		vecs := make([][]float32, len(input))
		for i := range vecs {
			vecs[i] = make([]float32, dim)
			vecs[i][0] = 1
		}
		return provider.EmbedResult{Embeddings: vecs, Dimension: dim}, nil
	}
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-dimmix", "m3", "u1", true)
	path := filepath.Join(dir, "dimmix.txt")
	content := strings.Repeat("a", 200) // 40 pieces -> 2 batches
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-dimmix", KnowledgeBaseID: "kb-dimmix",
		FileName: "dimmix.txt", FileType: FileTypeTxt, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-dimmix", 1); !errors.Is(err, ErrEmbeddingDimensionMismatch) {
		t.Fatalf("ProcessDocument err = %v, want ErrEmbeddingDimensionMismatch", err)
	}
	got, _ := repo.getDocument(ctx, "doc-dimmix")
	if got.Status != StatusFailed || !strings.Contains(got.ErrorMessage, "维度") {
		t.Fatalf("status=%s err=%q, want failed with dimension message", got.Status, got.ErrorMessage)
	}
	if n := countAllChunksForDocument(t, repo, "doc-dimmix"); n != 0 {
		t.Fatalf("chunks physically written despite dimension mismatch: %d, want 0", n)
	}
}

func TestIntegrationProcessDocumentVectorLengthMismatchFails(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	// result.Dimension 谎称 3，但第一个向量实际只有 2 个分量。
	fp.embed = func(providerID string, input []string) (provider.EmbedResult, error) {
		vecs := make([][]float32, len(input))
		for i := range vecs {
			vecs[i] = []float32{1, 0, 0}
		}
		if len(vecs) > 0 {
			vecs[0] = []float32{1, 0}
		}
		return provider.EmbedResult{Embeddings: vecs, Dimension: 3}, nil
	}
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-vecbad", "m3", "u1", true)
	path := filepath.Join(dir, "vecbad.txt")
	if err := os.WriteFile(path, []byte("aaaaabbbbb"), 0o644); err != nil { // 2 chunks
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-vecbad", KnowledgeBaseID: "kb-vecbad",
		FileName: "vecbad.txt", FileType: FileTypeTxt, FileSize: 10, StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-vecbad", 1); !errors.Is(err, ErrEmbeddingDimensionMismatch) {
		t.Fatalf("ProcessDocument err = %v, want ErrEmbeddingDimensionMismatch", err)
	}
	got, _ := repo.getDocument(ctx, "doc-vecbad")
	if got.Status != StatusFailed {
		t.Fatalf("status=%s, want failed", got.Status)
	}
	if n := countAllChunksForDocument(t, repo, "doc-vecbad"); n != 0 {
		t.Fatalf("chunks physically written despite vector length mismatch: %d, want 0", n)
	}
}

func TestIntegrationProcessDocumentMultiBatchConsistentDimensionSucceeds(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	callCount := 0
	fp.embed = func(providerID string, input []string) (provider.EmbedResult, error) {
		callCount++
		vecs := make([][]float32, len(input))
		for i := range vecs {
			vecs[i] = []float32{1, 0, 0}
		}
		return provider.EmbedResult{Embeddings: vecs, Dimension: 3}, nil
	}
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-dimok", "m3", "u1", true)
	path := filepath.Join(dir, "dimok.txt")
	content := strings.Repeat("a", 200) // 40 pieces -> 2 batches, both dim=3
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-dimok", KnowledgeBaseID: "kb-dimok",
		FileName: "dimok.txt", FileType: FileTypeTxt, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-dimok", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("embed called %d times, want 2", callCount)
	}
	got, err := repo.getDocument(ctx, "doc-dimok")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount != 40 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready/40", got.Status, got.ChunkCount)
	}
	if n := countAllChunksForDocument(t, repo, "doc-dimok"); n != 40 {
		t.Fatalf("chunk count = %d, want 40", n)
	}
}

// --- Citation V1：chunk 来源 metadata ---

func TestIntegrationProcessDocumentWritesDocumentNameSnapshot(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-docname", "m3", "u1", true)
	path := filepath.Join(dir, "architecture.txt")
	if err := os.WriteFile(path, []byte("aaaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := Document{ID: "doc-docname", KnowledgeBaseID: "kb-docname", FileName: "architecture.txt",
		FileType: FileTypeTxt, FileSize: 5, StoragePath: path, CreatedBy: "u1"}
	if err := repo.createDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, "doc-docname", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	got, err := repo.searchVectorChunks(ctx, []string{"kb-docname"}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("searchChunks = %v (err %v), want 1 chunk", ids(got), err)
	}
	if got[0].DocumentName != "architecture.txt" {
		t.Fatalf("DocumentName = %q, want the processed document's FileName snapshot", got[0].DocumentName)
	}
	// txt 从不产生页码/章节信息（该信息只对 pdf/md 有意义）——见
	// TestIntegrationProcessDocumentPDFPageNumbers /
	// TestIntegrationProcessDocumentMarkdownSectionTitleAndBreadcrumb。
	if got[0].PageNumber != nil || got[0].SectionTitle != nil {
		t.Fatalf("PageNumber/SectionTitle must stay nil for txt (never fabricated): page=%v section=%v", got[0].PageNumber, got[0].SectionTitle)
	}
}

func TestIntegrationProcessDocumentPDFPageNumbers(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	// seedKB's hardcoded ChunkSize=5 would shatter "alphaword"/"betaword"
	// into unrecognizable fragments — this test needs whole words intact
	// to prove cross-page isolation by content, so it creates the KB
	// directly with a size generous enough to keep each page's repeated
	// word whole.
	if err := repo.createKnowledgeBase(ctx, KnowledgeBase{
		ID: "kb-pdfpage", Name: "kb-pdfpage", EmbeddingModelID: "m3",
		ChunkSize: 60, ChunkOverlap: 10, CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	pdfPath := writeTestPDF(t, pdfLinesFromStrings(
		strings.Repeat("alphaword ", 15), // page 1, long enough to split into >=1 chunk
		strings.Repeat("betaword ", 15),  // page 2
	))
	if err := repo.createDocument(ctx, Document{ID: "doc-pdfpage", KnowledgeBaseID: "kb-pdfpage",
		FileName: "pages.pdf", FileType: FileTypePDF, FileSize: 1, StoragePath: pdfPath, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-pdfpage", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	got, err := repo.getDocument(ctx, "doc-pdfpage")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady || got.ChunkCount == 0 {
		t.Fatalf("doc status=%s chunkCount=%d, want ready with >0 chunks (err=%q)", got.Status, got.ChunkCount, got.ErrorMessage)
	}

	chunks, err := repo.searchVectorChunks(ctx, []string{"kb-pdfpage"}, []float32{1, 0, 0}, 100, RetrieveFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one searchable chunk")
	}
	// 006-pdf-layout-chunking 更新了本用例断言的契约。原来它断言"没有任何
	// chunk 同时含两页的内容"——那是按页硬切时期的性质，而这份夹具（两页都是
	// 无标点、接近满行宽的连续文本）恰恰是本功能存在的理由：它本来就是一段被
	// 页边界打断的连续内容，现在应该被接回去。继续断言旧性质等于断言那个缺陷。
	//
	// 现在断言的是更要紧的一条：不管切成什么样，**报出来的页码区间必须是真的**
	// ——两端都有值（C1）、起始 ≤ 结束（C2）、都落在文档的 2 页之内（C3），
	// 且确实出现了一个 1-2 的区间证明合并真的发生了。
	sawCrossPageInterval := false
	for _, c := range chunks {
		if c.SectionTitle != nil {
			t.Fatalf("pdf chunk must never fabricate a section title: %+v", c)
		}
		if c.PageNumber == nil || c.PageEnd == nil {
			t.Fatalf("pdf chunk missing a page interval end: %+v", c)
		}
		if *c.PageNumber > *c.PageEnd {
			t.Fatalf("inverted page interval %d-%d: %+v", *c.PageNumber, *c.PageEnd, c)
		}
		if *c.PageNumber < 1 || *c.PageEnd > 2 {
			t.Fatalf("page interval %d-%d outside the document's 2 pages: %+v", *c.PageNumber, *c.PageEnd, c)
		}
		if *c.PageNumber == 1 && *c.PageEnd == 2 {
			sawCrossPageInterval = true
		}
	}
	if !sawCrossPageInterval {
		t.Fatalf("no chunk reported a 1-2 interval, so the cross-page merge never happened")
	}
}

func TestIntegrationProcessDocumentMarkdownSectionTitleAndBreadcrumb(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	dir := t.TempDir()
	svc := newTestService(repo, fp, dir)
	ctx := context.Background()

	// Same reasoning as TestIntegrationProcessDocumentPDFPageNumbers:
	// seedKB's ChunkSize=5 would split each section's body far past one
	// chunk per heading, defeating this test's "exactly 2 chunks" check.
	if err := repo.createKnowledgeBase(ctx, KnowledgeBase{
		ID: "kb-mdsection", Name: "kb-mdsection", EmbeddingModelID: "m3",
		ChunkSize: 200, ChunkOverlap: 0, CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "doc.md")
	content := "# Guide\n\nIntro paragraph.\n\n## Setup\n\nSetup instructions go here."
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-mdsection", KnowledgeBaseID: "kb-mdsection",
		FileName: "doc.md", FileType: FileTypeMD, FileSize: len(content), StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-mdsection", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	chunks, err := repo.searchVectorChunks(ctx, []string{"kb-mdsection"}, []float32{1, 0, 0}, 100, RetrieveFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2 (one per section): %+v", len(chunks), ids(chunks))
	}
	var sawSetup bool
	for _, c := range chunks {
		if c.PageNumber != nil {
			t.Fatalf("md chunk must never fabricate a page number: %+v", c)
		}
		if c.SectionTitle != nil && *c.SectionTitle == "Setup" {
			sawSetup = true
			if !strings.Contains(c.Content, "Guide > Setup") {
				t.Fatalf("Setup chunk content missing heading breadcrumb: %q", c.Content)
			}
			if !strings.Contains(c.Content, "Setup instructions go here.") {
				t.Fatalf("Setup chunk content missing body: %q", c.Content)
			}
		}
	}
	if !sawSetup {
		t.Fatalf("expected one chunk with SectionTitle=Setup, got %+v", chunks)
	}
}

func TestIntegrationProcessDocumentEmptyContentFails(t *testing.T) {
	repo := setupIntegration(t)
	dir := t.TempDir()
	svc := newTestService(repo, newFakeProvider(), dir)
	ctx := context.Background()

	seedKB(t, repo, "kb-empty", "m3", "u1", true)
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte("   \n\n  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{ID: "doc-empty", KnowledgeBaseID: "kb-empty",
		FileName: "empty.txt", FileType: FileTypeTxt, FileSize: 7, StoragePath: path, CreatedBy: "u1"}); err != nil {
		t.Fatal(err)
	}

	if err := svc.ProcessDocument(ctx, "doc-empty", 1); !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("ProcessDocument err = %v, want ErrEmptyContent", err)
	}
	got, _ := repo.getDocument(ctx, "doc-empty")
	if got.Status != StatusFailed || !strings.Contains(got.ErrorMessage, "空") {
		t.Fatalf("status=%s err=%q, want failed with empty-content message", got.Status, got.ErrorMessage)
	}
}

func TestIntegrationSearchChunksHistoricalEmptyMetadataStillRetrievable(t *testing.T) {
	// 存量 chunk（迁移前写入，从未带 document_name）不能因为 metadata 缺失
	// 而检索失败或被排除——seedChunk 故意不设置 DocumentName，模拟这种
	// 历史行。
	repo := setupIntegration(t)
	ctx := context.Background()

	seedKB(t, repo, "kb-legacy", "m3", "u1", true)
	seedChunk(t, repo, "kb-legacy", "doc-legacy", "legacy-c1", []float32{1, 0, 0})

	got, err := repo.searchVectorChunks(ctx, []string{"kb-legacy"}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchChunks must not fail on empty source metadata: %v", err)
	}
	if len(got) != 1 || got[0].DocumentName != "" {
		t.Fatalf("got %+v, want 1 chunk with empty (not fabricated) DocumentName", got)
	}
	if got[0].PageNumber != nil || got[0].SectionTitle != nil {
		t.Fatalf("legacy chunk must not have fabricated page/section: page=%v section=%v", got[0].PageNumber, got[0].SectionTitle)
	}
}

// --- Phase 3: Hybrid Search（keyword / vector-independent-of-MySQL /
// fusion）集成测试 ---
//
// setupIntegration above requires BOTH docker-compose containers (MySQL
// for knowledge_bases/documents, Postgres for chunks) — evaluated as two
// separate arguments to NewRepository, so if only MySQL is down the whole
// test skips before Postgres is ever touched, even for a test body that
// never calls a single MySQL-backed method. Every test in this section
// only exercises chunks-table behavior (searchVectorChunks/
// searchKeywordChunks/createChunks/publishDocumentVersion — all PG-only,
// see repository.go), the same as TestIntegrationSearchVectorChunksOrderingAndDimensionFilter
// above already does even though it's gated by setupIntegration's MySQL
// requirement. setupPGOnlyIntegration drops that unnecessary MySQL
// dependency so these tests can run — and prove real pg_trgm/pgvector
// behavior against a real database — in any environment where Postgres
// alone is reachable, not just ones where the full docker-compose stack
// (including MySQL) happens to be up. It still skips (not fails) if
// Postgres itself is unreachable, via testutil.Postgres's own convention.
func setupPGOnlyIntegration(t *testing.T) *Repository {
	t.Helper()
	return NewRepository(nil, testutil.Postgres(t, "hybrid"))
}

// seedChunkWithContent is seedChunk's variant for keyword-search tests,
// which need real, distinguishable Content — seedChunk's fixed
// "content-"+chunkID filler has no meaningful trigram overlap with
// anything.
func seedChunkWithContent(t *testing.T, repo *Repository, kbID, docID, chunkID string, vec []float32, content string) {
	t.Helper()
	ctx := context.Background()
	if err := repo.createChunks(ctx, []Chunk{{
		ID: chunkID, KnowledgeBaseID: kbID, DocumentID: docID, ChunkIndex: 0,
		Content: content, ContentLength: len([]rune(content)),
		Embedding: vec, EmbeddingDimension: len(vec),
	}}, seedChunkVersion); err != nil {
		t.Fatalf("seed chunk %s: %v", chunkID, err)
	}
	if err := repo.publishDocumentVersion(ctx, docID, seedChunkVersion); err != nil {
		t.Fatalf("publish seeded chunk %s: %v", chunkID, err)
	}
}

// seedChunkWithContentSourceMeta is seedChunkWithContent plus the Citation
// V1 source-attribution fields (DocumentName/PageNumber/SectionTitle) —
// used only by TestIntegrationHybridSearchPreservesCitationMetadata.
func seedChunkWithContentSourceMeta(t *testing.T, repo *Repository, kbID, docID, chunkID string, vec []float32, content, documentName string, pageNumber *int, sectionTitle *string) {
	t.Helper()
	ctx := context.Background()
	if err := repo.createChunks(ctx, []Chunk{{
		ID: chunkID, KnowledgeBaseID: kbID, DocumentID: docID, ChunkIndex: 0,
		Content: content, ContentLength: len([]rune(content)),
		Embedding: vec, EmbeddingDimension: len(vec),
		DocumentName: documentName, PageNumber: pageNumber, PageEnd: pageNumber,
		SectionTitle: sectionTitle,
	}}, seedChunkVersion); err != nil {
		t.Fatalf("seed chunk %s: %v", chunkID, err)
	}
	if err := repo.publishDocumentVersion(ctx, docID, seedChunkVersion); err != nil {
		t.Fatalf("publish seeded chunk %s: %v", chunkID, err)
	}
}

// seedUnpublishedChunkWithContent writes a chunk and deliberately never
// publishes it — used only by the "unpublished chunk must not match"
// keyword-search test below.
func seedUnpublishedChunkWithContent(t *testing.T, repo *Repository, kbID, docID, chunkID string, vec []float32, content string) {
	t.Helper()
	if err := repo.createChunks(context.Background(), []Chunk{{
		ID: chunkID, KnowledgeBaseID: kbID, DocumentID: docID, ChunkIndex: 0,
		Content: content, ContentLength: len([]rune(content)),
		Embedding: vec, EmbeddingDimension: len(vec),
	}}, seedChunkVersion); err != nil {
		t.Fatalf("seed unpublished chunk %s: %v", chunkID, err)
	}
}

// 1. 关键词可以找回包含精确中文关键词的 chunk.
func TestIntegrationSearchKeywordChunksFindsExactChineseKeyword(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-kw-zh"
	seedChunkWithContent(t, repo, kb, "doc-kw-zh", "c-hit", []float32{1, 0, 0}, "本章介绍深度学习模型的训练与调参方法")
	seedChunkWithContent(t, repo, kb, "doc-kw-zh", "c-miss", []float32{1, 0, 0}, "今天天气不错，适合出门散步和摄影")

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "深度学习模型", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c-hit" {
		t.Fatalf("got %v, want exactly [c-hit] (the unrelated Chinese chunk must not clear the similarity floor)", ids(got))
	}
	if got[0].Score <= 0 {
		t.Fatalf("c-hit.Score = %f, want a positive word-similarity score", got[0].Score)
	}
}

// 2. 关键词可以找回英文关键词.
func TestIntegrationSearchKeywordChunksFindsExactEnglishKeyword(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-kw-en"
	seedChunkWithContent(t, repo, kb, "doc-kw-en", "c-hit-en", []float32{1, 0, 0}, "PostgreSQL vector search is powered by the pgvector extension")
	seedChunkWithContent(t, repo, kb, "doc-kw-en", "c-miss-en", []float32{1, 0, 0}, "The quick brown fox jumps over the lazy dog")

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "pgvector extension", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "c-hit-en" {
		t.Fatalf("got %v, want exactly [c-hit-en]", ids(got))
	}
}

// 3. 未发布 chunk 不能命中.
func TestIntegrationSearchKeywordChunksExcludesUnpublished(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-kw-unpub"
	seedUnpublishedChunkWithContent(t, repo, kb, "doc-kw-unpub", "c-draft", []float32{1, 0, 0}, "机密项目Zeta的详细技术方案说明")

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "机密项目Zeta", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty — unpublished chunk must never be keyword-searchable", ids(got))
	}
}

// 4. 其他知识库的 chunk 不能命中.
func TestIntegrationSearchKeywordChunksScopedToKnowledgeBase(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	seedChunkWithContent(t, repo, "kb-kw-a", "doc-kw-a", "c-a", []float32{1, 0, 0}, "跨知识库隔离测试关键词CROSSKBTOKEN")
	seedChunkWithContent(t, repo, "kb-kw-b", "doc-kw-b", "c-b", []float32{1, 0, 0}, "另一个完全无关的知识库内容")

	got, err := repo.searchKeywordChunks(ctx, []string{"kb-kw-b"}, "跨知识库隔离测试关键词CROSSKBTOKEN", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty — kb-kw-a's chunk must not leak into a kb-kw-b-scoped search", ids(got))
	}

	// Sanity check the positive case too, so a bug that made the filter a
	// no-op wouldn't be masked by both sides returning empty.
	gotOwn, err := repo.searchKeywordChunks(ctx, []string{"kb-kw-a"}, "跨知识库隔离测试关键词CROSSKBTOKEN", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(gotOwn) != 1 || gotOwn[0].ID != "c-a" {
		t.Fatalf("scoped to its own KB, got %v, want [c-a]", ids(gotOwn))
	}
}

// 5. keyword search 不受 embedding dimension 不同影响.
func TestIntegrationSearchKeywordChunksIgnoresEmbeddingDimension(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-kw-dim"
	seedChunkWithContent(t, repo, kb, "doc-kw-dim", "c-3d", []float32{1, 0, 0}, "维度隔离验证关键词DIMTOKEN三维版本")
	seedChunkWithContent(t, repo, kb, "doc-kw-dim", "c-2d", []float32{1, 0}, "维度隔离验证关键词DIMTOKEN二维版本")

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "维度隔离验证关键词DIMTOKEN", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d chunks %v, want 2 — keyword search must find both the 3-dim and 2-dim chunk (no dimension filter)", len(got), ids(got))
	}
}

// 6. vector search 仍保持维度过滤和余弦顺序.
//
// TestIntegrationSearchVectorChunksOrderingAndDimensionFilter above
// already asserts this, but it's gated by setupIntegration's MySQL
// requirement — in an environment with only Postgres up (like this one),
// that test skips without ever actually running. This PG-only variant
// re-asserts the same cosine-order + dimension-filter contract so Phase
// 3's "vector search behavior must be unchanged" claim has real execution
// proof, not just a skip.
func TestIntegrationSearchVectorChunksOrderingAndDimensionFilterPGOnly(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-vec-pgonly"
	seedChunk(t, repo, kb, "doc-vec-pgonly", "c-exact-pgonly", []float32{1, 0, 0})
	seedChunk(t, repo, kb, "doc-vec-pgonly", "c-mid-pgonly", []float32{1, 1, 0})
	seedChunk(t, repo, kb, "doc-vec-pgonly", "c-ortho-pgonly", []float32{0, 1, 0})
	seedChunk(t, repo, kb, "doc-vec-pgonly", "c-2d-pgonly", []float32{1, 0}) // 异维度，必须被过滤

	got, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3 (2-dim chunk must be filtered out)", len(got))
	}
	wantOrder := []string{"c-exact-pgonly", "c-mid-pgonly", "c-ortho-pgonly"}
	wantScore := []float64{1.0, math.Sqrt2 / 2, 0.0}
	for i := range got {
		if got[i].ID != wantOrder[i] {
			t.Fatalf("rank %d = %s, want %s (full: %v)", i, got[i].ID, wantOrder[i], ids(got))
		}
		if math.Abs(got[i].Score-wantScore[i]) > 1e-6 {
			t.Fatalf("score[%d] = %f, want %f", i, got[i].Score, wantScore[i])
		}
	}
}

// 6b. 多个向量候选余弦距离完全相同时，SearchVectorChunks 必须按 chunk ID
// 升序稳定排序——这是本轮审核修复的根因：ORDER BY embedding <=> query 单独
// 存在时，距离相同的行在 PostgreSQL 里返回顺序不定，rrfFuse 把这个返回
// 顺序当成 RRF 的 rank，顺序不稳会让同一批候选在不同次调用之间拿到不同
// rank，最终融合结果就跟着不稳定。四个 chunk 用完全相同的 embedding
// （因此余弦距离/相似度都完全相同）验证 SQL 的 id ASC 兜底真的生效。
func TestIntegrationSearchVectorChunksTiedScoreSortsStablyByID(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-vec-tie"
	vec := []float32{1, 0, 0}
	// 故意乱序插入，用来确认排序不是"插入顺序凑巧正确"。
	seedChunk(t, repo, kb, "doc-vec-tie", "tie-c", vec)
	seedChunk(t, repo, kb, "doc-vec-tie", "tie-a", vec)
	seedChunk(t, repo, kb, "doc-vec-tie", "tie-d", vec)
	seedChunk(t, repo, kb, "doc-vec-tie", "tie-b", vec)

	got, err := repo.searchVectorChunks(ctx, []string{kb}, vec, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d chunks, want 4", len(got))
	}
	for i, c := range got {
		if c.Score != 1.0 {
			t.Fatalf("chunk %d (%s) Score = %f, want 1.0 (identical embeddings must score identically)", i, c.ID, c.Score)
		}
	}
	want := []string{"tie-a", "tie-b", "tie-c", "tie-d"}
	if got := ids(got); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v (tied cosine score must sort by ID ascending)", got, want)
	}
}

// 7. Hybrid Search 能让关键词强匹配结果进入最终 topK.
//
// v1..v4 are pure vector hits (decreasing cosine similarity to the [1,0,0]
// query, no keyword overlap at all). kw1 is the opposite: cosine ~0 to the
// query (embedding [0,1,0], orthogonal), but its content is an exact match
// for the keyword query — the #1 keyword hit. A vector-only top4 would be
// [v1,v2,v3,v4] and would never surface kw1 at all. Fusing real
// searchVectorChunks/searchKeywordChunks output through rrfFuse must let
// kw1's strong keyword rank promote it into the final topK=4, displacing
// the weakest pure-vector hit (v4).
func TestIntegrationHybridSearchPromotesStrongKeywordMatchIntoTopK(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-hybrid-promote"
	queryVec := []float32{1, 0, 0}
	queryText := "唯一关键词ZZZTOKEN"

	seedChunkWithContent(t, repo, kb, "doc-hybrid", "v1", []float32{1, 0, 0}, "无关内容一：项目进度周报")
	seedChunkWithContent(t, repo, kb, "doc-hybrid", "v2", []float32{1, 0.1, 0}, "无关内容二：团队建设活动安排")
	seedChunkWithContent(t, repo, kb, "doc-hybrid", "v3", []float32{1, 0.2, 0}, "无关内容三：会议纪要草稿")
	seedChunkWithContent(t, repo, kb, "doc-hybrid", "v4", []float32{1, 0.3, 0}, "无关内容四：办公用品申购清单")
	seedChunkWithContent(t, repo, kb, "doc-hybrid", "kw1", []float32{0, 1, 0}, "本段内容包含唯一关键词ZZZTOKEN用于命中验证")

	const topK = 4
	cK := candidateK(topK)

	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	keywordChunks, err := repo.searchKeywordChunks(ctx, []string{kb}, queryText, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(keywordChunks) != 1 || keywordChunks[0].ID != "kw1" {
		t.Fatalf("keyword path got %v, want exactly [kw1]", ids(keywordChunks))
	}
	// Vector-only top4 (the pre-Hybrid-Search behavior) would have been
	// exactly these 4 — confirms kw1 really is excluded from a
	// vector-only view before we assert Hybrid Search rescues it.
	if len(vectorChunks) < 5 || vectorChunks[4].ID != "kw1" {
		t.Fatalf("expected kw1 to rank 5th (last) by cosine similarity among 5 seeded chunks, got vector order %v", ids(vectorChunks))
	}
	vectorOnlyTop4 := ids(vectorChunks[:4])
	for _, id := range vectorOnlyTop4 {
		if id == "kw1" {
			t.Fatalf("test setup invalid: kw1 must NOT be in the vector-only top4, got %v", vectorOnlyTop4)
		}
	}

	fused, _ := fuseTopK(vectorChunks, keywordChunks, topK)
	if len(fused) != topK {
		t.Fatalf("got %d fused results, want topK=%d", len(fused), topK)
	}
	foundKW1 := false
	for _, c := range fused {
		if c.ID == "kw1" {
			foundKW1 = true
		}
	}
	if !foundKW1 {
		t.Fatalf("Hybrid Search fused topK = %v, want kw1 present (its strong keyword rank should promote it past the weakest vector-only hit)", ids(fused))
	}
	if fused[len(fused)-1].ID == "v4" {
		t.Fatalf("v4 (weakest vector hit, no keyword match) should have been displaced out of topK by kw1, but final result is %v", ids(fused))
	}
}

// 8. 同一 chunk 不重复返回：一个 chunk 如果同时被向量和关键词两路命中，
// 融合后必须只出现一次.
func TestIntegrationHybridSearchDeduplicatesChunkHitByBothPaths(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-hybrid-dedup"
	queryVec := []float32{1, 0, 0}
	queryText := "去重验证关键词DEDUPTOKEN"

	// "both" scores well on cosine AND contains the exact keyword phrase
	// — it must show up in both searchVectorChunks and searchKeywordChunks
	// results, and rrfFuse must still return it exactly once.
	seedChunkWithContent(t, repo, kb, "doc-hybrid-dedup", "both", []float32{1, 0, 0}, "去重验证关键词DEDUPTOKEN同时命中向量与关键词两路")
	seedChunkWithContent(t, repo, kb, "doc-hybrid-dedup", "other", []float32{1, 0.5, 0}, "无关内容，仅用于填充候选集")

	cK := candidateK(5)
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	keywordChunks, err := repo.searchKeywordChunks(ctx, []string{kb}, queryText, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	bothInVector, bothInKeyword := false, false
	for _, c := range vectorChunks {
		if c.ID == "both" {
			bothInVector = true
		}
	}
	for _, c := range keywordChunks {
		if c.ID == "both" {
			bothInKeyword = true
		}
	}
	if !bothInVector || !bothInKeyword {
		t.Fatalf("test setup invalid: 'both' must appear in both raw candidate lists (vector=%v, keyword=%v)", ids(vectorChunks), ids(keywordChunks))
	}

	fused, _ := fuseTopK(vectorChunks, keywordChunks, 5)
	count := 0
	for _, c := range fused {
		if c.ID == "both" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("'both' appeared %d times in fused output %v, want exactly 1", count, ids(fused))
	}
}

// 9. Citation 的 document_name/page_number/section_title 保持完整——both
// through the raw repository calls and through rrfFuse's pass-through.
func TestIntegrationHybridSearchPreservesCitationMetadata(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-hybrid-citation"
	page := 12
	section := "4.3 退款政策"
	seedChunkWithContentSourceMeta(t, repo, kb, "doc-hybrid-citation", "c-cited", []float32{1, 0, 0},
		"引用元数据验证关键词CITETOKEN退款政策说明", "policy-handbook.pdf", &page, &section)

	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vectorChunks) != 1 || vectorChunks[0].DocumentName != "policy-handbook.pdf" ||
		vectorChunks[0].PageNumber == nil || *vectorChunks[0].PageNumber != page ||
		vectorChunks[0].SectionTitle == nil || *vectorChunks[0].SectionTitle != section {
		t.Fatalf("vector path lost Citation metadata: %+v", vectorChunks)
	}

	keywordChunks, err := repo.searchKeywordChunks(ctx, []string{kb}, "引用元数据验证关键词CITETOKEN", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(keywordChunks) != 1 || keywordChunks[0].DocumentName != "policy-handbook.pdf" ||
		keywordChunks[0].PageNumber == nil || *keywordChunks[0].PageNumber != page ||
		keywordChunks[0].SectionTitle == nil || *keywordChunks[0].SectionTitle != section {
		t.Fatalf("keyword path lost Citation metadata: %+v", keywordChunks)
	}

	fused, _ := fuseTopK(vectorChunks, keywordChunks, 10)
	if len(fused) != 1 || fused[0].DocumentName != "policy-handbook.pdf" ||
		fused[0].PageNumber == nil || *fused[0].PageNumber != page ||
		fused[0].SectionTitle == nil || *fused[0].SectionTitle != section {
		t.Fatalf("rrfFuse lost Citation metadata: %+v", fused)
	}
}

// --- Phase 5: exact content dedup, exercised against real Postgres ---
//
// These run the exact same sequence Service.Retrieve does
// (searchVectorChunks/searchKeywordChunks -> rrfFuse -> repo.
// findPublishedNeighborChunks -> expandWithNeighbors), driven by
// setupPGOnlyIntegration so they get real execution proof in this sandbox
// (no MySQL available here — see setupPGOnlyIntegration's own doc comment
// and TestIntegrationRetrieveDedupsExactDuplicateContentEndToEnd below for
// the full Service.Retrieve equivalent, which does need MySQL and skips
// here).

// 一 + 四（真实 Postgres 版本）: 两条不同 ID、正文完全相同的 chunk（一条
// 余弦相似度更高），topK 只够放 2 条时，去重后应该只保留分数更高的那条，
// 让第三条内容不同的候选补位，而不是把两条重复内容都占满 topK。
func TestIntegrationRRFFuseDedupsExactDuplicateCoreContentAgainstRealPostgres(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-dedup-core"
	queryVec := []float32{1, 0, 0}
	dupContent := "完全相同的正文内容，用于验证核心块内容去重"

	seedChunkWithContent(t, repo, kb, "doc-dedup-core", "dup-high", queryVec, dupContent) // cos = 1.0
	seedChunkWithContent(t, repo, kb, "doc-dedup-core", "dup-low", []float32{1, 0.05, 0}, dupContent)
	seedChunkWithContent(t, repo, kb, "doc-dedup-core", "unique", []float32{1, 0.2, 0}, "内容C，与前两条完全不同")

	const topK = 2
	cK := candidateK(topK)
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vectorChunks) != 3 {
		t.Fatalf("test setup invalid: got %d raw candidates %v, want all 3 seeded chunks back before dedup", len(vectorChunks), ids(vectorChunks))
	}

	fused, _ := fuseTopK(vectorChunks, nil, topK)

	want := []string{"dup-high", "unique"}
	if got := ids(fused); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v — dup-low must be dropped as a content duplicate of dup-high, and unique must fill the freed topK slot", got, want)
	}
}

// 五 + 核心去重后才查邻接（真实 Postgres 版本）: 一条内容重复、排名较低
// 的核心块必须在 rrfFuse 阶段就被淘汰，因此它绝不能进入
// buildNeighborGroups/findPublishedNeighborChunks 的查询范围——用它自己
// 独有的邻接块内容作为"污染探针"：如果这条邻接内容出现在最终结果里，说明
// 被淘汰的重复核心块仍然被查了邻接窗口，这是不允许的。
func TestIntegrationExpandWithNeighborWindowNeverQueriesNeighborsOfDedupedCoreChunk(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-dedup-neighbor-skip"
	queryVec := []float32{1, 0, 0}
	dupContent := "重复正文：去重后不应再查询邻接窗口"

	seedNeighborChunkBatch(t, repo, kb, "doc-dedup-high", 1, []neighborSeedChunk{
		{ID: "skip-dup-high", ChunkIndex: 5, Content: dupContent, Vec: queryVec},
	}, true)
	seedNeighborChunkBatch(t, repo, kb, "doc-dedup-low", 1, []neighborSeedChunk{
		// 排名更低（余弦相似度略小），内容和 dup-high 重复，会被 rrfFuse 淘汰。
		{ID: "skip-dup-low", ChunkIndex: 5, Content: dupContent, Vec: []float32{1, 0.05, 0}},
		// dup-low 独有的邻接块——如果被查询到，说明淘汰后的核心块仍然触发了
		// 邻接窗口查询，这是"探针"内容，绝不应该出现在最终结果里。
		{ID: "skip-dup-low-neighbor", ChunkIndex: 6, Content: "污染探针：不应该出现在结果里", Vec: []float32{1, 0.05, 0}},
	}, true)

	const topK = 1
	cK := candidateK(topK)
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}

	anchors, _ := fuseTopK(vectorChunks, nil, topK)
	if got := ids(anchors); !reflect.DeepEqual(got, []string{"skip-dup-high"}) {
		t.Fatalf("test setup invalid: anchors = %v, want exactly [dup-high] (dup-low must already be content-deduped out before neighbor lookup)", got)
	}

	groups := buildNeighborGroups(anchors)
	var allNeighbors []RetrievedChunk
	for key, idxSet := range groups {
		indexes := make([]int, 0, len(idxSet))
		for idx := range idxSet {
			indexes = append(indexes, idx)
		}
		neighbors, err := repo.findPublishedNeighborChunks(ctx, key.documentID, key.documentVersion, indexes)
		if err != nil {
			t.Fatalf("findPublishedNeighborChunks: %v", err)
		}
		allNeighbors = append(allNeighbors, neighbors...)
	}

	final, _ := expandWithNeighbors(anchors, allNeighbors)
	for _, c := range final {
		if c.ID == "skip-dup-low-neighbor" || c.DocumentID == "doc-dedup-low" {
			t.Fatalf("got %v — the eliminated dup-low chunk's neighbor window leaked into the result, but a content-deduped core chunk must never get a neighbor lookup", ids(final))
		}
	}
	if got := ids(final); !reflect.DeepEqual(got, []string{"skip-dup-high"}) {
		t.Fatalf("got %v, want exactly [dup-high] (no neighbors exist for doc-dedup-high at index 4 or 6)", got)
	}
}

// 五（核心与邻接重复，真实 Postgres 版本）: 邻接块的正文和某个核心块的正文
// 完全相同时，最终结果必须保留核心块、丢弃邻接块。
func TestIntegrationExpandWithNeighborsDedupPrefersCoreOverNeighborAgainstRealPostgres(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-dedup-core-vs-neighbor"
	sharedContent := "核心块与邻接块共享的正文内容"

	seedNeighborChunkBatch(t, repo, kb, "doc-cvn", 1, []neighborSeedChunk{
		{ID: "anchor", ChunkIndex: 5, Content: "anchor 自己的正文", Vec: []float32{1, 0, 0}},
		{ID: "anchor-prev", ChunkIndex: 4, Content: sharedContent, Vec: []float32{1, 0, 0}},
	}, false)
	seedNeighborChunkBatch(t, repo, kb, "doc-cvn-core2", 1, []neighborSeedChunk{
		// 另一个核心块，正文恰好和 anchor 的邻接块（anchor-prev）完全相同。
		{ID: "core2", ChunkIndex: 0, Content: sharedContent, Vec: []float32{1, 0, 0}},
	}, false)
	if err := repo.publishDocumentVersion(ctx, "doc-cvn", 1); err != nil {
		t.Fatalf("publish doc-cvn: %v", err)
	}
	if err := repo.publishDocumentVersion(ctx, "doc-cvn-core2", 1); err != nil {
		t.Fatalf("publish doc-cvn-core2: %v", err)
	}

	anchors := []RetrievedChunk{
		anchorRC("anchor", "doc-cvn", 1, 5, 0.9),
		anchorRC("core2", "doc-cvn-core2", 1, 0, 0.8),
	}
	// 手动补上 anchor.Content/core2.Content——anchorRC 只是测试构造器，真实
	// Content 要从数据库里查出来才能参与去重判断，所以这里直接查一遍确认
	// 种子数据本身是对的，再用真实 findPublishedNeighborChunks 结果驱动
	// expandWithNeighbors。
	anchors[0].Content = "anchor 自己的正文"
	anchors[1].Content = sharedContent

	groups := buildNeighborGroups(anchors)
	var allNeighbors []RetrievedChunk
	for key, idxSet := range groups {
		indexes := make([]int, 0, len(idxSet))
		for idx := range idxSet {
			indexes = append(indexes, idx)
		}
		neighbors, err := repo.findPublishedNeighborChunks(ctx, key.documentID, key.documentVersion, indexes)
		if err != nil {
			t.Fatalf("findPublishedNeighborChunks: %v", err)
		}
		allNeighbors = append(allNeighbors, neighbors...)
	}
	foundPrev := false
	for _, n := range allNeighbors {
		if n.ID == "anchor-prev" {
			foundPrev = true
		}
	}
	if !foundPrev {
		t.Fatalf("test setup invalid: anchor-prev must be fetched as anchor's neighbor, got %v", ids(allNeighbors))
	}

	final, _ := expandWithNeighbors(anchors, allNeighbors)
	want := []string{"anchor", "core2"}
	if got := ids(final); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v — anchor-prev duplicates core2's content and must be dropped in favor of the core hit core2", got, want)
	}
}

// 待修复项 4（审核修复）: 真实向量检索驱动的核心命中选择 + 邻接扩展去重，
// 端到端等价于 TestIntegrationRetrieveNeighborDedupPrefersCoreOverDuplicateNeighborContentEndToEnd
// （那个测试需要 MySQL，本沙箱按约定 SKIP）——这里用
// setupPGOnlyIntegration 跑相同的种子数据和相同的真实 searchVectorChunks
// -> rrfFuse -> buildNeighborGroups -> findPublishedNeighborChunks ->
// expandWithNeighbors 调用链（只是不经过 Service.Retrieve 本身的 MySQL
// knowledge_base 查询），在本沙箱内提供真实 Postgres 执行证明：
// anchor-prev 用较弱的向量（cos=0，非 1.0）不会在真实向量检索里赢得核心
// 命中名额，topK=2 时核心命中确实是 anchor 和 core2，随后邻接扩展正确地
// 把重复正文的 anchor-prev 去重掉。
func TestIntegrationRealVectorSearchDrivenAnchorSelectionPrefersCoreOverDuplicateNeighborContent(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-dedup-nb-real-vector"
	sharedContent := "核心块 core2 与 anchor 的邻接块共享的正文"
	queryVec := []float32{1, 0, 0}

	seedNeighborChunkBatch(t, repo, kb, "doc-anchor-real-vec", 1, []neighborSeedChunk{
		{ID: "rv-anchor", ChunkIndex: 5, Content: "anchor 自己独有的正文", Vec: queryVec},             // cos = 1.0
		{ID: "rv-anchor-prev", ChunkIndex: 4, Content: sharedContent, Vec: []float32{0, 1, 0}}, // cos = 0.0，绝不能赢得核心命中名额
	}, true)
	seedNeighborChunkBatch(t, repo, kb, "doc-core2-real-vec", 1, []neighborSeedChunk{
		{ID: "rv-core2", ChunkIndex: 0, Content: sharedContent, Vec: queryVec}, // cos = 1.0
	}, true)

	const topK = 2
	cK := candidateK(topK)
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vectorChunks) != 3 {
		t.Fatalf("test setup invalid: got %d candidates %v, want all 3 seeded chunks visible to vector search", len(vectorChunks), ids(vectorChunks))
	}

	// Note: pre-Phase-8, anchor-prev's content duplicate of core2 could
	// legitimately be caught at EITHER the core-dedup stage (rrfFuse) or
	// the neighbor-dedup stage (expandWithNeighbors), since anchor-prev's
	// weak-but-nonzero cosine score just meant it lost the topK race, not
	// that it was excluded outright. Since Phase 8, anchor-prev's cos=0.0
	// is also below vectorAdmissionThreshold (0.35) with no keyword signal
	// at all, so rrfFuse's admission gate now rejects it OUTRIGHT, before
	// it's even eligible for content-dedup consideration — its
	// core-duplicate-suppression count contribution is now always 0.
	// findPublishedNeighborChunks below still independently re-fetches it
	// as anchor's chunk_index-1 neighbor (admission only ever filters core
	// Hybrid Search candidates, never neighbor-window lookups — see the
	// design doc §7), so it's now deterministically caught at the
	// neighbor-dedup stage instead. What's asserted below is unchanged:
	// the FINAL result is correct, anchor and core2 (both real core hits)
	// both survive, anchor-prev never does, and dedup happened somewhere
	// in the pipeline.
	anchors, admission := fuseTopK(vectorChunks, nil, topK)
	wantAnchors := []string{"rv-anchor", "rv-core2"}
	if got := ids(anchors); !reflect.DeepEqual(got, wantAnchors) {
		t.Fatalf("got anchors %v, want %v — anchor-prev's weak cosine score must keep it out of the topK core hits, letting core2 (a real perfect-cosine hit) in instead", got, wantAnchors)
	}

	groups := buildNeighborGroups(anchors)
	var allNeighbors []RetrievedChunk
	for key, idxSet := range groups {
		indexes := make([]int, 0, len(idxSet))
		for idx := range idxSet {
			indexes = append(indexes, idx)
		}
		neighbors, err := repo.findPublishedNeighborChunks(ctx, key.documentID, key.documentVersion, indexes)
		if err != nil {
			t.Fatalf("findPublishedNeighborChunks: %v", err)
		}
		allNeighbors = append(allNeighbors, neighbors...)
	}

	final, neighborDuplicateCount := expandWithNeighbors(anchors, allNeighbors)
	if admission.ContentDuplicateCount+neighborDuplicateCount < 1 {
		t.Fatalf("ContentDuplicateCount(%d) + neighborDuplicateCount(%d) = 0, want at least 1 — anchor-prev's duplicate content must be caught (via admission-rejection-then-neighbor-dedup, or core-dedup) somewhere in the pipeline", admission.ContentDuplicateCount, neighborDuplicateCount)
	}
	wantFinal := []string{"rv-anchor", "rv-core2"}
	if got := ids(final); !reflect.DeepEqual(got, wantFinal) {
		t.Fatalf("got %v, want %v — anchor-prev must be dropped as a duplicate of core2, both real core hits must survive", got, wantFinal)
	}
	for _, c := range final {
		if c.ID == "rv-anchor-prev" {
			t.Fatalf("anchor-prev leaked into the final result %v — it duplicates core2's content and must never survive", ids(final))
		}
	}
}

// 10. 空 query、空知识库列表返回空.
func TestIntegrationSearchKeywordChunksEmptyQueryOrKBsReturnsEmpty(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	kb := "kb-kw-empty"
	seedChunkWithContent(t, repo, kb, "doc-kw-empty", "c1", []float32{1, 0, 0}, "任意内容用于确认过滤条件生效ANYTOKEN")

	if got, err := repo.searchKeywordChunks(ctx, []string{kb}, "", 10, RetrieveFilter{}); err != nil || got != nil {
		t.Fatalf("searchKeywordChunks(empty query) = %v, %v; want nil, nil", got, err)
	}
	if got, err := repo.searchKeywordChunks(ctx, nil, "ANYTOKEN", 10, RetrieveFilter{}); err != nil || got != nil {
		t.Fatalf("searchKeywordChunks(nil kbIDs) = %v, %v; want nil, nil", got, err)
	}
	if got, err := repo.searchKeywordChunks(ctx, []string{}, "ANYTOKEN", 10, RetrieveFilter{}); err != nil || got != nil {
		t.Fatalf("searchKeywordChunks(empty kbIDs) = %v, %v; want nil, nil", got, err)
	}
}

func ids(chunks []RetrievedChunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.ID
	}
	return out
}

func scores(chunks []RetrievedChunk) []float64 {
	out := make([]float64, len(chunks))
	for i, c := range chunks {
		out[i] = c.Score
	}
	return out
}

// --- Phase 4: 邻接分块扩展（Neighbor Window Retrieval）---

// neighborSeedChunk is one row's worth of input to seedNeighborChunkBatch.
type neighborSeedChunk struct {
	ID           string
	ChunkIndex   int
	Content      string
	Vec          []float32
	DocumentName string
	PageNumber   *int
	// PageEnd defaults to PageNumber when left nil, which is what every
	// pre-006 fixture means: a chunk that sits on exactly one page. Set it
	// explicitly only to seed a genuinely cross-page chunk (page_number=3,
	// page_end=4). Leaving it nil while PageNumber is set would violate
	// invariant C1 and the chunks_page_range_valid constraint would reject
	// the whole batch.
	PageEnd      *int
	SectionTitle *string
}

// seedPageEnd applies neighborSeedChunk.PageEnd's default: a fixture that
// only says "page 7" means the closed interval [7, 7].
func seedPageEnd(c neighborSeedChunk) *int {
	if c.PageEnd != nil {
		return c.PageEnd
	}
	return c.PageNumber
}

// seedNeighborChunkBatch writes every row in chunks under (kbID, docID,
// version) in one PG transaction (repo.createChunks already batches), then
// — only when publish is true — publishes that version. Publishing a
// version deletes every OTHER version of the same docID in the same PG
// transaction (see repository.go's publishDocumentVersion /
// DeleteObsoleteChunkVersions) — the exact production reprocessing
// behavior TestIntegrationFindPublishedNeighborChunksOldVersionDeletedReturnsEmpty
// relies on to simulate "the old version got reprocessed away for real".
func seedNeighborChunkBatch(t *testing.T, repo *Repository, kbID, docID string, version int64, chunks []neighborSeedChunk, publish bool) {
	t.Helper()
	ctx := context.Background()
	rows := make([]Chunk, 0, len(chunks))
	for _, c := range chunks {
		rows = append(rows, Chunk{
			ID: c.ID, KnowledgeBaseID: kbID, DocumentID: docID, ChunkIndex: c.ChunkIndex,
			Content: c.Content, ContentLength: len([]rune(c.Content)),
			Embedding: c.Vec, EmbeddingDimension: len(c.Vec),
			DocumentName: c.DocumentName, PageNumber: c.PageNumber, PageEnd: seedPageEnd(c),
			SectionTitle: c.SectionTitle,
		})
	}
	if err := repo.createChunks(ctx, rows, version); err != nil {
		t.Fatalf("seed neighbor chunk batch (doc=%s version=%d): %v", docID, version, err)
	}
	if publish {
		if err := repo.publishDocumentVersion(ctx, docID, version); err != nil {
			t.Fatalf("publish neighbor chunk batch (doc=%s version=%d): %v", docID, version, err)
		}
	}
}

// insertRawPublishedChunk bypasses createChunks/publishDocumentVersion to
// construct chunk rows in states normal production code never produces on
// its own — two is_published=true rows for the same document_id under
// different document_version (publishDocumentVersion's own transaction
// guarantees this can't happen through the real write path), or two rows
// sharing one chunk_index (chunkDocument always assigns a unique,
// contiguous index per document version). Used only to defense-in-depth
// test FindPublishedNeighborChunks' WHERE/ORDER BY clauses directly,
// independent of whether the normal write path happens to also exercise
// them.
func insertRawPublishedChunk(t *testing.T, repo *Repository, kbID, docID, chunkID string, version int64, chunkIndex int, content string) {
	t.Helper()
	_, err := repo.pgdb.ExecContext(context.Background(),
		`INSERT INTO chunks (id, knowledge_base_id, document_id, chunk_index, content, content_length, embedding, embedding_dimension, document_version, is_published, document_name)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, true, $10)`,
		chunkID, kbID, docID, chunkIndex, content, len([]rune(content)), pgvector.NewVector([]float32{1, 0, 0}), 3, version, "raw.pdf")
	if err != nil {
		t.Fatalf("insertRawPublishedChunk: %v", err)
	}
}

// 1. 查到同文档、同版本的前后 chunk.
func TestIntegrationFindPublishedNeighborChunksReturnsPreviousAndNext(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-basic"
	seedNeighborChunkBatch(t, repo, "kb-nb-basic", docID, 1, []neighborSeedChunk{
		{ID: "b-4", ChunkIndex: 4, Content: "c4", Vec: []float32{1, 0, 0}},
		{ID: "b-5", ChunkIndex: 5, Content: "c5-anchor", Vec: []float32{1, 0, 0}},
		{ID: "b-6", ChunkIndex: 6, Content: "c6", Vec: []float32{1, 0, 0}},
	}, true)

	got, err := repo.findPublishedNeighborChunks(ctx, docID, 1, neighborIndexesFor(5))
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	want := []string{"b-4", "b-6"}
	if !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("got %v, want %v (previous and next chunk of index 5)", ids(got), want)
	}
}

// 2. 不返回其他文档的相同 chunk index.
func TestIntegrationFindPublishedNeighborChunksExcludesOtherDocuments(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	seedNeighborChunkBatch(t, repo, "kb-nb-x", "doc-nb-x", 1, []neighborSeedChunk{
		{ID: "x-1", ChunkIndex: 1, Content: "x content", Vec: []float32{1, 0, 0}},
	}, true)
	seedNeighborChunkBatch(t, repo, "kb-nb-y", "doc-nb-y", 1, []neighborSeedChunk{
		{ID: "y-1", ChunkIndex: 1, Content: "y content", Vec: []float32{1, 0, 0}},
	}, true)

	got, err := repo.findPublishedNeighborChunks(ctx, "doc-nb-x", 1, []int{1})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "x-1" {
		t.Fatalf("got %v, want exactly [x-1] (doc-nb-y's same-index chunk must not leak in)", ids(got))
	}
}

// 3. 不返回其他 document version（正常代码路径不会让两个 version 同时
// is_published=true，这里用原始 SQL 直接构造，专门压测 WHERE
// document_version = $2 这一个过滤条件本身）.
func TestIntegrationFindPublishedNeighborChunksExcludesOtherVersions(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-version-filter"
	// kbID is a plain text column on chunks (see pgmigrations 000001) with
	// no FK back to MySQL's knowledge_bases — it doesn't need a real row to
	// exist there, and setupPGOnlyIntegration's repo has no MySQL
	// connection to write one to anyway.
	kbID := "kb-nb-version-filter"
	insertRawPublishedChunk(t, repo, kbID, docID, "wrong-version", 1, 5, "wrong version content")
	insertRawPublishedChunk(t, repo, kbID, docID, "right-version", 2, 5, "right version content")

	got, err := repo.findPublishedNeighborChunks(ctx, docID, 2, []int{5})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	if len(got) != 1 || got[0].ID != "right-version" {
		t.Fatalf("got %v, want exactly [right-version] (document_version=1 row must be excluded)", ids(got))
	}
}

// 4. 不返回 is_published=false.
func TestIntegrationFindPublishedNeighborChunksExcludesUnpublished(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-unpub"
	seedNeighborChunkBatch(t, repo, "kb-nb-unpub", docID, 1, []neighborSeedChunk{
		{ID: "u-1", ChunkIndex: 1, Content: "unpublished neighbor", Vec: []float32{1, 0, 0}},
	}, false) // never published

	got, err := repo.findPublishedNeighborChunks(ctx, docID, 1, []int{1})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty (unpublished draft rows must never be returned)", ids(got))
	}
}

// 5. chunk 0 不产生负数查询问题.
func TestIntegrationFindPublishedNeighborChunksChunkZeroNoNegativeIndexQuery(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-zero"
	seedNeighborChunkBatch(t, repo, "kb-nb-zero", docID, 1, []neighborSeedChunk{
		{ID: "z-0", ChunkIndex: 0, Content: "anchor", Vec: []float32{1, 0, 0}},
		{ID: "z-1", ChunkIndex: 1, Content: "next", Vec: []float32{1, 0, 0}},
	}, true)

	idxs := neighborIndexesFor(0)
	if len(idxs) != 1 || idxs[0] != 1 {
		t.Fatalf("test setup invalid: neighborIndexesFor(0) = %v, want [1]", idxs)
	}
	got, err := repo.findPublishedNeighborChunks(ctx, docID, 1, idxs)
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks with no negative index in the array: %v", err)
	}
	if len(got) != 1 || got[0].ID != "z-1" {
		t.Fatalf("got %v, want exactly [z-1]", ids(got))
	}
}

// 6. 返回顺序按 chunk_index ASC, id ASC.
func TestIntegrationFindPublishedNeighborChunksOrderedByChunkIndexThenID(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-order"
	kbID := "kb-nb-order"
	seedNeighborChunkBatch(t, repo, kbID, docID, 1, []neighborSeedChunk{
		{ID: "f-idx3", ChunkIndex: 3, Content: "c3", Vec: []float32{1, 0, 0}},
		{ID: "f-idx1", ChunkIndex: 1, Content: "c1", Vec: []float32{1, 0, 0}},
		{ID: "f-idx2", ChunkIndex: 2, Content: "c2", Vec: []float32{1, 0, 0}},
	}, true)

	got, err := repo.findPublishedNeighborChunks(ctx, docID, 1, []int{1, 2, 3})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	want := []string{"f-idx1", "f-idx2", "f-idx3"}
	if !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("got %v, want %v (ordered by chunk_index ASC)", ids(got), want)
	}

	// id ASC 兜底：正常生产流程不会让同一个 (document_id, document_version)
	// 出现两行相同 chunk_index，这里用原始 SQL 构造这个防御性场景。
	insertRawPublishedChunk(t, repo, kbID, docID, "z-dup", 1, 9, "dup z")
	insertRawPublishedChunk(t, repo, kbID, docID, "a-dup", 1, 9, "dup a")
	dup, err := repo.findPublishedNeighborChunks(ctx, docID, 1, []int{9})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks (id tiebreak): %v", err)
	}
	wantDup := []string{"a-dup", "z-dup"}
	if !reflect.DeepEqual(ids(dup), wantDup) {
		t.Fatalf("id ASC tiebreak not applied: got %v, want %v", ids(dup), wantDup)
	}
}

// 7. document_name/page_number/section_title 保持真实值（邻接块自己的，
// 不是核心块的）.
func TestIntegrationFindPublishedNeighborChunksPreservesCitationMetadata(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-cite"
	page := 9
	section := "5.1 附则"
	seedNeighborChunkBatch(t, repo, "kb-nb-cite", docID, 1, []neighborSeedChunk{
		{ID: "cite-anchor", ChunkIndex: 2, Content: "anchor content", Vec: []float32{1, 0, 0}, DocumentName: "policy.pdf"},
		{ID: "cite-next", ChunkIndex: 3, Content: "neighbor content", Vec: []float32{1, 0, 0}, DocumentName: "policy.pdf", PageNumber: &page, SectionTitle: &section},
	}, true)

	got, err := repo.findPublishedNeighborChunks(ctx, docID, 1, []int{3})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %v", len(got), ids(got))
	}
	n := got[0]
	if n.DocumentName != "policy.pdf" {
		t.Fatalf("DocumentName = %q, want %q", n.DocumentName, "policy.pdf")
	}
	if n.PageNumber == nil || *n.PageNumber != page {
		t.Fatalf("PageNumber = %v, want %d", n.PageNumber, page)
	}
	if n.SectionTitle == nil || *n.SectionTitle != section {
		t.Fatalf("SectionTitle = %v, want %q", n.SectionTitle, section)
	}
	if n.DocumentVersion != 1 {
		t.Fatalf("DocumentVersion = %d, want 1", n.DocumentVersion)
	}
}

// 8. Vector Search 和 Keyword Search 都正确返回 document_version.
func TestIntegrationSearchVectorAndKeywordChunksReturnDocumentVersion(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-version-tag"
	kbID := "kb-nb-version-tag"
	seedNeighborChunkBatch(t, repo, kbID, docID, 7, []neighborSeedChunk{
		{ID: "vt-1", ChunkIndex: 0, Content: "版本标记验证关键词VERSIONTAG", Vec: []float32{1, 0, 0}},
	}, true)

	vecGot, err := repo.searchVectorChunks(ctx, []string{kbID}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vecGot) != 1 || vecGot[0].DocumentVersion != 7 {
		t.Fatalf("searchVectorChunks DocumentVersion = %+v, want DocumentVersion=7", vecGot)
	}

	kwGot, err := repo.searchKeywordChunks(ctx, []string{kbID}, "版本标记验证关键词VERSIONTAG", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(kwGot) != 1 || kwGot[0].DocumentVersion != 7 {
		t.Fatalf("searchKeywordChunks DocumentVersion = %+v, want DocumentVersion=7", kwGot)
	}
}

// 9. 模拟核心块属于旧版本、旧版本已被删除后，邻接查询返回空而不是混入新版本.
func TestIntegrationFindPublishedNeighborChunksOldVersionDeletedReturnsEmpty(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-nb-reprocessed"
	kbID := "kb-nb-reprocessed"
	seedNeighborChunkBatch(t, repo, kbID, docID, 1, []neighborSeedChunk{
		{ID: "v1-0", ChunkIndex: 0, Content: "v1 chunk0", Vec: []float32{1, 0, 0}},
		{ID: "v1-1", ChunkIndex: 1, Content: "v1 chunk1", Vec: []float32{1, 0, 0}},
		{ID: "v1-2", ChunkIndex: 2, Content: "v1 chunk2", Vec: []float32{1, 0, 0}},
	}, true)

	// sanity: 重新处理之前，version 1 的邻接块能查到.
	before, err := repo.findPublishedNeighborChunks(ctx, docID, 1, []int{2})
	if err != nil || len(before) != 1 || before[0].ID != "v1-2" {
		t.Fatalf("pre-reprocess sanity check failed: got %v, err %v", ids(before), err)
	}

	// 模拟重新处理：写入新版本并发布——publishDocumentVersion 在同一个 PG
	// 事务里把 docID 的其他所有版本删除（见 repository.go），和真实生产
	// 流程完全一致，不是伪造的测试专用状态。
	seedNeighborChunkBatch(t, repo, kbID, docID, 2, []neighborSeedChunk{
		{ID: "v2-0", ChunkIndex: 0, Content: "v2 chunk0", Vec: []float32{1, 0, 0}},
		{ID: "v2-1", ChunkIndex: 1, Content: "v2 chunk1", Vec: []float32{1, 0, 0}},
		{ID: "v2-2", ChunkIndex: 2, Content: "v2 chunk2", Vec: []float32{1, 0, 0}},
	}, true)

	// 一个在重新处理之前拿到的、携带 DocumentVersion=1 的旧核心块，此时再
	// 查邻接必须返回空——绝不能被 v2 的同 index chunk 顶替.
	stale, err := repo.findPublishedNeighborChunks(ctx, docID, 1, []int{2})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks(old version): %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("got %v, want empty — old version's rows must be gone after reprocessing, not silently answered by v2", ids(stale))
	}

	// 对照组：同一个 index 换成新版本能正常查到——证明上面的空结果是真实
	// 的版本隔离，不是查询本身写错了。
	fresh, err := repo.findPublishedNeighborChunks(ctx, docID, 2, []int{2})
	if err != nil || len(fresh) != 1 || fresh[0].ID != "v2-2" {
		t.Fatalf("findPublishedNeighborChunks(new version) = %v, err %v, want [v2-2]", ids(fresh), err)
	}
}

// 10. 邻接查询普通失败时 Service 返回核心块（best-effort 降级）.
func TestIntegrationExpandWithNeighborWindowDegradesToAnchorsOnOrdinaryFailure(t *testing.T) {
	// 一个真实的、直接拒绝连接的 *sql.DB——不是 mock，是 database/sql +
	// lib/pq 对一个没有监听者的端口发起的真实连接尝试，产生的是一个
	// 普通的驱动层错误，不是 context.Canceled/DeadlineExceeded。刻意不
	// 触碰 testutil 的共享缓存连接（那个连接被其他测试复用，关掉会连带
	// 弄坏它们）。
	brokenDB, err := sql.Open("postgres", "postgres://hify:hify_dev@127.0.0.1:1/hify_test_nonexistent?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open broken postgres handle: %v", err)
	}
	t.Cleanup(func() { brokenDB.Close() })
	brokenRepo := NewRepository(nil, brokenDB)

	// Phase 7: expandWithNeighborWindow now reaches the database through
	// svc.findNeighborBatch, not svc.repo directly — see the service
	// struct's findNeighborBatch doc comment in service.go. A hand-built
	// &service{} literal (as every test in this file already does, since
	// service has no exported constructor tests can use) must set this
	// field explicitly, exactly the way wire.go's NewService does for
	// production, or expandWithNeighborWindow would call a nil func value.
	svc := &service{repo: brokenRepo, providerSvc: newFakeProvider(), storageDir: t.TempDir(), findNeighborBatch: brokenRepo.findPublishedNeighborChunksBatch}
	anchors := []RetrievedChunk{
		anchorRC("a1", "doc-broken", 1, 5, 0.9),
		anchorRC("a2", "doc-broken", 1, 0, 0.7),
	}

	got, _, err := svc.expandWithNeighborWindow(context.Background(), anchors)
	if err != nil {
		t.Fatalf("expandWithNeighborWindow returned an error for an ordinary DB failure, want nil (best-effort degrade): %v", err)
	}
	if !reflect.DeepEqual(ids(got), ids(anchors)) {
		t.Fatalf("got %v, want anchors unchanged %v (neighbor lookup failure must degrade to anchors-only)", ids(got), ids(anchors))
	}
}

// 11. context cancellation 正确传播，不能被邻接扩展的 best-effort 降级吞掉.
func TestIntegrationExpandWithNeighborWindowPropagatesContextCancellation(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	// See the sibling ...DegradesToAnchorsOnOrdinaryFailure test above for
	// why findNeighborBatch must be set explicitly on a hand-built
	// &service{} literal since Phase 7.
	svc := &service{repo: repo, providerSvc: newFakeProvider(), storageDir: t.TempDir(), findNeighborBatch: repo.findPublishedNeighborChunksBatch}

	anchors := []RetrievedChunk{anchorRC("a1", "doc-nb-cancel", 1, 5, 0.9)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, _, err := svc.expandWithNeighborWindow(ctx, anchors)
	if err == nil {
		t.Fatal("expandWithNeighborWindow with a canceled context returned nil error, want context.Canceled to propagate")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil result alongside the propagated error", got)
	}
}

// --- Phase 7: 邻接窗口批量查询（Batch Neighbor Lookup）---
//
// These tests exercise FindPublishedNeighborChunksBatch (via
// Repository.findPublishedNeighborChunksBatch) directly against real
// Postgres — the single-query counterpart to the FindPublishedNeighborChunks
// tests above, which covered the same isolation/ordering guarantees for the
// old per-group query. Every guarantee that query had to hold — cross-
// document isolation, cross-version isolation, unpublished exclusion,
// stable ordering — must hold identically here, now for a request set that
// spans multiple documents and versions in one call.

// 1. 多文档、多版本混在同一次批量请求里，一次查询正确取回全部匹配行，且
// 不会把 A 文档的坐标错配到 B 文档（或同一文档的另一个版本）上。
func TestIntegrationFindPublishedNeighborChunksBatchAcrossMultipleDocumentsAndVersions(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	seedNeighborChunkBatch(t, repo, "kb-batch-multi", "doc-batch-a", 1, []neighborSeedChunk{
		{ID: "a-4", ChunkIndex: 4, Content: "a4", Vec: []float32{1, 0, 0}},
		{ID: "a-5", ChunkIndex: 5, Content: "a5-anchor", Vec: []float32{1, 0, 0}},
		{ID: "a-6", ChunkIndex: 6, Content: "a6", Vec: []float32{1, 0, 0}},
	}, true)
	seedNeighborChunkBatch(t, repo, "kb-batch-multi", "doc-batch-b", 2, []neighborSeedChunk{
		{ID: "b-0", ChunkIndex: 0, Content: "b0-anchor", Vec: []float32{1, 0, 0}},
		{ID: "b-1", ChunkIndex: 1, Content: "b1", Vec: []float32{1, 0, 0}},
	}, true)
	// doc-batch-a 也存在一个未发布的旧版本 2，chunk_index 恰好和 v1 的
	// 4/6 相同——如果批量查询按 document_id 而不是 (document_id,
	// document_version) 匹配，这两行会被错误地一起带回来。
	seedNeighborChunkBatch(t, repo, "kb-batch-multi", "doc-batch-a", 2, []neighborSeedChunk{
		{ID: "a-v2-4", ChunkIndex: 4, Content: "a-v2-4 must never appear", Vec: []float32{1, 0, 0}},
	}, false)

	requests := []neighborRequest{
		{documentID: "doc-batch-a", documentVersion: 1, chunkIndex: 4},
		{documentID: "doc-batch-a", documentVersion: 1, chunkIndex: 6},
		{documentID: "doc-batch-b", documentVersion: 2, chunkIndex: 1},
	}
	got, err := repo.findPublishedNeighborChunksBatch(ctx, requests)
	if err != nil {
		t.Fatalf("findPublishedNeighborChunksBatch: %v", err)
	}
	want := []string{"a-4", "a-6", "b-1"}
	if !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("got %v, want %v — one batch call across 2 documents/versions must return exactly the requested rows, correctly isolated", ids(got), want)
	}
}

// 2. 跨版本隔离：同一份请求里显式问了旧版本坐标，绝不能被新版本的同 index
// 顶替（和 TestIntegrationFindPublishedNeighborChunksOldVersionDeletedReturnsEmpty
// 验证的是同一条生产规则，这里换成批量查询路径）。
func TestIntegrationFindPublishedNeighborChunksBatchIsolatesAcrossVersions(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	docID := "doc-batch-version-isolation"
	kbID := "kb-batch-version-isolation"
	seedNeighborChunkBatch(t, repo, kbID, docID, 1, []neighborSeedChunk{
		{ID: "batch-vi-v1-2", ChunkIndex: 2, Content: "v1 chunk2", Vec: []float32{1, 0, 0}},
	}, true)
	// 重新处理：发布 version 2 会在同一个 PG 事务里物理删除 version 1 的行
	// （publishDocumentVersion/DeleteObsoleteChunkVersions）。
	seedNeighborChunkBatch(t, repo, kbID, docID, 2, []neighborSeedChunk{
		{ID: "batch-vi-v2-2", ChunkIndex: 2, Content: "v2 chunk2", Vec: []float32{1, 0, 0}},
	}, true)

	// 请求里显式问的是已经被删除的 version 1 坐标——必须返回空，绝不能被
	// v2 的同 index 行顶替。
	stale, err := repo.findPublishedNeighborChunksBatch(ctx, []neighborRequest{
		{documentID: docID, documentVersion: 1, chunkIndex: 2},
	})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunksBatch(stale version): %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("got %v, want empty — a batch request for a reprocessed-away version must not be silently answered by the new version", ids(stale))
	}

	fresh, err := repo.findPublishedNeighborChunksBatch(ctx, []neighborRequest{
		{documentID: docID, documentVersion: 2, chunkIndex: 2},
	})
	if err != nil || len(fresh) != 1 || fresh[0].ID != "batch-vi-v2-2" {
		t.Fatalf("findPublishedNeighborChunksBatch(current version) = %v, err %v, want [batch-vi-v2-2]", ids(fresh), err)
	}
}

// 3. 未发布的草稿版本绝不能出现在批量查询结果里，即使请求坐标精确匹配。
func TestIntegrationFindPublishedNeighborChunksBatchExcludesUnpublished(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	seedNeighborChunkBatch(t, repo, "kb-batch-unpub", "doc-batch-unpub", 1, []neighborSeedChunk{
		{ID: "up-0", ChunkIndex: 0, Content: "draft chunk", Vec: []float32{1, 0, 0}},
	}, false) // 故意不发布

	got, err := repo.findPublishedNeighborChunksBatch(ctx, []neighborRequest{
		{documentID: "doc-batch-unpub", documentVersion: 1, chunkIndex: 0},
	})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunksBatch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty — an unpublished draft chunk must never be returned even when its exact coordinate is requested", ids(got))
	}
}

// 4. 重复/乱序请求：Codex 第一轮 Phase 7 审核发现，去重前的 SQL 对重复请求
// 坐标返回重复结果行（`[dup-0, dup-1, dup-1]`）——这不满足任务"批量请求包含
// 重复/乱序坐标时结果必须正确且确定"的验收要求：确定地返回重复行不是正确
// 结果，还会无谓放大 DB 返回行数，并把正确性完全押在调用方（buildNeighborRequests）
// 永远提前去重这一个假设上。修复后 requested CTE 加了 DISTINCT（见
// chunks.sql 的 doc 注释），这条测试验证的就是修复后的行为：即使传入的请
// 求乱序、且同一个坐标出现两次，最终结果里每个匹配的 chunk 也只出现一次，
// 顺带验证乱序请求不影响正确性（ORDER BY 是按结果排序，不是按请求顺序）。
// buildNeighborRequests 的 Go 层提前去重仍然保留（避免正常路径发送冗余坐
// 标），这条 SQL 层去重是独立的边界防御，不是替代它。
func TestIntegrationFindPublishedNeighborChunksBatchDuplicateAndOutOfOrderRequestsAreDeterministic(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	ctx := context.Background()

	seedNeighborChunkBatch(t, repo, "kb-batch-dup", "doc-batch-dup", 1, []neighborSeedChunk{
		{ID: "dup-0", ChunkIndex: 0, Content: "c0", Vec: []float32{1, 0, 0}},
		{ID: "dup-1", ChunkIndex: 1, Content: "c1", Vec: []float32{1, 0, 0}},
	}, true)

	// 乱序 + chunk_index=1 的坐标重复了两次.
	requests := []neighborRequest{
		{documentID: "doc-batch-dup", documentVersion: 1, chunkIndex: 1},
		{documentID: "doc-batch-dup", documentVersion: 1, chunkIndex: 0},
		{documentID: "doc-batch-dup", documentVersion: 1, chunkIndex: 1},
	}
	got, err := repo.findPublishedNeighborChunksBatch(ctx, requests)
	if err != nil {
		t.Fatalf("findPublishedNeighborChunksBatch: %v", err)
	}
	want := []string{"dup-0", "dup-1"} // ORDER BY chunk_index ASC — a duplicated request coordinate must NOT produce a duplicated result row; the requested CTE's DISTINCT collapses it to one
	if !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("got %v, want %v — a duplicated request coordinate must resolve to exactly one result row per matching chunk, and result order must be deterministic regardless of request order", ids(got), want)
	}
}

// 5. 空请求集合必须在 Go 层短路，绝不发起数据库查询——用一个真实拒绝连接
// 的 *sql.DB 证明这一点：如果 findPublishedNeighborChunksBatch 对空请求
// 仍然尝试执行 SQL，这个测试会因为连接失败而报错；它没有报错，就是空请求
// 从未触达数据库的证据。
func TestIntegrationFindPublishedNeighborChunksBatchEmptyRequestsNeverQueriesDatabase(t *testing.T) {
	brokenDB, err := sql.Open("postgres", "postgres://hify:hify_dev@127.0.0.1:1/hify_test_nonexistent?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("open broken postgres handle: %v", err)
	}
	t.Cleanup(func() { brokenDB.Close() })
	brokenRepo := NewRepository(nil, brokenDB)

	got, err := brokenRepo.findPublishedNeighborChunksBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("findPublishedNeighborChunksBatch(empty) against a broken DB returned an error, want nil — an empty request set must short-circuit before ever reaching the database: %v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// 6. 端到端：expandWithNeighborWindow 通过真实 Repository 驱动，多文档多
// 版本核心命中，一次批量查询后组装出的最终结果，必须和 Phase 4/5 逐组循环
// 查询时期望的结果完全一致——批量化是一次查询方式的重写，不能改变
// expandWithNeighbors 组装出的最终排序/去重结果。
func TestIntegrationExpandWithNeighborWindowProducesCorrectResultAgainstRealPostgres(t *testing.T) {
	repo := setupPGOnlyIntegration(t)
	svc := &service{repo: repo, providerSvc: newFakeProvider(), storageDir: t.TempDir(), findNeighborBatch: repo.findPublishedNeighborChunksBatch}

	seedNeighborChunkBatch(t, repo, "kb-batch-e2e", "doc-batch-e2e-1", 1, []neighborSeedChunk{
		{ID: "e2e-1-4", ChunkIndex: 4, Content: "e2e-1-4", Vec: []float32{1, 0, 0}},
		{ID: "e2e-1-5", ChunkIndex: 5, Content: "e2e-1-5-anchor", Vec: []float32{1, 0, 0}},
		{ID: "e2e-1-6", ChunkIndex: 6, Content: "e2e-1-6", Vec: []float32{1, 0, 0}},
	}, true)
	seedNeighborChunkBatch(t, repo, "kb-batch-e2e", "doc-batch-e2e-2", 1, []neighborSeedChunk{
		{ID: "e2e-2-0", ChunkIndex: 0, Content: "e2e-2-0-anchor", Vec: []float32{1, 0, 0}},
		{ID: "e2e-2-1", ChunkIndex: 1, Content: "e2e-2-1", Vec: []float32{1, 0, 0}},
	}, true)

	anchors := []RetrievedChunk{
		anchorRC("e2e-1-5-anchor-id", "doc-batch-e2e-1", 1, 5, 0.9),
		anchorRC("e2e-2-0-anchor-id", "doc-batch-e2e-2", 1, 0, 0.8),
	}
	// anchorRC 构造的 ID 和种子数据里真实 anchor 行的 ID 不需要一致——
	// expandWithNeighborWindow/expandWithNeighbors 只关心 anchor 的
	// DocumentID/DocumentVersion/ChunkIndex 用来算邻接坐标，Content 用来
	// 去重；这里的两个 anchor 各自的邻接块（previous/next）来自真实种子
	// 数据，用于验证批量查询确实按坐标取回了正确的行。
	got, dupCount, err := svc.expandWithNeighborWindow(context.Background(), anchors)
	if err != nil {
		t.Fatalf("expandWithNeighborWindow: %v", err)
	}
	if dupCount != 0 {
		t.Fatalf("neighborDuplicateCount = %d, want 0 (no shared content in this fixture)", dupCount)
	}
	// Tier 1: 两个 anchor，rank 不变；Tier 2: anchor1 的 previous/next，
	// 再是 anchor2 的 next（anchor2 在 index 0，没有 previous）。
	want := []string{"e2e-1-5-anchor-id", "e2e-2-0-anchor-id", "e2e-1-4", "e2e-1-6", "e2e-2-1"}
	if !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("got %v, want %v", ids(got), want)
	}
}

// --- Phase 8: Evidence Admission, exercised end to end through the real
// public Service.Retrieve entry point (setupIntegration — needs both MySQL
// and Postgres, same requirement every other setupIntegration test in this
// file already has). See docs/superpowers/specs/2026-08-08-rag-evidence-admission-design.md
// for the admission rule this section verifies against real pgvector
// cosine similarity and real pg_trgm word-similarity, not synthetic
// scores — admission_test.go and hybrid_test.go already cover the pure
// logic exhaustively; these tests exist to prove the same rule holds when
// wired through the real SQL scoring these thresholds were calibrated
// against.

// cosineVec returns a unit vector [c, sqrt(1-c*c), 0]. Every query
// embedding in this file's fake provider is the fixed unit vector
// [1,0,0], so pgvector's cosine similarity between this vector and the
// query is EXACTLY c (dot product of two unit vectors) — this is what
// lets these tests hit vectorAdmissionThreshold's 0.35 boundary precisely
// instead of approximately.
func cosineVec(c float64) []float32 {
	s := math.Sqrt(1 - c*c)
	return []float32{float32(c), float32(s), 0}
}

// seedVectorOnlyFillers seeds n throwaway chunks in the given KB/document
// prefix whose cosine score against the query descends from just under
// highCos, none of which share any keyword overlap with the tokens this
// file's Phase 8 tests search for (plain unrelated Chinese filler text,
// calibrated against real pg_trgm to word_similarity==0 — see this
// section's doc comment). Their only purpose is to occupy candidateK's
// LIMIT window ahead of a deliberately weak target chunk, so that target
// chunk falls out of searchVectorChunks' result set entirely rather than
// merely ranking low within it — see the tests that use this for exactly
// which scenario needs that.
func seedVectorOnlyFillers(t *testing.T, repo *Repository, kbID, docPrefix string, n int, highCos float64) {
	t.Helper()
	for i := 0; i < n; i++ {
		cos := highCos - float64(i)*0.01
		seedChunkWithContent(t, repo, kbID, fmt.Sprintf("%s-%d", docPrefix, i), fmt.Sprintf("%s-filler-%d", docPrefix, i),
			cosineVec(cos), fmt.Sprintf("内容与关键词完全无关的填充文本编号%d", i))
	}
}

// 1. 非空知识库 + 正交向量 + 无关键词命中，Service.Retrieve 返回空.
func TestIntegrationRetrieveAdmissionReturnsEmptyForIrrelevantQueryAgainstNonEmptyKB(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-admission-irrelevant", "m3", "u1", true)
	// Orthogonal to the query's fixed [1,0,0] embedding (cos=0) and, per
	// this section's doc comment, calibrated to zero pg_trgm overlap with
	// the query text too — genuinely no evidence on either path.
	seedChunkWithContent(t, repo, "kb-admission-irrelevant", "doc-irrelevant", "irrelevant-1",
		[]float32{0, 1, 0}, "内容与关键词完全无关的填充文本零一")

	got, err := svc.Retrieve(ctx, []string{"kb-admission-irrelevant"}, "BACKFILLTOKENXYZ", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty — a non-empty KB with zero qualifying evidence on either path must return empty, not the best-of-a-bad-lot candidate", ids(got))
	}
}

// 2. vector 0.35 边界通过、低于边界拒绝（真实 pgvector 余弦相似度）.
func TestIntegrationRetrieveAdmissionVectorThresholdBoundaryAgainstRealPgvector(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-admission-vec-boundary", "m3", "u1", true)
	seedChunkWithContent(t, repo, "kb-admission-vec-boundary", "doc-below", "vec-below",
		cosineVec(0.34), "内容与关键词完全无关的填充文本零二")
	// 0.36, not exactly 0.35: float32 storage (pgvector's column type) and
	// real SQL cosine computation round-trip a float64-computed 0.35
	// target to a value that can land a hair under 0.35 — a flaky false
	// rejection unrelated to what this test wants to prove. Exact
	// float64-precision equality AT the threshold is already covered
	// deterministically in admission_test.go's pure-logic tests
	// (TestAdmitBySourceSignalVectorThreshold's "equal" case); this test's
	// job is proving the real pgvector-computed score crosses the
	// threshold correctly, which 0.36 (clearly above, clearly a real
	// pgvector value) already does.
	seedChunkWithContent(t, repo, "kb-admission-vec-boundary", "doc-above", "vec-above",
		cosineVec(0.36), "内容与关键词完全无关的填充文本零三")

	got, err := svc.Retrieve(ctx, []string{"kb-admission-vec-boundary"}, "BACKFILLTOKENXYZ", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	want := []string{"vec-above"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v — cos=0.34 must be rejected, cos=0.36 (above the threshold) must be admitted", gotIDs, want)
	}
}

// 3. keyword 0.45 边界通过、低于边界拒绝（真实 pg_trgm word_similarity）.
//
// "abcdefghij" vs "xx abcd yy" / "xx abcde yy" are calibrated (see this
// phase's report) to real word_similarity scores of ~0.3636 (below 0.45)
// and ~0.4545 (above 0.45) against this repo's actual pg_trgm extension —
// not a hand-picked "should be close enough" pair.
func TestIntegrationRetrieveAdmissionKeywordThresholdBoundaryAgainstRealPgTrgm(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-admission-kw-boundary", "m3", "u1", true)
	// Orthogonal vector embedding on both — the vector path must not be
	// able to rescue either candidate, isolating this test to the keyword
	// threshold alone.
	seedChunkWithContent(t, repo, "kb-admission-kw-boundary", "doc-kw-below", "kw-below",
		[]float32{0, 1, 0}, "xx abcd yy")
	seedChunkWithContent(t, repo, "kb-admission-kw-boundary", "doc-kw-above", "kw-above",
		[]float32{0, 1, 0}, "xx abcde yy")

	got, err := svc.Retrieve(ctx, []string{"kb-admission-kw-boundary"}, "abcdefghij", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	want := []string{"kw-above"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v — word_similarity below 0.45 must be rejected, above 0.45 must be admitted", gotIDs, want)
	}
}

// 4. 两路都弱时拒绝，任一路强时通过.
func TestIntegrationRetrieveAdmissionEitherStrongPathAdmitsBothWeakRejects(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-admission-either-path", "m3", "u1", true)
	// Weak on both paths: low cosine, and content with no meaningful
	// overlap with the query text.
	seedChunkWithContent(t, repo, "kb-admission-either-path", "doc-weak-both", "weak-both",
		cosineVec(0.10), "完全不相关的向量填充内容，用于弱信号双路拒绝场景")
	// Strong vector, no keyword overlap.
	seedChunkWithContent(t, repo, "kb-admission-either-path", "doc-strong-vec", "strong-vec",
		cosineVec(0.90), "内容与关键词完全无关的填充文本零四")
	// Orthogonal vector, strong keyword overlap.
	seedChunkWithContent(t, repo, "kb-admission-either-path", "doc-strong-kw", "strong-kw",
		[]float32{0, 1, 0}, "zz STRONGKWTOKEN yy pure keyword strong hit content")

	got, err := svc.Retrieve(ctx, []string{"kb-admission-either-path"}, "STRONGKWTOKEN", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	gotIDs := ids(got)
	wantPresent := map[string]bool{"strong-vec": true, "strong-kw": true}
	if len(gotIDs) != 2 {
		t.Fatalf("got %v, want exactly 2 results (weak-both rejected, strong-vec and strong-kw admitted)", gotIDs)
	}
	for _, id := range gotIDs {
		if !wantPresent[id] {
			t.Fatalf("got %v, want only strong-vec and strong-kw — weak-both must be rejected", gotIDs)
		}
	}
}

// 5. topK 前部拒绝项被删除，后续合格候选补位.
//
// "rej-rank1" is the single highest-fusionScore candidate (vector rank 1,
// weight-heavy vector path) but its raw cosine (0.30) is below
// vectorAdmissionThreshold, so it must be rejected outright. It has no
// keyword overlap. "kw-1st"/"kw-2nd" are pure keyword hits (real
// word_similarity 1.0 / 0.8235, both well above keywordAdmissionThreshold)
// pushed out of the vector candidateK window by seedVectorOnlyFillers so
// their own (near-zero) cosine never contributes to their fusionScore —
// isolating this test to "does an admitted-but-lower-ranked candidate
// backfill the topK slot a higher-ranked rejected candidate would have
// wasted", not any other interaction.
func TestIntegrationRetrieveAdmissionRejectedTopCandidateLetsLowerRankedAdmittedCandidateBackfillTopK(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	const kb = "kb-admission-backfill"
	seedKB(t, repo, kb, "m3", "u1", true)

	seedChunkWithContent(t, repo, kb, "doc-rej-rank1", "rej-rank1",
		cosineVec(0.30), "内容与关键词完全无关的填充文本零五")
	// 8 fillers with cosine strictly between rej-rank1's 0.30 and kw-1st/
	// kw-2nd's near-zero cosine — candidateK(topK=2) = 8, so these fill the
	// entire vector LIMIT window, pushing kw-1st/kw-2nd's own weak cosine
	// completely out of searchVectorChunks' results.
	seedVectorOnlyFillers(t, repo, kb, "doc-filler", 8, 0.29)
	seedChunkWithContent(t, repo, kb, "doc-kw-1st", "kw-1st",
		cosineVec(0.01), "zz DUPTOKENQWERTY yy strong keyword match for admitted duplicate")
	// word_similarity("DUPTOKENQWERTY", this) == 0.8 in this repo's real
	// pg_trgm — a real second-place keyword rank, still comfortably above
	// keywordAdmissionThreshold (0.45).
	seedChunkWithContent(t, repo, kb, "doc-kw-2nd", "kw-2nd",
		cosineVec(0.01), "zz DUPTOKENQWER yy second ranked admitted duplicate content padding")

	got, err := svc.Retrieve(ctx, []string{kb}, "DUPTOKENQWERTY", 2, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	want := []string{"kw-1st", "kw-2nd"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v — rej-rank1 (highest fusionScore, but below vectorAdmissionThreshold) must be dropped before topK, letting kw-2nd (originally ranked below topK=2) backfill", gotIDs, want)
	}
}

// 6. 高排名不合格重复项不能让低排名合格重复项丢失.
//
// Note on what's realistically constructible against a REAL database vs.
// what needs the pure-logic tests: hybrid_test.go/admission_test.go
// (TestRRFFuseAdmissionBeforeDedupKeepsAdmittedDuplicateOverRejectedHigherRank)
// already proves, with directly-injected synthetic ranks/scores, that an
// unqualified higher-fusionScore duplicate never causes dedup to discard a
// qualified lower-fusionScore duplicate of the SAME content. That specific
// "same content, rank inverted relative to admission outcome" combination
// is mathematically impossible to reproduce through the REAL pipeline: two
// rows with identical `content` always get an IDENTICAL real
// word_similarity score (keyword admission ties for both), so the only
// remaining differentiator is each row's own vector embedding — but
// vector-path fusion rank is itself monotonic in real cosine score, so a
// row with a real cosine high enough to be independently vector-admitted
// can never rank BELOW a same-content row whose cosine is weaker (real
// sorting keeps the higher score ahead, never behind, within the same
// globally-sorted, LIMIT-bounded candidate list). This test instead
// verifies the realistic, DB-achievable form of the same underlying
// guarantee: a content-UNRELATED rejected candidate ranked ahead of a
// content-duplicated ADMITTED pair must not interfere with that pair's
// ordinary Phase 5 dedup resolution — the final result is neither the
// rejected candidate nor both halves of the duplicate, only the
// higher-keyword-ranked surviving half.
func TestIntegrationRetrieveAdmissionRejectedCandidateDoesNotInterfereWithDuplicateResolutionAmongAdmittedSurvivors(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	const kb = "kb-admission-dedup-order"
	seedKB(t, repo, kb, "m3", "u1", true)

	seedChunkWithContent(t, repo, kb, "doc-rej-unrelated", "rej-unrelated",
		cosineVec(0.30), "内容与关键词完全无关的填充文本一一")
	seedVectorOnlyFillers(t, repo, kb, "doc-dd-filler", 8, 0.29)
	dupContent := "zz DUPTOKENQWERTY yy 重复正文用于验证准入去重顺序"
	seedChunkWithContent(t, repo, kb, "doc-dup-a", "dup-a", cosineVec(0.01), dupContent) // keyword rank 1 (score 1.0)
	seedChunkWithContent(t, repo, kb, "doc-dup-b", "dup-b", cosineVec(0.01), dupContent) // same content, tied keyword score

	got, err := svc.Retrieve(ctx, []string{kb}, "DUPTOKENQWERTY", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %v, want exactly 1 result — rej-unrelated must be rejected by admission, and dup-a/dup-b's shared content must collapse to exactly one survivor", ids(got))
	}
	if got[0].ID != "dup-a" && got[0].ID != "dup-b" {
		t.Fatalf("got %v, want the surviving half of the dup-a/dup-b pair, not rej-unrelated", ids(got))
	}
	for _, c := range got {
		if c.ID == "rej-unrelated" {
			t.Fatalf("got %v — rej-unrelated must have been rejected by admission, never reaching the final result", ids(got))
		}
	}
}

// 7. 被拒绝候选不触发邻接查询.
//
// rejected-core's own document has a neighbor chunk carrying a distinctive
// "poison probe" content string. If the final Retrieve() result ever
// contained that probe (by ID or by its DocumentID), it would prove a
// rejected candidate's coordinates leaked into the neighbor batch request
// — buildNeighborRequests/expandWithNeighborWindow only ever look at
// anchors (post-admission), so this must never happen.
func TestIntegrationRetrieveAdmissionRejectedCandidateNeverTriggersNeighborLookup(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	const kb = "kb-admission-no-neighbor-probe"
	seedKB(t, repo, kb, "m3", "u1", true)

	seedNeighborChunkBatch(t, repo, kb, "doc-rejected-core", 1, []neighborSeedChunk{
		{ID: "rejected-core", ChunkIndex: 5, Content: "内容与关键词完全无关的填充文本零六", Vec: cosineVec(0.10)},
		{ID: "poison-probe", ChunkIndex: 6, Content: "污染探针：不应该出现在结果里（Phase 8 admission）", Vec: cosineVec(0.10)},
	}, true)
	// A genuinely admitted anchor in a separate document, so Retrieve
	// returns something and this isn't just re-testing scenario 1's
	// "everything empty" path.
	seedChunkWithContent(t, repo, kb, "doc-admitted-anchor", "admitted-anchor",
		cosineVec(0.90), "内容与关键词完全无关的填充文本零七")

	got, err := svc.Retrieve(ctx, []string{kb}, "BACKFILLTOKENXYZ", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, c := range got {
		if c.ID == "poison-probe" || c.DocumentID == "doc-rejected-core" {
			t.Fatalf("got %v — a rejected candidate's document must never be looked up for neighbors, but the poison probe leaked into the result", ids(got))
		}
	}
	want := []string{"admitted-anchor"}
	if gotIDs := ids(got); !reflect.DeepEqual(gotIDs, want) {
		t.Fatalf("got %v, want %v", gotIDs, want)
	}
}

// 8. 跨知识库、跨 embedding 模型组合仍正确.
//
// kb-a uses model m3 (3-dim), kb-b uses model m2 (2-dim) — two entirely
// separate per-model vector search groups (service.go's kbsByModel), each
// embedding the same query text independently, merged by
// sortVectorCandidatesByScoreThenID before ever reaching rrfFuse. Each KB
// contributes one admitted and one rejected candidate; admission must act
// identically regardless of which model/KB a candidate came from.
func TestIntegrationRetrieveAdmissionCorrectAcrossKnowledgeBasesAndEmbeddingModels(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestService(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-cross-a", "m3", "u1", true)
	seedKB(t, repo, "kb-cross-b", "m2", "u1", true)

	// kb-a (3-dim model m3): one admitted, one rejected.
	seedChunkWithContent(t, repo, "kb-cross-a", "doc-cross-a-admitted", "cross-a-admitted",
		cosineVec(0.80), "zz CROSSKBTOKEN yy cross kb keyword admitted content")
	seedChunkWithContent(t, repo, "kb-cross-a", "doc-cross-a-rejected", "cross-a-rejected",
		cosineVec(0.20), "内容与关键词完全无关的填充文本零八")

	// kb-b (2-dim model m2): fakeProvider's query embedding for any 2-dim
	// model is also the fixed unit vector [1,0], so the same cosine-vector
	// trick applies with a 2-component vector.
	seedChunkWithContent(t, repo, "kb-cross-b", "doc-cross-b-admitted", "cross-b-admitted",
		[]float32{0.9, float32(math.Sqrt(1 - 0.9*0.9))}, "内容与关键词完全无关的填充文本零九")
	seedChunkWithContent(t, repo, "kb-cross-b", "doc-cross-b-rejected", "cross-b-rejected",
		[]float32{0.2, float32(math.Sqrt(1 - 0.2*0.2))}, "内容与关键词完全无关的填充文本一零")

	got, err := svc.Retrieve(ctx, []string{"kb-cross-a", "kb-cross-b"}, "CROSSKBTOKEN", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	gotIDs := ids(got)
	wantPresent := map[string]bool{"cross-a-admitted": true, "cross-b-admitted": true}
	if len(gotIDs) != 2 {
		t.Fatalf("got %v, want exactly 2 (cross-a-admitted, cross-b-admitted) — both rejected candidates must be excluded regardless of which KB/model they came from", gotIDs)
	}
	for _, id := range gotIDs {
		if !wantPresent[id] {
			t.Fatalf("got %v, want only cross-a-admitted and cross-b-admitted", gotIDs)
		}
	}
}

// --- 001-rag-query-rerank US2：T031，真实 PostgreSQL + fake provider ---

// newTestServiceWithRerank 是 newTestService 的重排变体：真正走
// NewService（rerankEnabled=true、非空 rerankModelID），再把
// rerankScoreFn 换成调用方给定的固定打分函数——和 neighbor_batch_test.go
// 的 &service{findNeighborBatch: spy} 是同一个思路（service.go 的
// rerankScoreFn 文档注释），只是这里外层套了真实 Repository/Postgres，
// 断言的是"重排结果如何影响 Retrieve 对真实数据库发出的查询"，不是纯函数
// 本身（applyRerank 的纯函数覆盖见 rerank_test.go）。
func newTestServiceWithRerank(repo *Repository, fp *fakeProviderService, storageDir string, scoreFn func(ctx context.Context, query string, documents []string) (provider.RerankResult, error)) *service {
	svc := NewService(repo, fp, nil, storageDir, true, "rerank-model", 1500*time.Millisecond, false).(*service)
	svc.rerankScoreFn = scoreFn
	return svc
}

// T031：融合排名第 6 位的候选被重排到第 1 位、进入 topK=3 的结果；且邻接
// 批量查询只为"重排之后真正进入 topK 的核心块"发生——重排淘汰掉的候选
// （原本融合排名前 3，重排后掉出 topK）绝不能触发它自己文档的邻接查询
// （FR-012：不能为被重排淘汰的候选白付一次数据库查询）。
func TestIntegrationRetrieveRerankPromotesLowRankedCandidateIntoTopKAndSkipsNeighborLookupForDemoted(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	ctx := context.Background()

	const kb = "kb-rerank-promote"
	seedKB(t, repo, kb, "m3", "u1", true)

	// c1..c6：向量相似度严格递减，且全部清过 vectorAdmissionThreshold
	// (0.35)。内容彼此无关键词重叠（既有的"内容与关键词完全无关的填充文
	// 本零X"套路），让融合排序完全由向量路径决定——这样"融合排名第几位"
	// 就是可预测、可断言的。
	seedChunkWithContent(t, repo, kb, "doc-c1", "c1", cosineVec(0.90), "内容与关键词完全无关的填充文本零一")
	seedChunkWithContent(t, repo, kb, "doc-c2", "c2", cosineVec(0.85), "内容与关键词完全无关的填充文本零二")
	// c3 融合排名第 3（进 pre-rerank 的 topK=3），重排后必须被挤出去——它的
	// 文档里放一个"毒探针"邻居块：如果 Retrieve 在重排之后仍然为 c3 查邻
	// 接，这个探针就会泄漏进最终结果。
	seedNeighborChunkBatch(t, repo, kb, "doc-c3", 1, []neighborSeedChunk{
		{ID: "c3", ChunkIndex: 5, Content: "内容与关键词完全无关的填充文本零三", Vec: cosineVec(0.80)},
		{ID: "c3-poison-neighbor", ChunkIndex: 6, Content: "毒探针：c3 被重排淘汰后不该为它查邻接", Vec: cosineVec(0.80)},
	}, true)
	seedChunkWithContent(t, repo, kb, "doc-c4", "c4", cosineVec(0.75), "内容与关键词完全无关的填充文本零四")
	seedChunkWithContent(t, repo, kb, "doc-c5", "c5", cosineVec(0.70), "内容与关键词完全无关的填充文本零五")
	// c6 融合排名第 6（pre-rerank 会被 topK=3 截掉），重排后必须被提到第
	// 1 位、进入最终结果——它的文档里放一个真实邻居，断言重排"救回来"的
	// 核心块确实触发了邻接查询（不是"反正从来没查过邻接"这种假阳性）。
	seedNeighborChunkBatch(t, repo, kb, "doc-c6", 1, []neighborSeedChunk{
		{ID: "c6", ChunkIndex: 5, Content: "内容与关键词完全无关的填充文本零六", Vec: cosineVec(0.65)},
		{ID: "c6-real-neighbor", ChunkIndex: 6, Content: "c6 的真实邻居：重排把 c6 救进 topK 后必须能查到它", Vec: cosineVec(0.65)},
	}, true)

	// scoreFn 只认内容里的中文数字标记，不关心 index 顺序本身——c6 给最高
	// 分，其余原样递减，验证的是"分数决定顺序"而不是巧合。
	scoreFn := func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
		scoresByMarker := map[string]float64{
			"零一": 0.10, "零二": 0.09, "零三": 0.08,
			"零四": 0.07, "零五": 0.06, "零六": 0.99,
		}
		out := make([]provider.RerankScore, len(documents))
		for i, doc := range documents {
			var score float64
			for marker, s := range scoresByMarker {
				if strings.Contains(doc, marker) {
					score = s
					break
				}
			}
			out[i] = provider.RerankScore{Index: i, Score: score}
		}
		return provider.RerankResult{Scores: out}, nil
	}
	svc := newTestServiceWithRerank(repo, fp, t.TempDir(), scoreFn)

	got, err := svc.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	// 核心断言 1：c6 从融合排名第 6 位被重排到第 1 位，进入 topK=3 的结果。
	wantAnchors := []string{"c6", "c1", "c2"}
	var gotAnchors []string
	for _, c := range got {
		if c.NeighborOf == "" {
			gotAnchors = append(gotAnchors, c.ID)
		}
	}
	if !reflect.DeepEqual(gotAnchors, wantAnchors) {
		t.Fatalf("anchors = %v, want %v (c6 promoted to rank 1 by rerank, c3/c4/c5 demoted out of topK=3)", gotAnchors, wantAnchors)
	}

	// 核心断言 2：重排后真正进入 topK 的 c6 触发了邻接查询，它的真实邻居
	// 出现在结果里。
	foundRealNeighbor := false
	for _, c := range got {
		if c.ID == "c6-real-neighbor" {
			foundRealNeighbor = true
			if c.NeighborOf != "c6" {
				t.Fatalf("c6-real-neighbor.NeighborOf = %q, want c6", c.NeighborOf)
			}
		}
		// 核心断言 3：重排淘汰掉的 c3 绝不能触发它自己的邻接查询——毒探针
		// 绝不能出现在结果里。
		if c.ID == "c3-poison-neighbor" {
			t.Fatalf("c3-poison-neighbor leaked into the result — c3 was demoted out of topK by rerank and must never have had its neighbors looked up (FR-012)")
		}
	}
	if !foundRealNeighbor {
		t.Fatalf("got %v, want c6-real-neighbor present (c6 was promoted into topK by rerank, its neighbor lookup must have run)", ids(got))
	}
}

// --- 001-rag-query-rerank US3：T033，rerank 降级——三种触发条件都必须让最终
// 结果与"整个关闭重排"逐字一致（FR-011：整体丢弃，禁止部分采用）。三个用例
// 共用同一套种子数据（三个候选、向量相似度严格递减、内容互不重叠），跑法都
// 一样：先用 rerankEnabled=false 的 baseline service 跑一次拿到 want，再用
// rerankEnabled=true 但会触发某种降级路径的 service 跑一次拿到 got，
// reflect.DeepEqual 逐字段比较整个 []RetrievedChunk（不只是 ID 序列）——真的
// 跑两次比对，而不是只断言"没报错"。

// seedRerankDegradeFixture 的种子块 ID 按 kb 名字加前缀——三个 T033 用例共
// 用同一个（每个测试包缓存一份、跨用例共享的）hify_test_knowledge 库，块 ID
// 是全局主键，重名会撞 chunks_pkey，所以不能像 T031 那样三处都写死
// "d1/d2/d3"。
func seedRerankDegradeFixture(t *testing.T, repo *Repository, kb string) {
	t.Helper()
	seedKB(t, repo, kb, "m3", "u1", true)
	seedChunkWithContent(t, repo, kb, "doc-"+kb+"-1", kb+"-1", cosineVec(0.90), "内容与关键词完全无关的填充文本一一")
	seedChunkWithContent(t, repo, kb, "doc-"+kb+"-2", kb+"-2", cosineVec(0.85), "内容与关键词完全无关的填充文本一二")
	seedChunkWithContent(t, repo, kb, "doc-"+kb+"-3", kb+"-3", cosineVec(0.80), "内容与关键词完全无关的填充文本一三")
}

// T033 场景 1：rerank 调用返回 error（限流/供应商故障等）。
func TestIntegrationRetrieveRerankDegradesOnCallErrorMatchesDisabledOrderVerbatim(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	ctx := context.Background()

	const kb = "kb-rerank-degrade-err"
	seedRerankDegradeFixture(t, repo, kb)

	baseline := newTestService(repo, fp, t.TempDir())
	want, err := baseline.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("baseline (rerank disabled) Retrieve: %v", err)
	}

	errScoreFn := func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
		return provider.RerankResult{}, errors.New("simulated rerank provider failure")
	}
	degraded := newTestServiceWithRerank(repo, fp, t.TempDir(), errScoreFn)
	got, err := degraded.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("degraded (rerank call error) Retrieve: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rerank call error must degrade to fused order verbatim (FR-011):\n got  = %+v\n want = %+v", got, want)
	}
}

// T033 场景 2：rerank 调用超时。用极小的 rerankTimeout 配合一个真正尊重
// ctx.Done() 才返回的假实现来触发——不用 time.Sleep 硬等真实超时（慢且脆），
// 而是让 applyRerankStep 自己的 context.WithTimeout(ctx, s.rerankTimeout)
// 在到期那一刻主动取消 ctx，scoreFn 阻塞在 <-ctx.Done() 上收到取消信号后再
// 返回 ctx.Err()（= context.DeadlineExceeded）。这样测的是"超时配置真的生
// 效"这条生产路径本身，而不是我们自己伪造一个 DeadlineExceeded 错误值。
func TestIntegrationRetrieveRerankDegradesOnTimeoutMatchesDisabledOrderVerbatim(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	ctx := context.Background()

	const kb = "kb-rerank-degrade-timeout"
	seedRerankDegradeFixture(t, repo, kb)

	baseline := newTestService(repo, fp, t.TempDir())
	want, err := baseline.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("baseline (rerank disabled) Retrieve: %v", err)
	}

	blockingScoreFn := func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
		<-ctx.Done()
		return provider.RerankResult{}, ctx.Err()
	}
	svc := NewService(repo, fp, nil, t.TempDir(), true, "rerank-model", 10*time.Millisecond, false).(*service)
	svc.rerankScoreFn = blockingScoreFn

	got, err := svc.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("degraded (rerank timeout) Retrieve: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rerank timeout must degrade to fused order verbatim (FR-011):\n got  = %+v\n want = %+v", got, want)
	}
}

// T033 场景 3：rerank 响应含重复 index——不可信，contracts/rerank-http-api.md
// 的响应校验第 3 条。整体丢弃，不是"保留没冲突的那部分"。
func TestIntegrationRetrieveRerankDegradesOnDuplicateIndexMatchesDisabledOrderVerbatim(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	ctx := context.Background()

	const kb = "kb-rerank-degrade-dup"
	seedRerankDegradeFixture(t, repo, kb)

	baseline := newTestService(repo, fp, t.TempDir())
	want, err := baseline.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("baseline (rerank disabled) Retrieve: %v", err)
	}

	dupScoreFn := func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
		out := make([]provider.RerankScore, len(documents))
		for i := range documents {
			// 每条候选都打在 index 0 上——重复且未覆盖其余 index，
			// applyRerank 的第 2/4 条校验会拒绝它。
			out[i] = provider.RerankScore{Index: 0, Score: float64(i)}
		}
		return provider.RerankResult{Scores: out}, nil
	}
	degraded := newTestServiceWithRerank(repo, fp, t.TempDir(), dupScoreFn)
	got, err := degraded.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 3, RetrieveOptions{})
	if err != nil {
		t.Fatalf("degraded (rerank duplicate index) Retrieve: %v", err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rerank duplicate-index response must degrade to fused order verbatim (FR-011):\n got  = %+v\n want = %+v", got, want)
	}
}

// --- 001-rag-query-rerank US3：T037，确定性（SC-007）---

// TestIntegrationRetrieveDeterministicAcross20RunsWithFixedRerankScores 用
// 固定打分的假 rerank client 对同一份种子数据重复跑 20 次 Retrieve，断言每
// 次返回的 chunk ID 序列完全一致。两个候选（e2/e3）故意打相同分数，逼出
// applyRerank 的确定性 tie-break 分支（按 originalIndex 升序），不是只测
// "分数互不相同"这种更容易巧合过关的情况。
func TestIntegrationRetrieveDeterministicAcross20RunsWithFixedRerankScores(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	ctx := context.Background()

	const kb = "kb-rerank-determinism"
	seedKB(t, repo, kb, "m3", "u1", true)
	seedChunkWithContent(t, repo, kb, "doc-e1", "e1", cosineVec(0.90), "内容与关键词完全无关的填充文本二一")
	seedChunkWithContent(t, repo, kb, "doc-e2", "e2", cosineVec(0.85), "内容与关键词完全无关的填充文本二二")
	seedChunkWithContent(t, repo, kb, "doc-e3", "e3", cosineVec(0.80), "内容与关键词完全无关的填充文本二三")
	seedChunkWithContent(t, repo, kb, "doc-e4", "e4", cosineVec(0.75), "内容与关键词完全无关的填充文本二四")

	scoreFn := func(ctx context.Context, query string, documents []string) (provider.RerankResult, error) {
		scoresByMarker := map[string]float64{"二一": 0.5, "二二": 0.9, "二三": 0.9, "二四": 0.1}
		out := make([]provider.RerankScore, len(documents))
		for i, doc := range documents {
			var score float64
			for marker, s := range scoresByMarker {
				if strings.Contains(doc, marker) {
					score = s
					break
				}
			}
			out[i] = provider.RerankScore{Index: i, Score: score}
		}
		return provider.RerankResult{Scores: out}, nil
	}
	svc := newTestServiceWithRerank(repo, fp, t.TempDir(), scoreFn)

	var first []string
	for i := 0; i < 20; i++ {
		got, err := svc.Retrieve(ctx, []string{kb}, "内容与关键词完全无关的填充文本", 4, RetrieveOptions{})
		if err != nil {
			t.Fatalf("run %d Retrieve: %v", i, err)
		}
		gotIDs := ids(got)
		if i == 0 {
			first = gotIDs
			continue
		}
		if !reflect.DeepEqual(gotIDs, first) {
			t.Fatalf("run %d chunk ID sequence = %v, want %v (SC-007: must be 100%% identical across repeated runs)", i, gotIDs, first)
		}
	}
}

// --- 002-metadata-filter：检索元数据过滤（真实 PostgreSQL） ---
//
// 这一组用例验证的是纯逻辑单测覆盖不到的东西：过滤条件真的进了两路召回的
// SQL，而不是在 Go 里把结果筛了一遍。判空/校验/开关的纯函数部分在
// filter_test.go。

// TestIntegrationRetrieveFilterByDocument —— US1 / SC-001。
//
// fb1（文档 B）刻意被设计成**向量路和关键词路都会命中**：它的正文含有查询
// 词、向量也够近。因此"限定到 A 之后 fb1 消失"这一条同时证明了两路召回都
// 施加了过滤——只在一路过滤会让另一路把 B 的片段重新带回候选池（FR-007）。
func TestIntegrationRetrieveFilterByDocument(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-doc"
	seedKB(t, repo, kb, "m3", "u1", true)

	// 查询词 FILTERTOKENALPHA 同时出现在 A、B 的正文里，C 没有。
	seedNeighborChunkBatch(t, repo, kb, "doc-filter-a", 1, []neighborSeedChunk{
		{ID: "fa1", ChunkIndex: 0, Content: "文档A的正文 FILTERTOKENALPHA 出现在这里", Vec: []float32{1, 0, 0}},
		{ID: "fa2", ChunkIndex: 1, Content: "文档A的第二段正文，与查询词无关", Vec: []float32{1, 0.2, 0}},
	}, true)
	seedNeighborChunkBatch(t, repo, kb, "doc-filter-b", 1, []neighborSeedChunk{
		{ID: "fb1", ChunkIndex: 0, Content: "文档B也包含 FILTERTOKENALPHA 这个词", Vec: []float32{1, 0.1, 0}},
	}, true)
	seedNeighborChunkBatch(t, repo, kb, "doc-filter-c", 1, []neighborSeedChunk{
		{ID: "fc1", ChunkIndex: 0, Content: "文档C的正文，内容与前两份都不同", Vec: []float32{1, 0.3, 0}},
	}, true)

	// 1) 不带过滤：三份文档的片段都在。
	unfiltered, err := svc.Retrieve(ctx, []string{kb}, "FILTERTOKENALPHA", 4, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve（无过滤）: %v", err)
	}
	for _, want := range []string{"fa1", "fb1", "fc1"} {
		if !containsChunkID(unfiltered, want) {
			t.Fatalf("无过滤时应召回 %s，实际 %v", want, ids(unfiltered))
		}
	}

	// 2) 限定到 A：B、C 的片段一条都不能出现。
	filtered, err := svc.Retrieve(ctx, []string{kb}, "FILTERTOKENALPHA", 4, RetrieveOptions{
		Filter: RetrieveFilter{DocumentIDs: []string{"doc-filter-a"}},
	})
	if err != nil {
		t.Fatalf("Retrieve（限定到 A）: %v", err)
	}
	if len(filtered) == 0 {
		t.Fatal("限定到 A 之后结果为空，A 里确实有匹配的片段")
	}
	for _, c := range filtered {
		if c.DocumentID != "doc-filter-a" {
			t.Fatalf("限定到 A 之后出现了 %s 的片段 %s（完整结果 %v）", c.DocumentID, c.ID, ids(filtered))
		}
	}

	// 3) 过滤只做范围缩小，不改变打分与融合逻辑：A 的片段在 A 内部的相对
	//    顺序，过滤前后必须一致（FR-012）。
	if got, want := docOrder(filtered, "doc-filter-a"), docOrder(unfiltered, "doc-filter-a"); !equalStrings(got, want) {
		t.Fatalf("A 内部相对顺序被过滤改变了：过滤后 %v，过滤前 %v", got, want)
	}
}

// TestIntegrationRetrieveFilterUnknownDocument —— FR-010 / Edge Cases 第 1 条。
// 引用了不存在的、或属于另一个知识库的 document_id：必须是"无匹配"，
// 不是报错，也不是把这个条件悄悄丢掉当成无过滤。
func TestIntegrationRetrieveFilterUnknownDocument(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	seedKB(t, repo, "kb-filter-unknown", "m3", "u1", true)
	seedKB(t, repo, "kb-filter-other", "m3", "u1", true)
	seedChunkWithContent(t, repo, "kb-filter-unknown", "doc-known", "fu1", []float32{1, 0, 0}, "本知识库里的正文")
	seedChunkWithContent(t, repo, "kb-filter-other", "doc-elsewhere", "fo1", []float32{1, 0, 0}, "另一个知识库里的正文")

	for _, tc := range []struct {
		name       string
		documentID string
	}{
		{"完全不存在的文档", "doc-does-not-exist"},
		{"存在但属于另一个知识库的文档", "doc-elsewhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.Retrieve(ctx, []string{"kb-filter-unknown"}, "查询", 5, RetrieveOptions{
				Filter: RetrieveFilter{DocumentIDs: []string{tc.documentID}},
			})
			if err != nil {
				t.Fatalf("引用不存在的实体必须无匹配而不是报错，got err %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("want 空结果，got %v —— 该过滤条件被静默忽略了", ids(got))
			}
		})
	}
}

// TestIntegrationRetrieveFilterNoAutoRelax —— FR-009 / Edge Cases 最后一条。
// 过滤后候选数远低于 candidateK 时，绝不允许拿范围外的片段来补足名额。
func TestIntegrationRetrieveFilterNoAutoRelax(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-norelax"
	seedKB(t, repo, kb, "m3", "u1", true)
	seedChunkWithContent(t, repo, kb, "doc-narrow", "nr-only", []float32{1, 0.5, 0}, "范围内唯一的一条正文")
	// 范围外有大量分数更高的片段，足以填满 topK——如果实现里有任何"候选不足
	// 就放宽"的逻辑，它们就会冒出来。
	for i := 0; i < 8; i++ {
		seedChunkWithContent(t, repo, kb, "doc-wide", "nr-decoy-"+strconv.Itoa(i),
			[]float32{1, 0, 0}, "范围外的诱饵正文，各不相同 "+strconv.Itoa(i))
	}

	got, err := svc.Retrieve(ctx, []string{kb}, "查询", 5, RetrieveOptions{
		Filter: RetrieveFilter{DocumentIDs: []string{"doc-narrow"}},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 || got[0].ID != "nr-only" {
		t.Fatalf("want 只有 nr-only 一条（宁可少也不放宽），got %v", ids(got))
	}
}

// TestIntegrationRetrieveFilterByPageRange —— US2 / SC-002。
func TestIntegrationRetrieveFilterByPageRange(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-page"
	seedKB(t, repo, kb, "m3", "u1", true)
	p3, p12 := 3, 12
	seedNeighborChunkBatch(t, repo, kb, "doc-paged", 1, []neighborSeedChunk{
		{ID: "pg-early", ChunkIndex: 0, Content: "第 3 页上的正文", Vec: []float32{1, 0.2, 0}, PageNumber: &p3},
		{ID: "pg-target", ChunkIndex: 9, Content: "第 12 页上的目标正文", Vec: []float32{1, 0, 0}, PageNumber: &p12},
	}, true)

	inRange, err := svc.Retrieve(ctx, []string{kb}, "查询", 5, RetrieveOptions{
		Filter: RetrieveFilter{PageMin: intPtr(10), PageMax: intPtr(15)},
	})
	if err != nil {
		t.Fatalf("Retrieve（[10,15]）: %v", err)
	}
	if !containsChunkID(inRange, "pg-target") {
		t.Fatalf("页码范围 [10,15] 应召回第 12 页的 pg-target，got %v", ids(inRange))
	}
	// 第 3 页的片段不在范围内，也不该作为"命中"出现（它是 chunk_index=0，
	// 与 chunk_index=9 不相邻，因此也不会以邻接块身份进来）。
	if containsChunkID(inRange, "pg-early") {
		t.Fatalf("页码范围 [10,15] 不该召回第 3 页的 pg-early，got %v", ids(inRange))
	}

	outOfRange, err := svc.Retrieve(ctx, []string{kb}, "查询", 5, RetrieveOptions{
		Filter: RetrieveFilter{PageMin: intPtr(1), PageMax: intPtr(5)},
	})
	if err != nil {
		t.Fatalf("Retrieve（[1,5]）: %v", err)
	}
	if containsChunkID(outOfRange, "pg-target") {
		t.Fatalf("页码范围 [1,5] 不该召回第 12 页的 pg-target，got %v", ids(outOfRange))
	}
}

// TestFilterPageRangeExcludesNullPageChunks —— SC-005 修订 / FR-014 修订 /
// research.md R2。
//
// 无页码的 chunk（全部 txt/md、以及 000003 迁移之前入库的存量行）在页码过滤
// 下必须**不匹配**，而不是"无元数据即通过"；同时在**无过滤**检索下必须照常
// 可被命中——"没有这项数据"不等于"这条数据永久失效"。
//
// 这条用例同时锁定 chunks.sql 里那个隐式依赖：排除 NULL 靠的是 SQL 三值逻辑
// （NULL >= 10 求值为 NULL，不是 TRUE，因此被 WHERE 排除）。任何把它改写成
// COALESCE(page_number, 0) 的"修复"都会让这条用例失败——那等于给一个本来没有
// 页码的 chunk 编造出第 0 页。
func TestFilterPageRangeExcludesNullPageChunks(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-nullpage"
	seedKB(t, repo, kb, "m3", "u1", true)
	p7 := 7
	seedNeighborChunkBatch(t, repo, kb, "doc-nullpage", 1, []neighborSeedChunk{
		// 有页码的对照组，确保过滤本身确实生效了（否则"什么都没召回"也能
		// 让下面的断言通过）。
		{ID: "np-paged", ChunkIndex: 0, Content: "有页码的正文", Vec: []float32{1, 0.1, 0}, PageNumber: &p7},
		// PageNumber 为 nil：txt/md chunk 与存量行的真实形态。
		{ID: "np-null", ChunkIndex: 5, Content: "没有页码的正文", Vec: []float32{1, 0, 0}},
	}, true)

	// 三种形态各测一遍：闭区间、只给下界、只给上界。**缺一不可**——变异测试
	// 证实过：只用闭区间时，即使有人给 PageMin 那一侧错误地加上
	// "OR page_number IS NULL"，未被改动的 PageMax 一侧仍会把 NULL 行挡下来，
	// 用例照样通过，于是这条断言对"单侧回归"是瞎的。单端的两个子用例把每一侧
	// 独立暴露出来。
	for _, tc := range []struct {
		name   string
		filter RetrieveFilter
	}{
		{"闭区间 [1,100]", RetrieveFilter{PageMin: intPtr(1), PageMax: intPtr(100)}},
		{"只给下界 >=1", RetrieveFilter{PageMin: intPtr(1)}},
		{"只给上界 <=100", RetrieveFilter{PageMax: intPtr(100)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPageFilter, err := svc.Retrieve(ctx, []string{kb}, "查询", 5, RetrieveOptions{Filter: tc.filter})
			if err != nil {
				t.Fatalf("Retrieve（带页码过滤）: %v", err)
			}
			if containsChunkID(withPageFilter, "np-null") {
				t.Fatalf("无页码的 chunk 在页码过滤下被当作通过了，got %v", ids(withPageFilter))
			}
			// 对照组：确认过滤本身确实生效了，否则"什么都没召回"也能让上面
			// 那条断言通过。
			if !containsChunkID(withPageFilter, "np-paged") {
				t.Fatalf("对照组 np-paged（第 7 页）应被召回，got %v —— 过滤可能过严", ids(withPageFilter))
			}
		})
	}

	withoutFilter, err := svc.Retrieve(ctx, []string{kb}, "查询", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve（无过滤）: %v", err)
	}
	if !containsChunkID(withoutFilter, "np-null") {
		t.Fatalf("无过滤时无页码的 chunk 必须照常可被命中，got %v", ids(withoutFilter))
	}
}

// TestIntegrationRetrieveFilterPushedDownToRecall —— SC-004，本功能最关键的
// 一条用例。
//
// 它是"过滤下推到召回 SQL"与"先召回 topK 再在应用层筛"唯一能被外部观察到的
// 区别。构造方式：让目标文档的片段全部排在全库相似度榜的 candidateK 名之外。
//   - 若过滤是应用层筛选：候选窗口里全是诱饵，筛完结果为**空**；
//   - 若过滤真的进了 SQL：SQL 一开始就只看目标文档，照常返回它内部的 topK。
func TestIntegrationRetrieveFilterPushedDownToRecall(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-pushdown"
	seedKB(t, repo, kb, "m3", "u1", true)

	const topK = 3
	// candidateK(3) = 12。诱饵放 15 条，且每条都比目标文档更接近查询向量，
	// 于是不带过滤时候选窗口被诱饵占满，目标文档一条都进不去。
	decoys := make([]neighborSeedChunk, 0, 15)
	for i := 0; i < 15; i++ {
		decoys = append(decoys, neighborSeedChunk{
			ID:         "pd-decoy-" + strconv.Itoa(i),
			ChunkIndex: i,
			Content:    "诱饵文档的第 " + strconv.Itoa(i) + " 段正文，各不相同以避开内容去重",
			Vec:        []float32{1, float32(i) * 0.001, 0}, // cos ≈ 1.0
		})
	}
	seedNeighborChunkBatch(t, repo, kb, "doc-pushdown-decoy", 1, decoys, true)

	targets := make([]neighborSeedChunk, 0, 3)
	for i := 0; i < 3; i++ {
		targets = append(targets, neighborSeedChunk{
			ID:         "pd-target-" + strconv.Itoa(i),
			ChunkIndex: i,
			Content:    "目标文档的第 " + strconv.Itoa(i) + " 段正文，各不相同",
			Vec:        []float32{1, 0.9, 0}, // cos ≈ 0.743：高于准入阈值，但远低于所有诱饵
		})
	}
	seedNeighborChunkBatch(t, repo, kb, "doc-pushdown-target", 1, targets, true)

	// 前提校验：不带过滤时目标文档确实一条都进不了结果。这一步不成立的话，
	// 下面的断言就证明不了任何事情。
	unfiltered, err := svc.Retrieve(ctx, []string{kb}, "PUSHDOWNTOKEN", topK, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve（无过滤）: %v", err)
	}
	for _, c := range unfiltered {
		if c.DocumentID == "doc-pushdown-target" {
			t.Fatalf("用例前提不成立：不带过滤时目标文档就已经进入结果（%v），"+
				"诱饵数量或分数需要调整", ids(unfiltered))
		}
	}

	filtered, err := svc.Retrieve(ctx, []string{kb}, "PUSHDOWNTOKEN", topK, RetrieveOptions{
		Filter: RetrieveFilter{DocumentIDs: []string{"doc-pushdown-target"}},
	})
	if err != nil {
		t.Fatalf("Retrieve（限定到目标文档）: %v", err)
	}
	if len(filtered) == 0 {
		t.Fatal("限定到目标文档后结果为空 —— 这正是「先召回 topK 再在应用层筛」的症状：" +
			"过滤吃掉了召回名额，而不是重新定向了召回范围（FR-007 / SC-004）")
	}
	for _, c := range filtered {
		if c.DocumentID != "doc-pushdown-target" {
			t.Fatalf("结果里混入了 %s 的片段：%v", c.DocumentID, ids(filtered))
		}
	}
	// 拿到的是目标文档**内部**的 topK，不是"全库 topK 里恰好属于它的那几条"。
	if len(filtered) != topK {
		t.Fatalf("want 目标文档内的 %d 条候选，got %d 条 %v", topK, len(filtered), ids(filtered))
	}
}

// TestIntegrationRetrieveFilterExemptsNeighborsFromPageFilter —— FR-011。
//
// 邻接块是**上下文补全**而不是检索命中，一个页码范围不该把答案的后半句挡在
// 外面，所以 chunk 级过滤对邻接块豁免；但跨文档取邻接在任何情况下都不成立，
// 所以文档级过滤照旧生效。
//
// 实现上这两条都不需要给邻接查询加谓词：文档级是结构性满足的（邻接坐标全部
// 取自已经通过过滤的 anchors），页码级的豁免就是"不加"。这条用例存在的意义
// 正是防止将来有人"顺手统一一下"把过滤条件也加到邻接查询上。
func TestIntegrationRetrieveFilterExemptsNeighborsFromPageFilter(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-neighbor"
	seedKB(t, repo, kb, "m3", "u1", true)
	p1, p2, p3 := 1, 2, 3
	seedNeighborChunkBatch(t, repo, kb, "doc-neighbor-main", 1, []neighborSeedChunk{
		// 第 1 / 3 页：在页码过滤范围外，且向量分数很低（低于准入阈值），
		// 因此不可能自己成为 anchor，只能通过邻接窗口进来。
		{ID: "nb-prev", ChunkIndex: 0, Content: "第 1 页的正文，是答案的前半句", Vec: []float32{0, 1, 0}, PageNumber: &p1},
		{ID: "nb-anchor", ChunkIndex: 1, Content: "第 2 页的正文，是命中的那一句", Vec: []float32{1, 0, 0}, PageNumber: &p2},
		{ID: "nb-next", ChunkIndex: 2, Content: "第 3 页的正文，是答案的后半句", Vec: []float32{0, 1, 0}, PageNumber: &p3},
	}, true)
	// 另一份文档，也在第 2 页、分数也高——文档级过滤必须把它挡住，
	// 而且它绝不能以任何身份（含邻接块）出现。
	seedNeighborChunkBatch(t, repo, kb, "doc-neighbor-other", 1, []neighborSeedChunk{
		{ID: "nb-other", ChunkIndex: 0, Content: "另一份文档第 2 页的正文", Vec: []float32{1, 0, 0}, PageNumber: &p2},
	}, true)

	got, err := svc.Retrieve(ctx, []string{kb}, "查询", 5, RetrieveOptions{
		Filter: RetrieveFilter{
			DocumentIDs: []string{"doc-neighbor-main"},
			PageMin:     intPtr(2),
			PageMax:     intPtr(2),
		},
	})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if !containsChunkID(got, "nb-anchor") {
		t.Fatalf("第 2 页的 nb-anchor 应作为命中返回，got %v", ids(got))
	}
	// chunk 级（页码）豁免：范围外的邻接块必须还在。
	for _, want := range []string{"nb-prev", "nb-next"} {
		c := findChunk(got, want)
		if c == nil {
			t.Fatalf("邻接块 %s 落在页码范围外，但邻接块豁免 chunk 级过滤，必须仍然返回；got %v", want, ids(got))
		}
		if c.NeighborOf == "" {
			t.Fatalf("%s 应当以邻接块身份出现（NeighborOf 非空），got 命中身份", want)
		}
	}
	// 文档级过滤对邻接块照旧生效：另一份文档一条都不能进来。
	for _, c := range got {
		if c.DocumentID != "doc-neighbor-main" {
			t.Fatalf("文档级过滤对邻接块必须继续生效，但结果里出现了 %s 的 %s：%v",
				c.DocumentID, c.ID, ids(got))
		}
	}
}

// --- 002-metadata-filter 用例专用的小助手 ---

func containsChunkID(chunks []RetrievedChunk, id string) bool {
	return findChunk(chunks, id) != nil
}

func findChunk(chunks []RetrievedChunk, id string) *RetrievedChunk {
	for i := range chunks {
		if chunks[i].ID == id {
			return &chunks[i]
		}
	}
	return nil
}

// docOrder 返回结果中属于 documentID 的片段 ID，保持原有顺序——用于断言
// "过滤只做范围缩小，不改变同一文档内部的相对排序"。
func docOrder(chunks []RetrievedChunk, documentID string) []string {
	var out []string
	for _, c := range chunks {
		if c.DocumentID == documentID {
			out = append(out, c.ID)
		}
	}
	return out
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

// TestIntegrationRetrieveFilterDiagnosticsAreContentFree —— US4 / FR-017 / FR-018。
//
// 断言两件事：
//   - 诊断日志确实记录了过滤是否施加、各路候选数、是否零候选（否则用户抱怨
//     "我限定了文档怎么还是答不对"时无法排查）；
//   - 但**不含任何取值**——document_id、页码数值、查询原文、片段正文一律不进
//     日志。document_id 与页码都能反推出文件身份，这与 Phase 9 不记录逐条
//     rerank 分数是同一个口径。
func TestIntegrationRetrieveFilterDiagnosticsAreContentFree(t *testing.T) {
	repo := setupIntegration(t)
	fp := newFakeProvider()
	svc := newTestServiceWithFilter(repo, fp, t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-diag"
	seedKB(t, repo, kb, "m3", "u1", true)
	p42 := 42
	seedNeighborChunkBatch(t, repo, kb, "doc-diag-secret-filename", 1, []neighborSeedChunk{
		{ID: "dg1", ChunkIndex: 0, Content: "机密正文不得进入日志", Vec: []float32{1, 0, 0}, PageNumber: &p42},
	}, true)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	const secretQuery = "SECRETQUERYTEXT"
	if _, err := svc.Retrieve(ctx, []string{kb}, secretQuery, 5, RetrieveOptions{
		Filter: RetrieveFilter{
			DocumentIDs: []string{"doc-diag-secret-filename"},
			PageMin:     intPtr(40),
			PageMax:     intPtr(45),
		},
	}); err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	logged := buf.String()

	// 施加了过滤就必须留下可排查的痕迹（FR-017）。
	for _, want := range []string{
		"filter_applied=true",
		"filter_document_id_count=1",
		"filter_page_range_set=true",
		"vector_candidate_count=",
		"keyword_candidate_count=",
		"filter_zero_candidates=",
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("诊断日志缺少 %q，无法回答 US4 要区分的两件事；实际日志：\n%s", want, logged)
		}
	}

	// 但绝不能含取值（FR-018）。
	for _, forbidden := range []struct {
		what  string
		value string
	}{
		{"document_id 取值", "doc-diag-secret-filename"},
		{"片段正文", "机密正文不得进入日志"},
		{"查询原文", secretQuery},
		{"页码数值（下界）", "=40"},
		{"页码数值（上界）", "=45"},
		{"页码数值（chunk 自身）", "=42"},
	} {
		if strings.Contains(logged, forbidden.value) {
			t.Fatalf("诊断日志泄漏了%s（%q）——FR-018 要求只记种类与数量；实际日志：\n%s",
				forbidden.what, forbidden.value, logged)
		}
	}
}

// --- 006-pdf-layout-chunking US1：真实 PG 上的跨页召回与确定性 ---

// seedCrossPagePDFDoc 把 chunk_test.go 的 F1 夹具（第 3 页末尾与第 4 页开头
// 是同一段话）真的走一遍入库流水线，返回知识库 ID。
func seedCrossPagePDFDoc(t *testing.T, repo *Repository, svc Service, kbID, docID string) {
	t.Helper()
	ctx := context.Background()
	if err := repo.createKnowledgeBase(ctx, KnowledgeBase{
		ID: kbID, Name: kbID, EmbeddingModelID: "m3",
		ChunkSize: 400, ChunkOverlap: 40, CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{
		ID: docID, KnowledgeBaseID: kbID, FileName: "handbook.pdf", FileType: FileTypePDF,
		FileSize: 1, StoragePath: crossPageFixture(t), CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, docID, 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	doc, err := repo.getDocument(ctx, docID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != StatusReady {
		t.Fatalf("doc status=%s, want ready (err=%q)", doc.Status, doc.ErrorMessage)
	}
}

// TestIntegrationCrossPageParagraphIsRetrievableWhole is SC-001 at the far
// end of the pipeline: not "the chunker kept it together" but "asking about
// it gets the whole thing back, cited as pages 3-4".
//
// The query is the marker token from the FIRST half of the paragraph. Before
// this feature that token lived in a chunk which stopped mid-sentence — the
// answer's second half was in a different chunk on a different page, and
// retrieving the first half returned a fragment ending "...created before
// the", which is exactly the shape that invites a model to invent the rest.
func TestIntegrationCrossPageParagraphIsRetrievableWhole(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	seedCrossPagePDFDoc(t, repo, svc, "kb-xpage", "doc-xpage")

	out, err := svc.Retrieve(context.Background(), []string{"kb-xpage"}, crossPageTailMarker, 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("检索没有返回任何片段——跨页那段话应当被关键词路命中")
	}

	for _, c := range out {
		if !strings.Contains(c.Content, crossPageTailMarker) {
			continue
		}
		if !strings.Contains(c.Content, crossPageHeadMarker) {
			t.Fatalf("命中的片段只有跨页段落的前半截，后半截仍然在别的片段里：%q", c.Content)
		}
		if c.PageNumber == nil || c.PageEnd == nil {
			t.Fatalf("跨页片段缺少页码区间：%+v", c)
		}
		if *c.PageNumber != 3 || *c.PageEnd != 4 {
			t.Fatalf("跨页片段报出的页码是 %d-%d，应当是 3-4——它确实覆盖了这两页",
				*c.PageNumber, *c.PageEnd)
		}
		return
	}
	t.Fatalf("检索结果里没有包含跨页段落的片段：%+v", out)
}

// TestIntegrationPDFChunkingIsByteForByteRepeatable is SC-004 on the whole
// chain. Running the same PDF through chunkDocument twice must produce the
// same chunks with the same page intervals, byte for byte.
//
// ⚠️ This is aimed squarely at the map-iteration trap in layout.go's modal
// statistics: that bug does not fail every run, it fails a small fraction
// of them, so the loop count is the test. layout_test.go covers the same
// property on the pure functions with a tighter loop; this one proves it
// survives the real extractor, whose line widths do not tie as neatly as a
// hand-written fixture's.
func TestIntegrationPDFChunkingIsByteForByteRepeatable(t *testing.T) {
	path := crossPageFixture(t)
	parsed, err := parseFile(path, FileTypePDF)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}

	first := chunkDocument(FileTypePDF, parsed, 400, 40)
	if len(first) == 0 {
		t.Fatal("expected chunks")
	}
	render := func(pieces []chunkPiece) string {
		var sb strings.Builder
		for _, p := range pieces {
			start, end := "nil", "nil"
			if p.PageNumber != nil {
				start = strconv.Itoa(*p.PageNumber)
			}
			if p.PageEnd != nil {
				end = strconv.Itoa(*p.PageEnd)
			}
			fmt.Fprintf(&sb, "%s|%s|%s\n", start, end, p.Content)
		}
		return sb.String()
	}
	want := render(first)

	for i := 0; i < 50; i++ {
		reparsed, err := parseFile(path, FileTypePDF)
		if err != nil {
			t.Fatalf("parseFile run %d: %v", i, err)
		}
		if got := render(chunkDocument(FileTypePDF, reparsed, 400, 40)); got != want {
			t.Fatalf("run %d produced different chunks/page intervals:\n got:\n%s\nwant:\n%s", i, got, want)
		}
	}
}

// --- 006-pdf-layout-chunking US2：剥离审计日志（FR-008） ---

// TestIntegrationLayoutNoiseAuditLogIsWritten 断言 FR-008 的审计线索真的
// 从 ProcessDocument 里写出来了，而不是只存在于 stripLayoutNoise 的返回值里
// 却没人消费。
//
// ⚠️ 这条日志**会记录被剥离的文本**，与 002-metadata-filter 确立的"日志不记
// 片段正文"口径方向相反。理由与三条缓解措施见 service.go 的
// logStrippedLayoutNoise 注释；这里断言的是缓解措施确实生效：记的是**归一化
// 后**的文本（数字已被抹掉），且汇总行带上了 stripped_total。
func TestIntegrationLayoutNoiseAuditLogIsWritten(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	if err := repo.createKnowledgeBase(ctx, KnowledgeBase{
		ID: "kb-noise", Name: "kb-noise", EmbeddingModelID: "m3",
		ChunkSize: 400, ChunkOverlap: 40, CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{
		ID: "doc-noise", KnowledgeBaseID: "kb-noise", FileName: "handbook.pdf",
		FileType: FileTypePDF, FileSize: 1, StoragePath: headerFooterFixture(t), CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}

	prev := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	if err := svc.ProcessDocument(ctx, "doc-noise", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "layout noise stripped") {
		t.Fatalf("没有写出剥离汇总日志（FR-008）：%s", out)
	}
	if !strings.Contains(out, "stripped_total") {
		t.Fatalf("汇总日志缺少 stripped_total：%s", out)
	}
	if !strings.Contains(out, string(reasonRepeatedHeader)) {
		t.Fatalf("逐行记录里没有出现 %q 这个原因：%s", reasonRepeatedHeader, out)
	}
	// 记的是归一化后的文本：数字已被抹掉，所以原始页码行 "Page 3 of 6" 不该
	// 原样出现在日志里。
	if strings.Contains(out, "Page 3 of 6") {
		t.Fatalf("日志记录了未归一化的原始文本，缓解措施没有生效：%s", out)
	}
}

// --- 006-pdf-layout-chunking US3：页码过滤的区间相交语义 ---

// TestIntegrationPageFilterIntersectsChunkInterval 逐格走 contracts §3.3 的
// 对照表：一个跨第 3-4 页的片段与一个单独在第 3 页的片段，在九种过滤条件下
// 分别该不该命中。
//
// ⭐ 三个 ⭐ 格（min=4,max=4 / min=4,max=9 / 只给 min=4）是本次改动**唯一**
// 改变行为的地方，而且方向全是"原本漏掉的现在命中了"——过滤「第 4 页」本来
// 就该命中一个覆盖 3-4 页的片段，因为它确实包含第 4 页的内容。表里不存在
// 任何一格是"原本命中的现在不命中"，这正是 FR-022 与 SC-006 在契约层的表述。
//
// 单页片段那一列同时充当对照组：它在所有九格里的行为与改动前**完全一致**，
// 所以这张表也证明了 R2 的等价性论证（data-model.md §7.1）在真实 SQL 上成立，
// 而不只是在纸面上。
func TestIntegrationPageFilterIntersectsChunkInterval(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	kb := "kb-filter-interval"
	seedKB(t, repo, kb, "m3", "u1", true)
	p3, p4 := 3, 4
	seedNeighborChunkBatch(t, repo, kb, "doc-interval", 1, []neighborSeedChunk{
		// 真正的跨页片段：起始第 3 页、结束第 4 页。
		{ID: "iv-cross", ChunkIndex: 0, Content: "跨页片段的正文", Vec: []float32{1, 0, 0},
			PageNumber: &p3, PageEnd: &p4},
		// 对照：只在第 3 页。chunk_index 刻意与上面拉开，避免邻接窗口把它作为
		// 上下文补全带回来（那是 002 FR-011 的豁免，正确但会让断言失去针对性）。
		{ID: "iv-single", ChunkIndex: 9, Content: "单页片段的正文", Vec: []float32{1, 0.05, 0},
			PageNumber: &p3, PageEnd: &p3},
	}, true)

	cases := []struct {
		name         string
		filter       RetrieveFilter
		wantCross    bool
		wantSingle   bool
		changedBy006 bool
	}{
		{"min=3 max=3", RetrieveFilter{PageMin: intPtr(3), PageMax: intPtr(3)}, true, true, false},
		{"min=4 max=4 ⭐", RetrieveFilter{PageMin: intPtr(4), PageMax: intPtr(4)}, true, false, true},
		{"min=4 max=9 ⭐", RetrieveFilter{PageMin: intPtr(4), PageMax: intPtr(9)}, true, false, true},
		{"只给 min=4 ⭐", RetrieveFilter{PageMin: intPtr(4)}, true, false, true},
		{"min=1 max=2", RetrieveFilter{PageMin: intPtr(1), PageMax: intPtr(2)}, false, false, false},
		{"min=5 max=9", RetrieveFilter{PageMin: intPtr(5), PageMax: intPtr(9)}, false, false, false},
		{"min=1 max=10", RetrieveFilter{PageMin: intPtr(1), PageMax: intPtr(10)}, true, true, false},
		{"只给 max=3", RetrieveFilter{PageMax: intPtr(3)}, true, true, false},
		{"两端都不设", RetrieveFilter{}, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := svc.Retrieve(ctx, []string{kb}, "片段的正文", 10, RetrieveOptions{Filter: tc.filter})
			if err != nil {
				t.Fatalf("Retrieve: %v", err)
			}
			if got := containsChunkID(out, "iv-cross"); got != tc.wantCross {
				marker := ""
				if tc.changedBy006 {
					marker = "（这是 006 改变行为的三格之一：跨页片段确实包含该页的内容，应当命中）"
				}
				t.Fatalf("跨页片段(3-4) 命中=%v，期望 %v%s，got %v", got, tc.wantCross, marker, ids(out))
			}
			if got := containsChunkID(out, "iv-single"); got != tc.wantSingle {
				t.Fatalf("单页片段(3) 命中=%v，期望 %v —— 单页片段的行为必须与改动前完全一致，got %v",
					got, tc.wantSingle, ids(out))
			}
		})
	}
}

// TestIntegrationCrossPageChunkCarriesIntervalThroughRetrieval 锁定
// page_end 真的从库里读了出来，而不是只写进去。四处行映射漏掉任何一处，
// 那条路径返回的 chunk 的 PageEnd 就会恒为 nil，违反不变量 C1；如果 000005
// 的 CHECK 约束哪天被拿掉，这个错误会一路静默传到前端显示成「—」。
func TestIntegrationCrossPageChunkCarriesIntervalThroughRetrieval(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestServiceWithFilter(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	kb := "kb-interval-roundtrip"
	seedKB(t, repo, kb, "m3", "u1", true)
	p3, p4 := 3, 4
	seedNeighborChunkBatch(t, repo, kb, "doc-interval-rt", 1, []neighborSeedChunk{
		{ID: "rt-cross", ChunkIndex: 0, Content: "跨页片段的正文往返", Vec: []float32{1, 0, 0},
			PageNumber: &p3, PageEnd: &p4},
	}, true)

	out, err := svc.Retrieve(ctx, []string{kb}, "跨页片段的正文往返", 5, RetrieveOptions{})
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	for _, c := range out {
		if c.ID != "rt-cross" {
			continue
		}
		if c.PageNumber == nil || c.PageEnd == nil {
			t.Fatalf("跨页片段往返之后丢了区间的一端（C1）：%+v", c)
		}
		if *c.PageNumber != 3 || *c.PageEnd != 4 {
			t.Fatalf("往返之后区间变成 %d-%d，应当是 3-4", *c.PageNumber, *c.PageEnd)
		}
		return
	}
	t.Fatalf("没有召回 rt-cross：%v", ids(out))
}

// --- 006-pdf-layout-chunking US5：扫描件与空文件必须能被用户区分（SC-007） ---

func seedDocForProcessing(t *testing.T, repo *Repository, kbID, docID, fileName, fileType, path string) {
	t.Helper()
	ctx := context.Background()
	if err := repo.createKnowledgeBase(ctx, KnowledgeBase{
		ID: kbID, Name: kbID, EmbeddingModelID: "m3",
		ChunkSize: 400, ChunkOverlap: 40, CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.createDocument(ctx, Document{
		ID: docID, KnowledgeBaseID: kbID, FileName: fileName, FileType: fileType,
		FileSize: 1, StoragePath: path, CreatedBy: "u1",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestIntegrationScannedPDFIsDistinguishableFromAnEmptyFile 是 SC-007 的
// 验收形态：**用户看到的那句话**必须让他判断得出下一步该做什么。
//
// 改动前两种情况都返回「文档内容为空或无法提取到文本」——准确，但用户没法
// 知道自己是传了个空文件还是传了份需要 OCR 的扫描件，而这两件事的下一步完全
// 不同。改动后扫描件走专用错误并点名 OCR，空文件仍走原来那条（它的适用范围
// 由此**收缩**为"真正的空文件"，文案一个字没改）。
func TestIntegrationScannedPDFIsDistinguishableFromAnEmptyFile(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	// F4：所有页都没有可提取文本的纯扫描件。
	scanned := writeTestPDF(t, pdfLinesFromStrings("", "", ""))
	seedDocForProcessing(t, repo, "kb-scan", "doc-scan", "scan.pdf", FileTypePDF, scanned)
	scanErr := svc.ProcessDocument(ctx, "doc-scan", 1)
	if !errors.Is(scanErr, ErrPDFNoTextLayer) {
		t.Fatalf("纯扫描件返回的是 %v，应当是 ErrPDFNoTextLayer", scanErr)
	}

	// 真正的空文件：仍然走 ErrEmptyContent。
	emptyPath := filepath.Join(t.TempDir(), "empty.txt")
	if err := os.WriteFile(emptyPath, []byte("   \n\n  "), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDocForProcessing(t, repo, "kb-empty-scan006", "doc-empty-scan006", "empty.txt", FileTypeTxt, emptyPath)
	emptyErr := svc.ProcessDocument(ctx, "doc-empty-scan006", 1)
	if !errors.Is(emptyErr, ErrEmptyContent) {
		t.Fatalf("空文件返回的是 %v，应当是 ErrEmptyContent", emptyErr)
	}

	// ⭐ 两条用户可见文案必须真的不同，而且扫描件那条要点名 OCR——SC-007 的
	// 原话是"无需查阅日志就能判断下一步动作"。
	var scanApp, emptyApp *apperr.AppError
	if !errors.As(scanErr, &scanApp) || !errors.As(emptyErr, &emptyApp) {
		t.Fatalf("两个错误都应当是 AppError：%v / %v", scanErr, emptyErr)
	}
	if scanApp.Message == emptyApp.Message {
		t.Fatalf("扫描件与空文件的用户文案相同（%q），用户无法区分自己是哪种情况", scanApp.Message)
	}
	if !strings.Contains(scanApp.Message, "OCR") {
		t.Fatalf("扫描件的提示没有点名 OCR，用户仍然不知道下一步做什么：%q", scanApp.Message)
	}

	// 文档状态与 error_message 落库，前端文档列表就是从这里读的。
	got, err := repo.getDocument(ctx, "doc-scan")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || !strings.Contains(got.ErrorMessage, "OCR") {
		t.Fatalf("扫描件文档 status=%s error_message=%q，应当是 failed 且提示里带 OCR", got.Status, got.ErrorMessage)
	}
}

// TestIntegrationPartiallyScannedPDFStillIngestsItsTextPages 是夹具 F5。
//
// ⚠️ 这条**不断言任何"告知用户"的行为**：FR-018 已按 plan.md「三项拍板」
// 决策 1 整条推迟到下一期，本期用户界面上看不到任何提示，只有一条结构化日志。
// 这里锁定的是本期真正做到的两件事：有文本的页正常入库（不因为部分页缺文本
// 就整体失败），以及**页码不错位**——被跳过的页不能让后面的页码往前挪。
func TestIntegrationPartiallyScannedPDFStillIngestsItsTextPages(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	// 5 页里第 2、4 页无文本。
	// ⚠️ 只能用纯 ASCII：writeTestPDF 用的是 Courier + WinAnsi，中文会被渲染成
	// 一串乱码（helper 的文档注释写明了这条限制，不是本用例的问题）。
	path := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"",
		"pagethree body text standing alone.",
		"",
		"pagefive body text standing alone.",
	))
	seedDocForProcessing(t, repo, "kb-partial", "doc-partial", "partial.pdf", FileTypePDF, path)
	if err := svc.ProcessDocument(ctx, "doc-partial", 1); err != nil {
		t.Fatalf("部分扫描件应当正常入库有文本的部分，实际失败：%v", err)
	}

	chunks, err := repo.searchVectorChunks(ctx, []string{"kb-partial"}, []float32{1, 0, 0}, 100, RetrieveFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("有文本的三页一个片段都没入库")
	}
	byPage := map[int]string{}
	for _, c := range chunks {
		if c.PageNumber == nil || c.PageEnd == nil {
			t.Fatalf("片段缺少页码区间：%+v", c)
		}
		byPage[*c.PageNumber] += c.Content
	}
	// 页码必须指向**真实页号** 1/3/5，而不是被压缩成 1/2/3。
	for page, want := range map[int]string{1: "pageone", 3: "pagethree", 5: "pagefive"} {
		got, ok := byPage[page]
		if !ok {
			t.Fatalf("第 %d 页的内容没有以页码 %d 入库，实际页码分布：%v", page, page, byPage)
		}
		if !strings.Contains(got, want) {
			t.Fatalf("页码 %d 上的内容是 %q，应当包含 %q —— 跳过的空白页让页码错位了", page, got, want)
		}
	}
}

// TestIntegrationPageFilterIntersectionAppliesToBothRecallPaths 是变异测试
// 逼出来的用例。
//
// ⚠️ 发现经过：把 SearchVectorChunks 的下界谓词单独改回
// `page_number >= min`（即 006 之前的点落语义），
// TestIntegrationPageFilterIntersectsChunkInterval **照样通过**——因为它走的是
// 公开的 Retrieve，两路召回融合之后，未被改坏的关键词路仍然把跨页片段带了
// 回来，向量路的回归被整个掩盖掉。两处一起改才会失败。
//
// 这正是 002-metadata-filter 踩过的那个盲区换了个形状又出现了一次：**一条经过
// 融合的断言，对"只有一路出问题"是瞎的**。所以这条用例绕开融合，直接分别打
// 两条召回 SQL，让每一路各自暴露。
//
// 页码过滤的谓词在两条查询里是逐字重复的，重复就意味着可以只改一处——这条
// 用例存在的全部意义就是让那种改动响亮地失败。
func TestIntegrationPageFilterIntersectionAppliesToBothRecallPaths(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	kb := "kb-filter-bothpaths"
	seedKB(t, repo, kb, "m3", "u1", true)
	p3, p4 := 3, 4
	seedNeighborChunkBatch(t, repo, kb, "doc-bothpaths", 1, []neighborSeedChunk{
		{ID: "bp-cross", ChunkIndex: 0, Content: "BOTHPATHSTOKEN 跨页片段的正文", Vec: []float32{1, 0, 0},
			PageNumber: &p3, PageEnd: &p4},
	}, true)

	// 过滤「第 4 页」：跨页片段(3-4) 与它相交，两条路都必须召回它。
	// 点落语义下 page_number=3 不满足 >=4，任何一路退回点落语义都会在这里
	// 露出来。
	filter := RetrieveFilter{PageMin: intPtr(4), PageMax: intPtr(4)}

	vec, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10, filter)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if !containsChunkID(vec, "bp-cross") {
		t.Fatalf("向量召回路没有命中跨页片段(3-4)——这一路的页码谓词退回了点落语义，got %v", ids(vec))
	}

	kw, err := repo.searchKeywordChunks(ctx, []string{kb}, "BOTHPATHSTOKEN", 10, filter)
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if !containsChunkID(kw, "bp-cross") {
		t.Fatalf("关键词召回路没有命中跨页片段(3-4)——这一路的页码谓词退回了点落语义，got %v", ids(kw))
	}

	// 反向对照：范围完全落在区间之外时两条路都不该命中，否则上面两条断言用
	// 一个"永远命中"的实现也能通过。
	outside := RetrieveFilter{PageMin: intPtr(9), PageMax: intPtr(9)}
	vecOut, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10, outside)
	if err != nil {
		t.Fatalf("searchVectorChunks（范围外）: %v", err)
	}
	if containsChunkID(vecOut, "bp-cross") {
		t.Fatalf("向量召回路把区间外的片段也带回来了，got %v", ids(vecOut))
	}
	kwOut, err := repo.searchKeywordChunks(ctx, []string{kb}, "BOTHPATHSTOKEN", 10, outside)
	if err != nil {
		t.Fatalf("searchKeywordChunks（范围外）: %v", err)
	}
	if containsChunkID(kwOut, "bp-cross") {
		t.Fatalf("关键词召回路把区间外的片段也带回来了，got %v", ids(kwOut))
	}
}

// TestIntegrationPageEndIsReadBackOnEveryRepositoryPath 是变异测试逼出来的
// 第二条用例。
//
// ⚠️ 发现经过：repository.go 有**四处**行映射要把 page_end 读出来（向量召回、
// 关键词召回、邻接查询、邻接批量查询）。逐处删掉那一行做变异测试，只有第一处
// 被既有用例抓住，另外三处**全部逃逸**。
//
// 逃逸的后果不是报错，是**静默**：那条路径返回的 chunk 的 PageEnd 恒为 nil，
// 违反不变量 C1；前端拿到 page_number 有值而 page_end 为 null 的响应，会按
// R3 的三分支落到「—」，用户看到的是"这个片段没有页码"——一个看起来正常的
// 界面，背后是一条被悄悄丢掉的引用信息。
//
// 四条路径逐条断言，不走融合：融合会让任何单路的回归被其他路掩盖（同
// TestIntegrationPageFilterIntersectionAppliesToBothRecallPaths 的教训）。
func TestIntegrationPageEndIsReadBackOnEveryRepositoryPath(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	kb := "kb-pageend-paths"
	seedKB(t, repo, kb, "m3", "u1", true)
	p3, p4, p6 := 3, 4, 6
	seedNeighborChunkBatch(t, repo, kb, "doc-pageend", 1, []neighborSeedChunk{
		{ID: "pe-anchor", ChunkIndex: 0, Content: "PAGEENDTOKEN 锚点片段的正文", Vec: []float32{1, 0, 0},
			PageNumber: &p3, PageEnd: &p4},
		{ID: "pe-neighbor", ChunkIndex: 1, Content: "邻接片段的正文", Vec: []float32{0, 1, 0},
			PageNumber: &p6, PageEnd: &p6},
	}, true)

	assertInterval := func(path string, chunks []RetrievedChunk, id string, wantStart, wantEnd int) {
		t.Helper()
		for _, c := range chunks {
			if c.ID != id {
				continue
			}
			if c.PageNumber == nil || c.PageEnd == nil {
				t.Fatalf("%s 返回的 %s 缺少页码区间的一端（C1）——这条路径的行映射漏读了 page_end：%+v", path, id, c)
			}
			if *c.PageNumber != wantStart || *c.PageEnd != wantEnd {
				t.Fatalf("%s 返回的 %s 区间是 %d-%d，应当是 %d-%d", path, id, *c.PageNumber, *c.PageEnd, wantStart, wantEnd)
			}
			return
		}
		t.Fatalf("%s 没有返回 %s：%v", path, id, ids(chunks))
	}

	vec, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	assertInterval("向量召回", vec, "pe-anchor", 3, 4)

	kw, err := repo.searchKeywordChunks(ctx, []string{kb}, "PAGEENDTOKEN", 10, RetrieveFilter{})
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	assertInterval("关键词召回", kw, "pe-anchor", 3, 4)

	nb, err := repo.findPublishedNeighborChunks(ctx, "doc-pageend", 1, []int{1})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunks: %v", err)
	}
	assertInterval("邻接查询", nb, "pe-neighbor", 6, 6)

	nbb, err := repo.findPublishedNeighborChunksBatch(ctx, []neighborRequest{
		{documentID: "doc-pageend", documentVersion: 1, chunkIndex: 1},
	})
	if err != nil {
		t.Fatalf("findPublishedNeighborChunksBatch: %v", err)
	}
	assertInterval("邻接批量查询", nbb, "pe-neighbor", 6, 6)
}

// --- 007-document-processing-notice US1：部分内容没进去时用户能看见 ---

// TestIntegrationPartialScanNoticeReachesDocumentList 是 SC-001 的验收用例。
//
// ⚠️ 它刻意走 **ListDocuments**（文档列表）而不是 getDocument（单查）。
// documents.sql 有三处 SELECT 要带出 unextracted_pages，漏掉列表那一处的话
// 单查用例照样绿——而用户**唯一**能看到这条提示的地方恰恰是列表。用单查来
// 验收这个功能，是在验一条用户永远走不到的路径。
//
// 夹具 F1：5 页 PDF，第 2、4 页没有文本层（真实场景里就是夹在电子文档中间的
// 扫描签字页）。改动前的行为是：3 页正常入库、文档 ready、而"有 2 页没进去"
// 这件事只存在于一条 slog.Warn 里，用户看不到任何东西。
func TestIntegrationPartialScanNoticeReachesDocumentList(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	// ⚠️ 纯 ASCII：writeTestPDF 用 Courier + WinAnsi，中文会渲染成乱码。
	path := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"", // 第 2 页：扫描图，无文本层
		"pagethree body text standing alone.",
		"", // 第 4 页：扫描图，无文本层
		"pagefive body text standing alone.",
	))
	seedDocForProcessing(t, repo, "kb-notice", "doc-notice", "contract.pdf", FileTypePDF, path)

	if err := svc.ProcessDocument(ctx, "doc-notice", 1); err != nil {
		t.Fatalf("部分扫描件应当正常入库有文本的部分，实际失败：%v", err)
	}

	docs, _, err := svc.ListDocuments(ctx, "kb-notice", 10, 0)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	var got *Document
	for i := range docs {
		if docs[i].ID == "doc-notice" {
			got = &docs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("文档列表里没有 doc-notice：%+v", docs)
	}

	// 文档本身必须是可用的——这条提示不是错误（FR-002）。
	if got.Status != StatusReady {
		t.Fatalf("文档 status=%s，应当是 ready（携带提示不等于失败）", got.Status)
	}
	if got.ChunkCount == 0 {
		t.Fatal("有文本的三页应当正常入库，chunk_count 不该是 0")
	}

	// ⭐ 这条是 SC-001 的核心：用户能从列表上看到「哪几页没进去」。
	if len(got.UnextractedPages) == 0 {
		t.Fatalf("文档列表没有带回缺页信息——用户无法知道有 2 页没进去，"+
			"这条信息目前只存在于一条给开发者看的日志里。got=%+v", got)
	}
	want := []int{2, 4}
	if len(got.UnextractedPages) != len(want) {
		t.Fatalf("缺页页码 = %v，应当是 %v", got.UnextractedPages, want)
	}
	for i, p := range want {
		if got.UnextractedPages[i] != p {
			t.Fatalf("缺页页码 = %v，应当是 %v（1-indexed、升序）", got.UnextractedPages, want)
		}
	}
}

// --- 007 US3：提示随重新处理而更新（FR-004 / SC-005） ---

// reprocessDoc 用一份新文件重新处理同一个文档：把 storage_path 换掉、版本推进，
// 走一遍完整的 ProcessDocument。模拟用户把缺失页做了 OCR 后重新上传。
func reprocessDoc(t *testing.T, repo *Repository, svc Service, docID string, version int64, newPath string) {
	t.Helper()
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE documents SET storage_path = ?, status = 'pending', version = ? WHERE id = ?",
		newPath, version, docID); err != nil {
		t.Fatalf("reprocess setup: %v", err)
	}
	if err := svc.ProcessDocument(ctx, docID, version); err != nil {
		t.Fatalf("ProcessDocument(v%d): %v", version, err)
	}
}

func docByID(t *testing.T, repo *Repository, id string) Document {
	t.Helper()
	d, err := repo.getDocument(context.Background(), id)
	if err != nil {
		t.Fatalf("getDocument: %v", err)
	}
	return d
}

// TestIntegrationNoticeDisappearsWhenPagesNoLongerMissing 是 SC-005。
//
// ⚠️ 这条用例背后**几乎没有实现代码**：提示的清除是「写入并进 MarkDocumentReady」
// 这个决策免费带来的——那条 UPDATE 本来就在无条件清 error_message，多一个字段
// 语义完全对称。正因为它是免费的，它也**特别容易在将来被无声地弄坏**：
// 任何人把那条 SQL 拆开、或者给提示单开一条写入语句，清除就没了，而表现是
// 用户看到一条过期的提示——没有报错，没有日志，没人会来报 bug。
// 这条断言就是拦这个的。
func TestIntegrationNoticeDisappearsWhenPagesNoLongerMissing(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())

	withGap := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"", // 缺页
		"pagethree body text standing alone.",
	))
	seedDocForProcessing(t, repo, "kb-clear", "doc-clear", "contract.pdf", FileTypePDF, withGap)
	if err := svc.ProcessDocument(context.Background(), "doc-clear", 1); err != nil {
		t.Fatalf("ProcessDocument(v1): %v", err)
	}
	if got := docByID(t, repo, "doc-clear"); len(got.UnextractedPages) != 1 || got.UnextractedPages[0] != 2 {
		t.Fatalf("第一次处理后应当有提示 [2]，实际 %v", got.UnextractedPages)
	}

	// 用户把第 2 页做了 OCR 后重新上传：现在三页都有文本。
	ocred := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"pagetwo body text now readable after ocr.",
		"pagethree body text standing alone.",
	))
	reprocessDoc(t, repo, svc, "doc-clear", 2, ocred)

	got := docByID(t, repo, "doc-clear")
	if got.Status != StatusReady {
		t.Fatalf("重新处理后 status=%s，应当是 ready", got.Status)
	}
	if len(got.UnextractedPages) != 0 {
		t.Fatalf("重新处理后不再缺页，提示必须消失，实际 %v —— "+
			"一条不会消失的提示很快就会变成没人相信的陈旧信息", got.UnextractedPages)
	}
}

// TestIntegrationNoticeAppearsWhenPagesBecomeMissing 是反向：原本干净的文档
// 被换成含扫描页的版本后，提示要出现。没有这一条，一个「永远写 NULL」的实现
// 也能让上面那条用例通过。
func TestIntegrationNoticeAppearsWhenPagesBecomeMissing(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())

	clean := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"pagetwo body text standing alone.",
	))
	seedDocForProcessing(t, repo, "kb-appear", "doc-appear", "contract.pdf", FileTypePDF, clean)
	if err := svc.ProcessDocument(context.Background(), "doc-appear", 1); err != nil {
		t.Fatalf("ProcessDocument(v1): %v", err)
	}
	if got := docByID(t, repo, "doc-appear"); len(got.UnextractedPages) != 0 {
		t.Fatalf("干净文档不该有提示，实际 %v", got.UnextractedPages)
	}

	withGap := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"", // 换成扫描页
		"pagethree body text standing alone.",
	))
	reprocessDoc(t, repo, svc, "doc-appear", 2, withGap)

	got := docByID(t, repo, "doc-appear")
	if len(got.UnextractedPages) != 1 || got.UnextractedPages[0] != 2 {
		t.Fatalf("换成含扫描页的版本后应当出现提示 [2]，实际 %v", got.UnextractedPages)
	}
}

// TestIntegrationFailedReprocessDoesNotShowStaleNotice 是 FR-005。
//
// 一份曾经成功、带着提示的文档，重新处理时失败了。此时用户看到的必须是
// **失败原因**，而不是上一次成功留下的旧提示。
//
// ⚠️ 数据上那个字段**可能仍然有值**（它是上一次成功写的，这次根本没走到
// MarkDocumentReady）。所以判断依据是 **status**，不是字段有没有值——
// 这正是契约 C5 单独点出这一条的原因。
func TestIntegrationFailedReprocessDoesNotShowStaleNotice(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())

	withGap := writeTestPDF(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"",
		"pagethree body text standing alone.",
	))
	seedDocForProcessing(t, repo, "kb-stale", "doc-stale", "contract.pdf", FileTypePDF, withGap)
	if err := svc.ProcessDocument(context.Background(), "doc-stale", 1); err != nil {
		t.Fatalf("ProcessDocument(v1): %v", err)
	}
	if got := docByID(t, repo, "doc-stale"); len(got.UnextractedPages) == 0 {
		t.Fatal("第一次处理后应当有提示")
	}

	// 换成一份纯扫描件：整份无文本层 → 失败（006 的 FR-017）。
	scanned := writeTestPDF(t, pdfLinesFromStrings("", "", ""))
	ctx := context.Background()
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE documents SET storage_path = ?, status = 'pending', version = 2 WHERE id = 'doc-stale'",
		scanned); err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessDocument(ctx, "doc-stale", 2); !errors.Is(err, ErrPDFNoTextLayer) {
		t.Fatalf("纯扫描件应当失败于 ErrPDFNoTextLayer，实际 %v", err)
	}

	got := docByID(t, repo, "doc-stale")
	if got.Status != StatusFailed {
		t.Fatalf("status=%s，应当是 failed", got.Status)
	}
	if got.ErrorMessage == "" {
		t.Fatal("失败文档应当有 error_message")
	}
	// ⭐ 契约 C5 的实质：字段里可能还留着上一次的值，展示层**必须靠 status 判断**，
	// 不能靠"字段有没有值"。这里断言的是这个前提确实成立——即字段确实可能残留。
	// 前端据此必须 gate 在 status === "ready" 上（见 knowledge-documents-dialog.tsx）。
	t.Logf("失败后残留的 UnextractedPages=%v（这正是前端必须按 status 判断的原因）", got.UnextractedPages)
}

// TestIntegrationTxtAndMarkdownNeverCarryNotice 是 SC-007（007）/ SC-008（008）：
// 没有"页"概念的格式，**两类**提示都恒为空。
func TestIntegrationTxtAndMarkdownNeverCarryNotice(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()
	dir := t.TempDir()

	for _, tc := range []struct{ id, name, fileType, body string }{
		{"doc-nn-txt", "plain.txt", FileTypeTxt, "一段普通正文。\n\n另一段正文。"},
		{"doc-nn-md", "notes.md", FileTypeMD, "# 标题\n\n正文一段。\n\n## 二级\n\n正文两段。"},
	} {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		seedDocForProcessing(t, repo, "kb-nn-"+tc.id, tc.id, tc.name, tc.fileType, p)
		if err := svc.ProcessDocument(ctx, tc.id, 1); err != nil {
			t.Fatalf("ProcessDocument(%s): %v", tc.fileType, err)
		}
		// 008：两类都要断言。只断言其中一类的话，另一类被误写也不会被发现——
		// 变异测试实证过：把 unparseablePages 改成恒 []int{1}，这条用例照样绿。
		got := docByID(t, repo, tc.id)
		if len(got.UnextractedPages) != 0 || len(got.UnparseablePages) != 0 {
			t.Fatalf("%s 文档带上了缺页提示（unextracted=%v unparseable=%v）—— "+
				"它根本没有「页」这个概念",
				tc.fileType, got.UnextractedPages, got.UnparseablePages)
		}
	}
}

// --- 008-unparseable-page-notice US1：解析失败的页也要能被用户看见 ---

// TestIntegrationUnparseablePageNoticeReachesDocumentList 是 SC-001 的验收用例。
//
// ⚠️ 这里的"缺失"与 007 那条用例是**不同的机制**：007 覆盖的是「页面没有文本层」
// （扫描图），本条覆盖的是「页面根本解析不了」——006 给 rsc.io/pdf 加了逐页
// recover 之后，这种页被整页跳过，于是它**在统计有没有文本层之前就已经消失了**。
// 结果是 textLayerCoverage 看到的每一页都有文本，缺页列表为空，用户看到一份
// 「就绪、无提示」的文档，而那一页根本不在里面。
//
// 实测撞到过：一篇 15 页的 arXiv 论文第 1 页解析失败被跳过，007 的提示一个字
// 都不会出现。这就是 007 只覆盖了两种缺失里的一种。
//
// 同样刻意走 ListDocuments 而不是单查——用户唯一能看到提示的地方是列表。
func TestIntegrationUnparseablePageNoticeReachesDocumentList(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	// 第 2 页的 /Contents 指向一个不存在的对象：rsc.io/pdf 会在 Page.Content()
	// 上 panic，正是 006 的 safePageText 兜住的那条路径。其余两页正常。
	path := writeTestPDFWithBrokenPages(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"pagetwo body text standing alone.",
		"pagethree body text standing alone.",
	), []int{2})
	seedDocForProcessing(t, repo, "kb-unparseable", "doc-unparseable", "paper.pdf", FileTypePDF, path)

	if err := svc.ProcessDocument(ctx, "doc-unparseable", 1); err != nil {
		t.Fatalf("有两页正常，文档应当正常入库，实际失败：%v", err)
	}

	docs, _, err := svc.ListDocuments(ctx, "kb-unparseable", 10, 0)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	var got *Document
	for i := range docs {
		if docs[i].ID == "doc-unparseable" {
			got = &docs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("文档列表里没有 doc-unparseable：%+v", docs)
	}
	if got.Status != StatusReady {
		t.Fatalf("status=%s，应当是 ready（有两页正常入库了）", got.Status)
	}

	// ⭐ SC-001 的核心。
	if len(got.UnparseablePages) != 1 || got.UnparseablePages[0] != 2 {
		t.Fatalf("解析失败的页码 = %v，应当是 [2]——第 2 页根本没进知识库，"+
			"而用户在列表上看不到任何迹象。got=%+v", got.UnparseablePages, got)
	}
	// FR-003：两类互不重叠。这一页从没进过 textLayerCoverage 的视野。
	for _, p := range got.UnextractedPages {
		if p == 2 {
			t.Fatalf("第 2 页同时出现在两个列表里，两类必须互不重叠（FR-003）："+
				"unextracted=%v unparseable=%v", got.UnextractedPages, got.UnparseablePages)
		}
	}
}

// TestIntegrationBothFailureKindsStaySeparate 是 US2 / SC-003 / SC-004。
//
// ⚠️ 这条用例**是 008 不并进 unextracted_pages 那一列的全部理由**。如果两类最终
// 仍被搅成一句笼统的「有 N 页没进去」，这一整期的设计成本就白付了——还不如当初
// 直接塞进去。所以这里断言的不只是"两类都有值"，还有它们**互不重叠**。
func TestIntegrationBothFailureKindsStaySeparate(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	// 5 页：第 2 页无文本层（扫描图），第 4 页解析失败，其余正常。
	path := writeTestPDFWithBrokenPages(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"", // 第 2 页：有页面、无文本
		"pagethree body text standing alone.",
		"placeholder that will never be read",
		"pagefive body text standing alone.",
	), []int{4})
	seedDocForProcessing(t, repo, "kb-both", "doc-both", "mixed.pdf", FileTypePDF, path)
	if err := svc.ProcessDocument(ctx, "doc-both", 1); err != nil {
		t.Fatalf("三页正常，文档应当入库，实际失败：%v", err)
	}

	got := docByID(t, repo, "doc-both")
	if len(got.UnextractedPages) != 1 || got.UnextractedPages[0] != 2 {
		t.Fatalf("无文本层的页 = %v，应当是 [2]", got.UnextractedPages)
	}
	if len(got.UnparseablePages) != 1 || got.UnparseablePages[0] != 4 {
		t.Fatalf("解析失败的页 = %v，应当是 [4]", got.UnparseablePages)
	}

	// FR-003 / SC-004：互不重叠。这条看起来是废话——解析失败的页在
	// extractPDFPages 里就被跳过，根本进不了 textLayerCoverage 的视野，物理上
	// 不可能重叠。**正因为它显然成立才值得断言**：一旦不成立，说明上游的跳过
	// 逻辑变了，而那是个没有任何报错、也没人会注意到的变化。
	seen := map[int]bool{}
	for _, p := range got.UnextractedPages {
		seen[p] = true
	}
	for _, p := range got.UnparseablePages {
		if seen[p] {
			t.Fatalf("第 %d 页同时出现在两个列表里：unextracted=%v unparseable=%v",
				p, got.UnextractedPages, got.UnparseablePages)
		}
	}
}

// TestIntegrationOnlyOneFailureKindLeavesTheOtherEmpty 是 FR-008：只有一类缺失
// 时另一类必须为空，界面才不会出现一段空提示。没有这条，一个"两类总是一起写"
// 的实现也能让上面那条通过。
func TestIntegrationOnlyOneFailureKindLeavesTheOtherEmpty(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	// 只有解析失败，没有无文本层的页。
	path := writeTestPDFWithBrokenPages(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"placeholder that will never be read",
		"pagethree body text standing alone.",
	), []int{2})
	seedDocForProcessing(t, repo, "kb-onlyone", "doc-onlyone", "one.pdf", FileTypePDF, path)
	if err := svc.ProcessDocument(ctx, "doc-onlyone", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	got := docByID(t, repo, "doc-onlyone")
	if len(got.UnparseablePages) != 1 {
		t.Fatalf("解析失败的页 = %v，应当是 [2]", got.UnparseablePages)
	}
	if len(got.UnextractedPages) != 0 {
		t.Fatalf("没有无文本层的页，该列表必须为空，实际 %v——"+
			"否则界面会出现一段空提示", got.UnextractedPages)
	}
}

// TestIntegrationNoticeSurvivesReconciliationRecovery 是 US3 / SC-005，
// 修的是 **007 报告 §6.2 记录的已知缺陷**。
//
// 场景：文档已经把活儿干完、进入 publishing，但发布确认那一步没跑完（worker
// 崩了），由 ReconcileStuckDocuments 接手完成。恢复流程**没有重新解析过文件**，
// 因此它不可能知道缺页情况——007 时它只能传 nil，于是把提示清空了。
//
// 008 把写入前移到 markDocumentPublishing 之后，恢复流程什么都不用知道：
// publishing 阶段写下的值原样存活。**这条用例在 007 的实现上是 FAIL 的。**
func TestIntegrationNoticeSurvivesReconciliationRecovery(t *testing.T) {
	repo := setupIntegration(t)
	svc := newTestService(repo, newFakeProvider(), t.TempDir())
	ctx := context.Background()

	path := writeTestPDFWithBrokenPages(t, pdfLinesFromStrings(
		"pageone body text standing alone.",
		"placeholder that will never be read",
		"pagethree body text standing alone.",
	), []int{2})
	seedDocForProcessing(t, repo, "kb-recover", "doc-recover-notice", "paper.pdf", FileTypePDF, path)
	if err := svc.ProcessDocument(ctx, "doc-recover-notice", 1); err != nil {
		t.Fatalf("ProcessDocument: %v", err)
	}
	before := docByID(t, repo, "doc-recover-notice")
	if len(before.UnparseablePages) != 1 {
		t.Fatalf("前置条件不成立：应当先有提示，实际 %v", before.UnparseablePages)
	}

	// 把文档打回 publishing（模拟"活儿干完了、发布确认没跑完"），
	// 然后走恢复路径完成它——恢复流程只调 markDocumentReady。
	if _, err := repo.db.ExecContext(ctx,
		"UPDATE documents SET status = 'publishing', lease_expires_at = ? WHERE id = 'doc-recover-notice'",
		time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	svcImpl := &service{repo: repo, providerSvc: newFakeProvider(), storageDir: t.TempDir()}
	if err := svcImpl.publishAndComplete(ctx, "doc-recover-notice", 1); err != nil {
		t.Fatalf("publishAndComplete（恢复路径）: %v", err)
	}

	after := docByID(t, repo, "doc-recover-notice")
	if after.Status != StatusReady {
		t.Fatalf("恢复后 status=%s，应当是 ready", after.Status)
	}
	if len(after.UnparseablePages) != 1 || after.UnparseablePages[0] != 2 {
		t.Fatalf("恢复路径把提示弄丢了：恢复前 %v，恢复后 %v。"+
			"恢复流程没有重新解析过文件，它不该有能力清空 publishing 阶段写对的值",
			before.UnparseablePages, after.UnparseablePages)
	}
}
