package redisx

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness/core/sandbox"
)

// Coordinator is the sandbox.Coordinator backed by Redis; it also provides
// the key/value locks and liveness marks of the runtime (runtime.Locks).
type Coordinator struct{ RDB redis.UniversalClient }

var _ sandbox.Coordinator = Coordinator{}

// NewCoordinator returns a Redis Coordinator.
func NewCoordinator(rdb redis.UniversalClient) Coordinator { return Coordinator{RDB: rdb} }

// Exists reports whether key is set.
func (c Coordinator) Exists(ctx context.Context, key string) (bool, error) {
	n, err := c.RDB.Exists(ctx, key).Result()
	return n > 0, err
}

var (
	unhealthyScript = redis.NewScript(`
redis.call('HSETNX', KEYS[1], 'first', ARGV[1])
local checks = redis.call('HINCRBY', KEYS[1], 'checks', 1)
redis.call('EXPIRE', KEYS[1], ARGV[2])
return {redis.call('HGET', KEYS[1], 'first'), checks}`)
	unlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`)
	renewLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('PEXPIRE', KEYS[1], ARGV[2])
end
return 0`)
	takeScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v then redis.call('DEL', KEYS[1]) end
return v`)
)

func (c Coordinator) SetNX(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	return c.RDB.SetNX(ctx, key, token, ttl).Result()
}

func (c Coordinator) Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	n, err := renewLockScript.Run(ctx, c.RDB, []string{key}, token, ttl.Milliseconds()).Int()
	return n > 0, err
}

func (c Coordinator) Unlock(ctx context.Context, key, token string) error {
	return unlockScript.Run(ctx, c.RDB, []string{key}, token).Err()
}

func (c Coordinator) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.RDB.Set(ctx, key, value, ttl).Err()
}

func (c Coordinator) Del(ctx context.Context, key string) (bool, error) {
	n, err := c.RDB.Del(ctx, key).Result()
	return n > 0, err
}

func (c Coordinator) Take(ctx context.Context, key string) (string, error) {
	v, err := takeScript.Run(ctx, c.RDB, []string{key}).Text()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c Coordinator) Unhealthy(ctx context.Context, key string, nowMs int64, ttl time.Duration) (int64, int64, error) {
	res, err := unhealthyScript.Run(ctx, c.RDB, []string{key}, nowMs, int(ttl.Seconds())).Slice()
	if err != nil || len(res) != 2 {
		return 0, 0, err
	}
	firstStr, _ := res[0].(string)
	first, _ := strconv.ParseInt(firstStr, 10, 64)
	checks, _ := res[1].(int64)
	return first, checks, nil
}
