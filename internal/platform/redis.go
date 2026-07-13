package platform

import "github.com/redis/go-redis/v9"

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

// NewRedisClient builds the client used for caching and rate-limit counters.
// asynq uses its own connection option derived from the same RedisConfig via
// AsynqRedisOpt, not this client, since asynq manages its own pool.
func NewRedisClient(cfg RedisConfig) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
}
