package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func TestProcessAnEditOfAnUnreadFileShowsItAndTheNextEditWorks(t *testing.T) {
	// edit_file refusing a file the model has not read returns the file's numbered content and counts as a read, so the retried edit works in the next step (A6).
	// Arrange
	edit := func(id string) llmtest.Turn {
		return llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, id, "edit_file", `{"path":"/workspace/run.py","oldText":"x = 1","newText":"x = 2"}`)}}
	}
	r := newRig(t, edit("e1"), edit("e2"), llmtest.Turn{Chunks: llmtest.Text("done")})
	th, run := r.queued("set x to 2 in run.py")
	if _, err := r.mgr.Acquire(context.Background(), th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	sb := r.backend.Created()[0]
	sb.Put("/workspace/run.py", "import sys\nx = 1\nprint(x)\n")

	// Act
	got := r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if len(results) != 2 {
		t.Fatalf("results %v", results)
	}
	first, _ := results[0]["metadata"].(map[string]any)
	refusal := results[0]["content"].(string)
	if first["isError"] != true || !strings.Contains(refusal, "     2\tx = 1") || !strings.Contains(refusal, "counts as a read") {
		t.Fatalf("refusal %q", refusal)
	}
	if md, _ := results[1]["metadata"].(map[string]any); md["isError"] == true {
		t.Fatalf("retried edit refused: %v", results[1]["content"])
	}
	content, _ := sb.File("/workspace/run.py")
	if got.Status != store.StatusCompleted || content != "import sys\nx = 2\nprint(x)\n" {
		t.Fatalf("status %s content %q", got.Status, content)
	}
}
