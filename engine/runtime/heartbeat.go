package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// heartbeatKey is the Redis liveness signal of a run (value: its runtime id),
// kept for StaleAge: the sweeper spares runs that have one, so a Postgres
// outage longer than StaleAge does not get live runs interrupted.
func heartbeatKey(runID string) string { return "work:run:" + runID + ":hb" }

// heartbeat keeps the run's liveness signal fresh until done is closed, and removes it then. The first signal is written
// by the caller before this goroutine starts (execute), so every run writes at least one however short it is: a run that
// ended before the goroutine was first scheduled used to write none.
func (r *Runtime) heartbeat(run *store.Run, cancel context.CancelCauseFunc, done <-chan struct{}) {
	t := time.NewTimer(r.opts.Heartbeat)
	defer t.Stop()
	defer r.clearHeartbeat(run.ID)
	for {
		select {
		case <-done:
			return
		case <-t.C:
		}
		r.redisHeartbeat(run.ID)
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		owned, cancelRequested, err := r.store.Heartbeat(ctx, run.ID, r.opts.ID)
		c()
		next := r.opts.Heartbeat
		switch {
		case err != nil:
			// Retry soon: the first write after Postgres is back refreshes heartbeat_at.
			slog.Warn("heartbeat failed", "run_id", run.ID, "error", err)
			next = min(next, time.Second)
		case !owned:
			cancel(errLost)
		case cancelRequested:
			cancel(ErrCancelled)
		}
		t.Reset(next)
	}
}

func (r *Runtime) redisHeartbeat(runID string) {
	if r.locks == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.locks.Set(ctx, heartbeatKey(runID), r.opts.ID, r.opts.StaleAge); err != nil {
		slog.Warn("redis heartbeat failed", "run_id", runID, "error", err)
	}
}

func (r *Runtime) clearHeartbeat(runID string) {
	if r.locks == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = r.locks.Del(ctx, heartbeatKey(runID))
}

// aliveInRedis reports a run whose runtime heartbeats in Redis (or whose
// liveness cannot be checked: never interrupt on a Redis error).
func (r *Runtime) aliveInRedis(ctx context.Context) func(*store.Run) bool {
	return func(run *store.Run) bool {
		if r.locks == nil {
			return false
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		ok, err := r.locks.Exists(cctx, heartbeatKey(run.ID))
		return err != nil || ok
	}
}
