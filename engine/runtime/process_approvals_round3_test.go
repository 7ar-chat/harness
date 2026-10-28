package runtime_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestProcessStopEndsAnInteractiveWriteWhileItsSandboxStarts(t *testing.T) {
	// In interactive mode the work-area check of a write never starts a sandbox outside the run: Stop while the sandbox is still starting ends the run at once (N5).
	// Arrange
	r := newApprovalRig(t, llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/outputs/report.md","content":"# Report"}`)}})
	r.backend.CreateDelay = 20 * time.Second // a cold sandbox start
	_, run := r.interactive("write it")
	done := make(chan *store.Run, 1)
	go func() { done <- r.process(run) }()
	waitFor(t, "the write call", func() bool { return contains(r.liveTypes(run.ID), "TOOL_CALL_END") })
	time.Sleep(50 * time.Millisecond)

	// Act
	_, _ = r.st.RequestCancel(context.Background(), run.ID)
	_ = r.q.PublishCancel(context.Background(), run.ID)

	// Assert
	select {
	case got := <-done:
		if got.Status != store.StatusCancelled {
			t.Fatalf("status %s", got.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not end the run while its sandbox was starting")
	}
}

func TestProcessResolvesAWorkAreaPathAgainAfterACommandChangedIt(t *testing.T) {
	// In interactive mode a write is gated on the real path of its target as it is now: after an approved command turned a work-area directory into a symbolic link to /workspace/src, writing the same path again asks for approval (F6: no stale cached real path).
	// Arrange
	write := func(id string) llmtest.Turn {
		return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, id, "write_file", `{"path":"/workspace/outputs/x/a.md","content":"`+id+`"}`)}}
	}
	command := func(id, cmd string) llmtest.Turn {
		return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, id, "execute_command", `{"command":"`+cmd+`"}`)}}
	}
	r := newApprovalRig(t, command("x1", "ls"), write("w1"), command("x2", "rm -rf outputs/x && ln -s /workspace/src outputs/x"), write("w2"),
		llmtest.Turn{Chunks: llmtest.Text("done")})
	prev := r.backend.OnCreate
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		if prev != nil {
			prev(sb)
		}
		sb.SetJobs(func(_ context.Context, command string, _ func(stream, text string)) int {
			if strings.Contains(command, "ln -s") {
				sb.Symlink("/workspace/src", "/workspace/outputs/x")
			}
			return 0
		})
	}
	th, run := r.interactive("write it twice")
	r.process(run)
	a := r.thread(th.ID).PendingApproval
	if a == nil {
		t.Fatal("the first command did not ask")
	}
	resume := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: a.ID, Decision: store.DecisionApprove, Scope: store.ApprovalScopeRun}})

	// Act
	got := r.process(resume)

	// Assert
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || pending == nil || pending.ToolName != "write_file" || pending.ToolCallID != "w2" {
		t.Fatalf("status %s pending %+v", got.Status, pending)
	}
}

func TestProcessWritesAndEditsNeverGoThroughASymlink(t *testing.T) {
	// write_file and edit_file resolve their target in the same sandbox exec that writes it and refuse one that is, or is inside, a symbolic link (a work-area path the gate let through), so a file outside it is never changed (N6).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/outputs/x/main.py","content":"pwned"}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "r1", "read_file", `{"path":"/workspace/outputs/notes.md"}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "e1", "edit_file", `{"path":"/workspace/outputs/notes.md","oldText":"original","newText":"pwned"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("done")})
	th, run := r.queued("update the notes")
	if _, err := r.mgr.Acquire(context.Background(), th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	sb := r.backend.Created()[0]
	sb.Put("/workspace/src/main.py", "original")
	sb.Put("/etc/notes.md", "original")
	sb.Symlink("/workspace/src", "/workspace/outputs/x")
	sb.Symlink("/etc/notes.md", "/workspace/outputs/notes.md")

	// Act
	got := r.process(run)

	// Assert
	main, _ := sb.File("/workspace/src/main.py")
	notes, _ := sb.File("/etc/notes.md")
	results := map[string]event{}
	for _, e := range find(r.events(run.ID), "TOOL_CALL_RESULT") {
		results[e["toolCallId"].(string)] = e
	}
	for _, id := range []string{"w1", "e1"} {
		md, _ := results[id]["metadata"].(map[string]any)
		if md["isError"] != true || !strings.Contains(results[id]["content"].(string), "symbolic link") {
			t.Fatalf("%s: %v", id, results[id])
		}
	}
	if got.Status != store.StatusCompleted || main != "original" || notes != "original" {
		t.Fatalf("status %s main.py %q notes %q", got.Status, main, notes)
	}
}

func TestProcessInteractiveWriteInTheWorkAreaThroughTheOpenSandboxLineTransportDoesNotAsk(t *testing.T) {
	// In interactive mode, with the sandbox live and its output delivered like OpenSandbox's (a newline appended to every message), a write to /workspace/outputs/ runs without approval (ADISTU-6764: the work-area check failed closed and asked).
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/outputs/report.md","content":"# Report"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("done")},
	)
	th, run := r.interactive("write it")
	if _, err := r.mgr.Acquire(context.Background(), th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	sb := r.backend.Created()[0]
	sb.SetLineTransport(true)

	// Act
	got := r.process(run)

	// Assert
	content, wrote := r.file("/workspace/outputs/report.md")
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || pending != nil || !wrote || content != "# Report" || sb.RealPathCalls() == 0 {
		t.Fatalf("status %s pending %+v wrote %v %q realpaths %d", got.Status, pending, wrote, content, sb.RealPathCalls())
	}
}
