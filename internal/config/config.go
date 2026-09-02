// Package config loads Hify's runtime configuration from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env      string
	HTTPAddr string

	MySQLDSN string
	// PostgresDSN points at the pgvector-enabled PostgreSQL that stores
	// chunks (content + embedding vectors). MySQL remains the primary
	// business-data store; PG exists solely for vector retrieval.
	PostgresDSN string

	RedisAddr     string
	RedisPassword string
	RedisDB       int

	JWTSecret     string
	JWTAccessTTL  time.Duration
	JWTRefreshTTL time.Duration

	EncryptionKey string

	CORSAllowedOrigins []string

	// AdminEmail/AdminPassword are only required by the `seed-admin`
	// subcommand, not by serve/migrate — validated there, not here.
	AdminEmail    string
	AdminPassword string

	// KnowledgeStorageDir is where uploaded knowledge-base documents are
	// written to local disk — see the plan's known limitation that this
	// isn't multi-instance safe (fine at Hify's current single-instance
	// deployment scale).
	KnowledgeStorageDir string
	// AsynqConcurrency caps the background worker so document processing
	// can't starve the online API request path running in the same
	// process (see "已知性能风险" #3).
	AsynqConcurrency int

	// RAGQueryRewriteEnabled/RAGQueryRewriteModelID/RAGQueryRewriteTimeout
	// gate conversation's query-rewrite step (see
	// specs/001-rag-query-rerank/data-model.md §3). Default false keeps
	// output identical to pre-feature behavior (SC-003). An empty
	// RAGQueryRewriteModelID means "use the current Agent's own chat
	// model" — see conversation/queryrewrite.go.
	RAGQueryRewriteEnabled bool
	RAGQueryRewriteModelID string
	RAGQueryRewriteTimeout time.Duration

	// RAGRerankEnabled/RAGRerankModelID/RAGRerankTimeout gate knowledge's
	// rerank step. Unlike query rewrite, RAGRerankModelID has no "use the
	// current model" fallback — it must point at a capability='rerank'
	// model, or rerank silently degrades to disabled (validated in Load,
	// not left to fail the whole process — see below).
	RAGRerankEnabled bool
	RAGRerankModelID string
	RAGRerankTimeout time.Duration

	// RAGMetadataFilterEnabled 是检索元数据过滤（002-metadata-filter）的开关。
	//
	// **默认值在 004-agent-document-scope 从 false 改成了 true。**
	// 002 交付时默认关闭是对的——当时没有任何调用方会传非空过滤器。
	// 004 让 Agent 能绑定文档范围之后，关闭它反而制造了一条静默降级路径：
	// conversation 在 Retrieve 报错时的既有处理是"记 warn、candidates=nil、
	// 继续这一轮"（context.go，对真正的检索故障是正确的降级），于是一个配了
	// 文档范围的 Agent 会在开关关闭时**不带任何资料**去回答，用户看到的是
	// Agent 凭空作答，而不是"我被限定在这几份文档里，里面没有"。
	//
	// 改成 true 是安全的：002 的 SC-003 已经证明「空过滤器 + 开关开启」时
	// 检索输出与该功能上线前逐字一致（确定性门禁既有用例逐字段比对）。
	// 对没有配置文档范围的 Agent，这个默认值变更不改变任何行为。
	//
	// 注意这个开关控制的是「**是否接受**过滤请求」，而不是「过滤是否生效」：
	// 关闭时**空**过滤器行为与以前完全相同，**非空**过滤器则被直接拒绝
	// （ErrMetadataFilterDisabled）。调用方明明要求了更窄的范围、系统却悄悄
	// 拿范围外的资料去检索——这是本功能唯一绝对不能有的行为（见 research.md R4）。
	// 这也是它与 RAGRerankEnabled 不同、没有"配错了就静默降级"这条路径的原因：
	// 这里没有什么可配错的，而"降级"本身就是那个失败模式。
	RAGMetadataFilterEnabled bool
}

