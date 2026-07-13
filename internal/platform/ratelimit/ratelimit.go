// Package ratelimit provides a minimal Redis-backed fixed-window counter.
// It's intentionally simple (not a token bucket / sliding window) — good
// enough for coarse abuse protection like login-attempt throttling, where
// precision at window boundaries doesn't matter. The per-provider LLM rate
// limiting planned for internal/provider (Phase 1) is a separate, more
// precise concern and will get its own implementation when that module is
// built.
package ratelimit

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Allow increments the counter for key and reports whether it's still
// within limit for the current window. The first increment in a window
// sets the key's expiry so the counter resets automatically.
func Allow(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, error) {
	count, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("ratelimit: incr: %w", err)
	}
	if count == 1 {
		if err := rdb.Expire(ctx, key, window).Err(); err != nil {
			return false, fmt.Errorf("ratelimit: expire: %w", err)
		}
	}
	return count <= int64(limit), nil
}
