package provider

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"hify/internal/platform"
)

// TestVoyageRerankLive is an opt-in acceptance test. It resolves the model
// through Hify's real repository and registry, so the stored API key is read
// from MySQL, decrypted, and used by the same client path as knowledge.Retrieve.
func TestVoyageRerankLive(t *testing.T) {
	modelID := os.Getenv("HIFY_LIVE_VOYAGE_MODEL_ID")
	if modelID == "" {
		t.Skip("HIFY_LIVE_VOYAGE_MODEL_ID is not set")
	}

	db, err := platform.NewMySQLPool(os.Getenv("HIFY_MYSQL_DSN"))
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	defer db.Close()

	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("HIFY_REDIS_ADDR")})
	defer rdb.Close()

	svc, err := NewService(NewRepository(db), os.Getenv("HIFY_ENCRYPTION_KEY"), rdb)
	if err != nil {
		t.Fatalf("create provider service: %v", err)
	}
	model, err := svc.GetModel(context.Background(), modelID)
	if err != nil {
		t.Fatalf("get model: %v", err)
	}
	client, err := svc.ResolveClient(context.Background(), model.ProviderID)
	if err != nil {
		t.Fatalf("resolve client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := client.Rerank(ctx, RerankRequest{
		Model:     model.ModelName,
		Query:     "Go语言如何处理并发？",
		Documents: []string{"Go通过goroutine和channel实现并发。", "苹果是一种水果。", "MySQL是一种关系型数据库。"},
	})
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	if len(result.Scores) != 3 {
		t.Fatalf("score count = %d, want 3", len(result.Scores))
	}
	t.Logf("model=%s scores=[%.6f %.6f %.6f] total_tokens=%d", model.ModelName, result.Scores[0].Score, result.Scores[1].Score, result.Scores[2].Score, result.TotalTokens)
	if result.Scores[0].Score <= result.Scores[1].Score || result.Scores[0].Score <= result.Scores[2].Score {
		t.Fatalf("relevant document score %f is not highest", result.Scores[0].Score)
	}
	// 014 T011：真实 Voyage 响应必须带回 usage.total_tokens，否则托管 Rerank
	// 的费用证据无从核对。
	if result.TotalTokens <= 0 {
		t.Fatalf("total_tokens = %d, want real usage from Voyage response", result.TotalTokens)
	}
}
