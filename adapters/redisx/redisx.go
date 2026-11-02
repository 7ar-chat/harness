// Package redisx builds the Redis client (Sentinel or standalone).
package redisx

import (
	"strings"

	"github.com/redis/go-redis/v9"
)

// Options say how to reach Redis; the composition root fills them from its
// configuration.
type Options struct {
	// Sentinel selects a Sentinel failover client; else a standalone client.
	Sentinel bool
	// SentinelHosts are the comma-separated Sentinel addresses (Sentinel only).
	SentinelHosts string
	// MasterName is the Sentinel master (Sentinel only).
	MasterName string
	// Host and Port address a standalone server.
	Host, Port string
	Password   string
	DB         int
	// PoolSize must exceed concurrent SSE tailers (each holds a connection during XREAD BLOCK).
	PoolSize int
}

// New returns a Sentinel failover client when o.Sentinel is set, else a standalone client.
func New(o Options) redis.UniversalClient {
	if o.Sentinel {
		var addrs []string
		for _, h := range strings.Split(o.SentinelHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				addrs = append(addrs, h)
			}
		}
		return redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       o.MasterName,
			SentinelAddrs:    addrs,
			Password:         o.Password,
			SentinelPassword: o.Password,
			DB:               o.DB,
			PoolSize:         o.PoolSize,
		})
	}
	return redis.NewClient(&redis.Options{
		Addr:     o.Host + ":" + o.Port,
		Password: o.Password,
		DB:       o.DB,
		PoolSize: o.PoolSize,
	})
}
