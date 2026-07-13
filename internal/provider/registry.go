package provider

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// registry resolves a provider_id into a live, resilience-wrapped Client —
// the one place that maps DB config to a runtime adapter. Business code
// (and provider.Service itself) only ever depends on the Client interface
// this returns, never on a concrete adapter.
//
// Clients are cached per provider_id: the circuit breaker and semaphore
// inside WithResilience are only meaningful if they persist across calls
// (a breaker rebuilt fresh on every resolve() would never accumulate
// consecutive failures and would never trip). service.go's UpdateProvider
// invalidates the cache entry whenever a provider's config changes, so the
// next resolve() rebuilds against the new base_url/key/extra_config.
type registry struct {
	repo          *Repository
	encryptionKey []byte
	redis         *redis.Client

	mu    sync.RWMutex
	cache map[string]Client
}

func newRegistry(repo *Repository, encryptionKey []byte, rdb *redis.Client) *registry {
	return &registry{
		repo:          repo,
		encryptionKey: encryptionKey,
		redis:         rdb,
		cache:         make(map[string]Client),
	}
}

func (reg *registry) resolve(ctx context.Context, providerID string) (Client, error) {
	reg.mu.RLock()
	cached, ok := reg.cache[providerID]
	reg.mu.RUnlock()
	if ok {
		return cached, nil
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	// Re-check now that we hold the write lock — another goroutine may have
	// built and cached it while we were waiting.
	if cached, ok := reg.cache[providerID]; ok {
		return cached, nil
	}

	client, err := reg.build(ctx, providerID)
	if err != nil {
		return nil, err
	}
	reg.cache[providerID] = client
	return client, nil
}

// invalidate evicts a provider's cached client — called by service.go after
// any successful update, since base_url/auth/api_key/extra_config may have
// changed and the next resolve() must rebuild against the new config.
func (reg *registry) invalidate(providerID string) {
	reg.mu.Lock()
	delete(reg.cache, providerID)
	reg.mu.Unlock()
}

func (reg *registry) build(ctx context.Context, providerID string) (Client, error) {
	p, err := reg.repo.getProvider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if !p.IsActive {
		return nil, ErrInactive
	}

	var apiKey string
	if p.AuthType == AuthTypeAPIKey {
		encrypted, err := reg.repo.getEncryptedAPIKey(ctx, providerID)
		if err != nil {
			return nil, err
		}
		if len(encrypted) > 0 {
			apiKey, err = decryptAPIKey(reg.encryptionKey, encrypted)
			if err != nil {
				return nil, err
			}
		}
	}

	var client Client
	switch p.AdapterType {
	case AdapterOpenAICompatible:
		client = newOpenAICompatClient(p.BaseURL, apiKey, p.ExtraHeaders, &http.Client{})
	default:
		return nil, ErrUnsupportedType
	}

	return WithResilience(client, resilienceConfigFrom(p, reg.redis)), nil
}

// resilienceConfigFrom applies extra_config overrides on top of defaults
// sized for the "50 users, few thousand later" scale from the project
// plan — 30 concurrent in-flight requests and a generous per-minute budget
// so Hify's own limiter isn't the bottleneck (see 已知性能风险 #4).
func resilienceConfigFrom(p Provider, rdb *redis.Client) ResilienceConfig {
	idleTimeout := 45 * time.Second
	if p.ExtraConfig.IdleTimeoutSeconds > 0 {
		idleTimeout = time.Duration(p.ExtraConfig.IdleTimeoutSeconds) * time.Second
	}

	maxConcurrent := int64(30)
	if p.ExtraConfig.MaxConcurrent > 0 {
		maxConcurrent = int64(p.ExtraConfig.MaxConcurrent)
	}

	rateLimitPerMinute := 300
	if p.ExtraConfig.RateLimitPerMinute > 0 {
		rateLimitPerMinute = p.ExtraConfig.RateLimitPerMinute
	}

	return ResilienceConfig{
		ProviderID:         p.ID,
		MaxConcurrent:      maxConcurrent,
		RateLimitPerMinute: rateLimitPerMinute,
		IdleTimeout:        idleTimeout,
		MaxStreamDuration:  10 * time.Minute,
		MaxRetries:         2,
		Redis:              rdb,
	}
}
