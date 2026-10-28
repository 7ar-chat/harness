package gitsafe

import (
	"context"
	"errors"
	"log/slog"

	"github.com/GhaziBenDahmane/harness/store"
)

// Hook is an agent.RunStartHook that snapshots the repository of Dir before
// each run (nothing happens when Dir is not a git repository).
type Hook struct {
	Dir string
}

// BeforeRun implements agent.RunStartHook.
func (h Hook) BeforeRun(ctx context.Context, run *store.Run) {
	if run.Kind == store.KindCompact {
		return
	}
	s, err := Snapshot(ctx, h.Dir, run.ThreadID)
	switch {
	case errors.Is(err, ErrNotRepo):
	case err != nil:
		slog.Warn("gitsafe: snapshot failed", "run_id", run.ID, "error", err)
	default:
		slog.Info("gitsafe: snapshot", "run_id", run.ID, "ref", s.Ref)
	}
}
