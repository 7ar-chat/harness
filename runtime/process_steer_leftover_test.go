package runtime_test

import (
	"context"
	"testing"
	"time"

	goopenai "github.com/meguminnnnnnnnn/go-openai"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/queue"
)

// steerEvents returns the CUSTOM work.steer values and checks they come before the terminal event.
func steerEvents(t *testing.T, evs []event) []map[string]any {
	t.Helper()
	var out []map[string]any
	for i, e := range evs {
		if e["type"] == "CUSTOM" && e["name"] == "work.steer" {
			if i == len(evs)-1 {
				t.Fatalf("steer announced after the terminal event: %v", types(evs))
			}
			out = append(out, e["value"].(map[string]any))
		}
	}
	return out
}

func TestFailedRunAnnouncesLeftoverSteersBeforeRunError(t *testing.T) {
	// Steers kept in the thread because the run failed are announced as delivered=false before RUN_ERROR.
	// Arrange
	var runID string
	r := newRig(t)
	r.model = llmtest.New(llmtest.Turn{Err: &goopenai.APIError{HTTPStatusCode: 400}, OnCall: func() {
		_ = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "s-1", Text: "also B"})
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	_, run := r.queued("q")
	runID = run.ID

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	steers := steerEvents(t, evs)
	if len(steers) != 1 || steers[0]["id"] != "s-1" || steers[0]["delivered"] != false || last(evs)["type"] != "RUN_ERROR" {
		t.Fatalf("steers %v types %v", steers, types(evs))
	}
}

func TestDeliveredSteersAreMarkedDelivered(t *testing.T) {
	// Steers given to the model are announced with delivered=true.
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	_ = r.q.PushSteer(context.Background(), run.ID, queue.Steer{ID: "s-2", Text: "in French"})

	// Act
	r.process(run)

	// Assert
	steers := steerEvents(t, r.events(run.ID))
	if len(steers) != 1 || steers[0]["delivered"] != true {
		t.Fatalf("steers %v", steers)
	}
}

func TestSweptRunAnnouncesLeftoverSteersBeforeRunError(t *testing.T) {
	// The sweeper stores and announces pending steers of an interrupted run before its RUN_ERROR.
	// Arrange
	r := newRig(t)
	th, run := r.queued("q")
	ctx := context.Background()
	_, _ = r.st.ClaimRun(ctx, run.ID, "dead")
	_ = r.q.PushSteer(ctx, run.ID, queue.Steer{ID: "s-3", Text: "hello?"})
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))

	// Act
	r.runtime().Sweep(ctx)

	// Assert
	evs := r.events(run.ID)
	steers := steerEvents(t, evs)
	kept := false
	for _, m := range r.messages(th.ID) {
		kept = kept || (m.ID == "s-3" && m.IsSteer())
	}
	if len(steers) != 1 || steers[0]["delivered"] != false || last(evs)["type"] != "RUN_ERROR" || !kept {
		t.Fatalf("steers %v types %v kept %v", steers, types(evs), kept)
	}
}

func TestSteerMessagesRecordWhetherTheyWereDelivered(t *testing.T) {
	// Persisted steers carry metadata.delivered: true when the model saw them, false when kept for the next run.
	// Arrange
	var runID string
	r := newRig(t)
	r.model = llmtest.New(llmtest.Turn{Err: &goopenai.APIError{HTTPStatusCode: 400}, OnCall: func() {
		_ = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "late", Text: "kept"})
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	th, run := r.queued("q")
	runID = run.ID
	_ = r.q.PushSteer(context.Background(), run.ID, queue.Steer{ID: "early", Text: "seen"})

	// Act
	r.process(run)

	// Assert
	got := map[string]any{}
	for _, m := range r.messages(th.ID) {
		if m.IsSteer() {
			got[m.ID] = m.Metadata["delivered"]
		}
	}
	if got["early"] != true || got["late"] != false {
		t.Fatalf("delivered flags %v", got)
	}
}
