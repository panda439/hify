package platform

import "github.com/hibiken/asynq"

func AsynqRedisOpt(cfg RedisConfig) asynq.RedisClientOpt {
	return asynq.RedisClientOpt{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	}
}

func NewAsynqClient(cfg RedisConfig) *asynq.Client {
	return asynq.NewClient(AsynqRedisOpt(cfg))
}

// NewAsynqServer builds a worker server with an explicit concurrency cap so
// background jobs (document processing etc.) cannot starve the online API
// request path running in the same process (see "已知性能风险与应对" #3).
func NewAsynqServer(cfg RedisConfig, concurrency int) *asynq.Server {
	return asynq.NewServer(AsynqRedisOpt(cfg), asynq.Config{
		Concurrency: concurrency,
	})
}
