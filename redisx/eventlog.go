package redisx

// The Redis-backed eventlog.Log: Redis streams for live tailing, the store
// (Postgres work_run_events) for durability.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
	"github.com/GhaziBenDahmane/harness/store"
)

// Writer appends events of one run. There is a single writer per run.
type Writer interface {
	// Append assigns the next seq, publishes the event live and buffers it for Postgres.
	Append(ctx context.Context, ev agui.Event) (int64, error)
	// Flush persists buffered events to Postgres (and work_runs.last_seq).
	Flush(ctx context.Context) error
	// Close flushes and schedules expiry of the live stream.
	Close(ctx context.Context) error
	LastSeq() int64
	// Terminal returns the log's terminal event (RUN_FINISHED/RUN_ERROR) if it has one.
	Terminal() (store.Event, bool)
}

// Log creates writers and replays runs.
type Log interface {
	Writer(runID string, lastSeq int64) Writer
	// Resume positions a writer after the highest seq already written (Redis or
	// Postgres), backfilling Postgres from Redis first. Used to finish runs of a
	// dead runtime.
	Resume(ctx context.Context, runID string, dbLastSeq int64) (eventlog.Writer, error)
	// Repair copies, from Redis to Postgres, the events of runs whose final flush
	// failed (Postgres outage at run end). Idempotent; returns runs completed.
	Repair(ctx context.Context) (int, error)
	// Replay streams events with seq > afterSeq, gap- and duplicate-free, then
	// tails until a terminal event. The channel closes at the end or on ctx done.
	Replay(ctx context.Context, runID string, afterSeq int64) <-chan store.Event
	// Finalizing marks a run whose row is (about to be) terminal while its end-of-run
	// continuations still emit events before the terminal one: replay keeps waiting
	// for the real terminal event instead of synthesizing one. The returned func
	// clears the mark (it also expires after ttl).
	Finalizing(ctx context.Context, runID string, ttl time.Duration) func()
}

// Redis returns the underlying client.
func (l *EventLog) Redis() redis.UniversalClient { return l.rdb }

// EventKey is the Redis stream of a run's events.
func EventKey(runID string) string { return "work:run:" + runID + ":events" }

// finalizingKey marks a run whose terminal event is still to come (see Log.Finalizing).
func finalizingKey(runID string) string { return "work:run:" + runID + ":finalizing" }

// EventLog implements Log with Redis + store.Store.
type EventLog struct {
	rdb   redis.UniversalClient
	store store.Store
	// TailBlock is the XREAD BLOCK duration (default 15 s).
	TailBlock time.Duration
	// TTL is the stream expiry after run end (default 24 h).
	TTL time.Duration
	// Poll is the wait used when the live stream has a gap not yet in Postgres.
	Poll time.Duration
	// TerminalWait bounds how long replay waits for the terminal event of a run
	// whose row is already terminal, before synthesizing one (default 5 s).
	TerminalWait time.Duration
	// CloseRetry bounds how long Close retries a failing flush (default 30 s).
	CloseRetry time.Duration
}

// NewEventLog returns a EventLog.
func NewEventLog(rdb redis.UniversalClient, st store.Store) *EventLog {
	return &EventLog{rdb: rdb, store: st, TailBlock: 15 * time.Second, TTL: 24 * time.Hour, Poll: 500 * time.Millisecond, TerminalWait: 5 * time.Second, CloseRetry: 30 * time.Second}
}

var _ eventlog.Log = (*EventLog)(nil)

type writer struct {
	l     *EventLog
	runID string
	mu    sync.Mutex
	seq   int64
	buf   []store.Event
	last  store.Event // last event written or found in the log
}

// repairKey is the Redis set of runs whose events still need copying to Postgres.
const repairKey = "work:eventlog:repair"

func (l *EventLog) Writer(runID string, lastSeq int64) eventlog.Writer {
	return &writer{l: l, runID: runID, seq: lastSeq}
}

func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

