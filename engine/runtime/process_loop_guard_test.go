package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
)

func probeCall(id, args string) llmtest.Turn {
	return llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, id, "probe", args)}}
}

func TestProcessRefusesThirdIdenticalToolCallInARow(t *testing.T) {
	// The third consecutive call of a tool with the same arguments (key order and spacing aside) is not executed; the model gets an error redirecting it.
	// Arrange
	var runs int
	probe := &fakeTool{name: "probe", run: func(context.Context, string) (string, error) { runs++; return "same failure", nil }}
	r := newRig(t,
		probeCall("c1", `{"path":"a.docx","page":1}`), probeCall("c2", `{ "page": 1, "path": "a.docx" }`), probeCall("c3", `{"page":1,"path":"a.docx"}`),
		llmtest.Turn{Chunks: llmtest.Text("I am blocked.")},
	)
	r.withTools(probe)
	th, run := r.queued("check the page")

	// Act
	got := r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if got.Status != store.StatusCompleted || runs != 2 || len(results) != 3 {
		t.Fatalf("status %s runs %d results %d", got.Status, runs, len(results))
	}
	content := results[2]["content"].(string)
	meta, _ := results[2]["metadata"].(map[string]any)
	if !strings.HasPrefix(content, "Not executed: you called probe with identical arguments 3 times in a row.") ||
		!strings.Contains(content, "change your approach") || meta["isError"] != true {
		t.Fatalf("third result %v", results[2])
	}
	var row *store.Message
	for _, m := range r.messages(th.ID) {
		if m.ToolCallID == "c3" {
			row = m
		}
	}
	if row == nil || row.Error == "" || row.Content != content {
		t.Fatalf("persisted %+v", row)
	}
}

func TestProcessRunsRepeatedToolCallsWithChangedArguments(t *testing.T) {
	// A call that differs from one of the previous two (other arguments in between) is executed normally.
	// Arrange
	var runs int
	probe := &fakeTool{name: "probe", run: func(context.Context, string) (string, error) { runs++; return "ok", nil }}
	r := newRig(t, probeCall("c1", `{"n":1}`), probeCall("c2", `{"n":2}`), probeCall("c3", `{"n":1}`), probeCall("c4", `{"n":1}`),
		llmtest.Turn{Chunks: llmtest.Text("Probed.")})
	r.agent.MaxSteps = 6
	r.withTools(probe)
	_, run := r.queued("probe")

	// Act
	got := r.process(run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusCompleted || runs != 4 || fin["type"] != "RUN_FINISHED" {
		t.Fatalf("status %s runs %d last %v", got.Status, runs, fin)
	}
}

func TestProcessForgetsRepeatedCallsAfterASteer(t *testing.T) {
	// A steer delivered between identical calls resets the loop guard: the call after it is executed.
	// Arrange
	var runs int
	var runID string
	probe := &fakeTool{name: "probe", run: func(context.Context, string) (string, error) { runs++; return "ok", nil }}
	r := newRig(t)
	r.model = llmtest.New(
		probeCall("c1", `{}`),
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c2", "probe", `{}`)}, OnCall: func() {
			_ = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "s-1", Text: "try once more"})
		}},
		probeCall("c3", `{}`),
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
	)
	r.agent.Models = llmtest.Factory{M: r.model}
	r.withTools(probe)
	_, run := r.queued("probe")
	runID = run.ID

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || runs != 3 {
		t.Fatalf("status %s runs %d", got.Status, runs)
	}
}

func TestProcessForgetsRepeatedCallsAfterACorrectiveNote(t *testing.T) {
	// An answer-check note (like a nudge or a stop reminder) resets the loop guard: an identical call after it is executed.
	// Arrange
	var runs int
	probe := &fakeTool{name: "probe", run: func(context.Context, string) (string, error) { runs++; return "ok", nil }}
	r := newRig(t, probeCall("c1", `{}`), probeCall("c2", `{}`), llmtest.Turn{}, probeCall("c3", `{}`), llmtest.Turn{Chunks: llmtest.Text("Done.")})
	r.agent.MaxSteps = 8
	r.withTools(probe)
	_, run := r.queued("probe")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || runs != 3 {
		t.Fatalf("status %s runs %d", got.Status, runs)
	}
}
