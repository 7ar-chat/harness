package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
)

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestProcessStopsWhenUserCancels(t *testing.T) {
	// A cancel published on the control channel stops the model stream and finishes the run as cancelled.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("partial"), Block: true})
	th, run := r.queued("long task")
	done := make(chan *store.Run)
	go func() { done <- r.process(run) }()
	waitFor(t, "first text delta", func() bool { return contains(r.liveTypes(run.ID), "TEXT_MESSAGE_CONTENT") })

	// Act
	_, _ = r.st.RequestCancel(context.Background(), run.ID)
	_ = r.q.PublishCancel(context.Background(), run.ID)

	// Assert
	var got *store.Run
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop")
	}
	evs := r.events(run.ID)
	fin := last(evs)
	if got.Status != store.StatusCancelled || fin["type"] != "RUN_FINISHED" || fin["outcome"].(map[string]any)["type"] != "cancelled" {
		t.Fatalf("status %s last %v", got.Status, fin)
	}
	if !contains(types(evs), "TEXT_MESSAGE_END") {
		t.Fatalf("open text message not closed: %v", types(evs))
	}
	msgs := r.messages(th.ID)
	if len(msgs) != 2 || msgs[1].Content != "partial" {
		t.Fatalf("partial answer not kept: %+v", msgs)
	}
}

func TestProcessCancelInterruptsRunningCommandViaHeartbeat(t *testing.T) {
	// cancel_requested is also picked up by the heartbeat poll and interrupts an in-flight sandbox command.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "execute_command", `{"command":"sleep 600"}`)}})
	r.opts.Heartbeat = 20 * time.Millisecond
	started := make(chan struct{})
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(func(ctx context.Context, _ sandbox.ExecRequest) (sandbox.ExecResult, error) {
			close(started)
			<-ctx.Done()
			return sandbox.ExecResult{}, ctx.Err()
		})
	}
	_, run := r.queued("sleep")
	done := make(chan *store.Run)
	go func() { done <- r.process(run) }()
	<-started

	// Act
	_, _ = r.st.RequestCancel(context.Background(), run.ID)

	// Assert
	var got *store.Run
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("command was not interrupted")
	}
	evs := r.events(run.ID)
	res := find(evs, "TOOL_CALL_RESULT")
	if got.Status != store.StatusCancelled || len(res) != 1 || res[0]["metadata"].(map[string]any)["isError"] != true {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	if last(evs)["outcome"].(map[string]any)["type"] != "cancelled" {
		t.Fatalf("last %v", last(evs))
	}
}
