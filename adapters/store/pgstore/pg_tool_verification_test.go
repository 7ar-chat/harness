package pgstore_test

import (
	"context"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestPGAppendToolVerificationGoesToTheToolMessageOfTheCall(t *testing.T) {
	// Checks run after a tool call (the files past the per-call cap) are appended to metadata.verification of that call's tool message, keeping its other metadata (round 3, N2).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "go"))
	_ = s.AppendMessages(ctx,
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c0", Content: "other"},
		&store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleTool, ToolCallID: "c1", Content: "five files",
			Metadata: map[string]any{"verification": []any{map[string]any{"path": "/workspace/outputs/a.docx"}}, "verificationPending": []any{"/workspace/outputs/d.docx"}}})

	// Act
	err := s.AppendToolVerification(ctx, th.ID, "c1", []any{map[string]any{"path": "/workspace/outputs/d.docx", "ok": true}})
	missing := s.AppendToolVerification(ctx, th.ID, "nope", []any{map[string]any{"path": "x"}})

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	items, _ := msgs[2].Metadata["verification"].([]any)
	pending, _ := msgs[2].Metadata["verificationPending"].([]any)
	if err != nil || missing != nil || len(items) != 2 || items[1].(map[string]any)["path"] != "/workspace/outputs/d.docx" || len(pending) != 1 || msgs[1].Metadata != nil {
		t.Fatalf("err %v missing %v metadata %+v other %+v", err, missing, msgs[2].Metadata, msgs[1].Metadata)
	}
}
