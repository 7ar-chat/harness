package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

func TestASilentStreamThatSendsKeepAlivesIsNotCut(t *testing.T) {
	// A model stream that forwards no chunk for longer than both budgets but keeps sending events (silent reasoning, keep-alives) is not a stall: no retry, the answer arrives.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("forty-two"), KeepAlive: 250 * time.Millisecond})
	r.agent.ModelTimeout = 80 * time.Millisecond
	r.agent.StreamIdleTimeout = 80 * time.Millisecond
	recordSleeps(r)
	th, run := r.queued("think hard")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusCompleted || len(find(evs, "CUSTOM:work.retry")) != 0 || lastAnswer(t, &scriptedRig{rig: r}, th.ID).Content != "forty-two" {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
}

func TestHighReasoningEffortGetsALongerFirstChunkBudget(t *testing.T) {
	// At high (and xhigh) reasoning effort the first chunk may take longer than LLM_TIMEOUT_SECONDS (twice, three times as long) before the call counts as stalled.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("done thinking"), ChunkDelay: 150 * time.Millisecond})
	r.agent.ModelTimeout = 100 * time.Millisecond
	recordSleeps(r)
	th := r.thread(r.mustThread())
	run := r.create(th, store.NewRun{Model: "gpt-reasoning", ReasoningEffort: "high", Message: store.Message{Role: store.RoleUser, Content: "prove it"}})

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusCompleted || len(find(evs, "CUSTOM:work.retry")) != 0 {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
}

func TestAStreamThatOnlySendsKeepAliveCommentsTimesOut(t *testing.T) {
	// SSE comment lines (": keep-alive") are no provider activity: a gateway that pings while the upstream hangs does not keep the call alive, it ends with model_timeout (N3).
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("too late"), Pings: 400 * time.Millisecond})
	r.agent.ModelTimeout = 80 * time.Millisecond
	r.agent.StreamIdleTimeout = 80 * time.Millisecond
	r.agent.StepRetries = -1
	recordSleeps(r)
	_, run := r.queued("think hard")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusFailed || last(evs)["code"] != "model_timeout" {
		t.Fatalf("status %s last %v", got.Status, last(evs))
	}
}

func TestAModelCallNeverOutlivesItsCeiling(t *testing.T) {
	// A stream that keeps sending provider progress events but no answer is cut at LLM_CALL_MAX_SECONDS whatever its activity, and ends with model_timeout (N3).
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("much too late"), KeepAlive: 3 * time.Second})
	r.agent.ModelTimeout = 80 * time.Millisecond
	r.agent.StreamIdleTimeout = 80 * time.Millisecond
	r.agent.CallCeiling = 300 * time.Millisecond
	r.agent.StepRetries = -1
	recordSleeps(r)
	_, run := r.queued("think forever")

	// Act
	started := time.Now()
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusFailed || last(evs)["code"] != "model_timeout" || time.Since(started) > 2*time.Second {
		t.Fatalf("status %s last %v after %v", got.Status, last(evs), time.Since(started))
	}
}

// mustThread creates a thread of alice and returns its id.
func (r *rig) mustThread() string {
	r.t.Helper()
	th, err := r.st.CreateThread(context.Background(), "alice", "t")
	if err != nil {
		r.t.Fatal(err)
	}
	return th.ID
}
