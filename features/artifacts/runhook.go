package artifacts

import (
	"context"
	"time"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

// EndOfRunHook is the runtime hook that snapshots /workspace when a run ends
// (any status). Register it before the title and goal hooks: its work.artifact
// events come before theirs, and the goal evaluator sees the new artifacts.
type EndOfRunHook struct{ Svc *Service }

// Finishing implements runtime.Hook (compact runs only compact: no snapshot).
func (h EndOfRunHook) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if h.Svc != nil && f.Run.Kind != store.KindCompact {
		h.Svc.SnapshotEnd(ctx, f.Run, func(ev agui.Event) error { return f.Emit(ctx, ev) })
	}
	return nil
}

// PurgeJob is the retention job of the runtime sweeper (a runtime.Purger):
// threads soft-deleted more than After ago are hard-deleted, then the blobs
// nothing references any more. After ≤ 0 disables it.
type PurgeJob struct {
	Svc   *Service
	After time.Duration
}

// Purge implements runtime.Purger.
func (j PurgeJob) Purge(ctx context.Context) error {
	if j.Svc == nil || j.After <= 0 {
		return nil
	}
	_, _, err := j.Svc.Purge(ctx, j.After)
	return err
}
