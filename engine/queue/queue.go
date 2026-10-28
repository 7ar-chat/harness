// Package queue holds the Redis-backed run queue, the per-run control channel
// (cancel) and the per-run steer list.
package queue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/store"
)

const (
	// Stream is the run queue.
	Stream = "work:runs"
	// Group is the consumer group of runtimes.
	Group = "runtime"
	// MaxSteers is the cap of pending steer messages per run.
	MaxSteers = 20
)

var (
	// ErrTooManySteers is returned when MaxSteers are already pending.
	ErrTooManySteers = errors.New("too many pending steer messages")
	// ErrSteersClosed is returned once the run stopped accepting steers (final step or finished).
	ErrSteersClosed = errors.New("the run no longer accepts messages")
)

// Steer is a user message injected into a running run.
type Steer struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Message is a queue entry.
type Message struct {
	StreamID string
	RunID    string
}

// Queue enqueues runs.
type Queue interface {
	Enqueue(ctx context.Context, runID string) error
}

// Bus carries per-run control signals and steers.
type Bus interface {
	PublishCancel(ctx context.Context, runID string) error
	// SubscribeCancel calls fn when a cancel is published; the returned func unsubscribes.
	SubscribeCancel(ctx context.Context, runID string, fn func()) (func(), error)
	// PushSteer fails with ErrSteersClosed after FinalizeSteers.
	PushSteer(ctx context.Context, runID string, s Steer) error
	DrainSteers(ctx context.Context, runID string) ([]Steer, error)
	// FinalizeSteers atomically stops accepting steers and returns the pending ones.
	FinalizeSteers(ctx context.Context, runID string) ([]Steer, error)
	// ReopenSteers accepts steers again (the run continues after all).
	ReopenSteers(ctx context.Context, runID string) error
	// CloseSteers stops accepting steers and returns the pending ones without
	// removing them: AckSteers removes them once they are stored elsewhere.
	CloseSteers(ctx context.Context, runID string) ([]Steer, error)
	// AckSteers removes the first n pending steers (stored by the caller).
	AckSteers(ctx context.Context, runID string, n int) error
	// SteerRuns lists the runs that may still hold pending steers (the
	// sweeper stores those of finished runs).
	SteerRuns(ctx context.Context) ([]string, error)
}

// Redis implements Queue, Bus and the runtime consumer.
type Redis struct {
	rdb redis.UniversalClient
}

// New returns a Redis queue.
func New(rdb redis.UniversalClient) *Redis { return &Redis{rdb: rdb} }

var (
	_ Queue = (*Redis)(nil)
	_ Bus   = (*Redis)(nil)
)

func controlKey(runID string) string { return "work:run:" + runID + ":control" }
func steersKey(runID string) string  { return "work:run:" + runID + ":steers" }

// steerRunsKey indexes the runs with pending steers (SteerRuns).
const steerRunsKey = "work:steers:runs"

func closedKey(runID string) string { return "work:run:" + runID + ":steers:closed" }

func (q *Redis) Enqueue(ctx context.Context, runID string) error {
	return q.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: Stream,
		MaxLen: 100000,
		Approx: true,
		Values: []any{"runId", runID},
	}).Err()
}

// EnsureGroup creates the consumer group (idempotent), reading the stream
// from its start (entries enqueued before the first runtime started).
func (q *Redis) EnsureGroup(ctx context.Context) error { return q.ensureGroup(ctx, "0") }

func (q *Redis) ensureGroup(ctx context.Context, start string) error {
	err := q.rdb.XGroupCreateMkStream(ctx, Stream, Group, start).Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	return nil
}

// Read blocks up to block for at most count new entries for consumer.
func (q *Redis) Read(ctx context.Context, consumer string, count int, block time.Duration) ([]Message, error) {
	res, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    Group,
		Consumer: consumer,
		Streams:  []string{Stream, ">"},
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
	var out []Message
	for _, s := range res {
		for _, m := range s.Messages {
			id, _ := m.Values["runId"].(string)
			out = append(out, Message{StreamID: m.ID, RunID: id})
		}
	}
	return out, nil
}

