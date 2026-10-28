package permissions

import (
	"context"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestAskToolsAskOncePerRunUnlessARuleOrModeSaysOtherwise(t *testing.T) {
	// Arrange
	run := &store.Run{ID: "r"}
	approved := &store.Run{ID: "r", ApprovedTools: []string{approvals.Kind("fs__read")}}
	h, _ := newHook(ModeAsk)
	h.Policy.AskTools = []string{"fs__*"}
	yolo, _ := newHook(ModeYolo)
	yolo.Policy.AskTools = []string{"fs__*"}
	allowed, _ := newHook(ModeAsk, Rule{Action: Allow, Kind: kindTool, Tool: "fs__read", Pattern: "*"})
	allowed.Policy.AskTools = []string{"fs__*"}
	ctx := context.Background()

	// Act / Assert
	if !h.NeedsApproval(ctx, run, "fs__read", "{}") {
		t.Error("the first use of a stdio server's tool must ask")
	}
	if h.NeedsApproval(ctx, approved, "fs__read", "{}") {
		t.Error("an approved tool must not ask again in the same run")
	}
	if yolo.NeedsApproval(ctx, run, "fs__read", "{}") {
		t.Error("yolo mode never asks")
	}
	if allowed.NeedsApproval(ctx, run, "fs__read", "{}") {
		t.Error("an allow rule must stop the question")
	}
}
