package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	pgvector "github.com/pgvector/pgvector-go"

	"hify/internal/platform"
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

func newTestService(repo *Repository, fp *fakeProviderService, storageDir string) Service {
	// asynq client 传 nil：这些用例不走 UploadDocument/RetryDocument 的入队
	// 路径——真正需要入队（比如验证 RetryDocument 重新排队）的用例改用
	// newTestAsynqClient 构造一个连真实 Redis 的 client。
	return NewService(repo, fp, nil, storageDir)
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

	got, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10)
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
	top1, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 1)
	if err != nil || len(top1) != 1 || top1[0].ID != "c-exact" {
		t.Fatalf("topK=1 = %v (err %v), want [c-exact]", ids(top1), err)
	}

	// knowledge_base_id 过滤：别的 KB 查不到这些 chunk。
	other, err := repo.searchVectorChunks(ctx, []string{"kb-nonexistent"}, []float32{1, 0, 0}, 10)
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
	seedChunk(t, repo, "kb-r3", "doc-r", "r3-weak", []float32{1, 4, 0})
	seedChunk(t, repo, "kb-r2", "doc-r", "r2-hit", []float32{1, 0})
	seedChunk(t, repo, "kb-off", "doc-r", "off-hit", []float32{1, 0, 0})

	got, err := svc.Retrieve(ctx, []string{"kb-r3", "kb-r2", "kb-off", "kb-ghost"}, "查询", 3)
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
	if r, err := svc.Retrieve(ctx, nil, "q", 5); err != nil || r != nil {
		t.Fatalf("Retrieve(nil kbs) = %v, %v; want nil, nil", r, err)
	}
	if r, err := svc.Retrieve(ctx, []string{"kb-r3"}, "", 5); err != nil || r != nil {
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

	got, err := svc.Retrieve(ctx, []string{"kb-dedup-e2e"}, "查询", 2)
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

	got, err := svc.Retrieve(ctx, []string{"kb-dedup-nb-e2e"}, "查询", 2)
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
	got, err := svc.Retrieve(ctx, []string{"kb-del"}, "查询", 10)
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
	publishing, err := repo.markDocumentPublishing(ctx, "doc-race", 1, time.Now().Add(leaseDuration))
	if err != nil {
		t.Fatalf("markDocumentPublishing: %v", err)
	}
	if publishing {
		t.Fatal("markDocumentPublishing succeeded against a deleted document — should have been fenced")
	}

	// 即使 chunk 物理写入了，因为从未发布，检索永远看不到它。
	got, err := repo.searchVectorChunks(ctx, []string{"kb-race"}, []float32{1, 0, 0}, 10)
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

	got, err := svc.Retrieve(ctx, []string{"kb-topk"}, "查询", 999999)
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
	publishing, err := repo.markDocumentPublishing(ctx, "doc-cas", 1, time.Now().Add(leaseDuration))
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
	svc := NewService(repo, newFakeProvider(), newTestAsynqClient(t), t.TempDir())
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-retry-ready", 1, time.Now().Add(leaseDuration)); err != nil || !ok {
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-pubfail", 1, time.Now().Add(-time.Minute)); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// 此时文档处于 publishing，chunks 未发布，检索不到。
	if got, err := repo.searchVectorChunks(ctx, []string{"kb-pubfail"}, []float32{1, 0, 0}, 10); err != nil || len(got) != 0 {
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

	found, err := repo.searchVectorChunks(ctx, []string{"kb-pubfail"}, []float32{1, 0, 0}, 10)
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-pubcrash", 1, time.Now().Add(-time.Minute)); err != nil || !ok {
		t.Fatalf("markDocumentPublishing setup = %v, %v", ok, err)
	}

	// 模拟"PG 发布已经成功，但 worker 在 CAS ready 之前崩溃"：这里先真的
	// 调一次 publishDocumentVersion，让 chunks 已经处于已发布状态，MySQL
	// 侧却还停在 publishing。
	if err := repo.publishDocumentVersion(ctx, "doc-pubcrash", 1); err != nil {
		t.Fatalf("simulate pre-crash publish: %v", err)
	}
	if got, err := repo.searchVectorChunks(ctx, []string{"kb-pubcrash"}, []float32{1, 0, 0}, 10); err != nil || len(got) != 1 {
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-pubdead", 1, time.Now().Add(leaseDuration)); err != nil || !ok {
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-publease", 1, time.Now().Add(leaseDuration)); err != nil || !ok {
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
	if found, err := repo.searchVectorChunks(ctx, []string{"kb-publease"}, []float32{1, 0, 0}, 10); err != nil || len(found) != 1 {
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
	svc := NewService(repo, fp, newTestAsynqClient(t), dir) // reconciliation 的回收要真的入队
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-toctou-pub", 1, time.Now().Add(-time.Minute)); err != nil || !ok {
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
	if found, err := repo.searchVectorChunks(ctx, []string{"kb-toctou-pub"}, []float32{1, 0, 0}, 10); err != nil || len(found) != 0 {
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
	if ok, err := repo.markDocumentPublishing(ctx, "doc-toctou-pub2", 1, time.Now().Add(-time.Minute)); err != nil || !ok {
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
	found, err := repo.searchVectorChunks(ctx, []string{"kb-toctou-pub2"}, []float32{1, 0, 0}, 10)
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

	got, err := repo.searchVectorChunks(ctx, []string{"kb-docname"}, []float32{1, 0, 0}, 10)
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
	pdfPath := writeTestPDF(t, []string{
		strings.Repeat("alphaword ", 15), // page 1, long enough to split into >=1 chunk
		strings.Repeat("betaword ", 15),  // page 2
	})
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

	chunks, err := repo.searchVectorChunks(ctx, []string{"kb-pdfpage"}, []float32{1, 0, 0}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) == 0 {
		t.Fatal("expected at least one searchable chunk")
	}
	for _, c := range chunks {
		if c.SectionTitle != nil {
			t.Fatalf("pdf chunk must never fabricate a section title: %+v", c)
		}
		if c.PageNumber == nil {
			t.Fatalf("pdf chunk missing page number: %+v", c)
		}
		hasAlpha := strings.Contains(c.Content, "alphaword")
		hasBeta := strings.Contains(c.Content, "betaword")
		if hasAlpha && hasBeta {
			t.Fatalf("chunk spans both pages, page number would be unreliable: %+v", c)
		}
		if hasAlpha && *c.PageNumber != 1 {
			t.Fatalf("alpha content tagged with page %d, want 1: %+v", *c.PageNumber, c)
		}
		if hasBeta && *c.PageNumber != 2 {
			t.Fatalf("beta content tagged with page %d, want 2: %+v", *c.PageNumber, c)
		}
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
	chunks, err := repo.searchVectorChunks(ctx, []string{"kb-mdsection"}, []float32{1, 0, 0}, 100)
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

	got, err := repo.searchVectorChunks(ctx, []string{"kb-legacy"}, []float32{1, 0, 0}, 10)
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
		DocumentName: documentName, PageNumber: pageNumber, SectionTitle: sectionTitle,
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

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "深度学习模型", 10)
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

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "pgvector extension", 10)
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

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "机密项目Zeta", 10)
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

	got, err := repo.searchKeywordChunks(ctx, []string{"kb-kw-b"}, "跨知识库隔离测试关键词CROSSKBTOKEN", 10)
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty — kb-kw-a's chunk must not leak into a kb-kw-b-scoped search", ids(got))
	}

	// Sanity check the positive case too, so a bug that made the filter a
	// no-op wouldn't be masked by both sides returning empty.
	gotOwn, err := repo.searchKeywordChunks(ctx, []string{"kb-kw-a"}, "跨知识库隔离测试关键词CROSSKBTOKEN", 10)
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

	got, err := repo.searchKeywordChunks(ctx, []string{kb}, "维度隔离验证关键词DIMTOKEN", 10)
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

	got, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10)
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

	got, err := repo.searchVectorChunks(ctx, []string{kb}, vec, 10)
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

	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	keywordChunks, err := repo.searchKeywordChunks(ctx, []string{kb}, queryText, cK)
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

	fused, _ := rrfFuse(vectorChunks, keywordChunks, topK)
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
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	keywordChunks, err := repo.searchKeywordChunks(ctx, []string{kb}, queryText, cK)
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

	fused, _ := rrfFuse(vectorChunks, keywordChunks, 5)
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

	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vectorChunks) != 1 || vectorChunks[0].DocumentName != "policy-handbook.pdf" ||
		vectorChunks[0].PageNumber == nil || *vectorChunks[0].PageNumber != page ||
		vectorChunks[0].SectionTitle == nil || *vectorChunks[0].SectionTitle != section {
		t.Fatalf("vector path lost Citation metadata: %+v", vectorChunks)
	}

	keywordChunks, err := repo.searchKeywordChunks(ctx, []string{kb}, "引用元数据验证关键词CITETOKEN", 10)
	if err != nil {
		t.Fatalf("searchKeywordChunks: %v", err)
	}
	if len(keywordChunks) != 1 || keywordChunks[0].DocumentName != "policy-handbook.pdf" ||
		keywordChunks[0].PageNumber == nil || *keywordChunks[0].PageNumber != page ||
		keywordChunks[0].SectionTitle == nil || *keywordChunks[0].SectionTitle != section {
		t.Fatalf("keyword path lost Citation metadata: %+v", keywordChunks)
	}

	fused, _ := rrfFuse(vectorChunks, keywordChunks, 10)
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
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vectorChunks) != 3 {
		t.Fatalf("test setup invalid: got %d raw candidates %v, want all 3 seeded chunks back before dedup", len(vectorChunks), ids(vectorChunks))
	}

	fused, _ := rrfFuse(vectorChunks, nil, topK)

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
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}

	anchors, _ := rrfFuse(vectorChunks, nil, topK)
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
	vectorChunks, err := repo.searchVectorChunks(ctx, []string{kb}, queryVec, cK)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vectorChunks) != 3 {
		t.Fatalf("test setup invalid: got %d candidates %v, want all 3 seeded chunks visible to vector search", len(vectorChunks), ids(vectorChunks))
	}

	// Note: with only 3 chunks total in this KB, candidateK(2)=8 means all
	// 3 are visible to rrfFuse's full (not-yet-topK'd) candidate pool — so
	// anchor-prev's content duplicate of core2 can legitimately be caught
	// at EITHER the core-dedup stage (rrfFuse, since anchor-prev is
	// present in that same pool and loses to core2's higher cosine score)
	// or the neighbor-dedup stage (expandWithNeighbors, since
	// findPublishedNeighborChunks re-fetches anchor-prev independently of
	// whatever rrfFuse did with it). Which exact stage catches it is an
	// incidental detail of this KB's small size/candidateK, not something
	// this test asserts on — what matters, and what's asserted below, is
	// that the FINAL result is correct either way: anchor and core2 (both
	// real core hits) both survive, anchor-prev never does, and dedup
	// happened somewhere in the pipeline.
	anchors, coreDuplicateCount := rrfFuse(vectorChunks, nil, topK)
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
	if coreDuplicateCount+neighborDuplicateCount < 1 {
		t.Fatalf("coreDuplicateCount(%d) + neighborDuplicateCount(%d) = 0, want at least 1 — anchor-prev's duplicate content must be caught by dedup somewhere in the pipeline", coreDuplicateCount, neighborDuplicateCount)
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

	if got, err := repo.searchKeywordChunks(ctx, []string{kb}, "", 10); err != nil || got != nil {
		t.Fatalf("searchKeywordChunks(empty query) = %v, %v; want nil, nil", got, err)
	}
	if got, err := repo.searchKeywordChunks(ctx, nil, "ANYTOKEN", 10); err != nil || got != nil {
		t.Fatalf("searchKeywordChunks(nil kbIDs) = %v, %v; want nil, nil", got, err)
	}
	if got, err := repo.searchKeywordChunks(ctx, []string{}, "ANYTOKEN", 10); err != nil || got != nil {
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
	SectionTitle *string
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
			DocumentName: c.DocumentName, PageNumber: c.PageNumber, SectionTitle: c.SectionTitle,
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

	vecGot, err := repo.searchVectorChunks(ctx, []string{kbID}, []float32{1, 0, 0}, 10)
	if err != nil {
		t.Fatalf("searchVectorChunks: %v", err)
	}
	if len(vecGot) != 1 || vecGot[0].DocumentVersion != 7 {
		t.Fatalf("searchVectorChunks DocumentVersion = %+v, want DocumentVersion=7", vecGot)
	}

	kwGot, err := repo.searchKeywordChunks(ctx, []string{kbID}, "版本标记验证关键词VERSIONTAG", 10)
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

	svc := &service{repo: brokenRepo, providerSvc: newFakeProvider(), storageDir: t.TempDir()}
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
	svc := &service{repo: repo, providerSvc: newFakeProvider(), storageDir: t.TempDir()}

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
