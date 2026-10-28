package redisx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/engine/queue"
)

// Queue implements queue.Queue, queue.Bus and the runtime consumer on Redis streams, lists and pub/sub.
type Queue struct {
	rdb redis.UniversalClient
}

// NewQueue returns a Redis queue.
func NewQueue(rdb redis.UniversalClient) *Queue { return &Queue{rdb: rdb} }

var (
	_ queue.Queue = (*Queue)(nil)
	_ queue.Bus   = (*Queue)(nil)
)

func controlKey(runID string) string { return "work:run:" + runID + ":control" }
func steersKey(runID string) string  { return "work:run:" + runID + ":steers" }

// steerRunsKey indexes the runs with pending steers (SteerRuns).
const steerRunsKey = "work:steers:runs"

func closedKey(runID string) string { return "work:run:" + runID + ":steers:closed" }

func (q *Queue) Enqueue(ctx context.Context, runID string) error {
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: queue.Stream,
		MaxLen: 100000,
		Approx: true,
		Values: []any{"runId", runID},
	}).Err()
}

// EnsureGroup creates the consumer group (idempotent), reading the stream
// from its start (entries enqueued before the first runtime started).
func (q *Queue) EnsureGroup(ctx context.Context) error { return q.ensureGroup(ctx, "0") }

func (q *Queue) ensureGroup(ctx context.Context, start string) error {
	err := q.rdb.XGroupCreateMkStream(ctx, queue.Stream, queue.Group, start).Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Read blocks up to block for at most count new entries for consumer.
func (q *Queue) Read(ctx context.Context, consumer string, count int, block time.Duration) ([]queue.Message, error) {
	res, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    queue.Group,
		Consumer: consumer,
		Streams:  []string{queue.Stream, ">"},
		Count:    int64(count),
		Block:    block,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") {
			// The group was lost (key deleted, group destroyed): re-create it at the
			// end, never re-deliver the whole stream (up to 100k entries); runs still
			// queued are re-enqueued by the sweeper.
			if gerr := q.ensureGroup(ctx, "$"); gerr != nil {
				return nil, gerr
			}
			return nil, nil
		}
		return nil, err
	}
	var out []queue.Message
	for _, s := range res {
		for _, m := range s.Messages {
			id, _ := m.Values["runId"].(string)
			out = append(out, queue.Message{StreamID: m.ID, RunID: id})
		}
	}
	return out, nil
}

// Reclaim moves to consumer the entries pending for longer than minIdle on
// other consumers (read by a runtime that stopped before acking them).
func (q *Queue) Reclaim(ctx context.Context, consumer string, minIdle time.Duration) ([]queue.Message, error) {
	var out []queue.Message
	start := "0-0"
	for i := 0; i < 10; i++ {
		msgs, next, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: queue.Stream, Group: queue.Group, Consumer: consumer,
			MinIdle: minIdle, Start: start, Count: 100}).Result()
		if err != nil {
			if strings.Contains(err.Error(), "NOGROUP") {
				return out, nil
			}
			return out, err
		}
		for _, m := range msgs {
			id, _ := m.Values["runId"].(string)
			out = append(out, queue.Message{StreamID: m.ID, RunID: id})
		}
		if next == "0-0" || next == "" {
			break
		}
		start = next
	}
	return out, nil
}