// Reclaim moves to consumer the entries pending for longer than minIdle on
// other consumers (read by a runtime that stopped before acking them).
func (q *Redis) Reclaim(ctx context.Context, consumer string, minIdle time.Duration) ([]Message, error) {
	var out []Message
	start := "0-0"
	for i := 0; i < 10; i++ {
		msgs, next, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: Stream, Group: Group, Consumer: consumer,
			MinIdle: minIdle, Start: start, Count: 100}).Result()
		if err != nil {
			if strings.Contains(err.Error(), "NOGROUP") {
				return out, nil
			}
			return out, err
		}
		for _, m := range msgs {
			id, _ := m.Values["runId"].(string)
			out = append(out, Message{StreamID: m.ID, RunID: id})
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
func (q *Redis) PruneConsumers(ctx context.Context, keep string, idle time.Duration) (int, error) {
	consumers, err := q.rdb.XInfoConsumers(ctx, Stream, Group).Result()
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
		if err := q.rdb.XGroupDelConsumer(ctx, Stream, Group, c.Name).Err(); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Ack acknowledges a queue entry.
func (q *Redis) Ack(ctx context.Context, streamID string) error {
	return q.rdb.XAck(ctx, Stream, Group, streamID).Err()
}

func (q *Redis) PublishCancel(ctx context.Context, runID string) error {
	return q.rdb.Publish(ctx, controlKey(runID), "cancel").Err()
}

func (q *Redis) SubscribeCancel(ctx context.Context, runID string, fn func()) (func(), error) {
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

func (q *Redis) PushSteer(ctx context.Context, runID string, s Steer) error {
	data, _ := json.Marshal(s)
	ok, err := pushSteerScript.Run(ctx, q.rdb, []string{steersKey(runID), closedKey(runID), steerRunsKey}, string(data), MaxSteers, runID).Int()
	switch {
	case err != nil:
		return err
	case ok == -1:
		return ErrSteersClosed
	case ok == 0:
		return ErrTooManySteers
	}
	return nil
}

var drainScript = redis.NewScript(`
local items = redis.call('LRANGE', KEYS[1], 0, -1)
redis.call('DEL', KEYS[1])
redis.call('SREM', KEYS[2], ARGV[1])
return items`)

func (q *Redis) DrainSteers(ctx context.Context, runID string) ([]Steer, error) {
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

func (q *Redis) FinalizeSteers(ctx context.Context, runID string) ([]Steer, error) {
	items, err := finalizeScript.Run(ctx, q.rdb, []string{steersKey(runID), closedKey(runID), steerRunsKey}, runID).StringSlice()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, err
	}
	return decodeSteers(items), nil
}

func (q *Redis) ReopenSteers(ctx context.Context, runID string) error {
	return q.rdb.Del(ctx, closedKey(runID)).Err()
}

var closeScript = redis.NewScript(`
redis.call('SET', KEYS[2], '1', 'EX', 86400)
return redis.call('LRANGE', KEYS[1], 0, -1)`)

func (q *Redis) CloseSteers(ctx context.Context, runID string) ([]Steer, error) {
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

func (q *Redis) AckSteers(ctx context.Context, runID string, n int) error {
	return ackScript.Run(ctx, q.rdb, []string{steersKey(runID), steerRunsKey}, n, runID).Err()
}

func (q *Redis) SteerRuns(ctx context.Context) ([]string, error) {
	return q.rdb.SMembers(ctx, steerRunsKey).Result()
}

// MessageAppender stores thread messages (store.Store).
type MessageAppender interface {
	AppendMessages(ctx context.Context, msgs ...*store.Message) error
}

// KeepSteers closes a run's steer list and stores its pending steers in the
// thread (delivered=false), retrying transient Redis/Postgres errors until
// deadline. It returns the steers that are stored (only those may be announced
// as kept); on error the steers stay pending in Redis, listed by SteerRuns, so
// the sweeper stores them later: an accepted steer is never lost.
func KeepSteers(ctx context.Context, bus Bus, st MessageAppender, threadID, runID string, deadline time.Time) ([]Steer, error) {
	var left []Steer
	if err := retry(ctx, deadline, func(ctx context.Context) error {
		var err error
		left, err = bus.CloseSteers(ctx, runID)
		return err
	}); err != nil {
		return nil, err
	}
	if len(left) == 0 {
		_ = retry(ctx, deadline, func(ctx context.Context) error { return bus.AckSteers(ctx, runID, 0) })
		return nil, nil
	}
	msgs := SteerMessages(threadID, runID, left)
	if err := retry(ctx, deadline, func(ctx context.Context) error { return appendOnce(ctx, st, msgs) }); err != nil {
		return nil, err
	}
	// Stored: a failed ack only makes the sweeper find them stored already.
	_ = retry(ctx, deadline, func(ctx context.Context) error { return bus.AckSteers(ctx, runID, len(left)) })
	return left, nil
}

// appendOnce stores steer messages; ones already stored (same id) count as stored.
func appendOnce(ctx context.Context, st MessageAppender, msgs []*store.Message) error {
	err := st.AppendMessages(ctx, msgs...)
	if !errors.Is(err, store.ErrConflict) {
		return err
	}
	for _, m := range msgs {
		if err := st.AppendMessages(ctx, m); err != nil && !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return nil
}

// retry runs fn (5 s per attempt) with backoff until it succeeds or deadline passes.
func retry(ctx context.Context, deadline time.Time, fn func(ctx context.Context) error) error {
	for backoff := 50 * time.Millisecond; ; backoff = min(backoff*2, 2*time.Second) {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err := fn(actx)
		cancel()
		if err == nil || time.Now().Add(backoff).After(deadline) {
			return err
		}
		time.Sleep(backoff)
	}
}

func decodeSteers(items []string) []Steer {
	out := make([]Steer, 0, len(items))
	for _, it := range items {
		var s Steer
		if json.Unmarshal([]byte(it), &s) == nil && s.Text != "" {
			out = append(out, s)
		}
	}
	return out
}

// SteerMessages turns steers that will not reach the model of this run into
// thread messages, so the next run sees them.
func SteerMessages(threadID, runID string, steers []Steer) []*store.Message {
	out := make([]*store.Message, 0, len(steers))
	for _, st := range steers {
		out = append(out, &store.Message{ID: st.ID, ThreadID: threadID, RunID: runID, Role: store.RoleUser,
			Content: st.Text, Metadata: map[string]any{"steer": true, "delivered": false}})
	}
	return out
}
