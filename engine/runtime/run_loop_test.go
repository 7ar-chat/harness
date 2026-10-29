package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func TestRunClaimsQueuedRunsFromTheStream(t *testing.T) {
	// The consumer loop claims runs enqueued on work:runs and completes them.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.runtime().Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Act
	if err := r.q.Enqueue(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}

	// Assert
	waitFor(t, "run completed", func() bool { return r.reload(run).Status == store.StatusCompleted })
	if got := r.reload(run); got.RuntimeID != "rt-test" {
		t.Fatalf("runtime id %q", got.RuntimeID)
	}
}

func TestRunInterruptsInFlightRunsOnShutdown(t *testing.T) {
	// On shutdown, runs still working after the grace period finish as interrupted with RUN_ERROR.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("working"), Block: true})
	_, run := r.queued("q")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = r.runtime().Run(ctx); close(done) }()
	_ = r.q.Enqueue(context.Background(), run.ID)
	waitFor(t, "run streaming", func() bool { return contains(r.liveTypes(run.ID), "TEXT_MESSAGE_CONTENT") })

	// Act
	cancel()

	// Assert
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not stop")
	}
	evs := r.events(run.ID)
	if got := r.reload(run); got.Status != store.StatusInterrupted || last(evs)["type"] != "RUN_ERROR" || last(evs)["code"] != "interrupted" {
		t.Fatalf("status %s last %v", got.Status, last(evs))
	}
}
