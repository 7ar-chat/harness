package runtime_test

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/store"
)

func TestACommandThatDeletesItsOwnJobFilesStillReportsItsExitStatus(t *testing.T) {
	// A command that deletes /workspace/.work (and with it its own job files) gets its exit status and a result saying the command deleted its output files, not that the sandbox may have restarted (A11).
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: []*schema.Message{llmtest.ToolCall(0, "c1", "execute_command", `{"command":"rm -rf /workspace/.work && unzip -q in.zip -d /workspace/.work/in"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("done")})
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetJobs(func(_ context.Context, _ string, out func(stream, text string)) int {
			out("stdout", "unzipped\n")
			sb.RemoveTree("/workspace/.work")
			return 3
		})
	}
	_, run := r.queued("unpack the archive")

	// Act
	got := r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	content := results[0]["content"].(string)
	if got.Status != store.StatusCompleted || !strings.Contains(content, `"exitCode":3`) ||
		!strings.Contains(content, "deleted its own output files") || strings.Contains(content, "restarted") {
		t.Fatalf("status %s result %s", got.Status, content)
	}
}