func (w *writer) Append(ctx context.Context, ev agui.Event) (int64, error) {
	ctx, cancel := detached(ctx)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := ev["timestamp"]; !ok {
		ev["timestamp"] = time.Now().UnixMilli()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("eventlog: marshal: %w", err)
	}
	// Same bytes in Redis and Postgres: strip what Postgres jsonb rejects (\u0000).
	data = store.CleanJSON(data)
	seq := w.seq + 1
	err = w.l.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: EventKey(w.runID),
		ID:     "0-" + strconv.FormatInt(seq, 10),
		Values: []any{"data", string(data)},
	}).Err()
	if err != nil && strings.Contains(err.Error(), "equal or smaller") {
		// go-redis retries a command whose reply timed out, even when Redis applied
		// it: the entry already holding these exact bytes is our own write.
		if !w.ownEntry(ctx, seq, data) {
			return 0, eventlog.ErrConflict
		}
		err = nil
	}
	if err != nil {
		// Redis unavailable: keep going, Postgres stays the source of truth.
		slog.Warn("eventlog: redis append failed", "run_id", w.runID, "seq", seq, "error", err)
	}
	w.seq = seq
	w.last = store.Event{Seq: seq, Type: ev.Type(), Data: data}
	w.buf = append(w.buf, w.last)
	return seq, nil
}

// ownEntry reports whether the stream entry seq holds exactly data (an
// idempotent retry of this writer's XADD, not another writer's event).
func (w *writer) ownEntry(ctx context.Context, seq int64, data []byte) bool {
	id := "0-" + strconv.FormatInt(seq, 10)
	msgs, err := w.l.rdb.XRange(ctx, EventKey(w.runID), id, id).Result()
	if err != nil || len(msgs) != 1 {
		return false
	}
	stored, _ := msgs[0].Values["data"].(string)
	return stored == string(data)
}

func (w *writer) Flush(ctx context.Context) error {
	ctx, cancel := detached(ctx)
	defer cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return nil
	}
	kept, err := eventlog.Persist(ctx, w.l.store, w.runID, w.buf)
	w.buf = kept
	return err
}

// Close flushes, retrying with backoff for at most CloseRetry (or until ctx's
// deadline, if earlier: shutdown); if events are still unflushed the run is
// queued for Repair (Redis keeps them 24 h).
func (w *writer) Close(ctx context.Context) error {
	err := w.Flush(ctx)
	deadline := time.Now().Add(w.l.CloseRetry)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for backoff := 50 * time.Millisecond; err != nil && time.Now().Before(deadline); backoff = min(backoff*2, 2*time.Second) {
		time.Sleep(min(backoff, time.Until(deadline)))
		err = w.Flush(ctx)
	}
	if err != nil {
		rctx, cancel := detached(ctx)
		if serr := w.l.rdb.SAdd(rctx, repairKey, w.runID).Err(); serr != nil {
			slog.Error("eventlog: cannot schedule repair; events only in Redis", "run_id", w.runID, "error", serr)
		}
		cancel()
		slog.Error("eventlog: events not persisted at run end, scheduled for repair", "run_id", w.runID, "error", err)
	}
	ectx, cancel := detached(ctx)
	defer cancel()
	if xerr := w.l.rdb.Expire(ectx, EventKey(w.runID), w.l.TTL).Err(); xerr != nil {
		slog.Warn("eventlog: expire failed", "run_id", w.runID, "error", xerr)
	}
	return err
}

func (w *writer) Terminal() (store.Event, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last, agui.IsTerminal(w.last.Type)
}

func (w *writer) LastSeq() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

func (l *EventLog) Resume(ctx context.Context, runID string, dbLastSeq int64) (eventlog.Writer, error) {
	last, _, err := l.backfill(ctx, runID, dbLastSeq)
	if err != nil {
		return nil, err
	}
	// The terminal event is always the last one: look at the end of the whole
	// log (Redis stream and Postgres), not only after dbLastSeq.
	end, err := l.lastEvent(ctx, runID)
	if err != nil {
		return nil, err
	}
	return &writer{l: l, runID: runID, seq: max(last, end.Seq), last: end}, nil
}

// lastEvent is the highest-seq event of a run in Redis or Postgres.
func (l *EventLog) lastEvent(ctx context.Context, runID string) (store.Event, error) {
	var end store.Event
	msgs, err := l.rdb.XRevRangeN(ctx, EventKey(runID), "+", "-", 1).Result()
	if err != nil {
		return end, fmt.Errorf("eventlog: read stream end: %w", err)
	}
	if evs := toEvents(msgs); len(evs) == 1 {
		end = evs[0]
	}
	pg, ok, err := l.store.LastEvent(ctx, runID)
	if err != nil {
		return end, err
	}
	if ok && pg.Seq > end.Seq {
		end = pg
	}
	return end, nil
}

