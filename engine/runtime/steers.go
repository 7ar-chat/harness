package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
)

// keepUndeliveredSteers closes the run's steer list and stores leftovers as
// thread messages; it returns the stored ones (to announce). Steers it cannot
// store stay pending in Redis and are stored by a later sweep (sweepSteers).
func (r *Runtime) keepUndeliveredSteers(ctx context.Context, run *store.Run) []queue.Steer {
	if r.bus == nil {
		return nil
	}
	kept, err := queue.KeepSteers(ctx, r.bus, r.store, run.ThreadID, run.ID, r.finishDeadline(r.steerBudget()))
	if err != nil {
		slog.Error("keeping undelivered steers failed; the sweeper will store them", "run_id", run.ID, "error", err)
	}
	return kept
}

// steerBudget bounds the retries of keeping a run's steers (a third of FinishRetry).
func (r *Runtime) steerBudget() time.Duration { return r.opts.FinishRetry / 3 }

// sweepSteers stores the pending steers of finished runs whose runtime could
// not store them (Redis or Postgres down at finalize).
func (r *Runtime) sweepSteers(ctx context.Context) {
	if r.bus == nil {
		return
	}
	ids, err := r.bus.SteerRuns(ctx)
	if err != nil {
		slog.Warn("sweeper: listing runs with pending steers failed", "error", err)
		return
	}
	for _, id := range ids {
		run, err := r.store.GetRun(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			_ = r.bus.AckSteers(ctx, id, queue.MaxSteers)
			continue
		case err != nil || !store.IsTerminal(run.Status):
			continue
		}
		kept, err := queue.KeepSteers(ctx, r.bus, r.store, run.ThreadID, run.ID, time.Now().Add(time.Second))
		if err != nil {
			slog.Warn("sweeper: storing pending steers failed, will retry", "run_id", id, "error", err)
		} else if len(kept) > 0 {
			slog.Info("sweeper: stored pending steers of a finished run", "run_id", id, "steers", len(kept))
		}
	}
}

// announceSteers tells clients which steers were kept for the next run (before the terminal event).
func announceSteers(ctx context.Context, w eventlog.Writer, steers []queue.Steer) error {
	for _, s := range steers {
		if _, err := w.Append(ctx, agui.SteerEvent(s.ID, s.Text, false)); err != nil {
			return err
		}
	}
	return nil
}
