package websearch

import (
	"context"
	"log/slog"
	"time"

	"github.com/GhaziBenDahmane/harness/runtime"
	"github.com/GhaziBenDahmane/harness/store"
)

// ThreadReader finds the run started by a run's end (goal continuation).
type ThreadReader interface {
	GetThread(ctx context.Context, login, id string) (*store.Thread, error)
}

// Hook clears a run's token when the run ends (runtime.Hook). Listed after the
// goals hook: the goal run it started gets the token first (same user and
// thread; unless that run already has a fresher one), so autonomous goal turns
// keep web search like the run that started them. It also purges the tokens of
// runs that ended without this hook (sweeper, cancelled while queued) and the
// expired ones.
type Hook struct {
	Vault   *Vault
	Threads ThreadReader
}

// Handoff gives the token of the run that ended to the goal run it started,
// before that run is enqueued (goals.Hook.Handoff): the run sees the token
// when it starts.
func (h *Hook) Handoff(ctx context.Context, prev, next *store.Run) {
	if prev.UserLogin != next.UserLogin || prev.ThreadID != next.ThreadID {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if ok, err := h.Vault.Handoff(ctx, prev.ID, next.ID, prev.UserLogin); err == nil && ok {
		slog.Debug("web search token handed to the goal run", "run_id", prev.ID, "next_run_id", next.ID)
	}
}

// Finishing implements runtime.Hook.
func (h *Hook) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	run := f.Run
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if h.Threads != nil {
			if t, err := h.Threads.GetThread(ctx, run.UserLogin, run.ThreadID); err == nil && t.ActiveRun != nil && t.ActiveRun.ID != run.ID {
				if ok, err := h.Vault.Handoff(ctx, run.ID, t.ActiveRun.ID, run.UserLogin); err == nil && ok {
					slog.Debug("web search token handed to the next run", "run_id", run.ID, "next_run_id", t.ActiveRun.ID)
				}
			}
		}
		if err := h.Vault.Delete(ctx, run.ID); err != nil {
			slog.Warn("web search: clearing the run token failed (purged later)", "run_id", run.ID, "error", err)
		}
		if _, err := h.Vault.Store.PurgeRunCredentials(ctx); err != nil {
			slog.Warn("web search: purging run tokens failed", "error", err)
		}
	}
}
