// Package config loads Hify's runtime configuration from environment variables.
package config

import (
	"fmt"
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
