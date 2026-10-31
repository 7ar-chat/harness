package runtime_test

import (
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestProcessPassesReasoningEffortToReasoningModels(t *testing.T) {
	// The run's reasoningEffort reaches every call of a model in LLM_REASONING_MODELS and no other model.
	// Arrange
	r := newRig(t)
	th, _ := r.st.CreateThread(t.Context(), "alice", "t")
	reasoning := r.create(th, store.NewRun{Model: "gpt-reasoning", ReasoningEffort: "high", Message: store.Message{Role: store.RoleUser, Content: "think hard"}})

	// Act
	r.process(reasoning)

	// Assert
	if calls := r.model.Calls(); len(calls) != 1 || calls[0].ReasoningEffort != "high" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestProcessIgnoresReasoningEffortForOtherModels(t *testing.T) {
	// A model outside LLM_REASONING_MODELS gets no reasoning_effort.
	// Arrange
	r := newRig(t)
	th, _ := r.st.CreateThread(t.Context(), "alice", "t")
	run := r.create(th, store.NewRun{ReasoningEffort: "high", Message: store.Message{Role: store.RoleUser, Content: "hi"}})

	// Act
	r.process(run)

	// Assert
	if calls := r.model.Calls(); len(calls) != 1 || calls[0].ReasoningEffort != "" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestProcessSendsTheNearestEffortTheModelAccepts(t *testing.T) {
	// A stored effort the model does not accept (an older run's "minimal" on GPT-5.6) is sent as the nearest accepted one, never as is (A-29).
	// Arrange
	r := newRig(t)
	r.agent.Harness.ReasoningEffort = func(model, effort string) string {
		if model == "gpt-reasoning" && effort == "minimal" {
			return "low"
		}
		return effort
	}
	th, _ := r.st.CreateThread(t.Context(), "alice", "t")
	run := r.create(th, store.NewRun{Model: "gpt-reasoning", ReasoningEffort: "minimal", Message: store.Message{Role: store.RoleUser, Content: "hi"}})

	// Act
	r.process(run)

	// Assert
	if calls := r.model.Calls(); len(calls) != 1 || calls[0].ReasoningEffort != "low" {
		t.Fatalf("calls %+v", calls)
	}
}
