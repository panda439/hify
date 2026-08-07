package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"

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

func TestIntegrationSearchChunksOrderingAndDimensionFilter(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	kb := "kb-search"
	seedChunk(t, repo, kb, "doc-s", "c-exact", []float32{1, 0, 0}) // cos = 1.0
	seedChunk(t, repo, kb, "doc-s", "c-mid", []float32{1, 1, 0})   // cos ≈ 0.7071
	seedChunk(t, repo, kb, "doc-s", "c-ortho", []float32{0, 1, 0}) // cos = 0
	seedChunk(t, repo, kb, "doc-s", "c-2d", []float32{1, 0})       // 异维度，必须被过滤

	got, err := repo.searchChunks(ctx, []string{kb}, []float32{1, 0, 0}, 10)
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
	top1, err := repo.searchChunks(ctx, []string{kb}, []float32{1, 0, 0}, 1)
	if err != nil || len(top1) != 1 || top1[0].ID != "c-exact" {
		t.Fatalf("topK=1 = %v (err %v), want [c-exact]", ids(top1), err)
	}

	// knowledge_base_id 过滤：别的 KB 查不到这些 chunk。
	other, err := repo.searchChunks(ctx, []string{"kb-nonexistent"}, []float32{1, 0, 0}, 10)
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
	got, err := repo.searchChunks(ctx, []string{"kb-race"}, []float32{1, 0, 0}, 10)
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
	if got, err := repo.searchChunks(ctx, []string{"kb-pubfail"}, []float32{1, 0, 0}, 10); err != nil || len(got) != 0 {
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

	found, err := repo.searchChunks(ctx, []string{"kb-pubfail"}, []float32{1, 0, 0}, 10)
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
	if got, err := repo.searchChunks(ctx, []string{"kb-pubcrash"}, []float32{1, 0, 0}, 10); err != nil || len(got) != 1 {
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
	if found, err := repo.searchChunks(ctx, []string{"kb-publease"}, []float32{1, 0, 0}, 10); err != nil || len(found) != 1 {
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
	if found, err := repo.searchChunks(ctx, []string{"kb-toctou-pub"}, []float32{1, 0, 0}, 10); err != nil || len(found) != 0 {
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
	found, err := repo.searchChunks(ctx, []string{"kb-toctou-pub2"}, []float32{1, 0, 0}, 10)
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

	got, err := repo.searchChunks(ctx, []string{"kb-docname"}, []float32{1, 0, 0}, 10)
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

	chunks, err := repo.searchChunks(ctx, []string{"kb-pdfpage"}, []float32{1, 0, 0}, 100)
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
	chunks, err := repo.searchChunks(ctx, []string{"kb-mdsection"}, []float32{1, 0, 0}, 100)
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

	got, err := repo.searchChunks(ctx, []string{"kb-legacy"}, []float32{1, 0, 0}, 10)
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
