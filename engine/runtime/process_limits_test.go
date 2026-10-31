package runtime_test

import (
	"github.com/GhaziBenDahmane/harness/ai/openai"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
)

func TestProcessStopsAtStepLimit(t *testing.T) {
	// On the last step the model is nudged (tool calls forbidden); more tool calls end in RUN_ERROR max_steps.
	// Arrange
	call := func(id string) llmtest.Turn {
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, id, "list_files", `{}`)}}
	}
	r := newRig(t, call("c1"), call("c2"))
	r.agent.MaxSteps = 2
	_, run := r.queued("loop forever")

	// Act
	got := r.process(run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusFailed || fin["type"] != "RUN_ERROR" || fin["code"] != "max_steps" {
		t.Fatalf("status %s last %v", got.Status, fin)
	}
	calls := r.model.Calls()
	lastCall := calls[1]
	if len(calls) != 2 || lastCall.Messages[len(lastCall.Messages)-1].Content != agent.StepLimitNudge {
		t.Fatalf("last call tools=%v messages=%+v", lastCall.Tools, lastCall.Messages)
	}
}

func TestProcessRetriesTransientModelErrors(t *testing.T) {
	// 429 and 5xx errors before the first token are retried.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Err: &openai.APIError{Status: 429, Message: "slow down"}},
		llmtest.Turn{Err: &openai.APIError{Status: 503, Message: "unavailable"}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.model.Calls()) != 3 {
		t.Fatalf("status %s calls %d", got.Status, len(r.model.Calls()))
	}
}

func TestProcessFailsOnModelError(t *testing.T) {
	// A non-transient model error (or exhausted retries) ends the run with RUN_ERROR model_error.
	// Arrange
	r := newRig(t, llmtest.Turn{Err: &openai.APIError{Status: 400, Message: "bad request"}})
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusFailed || fin["code"] != "model_error" || got.Error == "" || len(r.model.Calls()) != 1 {
		t.Fatalf("status %s last %v calls %d", got.Status, fin, len(r.model.Calls()))
	}
}

func TestProcessFailsWhenEveryAttemptBreaksMidAnswer(t *testing.T) {
	// A stream that breaks on every attempt fails the run with model_error after the retries; the partial text is closed and never persisted (A-17).
	// Arrange
	broken := llmtest.Turn{Chunks: llmtest.Text("par"), FailAfter: &openai.APIError{Status: 500}}
	r := newRig(t, broken, broken, broken, broken)
	th, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	if got.Status != store.StatusFailed || last(evs)["code"] != "model_error" || len(find(evs, "TEXT_MESSAGE_END")) != 4 || len(r.model.Calls()) != 4 {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	if msgs := r.messages(th.ID); len(msgs) != 1 {
		t.Fatalf("partial answer persisted: %+v", msgs)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
