package runtime_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
)

func TestAViewThatFailedAfterTheLastChangeDoesNotBlockTheStop(t *testing.T) {
	// When view_file could not render a deliverable after its last change, the StopGuard accepts the stop (it cannot be satisfied) and the answer's metadata.verification lists the file as unverified with "render failed: <reason>" (A5).
	// Arrange
	r := verifyRig(t, true,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view_file", `{"path":"outputs/report.docx"}`)}},
		llmtest.Turn{Chunks: llmtest.Text("Report ready; I could not view its pages (render failed).")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected call")},
	)
	th, run := r.queued("write the report")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	if calls := r.model.Calls(); len(calls) != 3 {
		t.Fatalf("calls %d (the stop was refused)", len(calls))
	}
	for _, e := range find(r.events(run.ID), "TOOL_CALL_RESULT") {
		if e["toolCallId"] == "v1" && !strings.Contains(e["content"].(string), "say in your answer that you could not view it") {
			t.Fatalf("view result %v", e["content"])
		}
	}
	v := verificationOf(t, lastAnswer(t, r, th.ID))
	un, _ := v["unverified"].([]any)
	if len(un) != 1 {
		t.Fatalf("verification %v", v)
	}
	item := un[0].(map[string]any)
	if item["path"] != "/workspace/outputs/report.docx" || !strings.HasPrefix(fmt.Sprint(item["reason"]), "render failed: ") {
		t.Fatalf("unverified %v", item)
	}
}