// backfill copies Redis events after `after` into Postgres; returns the last seq/type seen.
func (l *EventLog) backfill(ctx context.Context, runID string, after int64) (int64, string, error) {
	last, lastType := after, ""
	for {
		evs, _, err := l.redisRange(ctx, runID, last)
		if err != nil {
			return last, lastType, fmt.Errorf("eventlog: backfill read: %w", err)
		}
		if len(evs) == 0 {
			return last, lastType, nil
		}
		if _, err := eventlog.Persist(ctx, l.store, runID, evs); err != nil {
			return last, lastType, err
		}
		last, lastType = evs[len(evs)-1].Seq, evs[len(evs)-1].Type
	}
}

func (l *EventLog) Repair(ctx context.Context) (int, error) {
	ids, err := l.rdb.SMembers(ctx, repairKey).Result()
	if err != nil {
		return 0, err
	}
	done := 0
	for _, id := range ids {
		run, err := l.store.GetRun(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			_ = l.rdb.SRem(ctx, repairKey, id).Err()
			continue
		}
		if err != nil {
			return done, err
		}
		if _, _, err := l.backfill(ctx, id, run.LastSeq); err != nil {
			slog.Warn("eventlog: repair failed, will retry", "run_id", id, "error", err)
			continue
		}
		if store.IsTerminal(run.Status) {
			_ = l.rdb.SRem(ctx, repairKey, id).Err()
			done++
			slog.Info("eventlog: run events repaired from Redis", "run_id", id)
		}
	}
	return done, nil
}

func parseSeq(id string) (int64, bool) {
	_, after, ok := strings.Cut(id, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(after, 10, 64)
	return n, err == nil
}

func toEvents(msgs []redis.XMessage) []store.Event {
	out := make([]store.Event, 0, len(msgs))
	for _, m := range msgs {
		seq, ok := parseSeq(m.ID)
		if !ok {
			continue
		}
		data, _ := m.Values["data"].(string)
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(data), &head)
		out = append(out, store.Event{Seq: seq, Type: head.Type, Data: json.RawMessage(data)})
	}
	return out
}

// redisRange returns events with seq > after; exists=false when the key is absent.
func (l *EventLog) redisRange(ctx context.Context, runID string, after int64) ([]store.Event, bool, error) {
	msgs, err := l.rdb.XRangeN(ctx, EventKey(runID), "0-"+strconv.FormatInt(after+1, 10), "+", 1000).Result()
	if err != nil {
		return nil, false, err
	}
	if len(msgs) > 0 {
		return toEvents(msgs), true, nil
	}
	n, err := l.rdb.Exists(ctx, EventKey(runID)).Result()
	if err != nil {
		return nil, false, err
	}
	return nil, n > 0, nil
}

func (l *EventLog) Replay(ctx context.Context, runID string, afterSeq int64) <-chan store.Event {
	ch := make(chan store.Event, 64)
	go func() {
		defer close(ch)
		r := &replayer{l: l, runID: runID, last: afterSeq, ch: ch}
		r.run(ctx)
	}()
	return ch
}

type replayer struct {
	l     *EventLog
	runID string
	last  int64
	ch    chan<- store.Event
	done  bool
}

// emit forwards contiguous events; returns progress and whether a gap was hit.
func (r *replayer) emit(ctx context.Context, evs []store.Event) (progress, gap bool) {
	for _, e := range evs {
		if r.done {
			return progress, false
		}
		if e.Seq <= r.last {
			continue
		}
		if e.Seq != r.last+1 {
			return progress, true
		}
		select {
		case r.ch <- e:
		case <-ctx.Done():
			r.done = true
			return progress, false
		}
		r.last = e.Seq
		progress = true
		if agui.IsTerminal(e.Type) {
			r.done = true
		}
	}
	return progress, false
}

// catchUp reads everything currently available (Redis, Postgres for gaps or
// when the stream is gone). Returns whether anything was emitted and whether a
// gap remains.
func (r *replayer) catchUp(ctx context.Context) (progress, gap bool) {
	for !r.done && ctx.Err() == nil {
		evs, exists, err := r.l.redisRange(ctx, r.runID, r.last)
		if err != nil && ctx.Err() == nil {
			slog.Warn("eventlog: redis replay failed, using postgres", "run_id", r.runID, "error", err)
		}
		needPG := err != nil || !exists || (len(evs) > 0 && evs[0].Seq > r.last+1)
		stepProgress := false
		if needPG {
			pg, perr := r.l.store.ListEvents(ctx, r.runID, r.last, 1000)
			if perr != nil {
				if ctx.Err() == nil {
					slog.Warn("eventlog: postgres replay failed", "run_id", r.runID, "error", perr)
				}
				return progress, true
			}
			p, _ := r.emit(ctx, pg)
			stepProgress = stepProgress || p
		}
		p, g := r.emit(ctx, evs)
		stepProgress = stepProgress || p
		progress = progress || stepProgress
		if !stepProgress {
			return progress, g
		}
	}
	return progress, false
}

