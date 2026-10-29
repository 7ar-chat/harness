package runtime_test

import (
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func TestProcessInteractiveRunWritesDeliverablesWithoutAsking(t *testing.T) {
	// In interactive mode writes under /workspace/outputs/ and /workspace/.work/ run at once; only the command waits for approval.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{
			llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/outputs/report.md","content":"# R"}`),
			llmtest.ToolCall(1, "w2", "write_file", `{"path":".work/notes.txt","content":"n"}`),
			llmtest.ToolCall(2, "x1", "execute_command", `{"command":"ls"}`)}},
	)
	th, run := r.interactive("write the report")

	// Act
	got := r.process(run)

	// Assert
	report, okReport := r.file("/workspace/outputs/report.md")
	_, okNotes := r.file("/workspace/.work/notes.txt")
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || !okReport || report != "# R" || !okNotes || pending == nil || pending.ToolName != "execute_command" {
		t.Fatalf("status %s report %v notes %v pending %+v types %v", got.Status, okReport, okNotes, pending, types(r.events(run.ID)))
	}
}

func TestProcessApprovalForTheTaskCoversLaterCallsOfTheSameKind(t *testing.T) {
	// "Allow for this task" (scope run): the approved command runs, and later commands of the run chain run without asking; a risky write still asks.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "x1", "execute_command", `{"command":"ls"}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "x2", "execute_command", `{"command":"pwd"}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", `{"path":"/workspace/setup.sh","content":"x"}`)}},
	)
	th, run := r.interactive("inspect the workspace")
	r.process(run)
	a := r.thread(th.ID).PendingApproval
	resume := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: a.ID, Decision: store.DecisionApprove, Scope: store.ApprovalScopeRun}})

	// Act
	got := r.process(resume)

	// Assert
	sb := r.backend.Created()[0]
	var commands []string
	for _, e := range sb.Execs() {
		commands = append(commands, e.Command)
	}
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || index(commands, "ls") < 0 || index(commands, "pwd") < 0 || pending == nil || pending.ToolName != "write_file" {
		t.Fatalf("status %s commands %v pending %+v", got.Status, commands, pending)
	}
}
