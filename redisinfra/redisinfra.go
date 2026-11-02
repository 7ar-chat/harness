// Package redisinfra builds the harness.Infra of a deployment that shares Redis between processes: the run queue and
// control bus, the event log, locks, the background-job list and the skill installs.
//
// It is a separate package so that a program using the harness in one process does not link go-redis:
//
//	rdb := redisinfra.NewClient(cfg)
//	infra := redisinfra.Infra(rdb, store)
//	h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
package redisinfra

import (
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/config"
)

// NewClient builds the Redis client of the configuration (REDIS_*, USE_SENTINEL): a Sentinel failover client or a
// standalone one.
func NewClient(cfg *config.Config) redis.UniversalClient {
	return redisx.New(redisx.Options{
		Sentinel: cfg.UseSentinel, SentinelHosts: cfg.RedisSentinelHosts, MasterName: cfg.RedisMasterName,
		Host: cfg.RedisHost, Port: cfg.RedisPort, Password: cfg.RedisPassword, DB: cfg.RedisDB, PoolSize: cfg.RedisPoolSize,
	})
}

// Infra builds the harness.Infra over rdb: event log (streams over st), queue and bus, locks, job list and skill installs.
func Infra(rdb redis.UniversalClient, st harness.Store) harness.Infra {
	return harness.Infra{
		Log:         redisx.NewEventLog(rdb, st),
		Broker:      redisx.NewQueue(rdb),
		Coordinator: redisx.NewCoordinator(rdb),
		Jobs:        redisx.NewJobs(rdb),
		Installs:    redisx.NewSkillInstalls(rdb),
	}
}
