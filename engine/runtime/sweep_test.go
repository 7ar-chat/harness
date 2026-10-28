package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestSweepInterruptsRunsOfDeadRuntimes(t *testing.T) {
	// A running run without heartbeat for 60 s is marked interrupted and gets RUN_ERROR after its unflushed events.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	ctx := context.Background()
	_, _ = r.st.ClaimRun(ctx, run.ID, "dead-runtime")
	w := r.log.Writer(run.ID, 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
	_, _ = w.Append(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": "step-1"}))
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))

	// Act
	r.runtime().Sweep(ctx)

	// Assert
	got := r.reload(run)
	evs := r.events(run.ID)
	if got.Status != store.StatusInterrupted || !equal(types(evs), []string{"RUN_STARTED", "STEP_STARTED", "RUN_ERROR"}) || last(evs)["code"] != "interrupted" {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
}

func TestSweepLeavesHealthyRunsAlone(t *testing.T) {
	// Runs with a recent heartbeat are not interrupted.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	_, _ = r.st.ClaimRun(context.Background(), run.ID, "alive")

	// Act
	r.runtime().Sweep(context.Background())

	// Assert
	if got := r.reload(run); got.Status != store.StatusRunning {
		t.Fatalf("status %s", got.Status)
	}
}

func TestSweepRequeuesStaleQueuedRuns(t *testing.T) {
	// Queued runs older than 30 s are pushed to work:runs again.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	r.st.SetRunCreatedAt(run.ID, time.Now().Add(-time.Minute))

	// Act
	r.runtime().Sweep(context.Background())

	// Assert
	entries, _ := r.rdb.XRange(context.Background(), "work:runs", "-", "+").Result()
	if len(entries) != 1 || entries[0].Values["runId"] != run.ID {
		t.Fatalf("entries %v", entries)
	}
}

func TestSweepWritesRunStartedBeforeRunErrorOfARunWithAnEmptyLog(t *testing.T) {
	// A run whose runtime died between claim and its first event is interrupted with RUN_STARTED then RUN_ERROR.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	ctx := context.Background()
	_, _ = r.st.ClaimRun(ctx, run.ID, "dead-runtime")
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))

	// Act
	r.runtime().Sweep(ctx)

	// Assert
	if evs := r.events(run.ID); !equal(types(evs), []string{"RUN_STARTED", "RUN_ERROR"}) {
		t.Fatalf("types %v", types(evs))
	}
}
