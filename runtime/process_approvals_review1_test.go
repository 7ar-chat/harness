package runtime_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestProcessInteractiveCaseVariantPathKeyStillAsks(t *testing.T) {
	// A write whose arguments repeat the path under another case ("path" in outputs, "Path" in src) waits for approval and writes nothing (review round 1 P2 #1).
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file",
			`{"path":"/workspace/outputs/ok.txt","Path":"/workspace/src/main.py","content":"pwned"}`)}},
	)
	th, run := r.interactive("write it")

	// Act
	got := r.process(run)

	// Assert
	_, wroteSrc := r.file("/workspace/src/main.py")
	_, wroteOK := r.file("/workspace/outputs/ok.txt")
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || wroteSrc || wroteOK || pending == nil || pending.ToolName != "write_file" {
		t.Fatalf("status %s src %v ok %v pending %+v types %v", got.Status, wroteSrc, wroteOK, pending, types(r.events(run.ID)))
	}
}

func TestProcessInteractiveWriteThroughASymlinkOutOfTheWorkAreasAsks(t *testing.T) {
	// /workspace/outputs/x is a symlink to /workspace/src: a write to outputs/x/main.py waits for approval and nothing is written (paths resolve symlinks in the sandbox).
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/outputs/x/main.py","content":"pwned"}`)}},
	)
	th, run := r.interactive("write it")
	if _, err := r.mgr.Acquire(context.Background(), th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	r.backend.Created()[0].Symlink("/workspace/src", "/workspace/outputs/x")

	// Act
	got := r.process(run)

	// Assert
	_, wroteSrc := r.file("/workspace/src/main.py")
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || wroteSrc || pending == nil || pending.ToolName != "write_file" {
		t.Fatalf("status %s src %v pending %+v types %v", got.Status, wroteSrc, pending, types(r.events(run.ID)))
	}
}
