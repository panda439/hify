package provider

import (
	"context"
	"testing"

	"hify/internal/platform"
	"hify/internal/testutil"
)

// T016：migration 000012 的正确性验证。宪法第 VI 条禁止把"应该没问题"当
// 证据，而 knowledge 包的集成测试虽然也跑同一条迁移链，但从不写
// provider_models 表——不足以证明 CHECK 约束真的放行了 'rerank'、真的仍
// 然拒绝非法值。这里直接对 testutil 建的全新 hify_test_provider 库（跑完
// 整迁移链，含 000012）做一次真实写入，是唯一能证明约束本身改对了的证据
// （worktree 不允许对开发库跑 make migrate-up，见任务说明）。
func TestMigration000012AllowsRerankCapabilityRejectsInvalid(t *testing.T) {
	db := testutil.MySQL(t, "provider")
	repo := NewRepository(db)
	ctx := context.Background()

	providerID := platform.NewID()
	if err := repo.createProvider(ctx, Provider{
		ID: providerID, Name: "p-" + providerID, AdapterType: AdapterOpenAICompatible,
		BaseURL: "http://example.invalid", AuthType: AuthTypeNone, CreatedBy: "u1",
	}, nil); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	if err := repo.createModel(ctx, Model{
		ID: platform.NewID(), ProviderID: providerID, ModelName: "bge-reranker-v2-m3",
		Capability: CapabilityRerank, IsActive: true,
	}); err != nil {
		t.Fatalf("insert capability=rerank model, want the CHECK constraint to allow it after migration 000012: %v", err)
	}

	err := repo.createModel(ctx, Model{
		ID: platform.NewID(), ProviderID: providerID, ModelName: "bogus-model",
		Capability: "not_a_real_capability", IsActive: true,
	})
	if err == nil {
		t.Fatal("insert capability='not_a_real_capability' model succeeded, want the CHECK constraint to still reject unknown values")
	}
}
