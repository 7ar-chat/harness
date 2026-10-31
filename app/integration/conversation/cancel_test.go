package conversation_test

import (
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

// hangModel calls the "hang" tool for "start the long job", answers "Tool done" to a tool result and
// "Back to normal" to anything else.
func hangModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if titleCall(c) {
			return titleTurn()
		}
		last := c.Messages[len(c.Messages)-1]
		switch {
		case last.Role == ai.RoleTool:
			return llmtest.Turn{Chunks: llmtest.Text("Tool done")}
		case last.Content == "start the long job":
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "hang", `{}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("Back to normal")}
	})
}

// settledGoroutines polls until the goroutine count is at most want (winding-down goroutines are no leak) and returns the last count.
func settledGoroutines(want int) int {
	end := time.Now().Add(deadline)
	for {
		n := runtime.NumGoroutine()
		if n <= want || time.Now().After(end) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCancelMidToolEndsTheRunCancelledAndTheThreadKeepsWorking(t *testing.T) {
	// Arrange
	baseline := runtime.NumGoroutine()
	hang := newGateTool("hang", "never returned")
	model := hangModel()
	r := newRig(t, rigOpts{model: model, tools: []tools.Provider{hang}})
	id := r.newThread("alice")
	runID := r.send("alice", id, "start the long job", false)
	live := r.open(runID, "alice", "")
	hang.waitEntered(t)
	if th := r.thread("alice", id); th["activeRun"] == nil {
		t.Fatal("control: the thread has no active run while its tool is running")
	}

	// Act
	status, body := r.request("POST", "/api/runs/"+runID+"/cancel", "alice", "")
	evs := live.all()
	toolErr := hang.waitExit(t)
	th := r.waitIdle("alice", id)
	next := r.send("alice", id, "are you there?", false)
	nextEvs := r.replay(next, "alice")
	r.waitIdle("alice", id)

	// Assert: the cancelled run.
	if status != 202 {
		t.Fatalf("cancel = %d %s, want 202", status, body)
	}
	if typ, _ := outcome(t, evs); typ != "cancelled" {
		t.Errorf("stream outcome %q (%v), want cancelled", typ, types(evs))
	}
	if toolErr == nil {
		t.Error("the tool returned normally: its context was not cancelled")
	}
	if run := r.run("alice", runID); run["status"] != store.StatusCancelled {
		t.Errorf("run status %v, want cancelled", run["status"])
	}
	if th["activeRun"] != nil {
		t.Errorf("activeRun = %v after the cancel", th["activeRun"])
	}
	if n := len(agentCalls(model)); n != 2 {
		t.Errorf("%d agent model calls, want 2: the cancelled run must not ask the model again", n)
	}

	// Assert: the follow-up sees the cancelled turn and completes.
	if typ, _ := outcome(t, nextEvs); typ != "success" {
		t.Errorf("follow-up outcome %q", typ)
	}
	if text, _ := deltas(nextEvs); text != "Back to normal" {
		t.Errorf("follow-up answer %q", text)
	}
	calls := agentCalls(model)
	want := []string{"user:start the long job", "assistant:", "tool:" + store.ApprovalCancelledResult, "user:are you there?"}
	if h := history(calls[len(calls)-1]); !slices.Equal(h, want) {
		t.Errorf("follow-up request history:\n got %q\nwant %q", h, want)
	}

	// Assert: nothing is left running once the stack is stopped.
	r.stop()
	if n := settledGoroutines(baseline); n > baseline {
		t.Errorf("goroutines: %d before the run, %d after stopping", baseline, n)
	}
}

func TestOnlyTheOwnerCanCancelARun(t *testing.T) {
	// Arrange
	hang := newGateTool("hang", "x")
	r := newRig(t, rigOpts{model: hangModel(), tools: []tools.Provider{hang}})
	id := r.newThread("alice")
	runID := r.send("alice", id, "start the long job", false)
	hang.waitEntered(t)

	// Act
	foreign, _ := r.request("POST", "/api/runs/"+runID+"/cancel", "mallory", "")
	stillRunning := r.run("alice", runID)["status"]
	owner, _ := r.request("POST", "/api/runs/"+runID+"/cancel", "alice", "")
	evs := r.replay(runID, "alice")

	// Assert
	if foreign != 404 || stillRunning != store.StatusRunning {
		t.Errorf("another user's cancel: %d, run status %v, want 404 and running", foreign, stillRunning)
	}
	if typ, _ := outcome(t, evs); owner != 202 || typ != "cancelled" {
		t.Errorf("control: the owner's cancel = %d, outcome %q, want 202 and cancelled", owner, typ)
	}
}

func TestCancellingAFinishedRunChangesNothing(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{model: hangModel()})
	id := r.newThread("alice")
	runID := r.send("alice", id, "hello", false)
	before := r.replay(runID, "alice")
	r.waitIdle("alice", id)

	// Act
	status, _ := r.request("POST", "/api/runs/"+runID+"/cancel", "alice", "")
	after := r.replay(runID, "alice")

	// Assert
	if status != 202 {
		t.Errorf("cancel of a finished run = %d, want the idempotent 202", status)
	}
	if run := r.run("alice", runID); run["status"] != store.StatusCompleted {
		t.Errorf("status %v after cancelling a completed run, want completed", run["status"])
	}
	if !slices.Equal(before, after) {
		t.Errorf("the event log changed: %v -> %v", types(before), types(after))
	}
}
