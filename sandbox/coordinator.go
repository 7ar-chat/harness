package sandbox

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Coordinator is the small shared state the manager needs to cooperate across
// replicas: creation locks, health-streak counters and one-shot markers.
// RedisCoordinator shares it between processes; LocalCoordinator keeps it in
// memory (a single process: a TUI, a desktop app or tests).
type Coordinator interface {
	// SetNX stores key=token for ttl unless the key exists.
	SetNX(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	// Renew extends key's ttl while it still holds token.
	Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error)
	// Unlock deletes key while it still holds token.
	Unlock(ctx context.Context, key, token string) error
	Set(ctx context.Context, key, value string, ttl time.Duration) error
	// Del reports whether the key existed.
	Del(ctx context.Context, key string) (bool, error)
	// Take returns the value of key and deletes it ("" when absent).
	Take(ctx context.Context, key string) (string, error)
	// Unhealthy records a failed health check: it returns when the streak
	// started (unix ms, the first call's now) and the number of checks so far.
	Unhealthy(ctx context.Context, key string, nowMs int64, ttl time.Duration) (firstMs, checks int64, err error)
}

// --- Redis ---

// RedisCoordinator is the Coordinator backed by Redis.
type RedisCoordinator struct{ RDB redis.UniversalClient }

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

func (c RedisCoordinator) SetNX(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	return c.RDB.SetNX(ctx, key, token, ttl).Result()
}

func (c RedisCoordinator) Renew(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	n, err := renewLockScript.Run(ctx, c.RDB, []string{key}, token, ttl.Milliseconds()).Int()
	return n > 0, err
}

func (c RedisCoordinator) Unlock(ctx context.Context, key, token string) error {
	return unlockScript.Run(ctx, c.RDB, []string{key}, token).Err()
}

func (c RedisCoordinator) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.RDB.Set(ctx, key, value, ttl).Err()
}

func (c RedisCoordinator) Del(ctx context.Context, key string) (bool, error) {
	n, err := c.RDB.Del(ctx, key).Result()
	return n > 0, err
}

func (c RedisCoordinator) Take(ctx context.Context, key string) (string, error) {
	v, err := takeScript.Run(ctx, c.RDB, []string{key}).Text()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

func (c RedisCoordinator) Unhealthy(ctx context.Context, key string, nowMs int64, ttl time.Duration) (int64, int64, error) {
	res, err := unhealthyScript.Run(ctx, c.RDB, []string{key}, nowMs, int(ttl.Seconds())).Slice()
	if err != nil || len(res) != 2 {
		return 0, 0, err
	}
	firstStr, _ := res[0].(string)
	first, _ := strconv.ParseInt(firstStr, 10, 64)
	checks, _ := res[1].(int64)
	return first, checks, nil
}

// --- in memory ---

// LocalCoordinator is the in-process Coordinator.
type LocalCoordinator struct {
	mu      sync.Mutex
	vals    map[string]localVal
	streaks map[string]*localStreak
}

type localVal struct {
	v       string
	expires time.Time
}

type localStreak struct {
	first, checks int64
	expires       time.Time
}

// NewLocalCoordinator returns an empty in-memory Coordinator.
func NewLocalCoordinator() *LocalCoordinator {
	return &LocalCoordinator{vals: map[string]localVal{}, streaks: map[string]*localStreak{}}
}

// live returns the value of key unless it expired (mu held).
func (c *LocalCoordinator) live(key string) (localVal, bool) {
	v, ok := c.vals[key]
	if ok && !v.expires.After(time.Now()) {
		delete(c.vals, key)
		return localVal{}, false
	}
	return v, ok
}

func (c *LocalCoordinator) SetNX(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.live(key); ok {
		return false, nil
	}
	c.vals[key] = localVal{token, time.Now().Add(ttl)}
	return true, nil
}

func (c *LocalCoordinator) Renew(_ context.Context, key, token string, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.live(key)
	if !ok || v.v != token {
		return false, nil
	}
	c.vals[key] = localVal{token, time.Now().Add(ttl)}
	return true, nil
}

func (c *LocalCoordinator) Unlock(_ context.Context, key, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.live(key); ok && v.v == token {
		delete(c.vals, key)
	}
	return nil
}

func (c *LocalCoordinator) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.vals[key] = localVal{value, time.Now().Add(ttl)}
	return nil
}

func (c *LocalCoordinator) Del(_ context.Context, key string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.live(key)
	delete(c.vals, key)
	delete(c.streaks, key)
	return ok, nil
}

func (c *LocalCoordinator) Take(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.live(key)
	delete(c.vals, key)
	if !ok {
		return "", nil
	}
	return v.v, nil
}

func (c *LocalCoordinator) Unhealthy(_ context.Context, key string, nowMs int64, ttl time.Duration) (int64, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streaks[key]
	if s == nil || !s.expires.After(time.Now()) {
		s = &localStreak{first: nowMs}
		c.streaks[key] = s
	}
	s.checks++
	s.expires = time.Now().Add(ttl)
	return s.first, s.checks, nil
}
