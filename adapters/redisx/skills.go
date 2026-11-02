package redisx

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// SkillInstalls is the skills.InstallState on Redis: a hash per sandbox of the
// installed revision of each skill, and SET NX PX locks.
type SkillInstalls struct{ RDB redis.UniversalClient }

// NewSkillInstalls returns a Redis skills.InstallState.
func NewSkillInstalls(rdb redis.UniversalClient) SkillInstalls { return SkillInstalls{RDB: rdb} }

func installedKey(sandboxID string) string { return "work:skills:installed:" + sandboxID }

// Revision implements skills.InstallState.
func (s SkillInstalls) Revision(ctx context.Context, sandboxID, skill string) (string, error) {
	got, err := s.RDB.HGet(ctx, installedKey(sandboxID), skill).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return got, err
}

// Record implements skills.InstallState.
func (s SkillInstalls) Record(ctx context.Context, sandboxID string, revisions map[string]string, ttl time.Duration) error {
	key := installedKey(sandboxID)
	pipe := s.RDB.TxPipeline()
	for k, v := range revisions {
		pipe.HSet(ctx, key, k, v)
	}
	pipe.Expire(ctx, key, ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// TryLock implements skills.InstallState.
func (s SkillInstalls) TryLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	return s.RDB.SetNX(ctx, key, token, ttl).Result()
}

// Unlock implements skills.InstallState.
func (s SkillInstalls) Unlock(ctx context.Context, key, token string) error {
	return unlockScript.Run(ctx, s.RDB, []string{key}, token).Err()
}
