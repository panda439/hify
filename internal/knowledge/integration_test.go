package knowledge

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	// asynq client 传 nil：这些用例不走 UploadDocument 的入队路径。
	return NewService(repo, fp, nil, storageDir)
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

func seedChunk(t *testing.T, repo *Repository, kbID, docID, chunkID string, vec []float32) {
	t.Helper()
	err := repo.createChunks(context.Background(), []Chunk{{
		ID: chunkID, KnowledgeBaseID: kbID, DocumentID: docID, ChunkIndex: 0,
		Content: "content-" + chunkID, ContentLength: 1,
		Embedding: vec, EmbeddingDimension: len(vec),
	}})
	if err != nil {
		t.Fatalf("seed chunk %s: %v", chunkID, err)
	}
}

// --- 链路 3：pgvector 检索 ---

func TestIntegrationSearchChunksOrderingAndDimensionFilter(t *testing.T) {
	repo := setupIntegration(t)
	ctx := context.Background()

	kb := "kb-search"
	seedChunk(t, repo, kb, "doc-s", "c-exact", []float32{1, 0, 0})   // cos = 1.0
	seedChunk(t, repo, kb, "doc-s", "c-mid", []float32{1, 1, 0})     // cos ≈ 0.7071
	seedChunk(t, repo, kb, "doc-s", "c-ortho", []float32{0, 1, 0})   // cos = 0
	seedChunk(t, repo, kb, "doc-s", "c-2d", []float32{1, 0})         // 异维度，必须被过滤

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

	if err := svc.ProcessDocument(ctx, "doc-ok"); err != nil {
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

	if err := svc.ProcessDocument(ctx, "doc-mis"); err == nil {
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

	if err := svc.ProcessDocument(ctx, "doc-gone"); err == nil {
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
