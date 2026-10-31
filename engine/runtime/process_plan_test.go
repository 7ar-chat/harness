package runtime_test

import (
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/ai"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
)

func TestProcessUpdatePlanPersistsAndSnapshotsThePlan(t *testing.T) {
	// update_plan stores the items on the thread and emits STATE_SNAPSHOT {plan} right after its TOOL_CALL_RESULT.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "p1", "update_plan",
			`{"items":[{"id":"1","title":"Load the data","status":"completed"},{"id":"2","title":"Plot it","status":"in_progress"},{"id":"3","title":"Write the report","status":"pending"}]}`)}},
		llmtest.Turn{Chunks: llmtest.Text("Working on it")},
	)
	th, run := r.queued("analyse sales")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	tt := types(evs)
	res, snap := index(tt, "TOOL_CALL_RESULT"), index(tt, "STATE_SNAPSHOT")
	if res < 0 || snap != res+1 {
		t.Fatalf("types %v", tt)
	}
	content := evs[res]["content"].(string)
	if !strings.Contains(content, "1/3 completed") || !strings.Contains(content, "Plot it") {
		t.Fatalf("result %q", content)
	}
	items := evs[snap]["snapshot"].(map[string]any)["plan"].([]any)
	if len(items) != 3 || items[1].(map[string]any)["status"] != "in_progress" {
		t.Fatalf("snapshot %v", evs[snap])
	}
	plan := r.thread(th.ID).Plan
	if len(plan) != 3 || plan[2] != (store.PlanItem{ID: "3", Title: "Write the report", Status: "pending"}) {
		t.Fatalf("plan %+v", plan)
	}
}

func TestProcessUpdatePlanRejectsInvalidItems(t *testing.T) {
	// Unknown statuses or duplicate ids are a tool error for the model; the stored plan is unchanged.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "p1", "update_plan", `{"items":[{"id":"1","title":"A","status":"done"}]}`)}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	th, run := r.queued("q")

	// Act
	r.process(run)

	// Assert
	res := find(r.events(run.ID), "TOOL_CALL_RESULT")[0]
	if res["metadata"] == nil || !strings.Contains(res["content"].(string), "status must be") || len(find(r.events(run.ID), "STATE_SNAPSHOT")) != 0 {
		t.Fatalf("result %v", res)
	}
	if len(r.thread(th.ID).Plan) != 0 {
		t.Fatal("invalid plan stored")
	}
}
