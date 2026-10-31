package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

func TestShutdownFinishesInFlightRunsWithinItsBudget(t *testing.T) {
	// On shutdown with Postgres unable to record the final status, the in-flight run still writes its RUN_ERROR and stops before ShutdownGrace + InterruptWait elapse (no 30 s retries past SIGKILL).
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("working"), Block: true})
	st := &finishOutage{Store: r.st}
	st.until.Store(time.Now().Add(time.Hour).UnixNano())
	r.opts.ShutdownGrace, r.opts.InterruptWait = 100*time.Millisecond, 500*time.Millisecond
	rt := runtime.New(r.opts, st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	_, run := r.queued("q")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rt.Run(ctx); close(done) }()
	_ = r.q.Enqueue(context.Background(), run.ID)
	waitFor(t, "run streaming", func() bool { return contains(r.liveTypes(run.ID), "TEXT_MESSAGE_CONTENT") })
	start := time.Now()

	// Act
	cancel()
	<-done
	stopped := time.Since(start)

	// Assert
	live := r.liveTypes(run.ID)
	if stopped > time.Second || len(live) == 0 || live[len(live)-1] != "RUN_ERROR" {
		t.Fatalf("stopped after %s, live log %v", stopped, live)
	}
}
