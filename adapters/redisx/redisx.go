// Package redisx builds the Redis client (Sentinel or standalone).
package redisx

import (
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/config"
)

// New returns a Sentinel failover client when USE_SENTINEL=true, else a standalone client.
func New(cfg *config.Config) redis.UniversalClient {
	if cfg.UseSentinel {
		var addrs []string
		for _, h := range strings.Split(cfg.RedisSentinelHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				addrs = append(addrs, h)
			}
		}
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.RedisMasterName,
			SentinelAddrs:    addrs,
			Password:         cfg.RedisPassword,
			SentinelPassword: cfg.RedisPassword,
			DB:               cfg.RedisDB,
			PoolSize:         cfg.RedisPoolSize,
		})
	}
	return redis.NewClient(&redis.Options{
		Addr:     cfg.RedisHost + ":" + cfg.RedisPort,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
		PoolSize: cfg.RedisPoolSize,
	})
}
