package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

// finishOutage makes FinishRun fail until a deadline, like Postgres being down at the end of a run.
type finishOutage struct {
	*memstore.Store
	until atomic.Int64
}

func (f *finishOutage) FinishRun(ctx context.Context, id, rt, status, code, msg string) error {
	if time.Now().UnixNano() < f.until.Load() {
		return errors.New("dial tcp: connection refused")
	}
	return f.Store.FinishRun(ctx, id, rt, status, code, msg)
}

func TestFinishRunSurvivesAShortPostgresOutage(t *testing.T) {
	// A Postgres outage of a few seconds at run end still records the run as completed (same retry budget as the event log).
	// Arrange
	r := newRig(t)
	st := &finishOutage{Store: r.st}
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("done"), OnCall: func() {
		st.until.Store(time.Now().Add(1600 * time.Millisecond).UnixNano())
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	rt := runtime.New(r.opts, st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	_, run := r.queued("q")

	// Act
	_ = rt.Process(context.Background(), run.ID)

	// Assert
	if got := r.reload(run); got.Status != store.StatusCompleted {
		t.Fatalf("status %s", got.Status)
	}
}

func TestSweepNeverAppendsASecondTerminalEvent(t *testing.T) {
	// A stale run whose log already ends with RUN_FINISHED (persisted) gets no RUN_ERROR; its row is set to match.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	ctx := context.Background()
	_, _ = r.st.ClaimRun(ctx, run.ID, "dead")
	w := r.log.Writer(run.ID, 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
	_, _ = w.Append(ctx, agui.RunFinishedEvent(run.ThreadID, run.ID, "cancelled", nil))
	_ = w.Close(ctx)
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))

	// Act
	r.runtime().Sweep(ctx)

	// Assert
	evs := r.events(run.ID)
	if !equal(types(evs), []string{"RUN_STARTED", "RUN_FINISHED"}) || !equal(r.liveTypes(run.ID), []string{"RUN_STARTED", "RUN_FINISHED"}) {
		t.Fatalf("events %v live %v", types(evs), r.liveTypes(run.ID))
	}
	if got := r.reload(run); got.Status != store.StatusCancelled {
		t.Fatalf("status %s", got.Status)
	}
}
