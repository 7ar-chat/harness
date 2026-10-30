package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
	"github.com/GhaziBenDahmane/harness/engine/queue"
)

func (r *Runtime) sweepLoop(ctx context.Context) {
	t := time.NewTicker(r.opts.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep(ctx)
		}
	}
}

// Sweep re-enqueues stale queued runs and interrupts runs whose runtime stopped heartbeating.
func (r *Runtime) Sweep(ctx context.Context) {
	if r.locks != nil {
		ttl := r.opts.SweepEvery - time.Second
		if ttl < time.Second {
			ttl = time.Second
		}
		ok, err := r.locks.SetNX(ctx, "work:sweeper:lock", r.opts.ID, ttl)
		if err != nil || !ok {
			return
		}
	}
	ids, err := r.store.ListQueuedRunsOlderThan(ctx, r.opts.QueuedAge)
	if err != nil {
		slog.Warn("sweeper: listing queued runs failed", "error", err)
	}
	for _, id := range ids {
		if err := r.q.Enqueue(ctx, id); err != nil {
			slog.Warn("sweeper: re-enqueue failed", "run_id", id, "error", err)
		}
	}
	r.queueHygiene(ctx)
	if n, err := r.log.Repair(ctx); err != nil {
		slog.Warn("sweeper: event repair failed", "error", err)
	} else if n > 0 {
		slog.Info("sweeper: repaired run event logs", "runs", n)
	}
	stale, err := r.store.InterruptStaleRuns(ctx, r.opts.StaleAge, r.aliveInRedis(ctx))
	if err != nil {
		slog.Warn("sweeper: interrupting stale runs failed", "error", err)
	}
	for _, run := range stale {
		slog.Warn("sweeper: run interrupted (runtime stopped heartbeating)", "run_id", run.ID, "runtime_id", run.RuntimeID)
		r.finishInterrupted(ctx, run, r.keepUndeliveredSteers(ctx, run))
	}
	r.sweepSteers(ctx)
	r.purge(ctx)
	r.runJobs(ctx)
}

// queueHygiene reclaims queue entries left pending by stopped runtimes (their
// runs are enqueued again when still queued, the entries acked) and deletes
// the consumers of stopped runtimes.
func (r *Runtime) queueHygiene(ctx context.Context) {
	msgs, err := r.q.Reclaim(ctx, r.opts.ID, r.opts.QueuedAge)
	if err != nil {
		slog.Warn("sweeper: reclaiming pending queue entries failed", "error", err)
	}
	for _, m := range msgs {
		if run, err := r.store.GetRun(ctx, m.RunID); err == nil && run.Status == store.StatusQueued {
			if err := r.q.Enqueue(ctx, m.RunID); err != nil {
				slog.Warn("sweeper: re-enqueue failed", "run_id", m.RunID, "error", err)
				continue
			}
		}
		if err := r.q.Ack(ctx, m.StreamID); err != nil {
			slog.Warn("sweeper: ack of a reclaimed entry failed", "error", err)
		}
	}
	if n, err := r.q.PruneConsumers(ctx, r.opts.ID, r.opts.ConsumerIdle); err != nil {
		slog.Warn("sweeper: pruning queue consumers failed", "error", err)
	} else if n > 0 {
		slog.Info("sweeper: deleted queue consumers of stopped runtimes", "consumers", n)
	}
}

// finishInterrupted appends RUN_ERROR after the highest seq written so far. A
// runtime that is still alive may append concurrently: on conflict, re-read and retry.
func (r *Runtime) finishInterrupted(ctx context.Context, run *store.Run, leftover []queue.Steer) {
	for attempt := 0; attempt < 5; attempt++ {
		w, err := r.log.Resume(ctx, run.ID, run.LastSeq)
		if err != nil {
			slog.Error("sweeper: resume event log failed", "run_id", run.ID, "error", err)
			return
		}
		if end, ok := w.Terminal(); ok {
			// The runtime did finish (e.g. Postgres was down at run end): never add a
			// second terminal event; make the row match the log instead.
			status, code, msg := outcomeOf(end)
			if err := r.store.SetRunOutcome(ctx, run.ID, status, code, msg); err != nil {
				slog.Error("sweeper: correcting run status failed", "run_id", run.ID, "error", err)
			}
			_ = w.Close(ctx)
			return
		}
		if w.LastSeq() == 0 {
			// The runtime died before its first event: strict AG-UI order.
			_, err = w.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
		}
		if err == nil {
			err = announceSteers(ctx, w, leftover)
		}
		if err == nil {
			_, err = w.Append(ctx, agui.RunErrorEvent(agui.CodeInterrupted, InterruptedMessage))
		}
		if errors.Is(err, eventlog.ErrConflict) {
			continue
		}
		if err != nil {
			slog.Error("sweeper: writing RUN_ERROR failed", "run_id", run.ID, "error", err)
		}
		_ = w.Close(ctx)
		return
	}
	slog.Error("sweeper: gave up writing RUN_ERROR (log keeps moving)", "run_id", run.ID)
}