// PruneConsumers deletes the consumers other than keep that have no pending
// entry and were idle for longer than idle (runtimes that stopped: every
// start adds a consumer). It returns how many were deleted.
func (q *Queue) PruneConsumers(ctx context.Context, keep string, idle time.Duration) (int, error) {
	consumers, err := q.rdb.XInfoConsumers(ctx, queue.Stream, queue.Group).Result()
	if err != nil {
		if strings.Contains(err.Error(), "NOGROUP") || strings.Contains(err.Error(), "no such key") {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, c := range consumers {
		// A negative idle means unknown (some Redis-compatible servers): no pending entry makes it safe.
		if c.Name == keep || c.Pending > 0 || (c.Idle >= 0 && c.Idle < idle) {
			continue
		}
		if err := q.rdb.XGroupDelConsumer(ctx, queue.Stream, queue.Group, c.Name).Err(); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Ack acknowledges a queue entry.
func (q *Queue) Ack(ctx context.Context, streamID string) error {
	return q.rdb.XAck(ctx, queue.Stream, queue.Group, streamID).Err()
}

func (q *Queue) PublishCancel(ctx context.Context, runID string) error {
	return q.rdb.Publish(ctx, controlKey(runID), "cancel").Err()
}

func (q *Queue) SubscribeCancel(ctx context.Context, runID string, fn func()) (func(), error) {
	sub := q.rdb.Subscribe(ctx, controlKey(runID))
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return func() {}, err
	}
	ch := sub.Channel()
	go func() {
		for msg := range ch {
			if msg.Payload == "cancel" {
				fn()
			}
		}
	}()
	return func() { _ = sub.Close() }, nil
}

var pushSteerScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
  return -1
end
if redis.call('LLEN', KEYS[1]) >= tonumber(ARGV[2]) then
  return 0
end
redis.call('RPUSH', KEYS[1], ARGV[1])
redis.call('EXPIRE', KEYS[1], 86400)
redis.call('SADD', KEYS[3], ARGV[3])
return 1`)

func (q *Queue) PushSteer(ctx context.Context, runID string, s queue.Steer) error {
	data, _ := json.Marshal(s)
	ok, err := pushSteerScript.Run(ctx, q.rdb, []string{steersKey(runID), closedKey(runID), steerRunsKey}, string(data), queue.MaxSteers, runID).Int()
	switch {
	case err != nil:
		return err
	case ok == -1:
		return queue.ErrSteersClosed
	case ok == 0:
		return queue.ErrTooManySteers
	}
	return nil
}

var drainScript = redis.NewScript(`
local items = redis.call('LRANGE', KEYS[1], 0, -1)
redis.call('DEL', KEYS[1])
redis.call('SREM', KEYS[2], ARGV[1])
return items`)

func (q *Queue) DrainSteers(ctx context.Context, runID string) ([]queue.Steer, error) {
	items, err := drainScript.Run(ctx, q.rdb, []string{steersKey(runID), steerRunsKey}, runID).StringSlice()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	return decodeSteers(items), nil
}

var finalizeScript = redis.NewScript(`
redis.call('SET', KEYS[2], '1', 'EX', 86400)
local items = redis.call('LRANGE', KEYS[1], 0, -1)
redis.call('DEL', KEYS[1])
redis.call('SREM', KEYS[3], ARGV[1])
return items`)

func (q *Queue) FinalizeSteers(ctx context.Context, runID string) ([]queue.Steer, error) {
	items, err := finalizeScript.Run(ctx, q.rdb, []string{steersKey(runID), closedKey(runID), steerRunsKey}, runID).StringSlice()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	return decodeSteers(items), nil
}

func (q *Queue) ReopenSteers(ctx context.Context, runID string) error {
	return q.rdb.Del(ctx, closedKey(runID)).Err()
}

var closeScript = redis.NewScript(`
redis.call('SET', KEYS[2], '1', 'EX', 86400)
return redis.call('LRANGE', KEYS[1], 0, -1)`)

func (q *Queue) CloseSteers(ctx context.Context, runID string) ([]queue.Steer, error) {
	items, err := closeScript.Run(ctx, q.rdb, []string{steersKey(runID), closedKey(runID)}).StringSlice()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	return decodeSteers(items), nil
}

var ackScript = redis.NewScript(`
redis.call('LTRIM', KEYS[1], tonumber(ARGV[1]), -1)
if redis.call('LLEN', KEYS[1]) == 0 then
  redis.call('DEL', KEYS[1])
  redis.call('SREM', KEYS[2], ARGV[2])
end
return 1`)

func (q *Queue) AckSteers(ctx context.Context, runID string, n int) error {
	return ackScript.Run(ctx, q.rdb, []string{steersKey(runID), steerRunsKey}, n, runID).Err()
}

func (q *Queue) SteerRuns(ctx context.Context) ([]string, error) {
	return q.rdb.SMembers(ctx, steerRunsKey).Result()
}

func decodeSteers(items []string) []queue.Steer {
	out := make([]queue.Steer, 0, len(items))
	for _, it := range items {
		var s queue.Steer
		if json.Unmarshal([]byte(it), &s) == nil && s.Text != "" {
			out = append(out, s)
		}
	}
	return out
}
