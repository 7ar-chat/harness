package redisx_test

import (
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newRedis starts a miniredis and a client on it that never retries, so a stopped server fails fast.
func newRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, rdb
}

var bg = context.Background()

func splitAddr(t *testing.T, addr string) (host, port string) {
	t.Helper()
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		t.Fatalf("address %q", addr)
	}
	return addr[:i], addr[i+1:]
}
