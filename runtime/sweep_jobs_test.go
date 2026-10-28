package runtime_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/memory"
	"github.com/GhaziBenDahmane/harness/runtime"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestSweepRunsPeriodicJobsAtMostOncePerPeriodAcrossRuntimes(t *testing.T) {
	// A job runs on the first sweep and not on another runtime within its period (shared Redis lock); once the lock expired, a runtime runs it again.
	// Arrange
	r := newRig(t)
	var runs atomic.Int32
	r.opts.Jobs = []runtime.Job{{Name: "count", Every: time.Hour, Run: func(context.Context) error { runs.Add(1); return nil }}}
	ctx := context.Background()
	newRuntime := func(id string) *runtime.Runtime {
		return runtime.New(runtime.Options{ID: id, Jobs: r.opts.Jobs}, r.st, r.log, r.q, r.q, r.agent, r.rdb)
	}

	// Act
	r.runtime().Sweep(ctx)
	r.runtime().Sweep(ctx)
	r.mr.FastForward(time.Minute) // sweeper lock only
	newRuntime("rt-other").Sweep(ctx)
	withinPeriod := runs.Load()
	r.mr.FastForward(2 * time.Hour)
	newRuntime("rt-restarted").Sweep(ctx)

	// Assert
	if withinPeriod != 1 || runs.Load() != 2 {
		t.Fatalf("runs within the period %d, after it %d", withinPeriod, runs.Load())
	}
}

func TestSweepConsolidatesMemory(t *testing.T) {
	// Wired like cmd/work, the memory consolidation job soft-deletes duplicate semantic entries.
	// Arrange
	r := newRig(t)
	r.remember("Prefers French answers", "memories/semantic/preferences/a.md")
	r.remember("Prefers French answers", "memories/semantic/preferences/b.md")
	r.opts.Jobs = []runtime.Job{{Name: "memory-consolidation", Every: time.Hour,
		Run: memory.Consolidator{Store: r.st, Retention: 30 * 24 * time.Hour}.Run}}

	// Act
	r.runtime().Sweep(context.Background())

	// Assert
	if active, deleted := r.entries(store.MemoryActive), r.entries(store.MemoryDeleted); len(active) != 1 || len(deleted) != 1 {
		t.Fatalf("active %d deleted %d", len(active), len(deleted))
	}
}
