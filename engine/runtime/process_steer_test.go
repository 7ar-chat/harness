package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/queue"
)

func TestProcessInjectsSteerBeforeNextModelCall(t *testing.T) {
	// A steer sent while a tool runs is given to the model before its next call, persisted and announced.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "execute_command", `{"command":"ls"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("Bien sûr")},
	)
	var runID string
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(ctx context.Context, _ sandbox.ExecRequest) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{}, r.q.PushSteer(ctx, runID, queue.Steer{ID: "s-1", Text: "answer in French"})
		})
	}
	th, run := r.queued("list files")
	runID = run.ID

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	steer := find(evs, "CUSTOM:work.steer")
	if got.Status != store.StatusCompleted || len(steer) != 1 {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	if v := steer[0]["value"].(map[string]any); v["id"] != "s-1" || v["text"] != "answer in French" {
		t.Fatalf("steer event %v", steer[0])
	}
	second := r.model.Calls()[1].Messages
	lastMsg := second[len(second)-1]
	if lastMsg.Role != ai.RoleUser || lastMsg.Content != agent.SteerPrefix+"answer in French" {
		t.Fatalf("model input %+v", lastMsg)
	}
	var persisted *store.Message
	for _, m := range r.messages(th.ID) {
		if m.ID == "s-1" {
			persisted = m
		}
	}
	if persisted == nil || !persisted.IsSteer() || persisted.Content != "answer in French" || persisted.RunID != run.ID {
		t.Fatalf("steer not persisted: %+v", persisted)
	}
}

func TestProcessSteerDuringFinalAnswerGetsAnotherStep(t *testing.T) {
	// A steer arriving while the model writes its final answer triggers one more step instead of being lost.
	// Arrange
	var runID string
	r := newRig(t)
	r.model = llmtest.New(
		llmtest.Turn{Chunks: llmtest.Text("first answer"), OnCall: func() {
			_ = r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "s-2", Text: "and a summary"})
		}},
		llmtest.Turn{Chunks: llmtest.Text("summary")},
	)
	r.agent.Models = llmtest.Factory{M: r.model}
	_, run := r.queued("q")
	runID = run.ID

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	if got.Status != store.StatusCompleted || len(find(evs, "STEP_STARTED")) != 2 || len(r.model.Calls()) != 2 {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	if !strings.Contains(r.model.Calls()[1].Messages[len(r.model.Calls()[1].Messages)-1].Content, "and a summary") {
		t.Fatal("steer not given to the model")
	}
}