func Load() (Config, error) {
	cfg := Config{
		Env:                 getEnv("HIFY_ENV", "development"),
		HTTPAddr:            getEnv("HIFY_HTTP_ADDR", ":8080"),
		MySQLDSN:            os.Getenv("HIFY_MYSQL_DSN"),
		PostgresDSN:         os.Getenv("HIFY_POSTGRES_DSN"),
		RedisAddr:           getEnv("HIFY_REDIS_ADDR", "127.0.0.1:6379"),
		RedisPassword:       os.Getenv("HIFY_REDIS_PASSWORD"),
		JWTSecret:           os.Getenv("HIFY_JWT_SECRET"),
		EncryptionKey:       os.Getenv("HIFY_ENCRYPTION_KEY"),
		AdminEmail:          os.Getenv("ADMIN_EMAIL"),
		AdminPassword:       os.Getenv("ADMIN_PASSWORD"),
		KnowledgeStorageDir: getEnv("HIFY_KNOWLEDGE_STORAGE_DIR", "./data/knowledge"),
	}

	cfg.CORSAllowedOrigins = splitAndTrim(getEnv("HIFY_CORS_ALLOWED_ORIGINS", "http://localhost:5173"))

	asynqConcurrency, err := strconv.Atoi(getEnv("HIFY_ASYNQ_CONCURRENCY", "2"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_ASYNQ_CONCURRENCY: %w", err)
	}
	cfg.AsynqConcurrency = asynqConcurrency

	redisDB, err := strconv.Atoi(getEnv("HIFY_REDIS_DB", "0"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_REDIS_DB: %w", err)
	}
	cfg.RedisDB = redisDB

	accessTTL, err := time.ParseDuration(getEnv("HIFY_JWT_ACCESS_TTL", "20m"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_JWT_ACCESS_TTL: %w", err)
	}
	cfg.JWTAccessTTL = accessTTL

	refreshTTL, err := time.ParseDuration(getEnv("HIFY_JWT_REFRESH_TTL", "720h"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_JWT_REFRESH_TTL: %w", err)
	}
	cfg.JWTRefreshTTL = refreshTTL

	rewriteEnabled, err := strconv.ParseBool(getEnv("HIFY_RAG_QUERY_REWRITE_ENABLED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_RAG_QUERY_REWRITE_ENABLED: %w", err)
	}
	cfg.RAGQueryRewriteEnabled = rewriteEnabled
	cfg.RAGQueryRewriteModelID = os.Getenv("HIFY_RAG_QUERY_REWRITE_MODEL_ID")

	rewriteTimeout, err := time.ParseDuration(getEnv("HIFY_RAG_QUERY_REWRITE_TIMEOUT", "1500ms"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_RAG_QUERY_REWRITE_TIMEOUT: %w", err)
	}
	cfg.RAGQueryRewriteTimeout = rewriteTimeout

	rerankEnabled, err := strconv.ParseBool(getEnv("HIFY_RAG_RERANK_ENABLED", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_RAG_RERANK_ENABLED: %w", err)
	}
	cfg.RAGRerankModelID = os.Getenv("HIFY_RAG_RERANK_MODEL_ID")
	// A rerank model ID's existence/capability/is_active is only checked
	// where it's actually used (knowledge, on first use — see
	// data-model.md §3) — this is only the "did the operator even
	// configure one" guard. Enabled-but-unconfigured must NOT fail
	// process startup (FR-014's "any failure degrades" applies to
	// misconfiguration too, not just runtime failures), so this only
	// downgrades cfg.RAGRerankEnabled and warns, never returns an error.
	if rerankEnabled && cfg.RAGRerankModelID == "" {
		slog.Warn("config: HIFY_RAG_RERANK_ENABLED=true but HIFY_RAG_RERANK_MODEL_ID is empty, disabling rerank")
		rerankEnabled = false
	}
	cfg.RAGRerankEnabled = rerankEnabled

	rerankTimeout, err := time.ParseDuration(getEnv("HIFY_RAG_RERANK_TIMEOUT", "1500ms"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_RAG_RERANK_TIMEOUT: %w", err)
	}
	cfg.RAGRerankTimeout = rerankTimeout

	metadataFilterEnabled, err := strconv.ParseBool(getEnv("HIFY_RAG_METADATA_FILTER_ENABLED", "true"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parse HIFY_RAG_METADATA_FILTER_ENABLED: %w", err)
	}
	cfg.RAGMetadataFilterEnabled = metadataFilterEnabled

	if cfg.MySQLDSN == "" {
		return Config{}, fmt.Errorf("config: HIFY_MYSQL_DSN is required")
	}
	if cfg.PostgresDSN == "" {
		return Config{}, fmt.Errorf("config: HIFY_POSTGRES_DSN is required")
	}
	if cfg.JWTSecret == "" {
		return Config{}, fmt.Errorf("config: HIFY_JWT_SECRET is required")
	}
	if cfg.EncryptionKey == "" {
		return Config{}, fmt.Errorf("config: HIFY_ENCRYPTION_KEY is required")
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
