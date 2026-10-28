package sandboxtools

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisJobsKey is the hash of the running background jobs ("thread/job" → start,
// unix milliseconds; older entries hold unix seconds).
const redisJobsKey = "work:sbx:jobs"

// redisJobLeasePrefix + "thread/job" is the lease of the process keeping a job's sandbox alive.
const redisJobLeasePrefix = "work:sbx:job-lease:"

// RedisJobs is the JobList shared by the runtime processes.
type RedisJobs struct{ RDB redis.UniversalClient }

// Add implements JobList: the first recorded start is kept (HSETNX).
func (r RedisJobs) Add(ctx context.Context, threadID, jobID string) (time.Time, error) {
	key := jobKey(threadID, jobID)
	now := time.Now()
	if err := r.RDB.HSetNX(ctx, redisJobsKey, key, now.UnixMilli()).Err(); err != nil {
		return now, err
	}
	raw, err := r.RDB.HGet(ctx, redisJobsKey, key).Result()
	if err != nil {
		return now, err
	}
	return parseStart(raw, now), nil
}

// Remove implements JobList.
func (r RedisJobs) Remove(ctx context.Context, threadID, jobID string) error {
	return r.RDB.HDel(ctx, redisJobsKey, jobKey(threadID, jobID)).Err()
}

// List implements JobList.
func (r RedisJobs) List(ctx context.Context) ([]ListedJob, error) {
	all, err := r.RDB.HGetAll(ctx, redisJobsKey).Result()
	if err != nil {
		return nil, err
	}
	out := make([]ListedJob, 0, len(all))
	for k, v := range all {
		if thread, job, ok := strings.Cut(k, "/"); ok {
			out = append(out, ListedJob{ThreadID: thread, JobID: job, Started: parseStart(v, time.Now())})
		}
	}
	return out, nil
}

// claimScript takes the lease when free, renews it for its owner, else refuses.
var claimScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == false then redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2]) return 1 end
if v == ARGV[1] then redis.call('PEXPIRE', KEYS[1], ARGV[2]) return 1 end
return 0`)

// releaseScript drops the lease of its owner only.
var releaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

// Claim implements JobList.
func (r RedisJobs) Claim(ctx context.Context, threadID, jobID, owner string, ttl time.Duration) (bool, error) {
	n, err := claimScript.Run(ctx, r.RDB, []string{redisJobLeasePrefix + jobKey(threadID, jobID)}, owner, max(ttl.Milliseconds(), 1)).Int()
	return n == 1, err
}

// Release implements JobList.
func (r RedisJobs) Release(ctx context.Context, threadID, jobID, owner string) error {
	err := releaseScript.Run(ctx, r.RDB, []string{redisJobLeasePrefix + jobKey(threadID, jobID)}, owner).Err()
	if errors.Is(err, redis.Nil) {
		return nil
	}
	return err
}

func parseStart(raw string, fallback time.Time) time.Time {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	switch {
	case err != nil || n <= 0:
	case n < 1e12: // unix seconds (entries of the previous release)
		return time.Unix(n, 0)
	default:
		return time.UnixMilli(n)
	}
	return fallback
}
