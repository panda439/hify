package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	retry "github.com/avast/retry-go/v4"
	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker"
	"golang.org/x/sync/semaphore"

	"hify/internal/platform/apperr"
	"hify/internal/platform/ratelimit"
)

// ResilienceConfig holds the per-provider knobs; registry.go derives these
// from Provider.ExtraConfig plus sane defaults (see resilienceConfigFrom).
type ResilienceConfig struct {
	ProviderID         string
	MaxConcurrent      int64
	RateLimitPerMinute int
	IdleTimeout        time.Duration
	MaxStreamDuration  time.Duration
	MaxRetries         int
	Redis              *redis.Client
}

// retryAfterProvider is an optional interface an adapter can implement to
// let the retry backoff honor a 429's Retry-After header instead of
// guessing (see openai_compat.go's retryAfterHolder).
type retryAfterProvider interface {
	lastRetryAfter() time.Duration
}

type resilientClient struct {
	inner   Client
	sem     *semaphore.Weighted
	breaker *gobreaker.CircuitBreaker
	cfg     ResilienceConfig
}

// WithResilience wraps inner with per-provider concurrency limiting,
// Redis-backed rate limiting, a circuit breaker, idle-timeout enforcement
// for streams, and retry-with-backoff — see CLAUDE.md's "弹性装饰器" design.
// registry.go applies this to every adapter it constructs; business code
// only ever sees the plain Client interface.
func WithResilience(inner Client, cfg ResilienceConfig) Client {
	breaker := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name: "provider:" + cfg.ProviderID,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 5
		},
		Timeout: 30 * time.Second,
	})
	return &resilientClient{
		inner:   inner,
		sem:     semaphore.NewWeighted(cfg.MaxConcurrent),
		breaker: breaker,
		cfg:     cfg,
	}
}

func (r *resilientClient) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	if err := r.acquire(ctx); err != nil {
		return Message{}, err
	}
	defer r.sem.Release(1)
	if err := r.checkRateLimit(ctx); err != nil {
		return Message{}, err
	}
	return callWithRetry(ctx, r.breaker, r.inner, r.cfg.MaxRetries, func() (Message, error) {
		return r.inner.Chat(ctx, req)
	})
}

func (r *resilientClient) Embed(ctx context.Context, req EmbedRequest) (EmbedResult, error) {
	if err := r.acquire(ctx); err != nil {
		return EmbedResult{}, err
	}
	defer r.sem.Release(1)
	if err := r.checkRateLimit(ctx); err != nil {
		return EmbedResult{}, err
	}
	return callWithRetry(ctx, r.breaker, r.inner, r.cfg.MaxRetries, func() (EmbedResult, error) {
		return r.inner.Embed(ctx, req)
	})
}

func (r *resilientClient) TestConnection(ctx context.Context) error {
	if err := r.acquire(ctx); err != nil {
		return err
	}
	defer r.sem.Release(1)
	_, err := callWithRetry(ctx, r.breaker, r.inner, r.cfg.MaxRetries, func() (struct{}, error) {
		return struct{}{}, r.inner.TestConnection(ctx)
	})
	return err
}

// ChatStream retries the initial call (before any chunk reaches the
// caller); once the inner channel starts emitting, a mid-stream failure is
// surfaced as a ChatChunk.Err and never silently retried — retrying after
// content has already streamed to the client would duplicate/garble output
// (see CLAUDE.md's resilience design).
func (r *resilientClient) ChatStream(ctx context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	if err := r.acquire(ctx); err != nil {
		return nil, err
	}
	if err := r.checkRateLimit(ctx); err != nil {
		r.sem.Release(1)
		return nil, err
	}

	inner, err := callWithRetry(ctx, r.breaker, r.inner, r.cfg.MaxRetries, func() (<-chan ChatChunk, error) {
		return r.inner.ChatStream(ctx, req)
	})
	if err != nil {
		r.sem.Release(1)
		return nil, err
	}

	out := make(chan ChatChunk)
	go func() {
		defer close(out)
		defer r.sem.Release(1)

		streamCtx, cancel := context.WithTimeout(ctx, r.cfg.MaxStreamDuration)
		defer cancel()

		idleTimer := time.NewTimer(r.cfg.IdleTimeout)
		defer idleTimer.Stop()

		for {
			select {
			case chunk, ok := <-inner:
				if !ok {
					return
				}
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(r.cfg.IdleTimeout)
				out <- chunk
				if chunk.Err != nil || chunk.FinishReason != "" {
					return
				}
			case <-idleTimer.C:
				out <- ChatChunk{Err: fmt.Errorf("provider: idle timeout waiting for stream data")}
				return
			case <-streamCtx.Done():
				out <- ChatChunk{Err: streamCtx.Err()}
				return
			}
		}
	}()
	return out, nil
}

func (r *resilientClient) acquire(ctx context.Context) error {
	if err := r.sem.Acquire(ctx, 1); err != nil {
		return fmt.Errorf("provider: acquire concurrency slot: %w", err)
	}
	return nil
}

func (r *resilientClient) checkRateLimit(ctx context.Context) error {
	if r.cfg.Redis == nil || r.cfg.RateLimitPerMinute <= 0 {
		return nil
	}
	allowed, err := ratelimit.Allow(ctx, r.cfg.Redis, "provider_rpm:"+r.cfg.ProviderID, r.cfg.RateLimitPerMinute, time.Minute)
	if err != nil {
		return fmt.Errorf("provider: rate limit check: %w", err)
	}
	if !allowed {
		return apperr.TooManyRequests("provider.rate_limited", "该供应商当前请求过于频繁，请稍后重试")
	}
	return nil
}

func callWithRetry[T any](ctx context.Context, breaker *gobreaker.CircuitBreaker, inner Client, maxRetries int, op func() (T, error)) (T, error) {
	var result T
	err := retry.Do(
		func() error {
			v, err := breaker.Execute(func() (interface{}, error) {
				return op()
			})
			if err != nil {
				return err
			}
			result = v.(T)
			return nil
		},
		retry.Context(ctx),
		retry.Attempts(uint(maxRetries+1)),
		retry.Delay(500*time.Millisecond),
		retry.MaxDelay(15*time.Second),
		retry.DelayType(retryAfterAwareDelay(inner)),
		retry.RetryIf(isRetryable),
		retry.LastErrorOnly(true),
	)
	return result, err
}

// retryAfterAwareDelay prefers a provider-supplied Retry-After value (see
// retryAfterProvider) over the default exponential backoff+jitter.
func retryAfterAwareDelay(inner Client) retry.DelayTypeFunc {
	return func(n uint, err error, cfg *retry.Config) time.Duration {
		if rap, ok := inner.(retryAfterProvider); ok {
			if d := rap.lastRetryAfter(); d > 0 {
				return d
			}
		}
		return retry.BackOffDelay(n, err, cfg)
	}
}

func isRetryable(err error) bool {
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		// Circuit is open: fail fast, don't retry within this call.
		return false
	}
	var ae *adapterError
	if errors.As(err, &ae) {
		if ae.status == 0 {
			return true // transport-level failure (timeout/DNS/connection refused)
		}
		return ae.status == 429 || ae.status >= 500
	}
	return false
}
