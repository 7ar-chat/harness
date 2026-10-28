package runtime_test

import (
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
)

func TestTheRuntimeDoesNotReCheckAFileTheCommandJustChecked(t *testing.T) {
	// A command whose last step runs the file's own checker command on the deliverable it wrote already checked the final content: the runtime takes that result instead of running the checker a second time (A3).
	// Arrange
	check := "bash " + fixtureChecker(t) + " /workspace/outputs/report.docx"
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"printf fine > /workspace/outputs/report.docx && `+check+`"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Report ready.")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected call")},
	)
	th, run := r.queued("write the report")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	runs := 0
	for _, e := range r.sandbox().Execs() {
		runs += strings.Count(e.Command, "check_doc.sh")
	}
	checks := 0
	for _, e := range find(r.events(run.ID), "CUSTOM:work.verification") {
		if v := e["value"].(map[string]any); v["toolCallId"] == "c1" {
			checks++
		}
	}
	v := verificationOf(t, lastAnswer(t, r, th.ID))
	if runs != 1 || checks != 1 || len(r.model.Calls()) != 2 || len(paths(v["verified"])) != 1 {
		t.Fatalf("checker runs %d, work.verification %d, calls %d, verification %v", runs, checks, len(r.model.Calls()), v)
	}
}