func (r *replayer) run(ctx context.Context) {
	if r.last > 0 && r.endedBefore(ctx) {
		return
	}
	var terminalSince time.Time
	for !r.done && ctx.Err() == nil {
		progress, gap := r.catchUp(ctx)
		if r.done || ctx.Err() != nil {
			return
		}
		if progress {
			terminalSince = time.Time{}
			continue
		}
		run, err := r.l.store.GetRun(ctx, r.runID)
		if err == nil && store.IsTerminal(run.Status) {
			// The terminal event is written right after the status change: keep
			// re-reading Postgres and Redis for TerminalWait, then end with a
			// synthetic event derived from the run row so clients always stop.
			if terminalSince.IsZero() || r.finalizing(ctx) {
				terminalSince = time.Now()
			} else if time.Since(terminalSince) >= r.l.TerminalWait {
				r.emitSynthetic(ctx, run)
				return
			}
			r.sleep(ctx, r.l.Poll)
			continue
		}
		if gap || err != nil {
			r.sleep(ctx, r.l.Poll)
			continue
		}
		r.block(ctx)
	}
}

// endedBefore reports a log whose terminal event is at or before the replay
// position (a client reconnecting after the end): nothing is left to send.
func (r *replayer) endedBefore(ctx context.Context) bool {
	end, err := r.l.lastEvent(ctx, r.runID)
	return err == nil && agui.IsTerminal(end.Type) && end.Seq <= r.last
}

// finalizing reports a run whose runtime still runs its end-of-run continuations.
func (r *replayer) finalizing(ctx context.Context) bool {
	n, err := r.l.rdb.Exists(ctx, finalizingKey(r.runID)).Result()
	return err == nil && n > 0
}

func (l *EventLog) Finalizing(ctx context.Context, runID string, ttl time.Duration) func() {
	sctx, cancel := detached(ctx)
	defer cancel()
	if err := l.rdb.Set(sctx, finalizingKey(runID), "1", ttl).Err(); err != nil {
		slog.Warn("eventlog: marking the run finalizing failed", "run_id", runID, "error", err)
	}
	return func() {
		cctx, cancel := detached(ctx)
		defer cancel()
		_ = l.rdb.Del(cctx, finalizingKey(runID)).Err()
	}
}

// emitSynthetic ends a finished run whose log has no terminal event (not
// persisted). An empty log first gets RUN_STARTED (strict AG-UI order).
func (r *replayer) emitSynthetic(ctx context.Context, run *store.Run) {
	if r.last == 0 {
		start := agui.RunStartedEvent(run.ThreadID, run.ID)
		start["timestamp"] = time.Now().UnixMilli()
		data, _ := json.Marshal(start)
		select {
		case r.ch <- store.Event{Seq: 1, Type: start.Type(), Data: data}:
			r.last = 1
		case <-ctx.Done():
			return
		}
	}
	var ev agui.Event
	switch run.Status {
	case store.StatusCompleted:
		ev = agui.RunFinishedEvent(run.ThreadID, run.ID, "success", nil)
	case store.StatusCancelled:
		ev = agui.RunFinishedEvent(run.ThreadID, run.ID, "cancelled", nil)
	default:
		ev = agui.RunErrorEvent(store.ErrorCodeFor(run.Status, run.ErrorCode), "The run ended unexpectedly.")
	}
	ev["timestamp"] = time.Now().UnixMilli()
	data, _ := json.Marshal(ev)
	select {
	case r.ch <- store.Event{Seq: r.last + 1, Type: ev.Type(), Data: data}:
	case <-ctx.Done():
	}
}

func (r *replayer) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// block waits until the live stream has an entry after r.last (or timeout).
func (r *replayer) block(ctx context.Context) {
	_, err := r.l.rdb.XRead(ctx, &redis.XReadArgs{
		Streams: []string{EventKey(r.runID), "0-" + strconv.FormatInt(r.last, 10)},
		Count:   1,
		Block:   r.l.TailBlock,
	}).Result()
	if err != nil && !errors.Is(err, redis.Nil) && ctx.Err() == nil {
		r.sleep(ctx, time.Second)
	}
}
