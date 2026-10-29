package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

// racingLog lets a still-alive runtime write one more event right after the sweeper resumed the log.
type racingLog struct {
	eventlog.Log
	live eventlog.Writer
	once bool
}

func (l *racingLog) Resume(ctx context.Context, runID string, last int64) (eventlog.Writer, error) {
	w, err := l.Log.Resume(ctx, runID, last)
	if !l.once {
		l.once = true
		_, _ = l.live.Append(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": "step-2"}))
	}
	return w, err
}

func TestSweepRetriesWhenAliveRuntimeWritesConcurrently(t *testing.T) {
	// If the old runtime appends while the sweeper finishes its run, the sweeper retries so RUN_ERROR is still written last.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	ctx := context.Background()
	_, _ = r.st.ClaimRun(ctx, run.ID, "slow-runtime")
	live := r.log.Writer(run.ID, 0)
	_, _ = live.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))
	rt := runtime.New(r.opts, r.st, &racingLog{Log: r.log, live: live}, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))

	// Act
	rt.Sweep(ctx)

	// Assert
	types := r.liveTypes(run.ID)
	if len(types) != 3 || types[2] != "RUN_ERROR" {
		t.Fatalf("live events %v", types)
	}
}

func TestRunThatLostOwnershipStillExpiresItsStream(t *testing.T) {
	// A runtime that discovers its run was taken over stops and still schedules expiry of the live stream.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("working"), Block: true})
	r.opts.Heartbeat = 20 * time.Millisecond
	_, run := r.queued("q")
	done := make(chan struct{})
	go func() { r.process(run); close(done) }()
	waitFor(t, "streaming", func() bool { return contains(r.liveTypes(run.ID), "TEXT_MESSAGE_CONTENT") })

	// Act
	_, _ = r.st.InterruptStaleRuns(context.Background(), -time.Hour)

	// Assert
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop")
	}
	if ttl := r.mr.TTL(redisx.EventKey(run.ID)); ttl <= 0 {
		t.Fatalf("stream ttl %v", ttl)
	}
	if got := r.reload(run); got.Status != store.StatusInterrupted {
		t.Fatalf("status %s", got.Status)
	}
}
