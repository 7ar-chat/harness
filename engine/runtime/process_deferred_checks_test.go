package runtime_test

import (
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

// toolMessage returns the stored result of a tool call.
func toolMessage(t *testing.T, r *scriptedRig, threadID, callID string) *store.Message {
	t.Helper()
	for _, m := range r.messages(threadID) {
		if m.Role == store.RoleTool && m.ToolCallID == callID {
			return m
		}
	}
	t.Fatalf("no result of %s", callID)
	return nil
}

func TestDeferredChecksArePersistedOnTheOriginToolMessage(t *testing.T) {
	// The checks of files past the per-call cap, run before the next model call, are stored in the origin tool message's metadata.verification: the turn's deliverables carry their checker and verdict, and a resumed run of the turn neither re-checks them nor announces them again (N2).
	// Arrange
	write := "for f in a b c d; do printf fine > /workspace/outputs/$f.docx; done; printf BROKEN > /workspace/outputs/e.docx"
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"`+write+`"}`)},
		llmtest.Turn{Chunks: streamedCall("call_ask", "ask_user", askArgs)},
		llmtest.Turn{Chunks: llmtest.Text("Four are fine; the fifth still fails its checker.")},
	)
	th, first := r.queued("write five reports")
	r.process(first)
	q := r.thread(th.ID).PendingQuestion
	second := r.create(th, store.NewRun{Resume: &store.Resume{InterruptID: q.InterruptID, Content: "The user chose: PDF"}})

	// Act
	r.process(second)

	// Assert
	items, _ := toolMessage(t, r, th.ID, "c1").Metadata["verification"].([]any)
	if got := paths(items); len(got) != 5 {
		t.Fatalf("metadata.verification of c1: %v", items)
	}
	for _, d := range agent.TurnDeliverables(r.messages(th.ID)) {
		if d.Checker != "inspect_docx" || d.OK == nil || *d.OK != (d.Path != "/workspace/outputs/e.docx") {
			t.Fatalf("deliverable %+v", d)
		}
	}
	for _, e := range find(r.events(second.ID), "CUSTOM:work.verification") {
		if v := e["value"].(map[string]any); v["toolCallId"] != nil { // answer summaries carry a messageId instead
			t.Fatalf("the resumed run re-announced a check: %v", v)
		}
	}
	if v := verificationOf(t, lastAnswer(t, r, th.ID)); len(paths(v["verified"])) != 4 || len(paths(v["failed"])) != 1 {
		t.Fatalf("verification %v", v)
	}
}
