package runtime_test

import (
	"context"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func switchModeTool(t *testing.T, r *rig, th *store.Thread, mode string) tool.BaseTool {
	t.Helper()
	return scriptedTool(t, "switch_mode", func(ctx context.Context) (string, error) {
		if err := r.st.SetThreadInteractionMode(ctx, "alice", th.ID, mode); err != nil {
			return "", err
		}
		return "switched", nil
	})
}

func TestProcessSwitchToInteractiveMidRunGatesTheNextRiskyCall(t *testing.T) {
	// A run started automatic asks for approval of the risky calls made after the user switched the thread to interactive.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "s1", "switch_mode", `{}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
	)
	th, run := r.queued("write the report")
	addTools(t, r, switchModeTool(t, r, th, store.InteractionInteractive))

	// Act
	got := r.process(run)

	// Assert
	pending := r.thread(th.ID).PendingApproval
	if got.Status != store.StatusCompleted || pending == nil || pending.ToolCallID != "w1" || pending.Status != store.ApprovalPending {
		t.Fatalf("status %s pending %+v types %v", got.Status, pending, types(r.events(run.ID)))
	}
	if _, ok := r.file("/workspace/report.md"); ok {
		t.Fatal("the risky call ran after the switch to interactive")
	}
}

func TestProcessSwitchToAutomaticMidRunStopsAskingForApproval(t *testing.T) {
	// A run started interactive runs the risky calls made after the user switched the thread to automatic without asking.
	// Arrange
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "s1", "switch_mode", `{}`)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Written")},
	)
	th, run := r.interactive("write the report")
	addTools(t, r, switchModeTool(t, r, th, store.InteractionAutomatic))

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.st.ApprovalsOf(th.ID)) != 0 || last(r.events(run.ID))["outcome"].(map[string]any)["type"] != "success" {
		t.Fatalf("status %s approvals %d types %v", got.Status, len(r.st.ApprovalsOf(th.ID)), types(r.events(run.ID)))
	}
	if content, ok := r.file("/workspace/report.md"); !ok || content != "# Report" {
		t.Fatalf("file %q %v", content, ok)
	}
}

func TestProcessSwitchWhileAnApprovalIsPendingKeepsTheCardAndResumesInTheNewMode(t *testing.T) {
	// Switching to automatic while a card waits does not decide it; once approved, the resume run runs its later risky calls without asking.
	// Arrange
	notesArgs := `{"path":"/workspace/notes.md","content":"notes"}`
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w1", "write_file", writeArgs)}},
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "w2", "write_file", notesArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Both written")},
	)
	th, first := r.interactive("write the report and the notes")
	r.process(first)
	if err := r.st.SetThreadInteractionMode(context.Background(), "alice", th.ID, store.InteractionAutomatic); err != nil {
		t.Fatal(err)
	}
	if pending := r.thread(th.ID).PendingApproval; pending == nil || pending.Status != store.ApprovalPending {
		t.Fatalf("the switch decided the card: %+v", pending)
	}
	second := r.decide(th, store.DecisionApprove, store.ApprovalResponse{})

	// Act
	got := r.process(second)

	// Assert
	if got.Status != store.StatusCompleted || got.InteractionMode != store.InteractionAutomatic || len(r.st.ApprovalsOf(th.ID)) != 1 {
		t.Fatalf("status %s mode %q approvals %d types %v", got.Status, got.InteractionMode, len(r.st.ApprovalsOf(th.ID)), types(r.events(second.ID)))
	}
	report, okReport := r.file("/workspace/report.md")
	notes, okNotes := r.file("/workspace/notes.md")
	if !okReport || report != "# Report" || !okNotes || notes != "notes" {
		t.Fatalf("report %q %v notes %q %v", report, okReport, notes, okNotes)
	}
}

func TestProcessSwitchToAutomaticMidStepKeepsLaterRiskyCallsBehindThePausedOne(t *testing.T) {
	// [write_file A, switch to automatic, write_file B]: A pauses, the switch runs, B waits behind A instead of running first; once A is approved the resume run takes A then B, in order, without asking again.
	// Arrange
	notesArgs := `{"path":"/workspace/notes.md","content":"notes"}`
	r := newApprovalRig(t,
		llmtest.Turn{Chunks: []*schema.Message{
			llmtest.ToolCall(0, "w1", "write_file", writeArgs),
			llmtest.ToolCall(1, "s1", "switch_mode", `{}`),
			llmtest.ToolCall(2, "w2", "write_file", notesArgs)}},
		llmtest.Turn{Chunks: llmtest.Text("Both written")},
	)
	th, first := r.interactive("write the report and the notes")
	addTools(t, r, switchModeTool(t, r, th, store.InteractionAutomatic))

	// Act
	r.process(first)
	firstResults := find(r.events(first.ID), "TOOL_CALL_RESULT")
	_, notesBeforeDecision := r.file("/workspace/notes.md")
	pending := r.thread(th.ID).PendingApproval
	second := r.process(r.decide(th, store.DecisionApprove, store.ApprovalResponse{}))

	// Assert
	if len(firstResults) != 1 || firstResults[0]["toolCallId"] != "s1" || notesBeforeDecision {
		t.Fatalf("first run: results %v, notes written before the paused call: %v", firstResults, notesBeforeDecision)
	}
	if pending == nil || pending.ToolCallID != "w1" || pending.Status != store.ApprovalPending {
		t.Fatalf("pending %+v", pending)
	}
	var order []any
	for _, e := range find(r.events(second.ID), "TOOL_CALL_RESULT") {
		order = append(order, e["toolCallId"])
	}
	if second.Status != store.StatusCompleted || len(order) != 2 || order[0] != "w1" || order[1] != "w2" || len(r.st.ApprovalsOf(th.ID)) != 1 {
		t.Fatalf("resume: status %s results %v approvals %d types %v", second.Status, order, len(r.st.ApprovalsOf(th.ID)), types(r.events(second.ID)))
	}
	if notes, ok := r.file("/workspace/notes.md"); !ok || notes != "notes" {
		t.Fatalf("notes %q %v", notes, ok)
	}
}
