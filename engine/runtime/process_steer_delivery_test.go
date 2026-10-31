package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai/openai"

	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/queue"
)

func TestSteerAfterFinalDrainIsRefused(t *testing.T) {
	// Once the run stops accepting steers (last step / finishing), pushing one fails so the API can answer 409.
	// Arrange
	var runID string
	var pushErr error
	r := newRig(t)
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("final"), OnCall: func() {
		pushErr = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "late", Text: "too late"})
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	r.agent.MaxSteps = 1
	_, run := r.queued("q")
	runID = run.ID

	// Act
	got := r.process(run)

	// Assert
	after := r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "later", Text: "after"})
	if got.Status != store.StatusCompleted || !errors.Is(pushErr, queue.ErrSteersClosed) || !errors.Is(after, queue.ErrSteersClosed) {
		t.Fatalf("status %s during %v after %v", got.Status, pushErr, after)
	}
}

func TestAcceptedSteerOfFailedRunIsKeptInThread(t *testing.T) {
	// A steer accepted before the run fails is persisted in the thread, so the next run sees it.
	// Arrange
	var runID string
	r := newRig(t)
	r.model = llmtest.New(llmtest.Turn{Err: &openai.APIError{Status: 400}, OnCall: func() {
		_ = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "s-9", Text: "also check B"})
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	th, run := r.queued("q")
	runID = run.ID

	// Act
	got := r.process(run)

	// Assert
	var steer *store.Message
	for _, m := range r.messages(th.ID) {
		if m.ID == "s-9" {
			steer = m
		}
	}
	if got.Status != store.StatusFailed || steer == nil || !steer.IsSteer() || steer.Content != "also check B" {
		t.Fatalf("status %s steer %+v", got.Status, steer)
	}
}
