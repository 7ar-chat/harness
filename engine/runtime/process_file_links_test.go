package runtime_test

import (
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
)

// knownTypes are the event types of an ordinary run with tool calls and artifacts.
var knownTypes = map[string]bool{
	"RUN_STARTED": true, "STEP_STARTED": true, "STEP_FINISHED": true,
	"TEXT_MESSAGE_START": true, "TEXT_MESSAGE_CONTENT": true, "TEXT_MESSAGE_END": true,
	"TOOL_CALL_START": true, "TOOL_CALL_ARGS": true, "TOOL_CALL_END": true, "TOOL_CALL_RESULT": true,
	"CUSTOM:work.usage": true, "CUSTOM:work.artifact": true, "RUN_FINISHED": true,
}

// lastUserText is the last user message sent to the model in a call.
func lastUserText(msgs []*ai.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == ai.RoleUser {
			return msgs[i].Content
		}
	}
	return ""
}

func TestSystemPromptSendsDeliverablesToOutputsAndOnlyLinksSavedFiles(t *testing.T) {
	// With a sandbox, the system prompt puts final deliverables in /workspace/outputs/ and forbids links to files that do not exist or live in /workspace/.work/.
	// Arrange
	r := newScriptedRig(t, false, llmtest.Turn{Chunks: llmtest.Text("Hi")})
	_, run := r.queued("hello")

	// Act
	r.process(run)

	// Assert
	prompt := r.model.Calls()[0].Messages[0].Content
	if !strings.Contains(prompt, "final deliverables in /workspace/outputs/") || !strings.Contains(prompt, "Never link a file under /workspace/.work/") ||
		strings.Contains(prompt, "Produce final files in /workspace and") {
		t.Fatalf("prompt %q", prompt)
	}
}

func TestAnswerLinkingUnsavedFilesGetsOneCorrectiveStep(t *testing.T) {
	// A final answer linking files that are not saved artifacts (a /workspace/.work/ chart, a file never written) gets one corrective step with a user-side note, then the run finishes with the corrected answer and only the usual event types.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"/workspace/.work/chart.png","content":"png"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "write_file", `{"path":"/workspace/outputs/report.md","content":"# R"}`)},
		llmtest.Turn{Chunks: llmtest.Text("See [chart](workspace-file:///workspace/.work/chart.png), [report](workspace-file:///workspace/outputs/report.md) and [data](workspace-file:///workspace/outputs/my%20data.csv).")},
		llmtest.Turn{Chunks: streamedCall("call_3", "write_file", `{"path":"/workspace/outputs/chart.png","content":"png"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Fixed: [chart](workspace-file:///workspace/outputs/chart.png), [report](workspace-file:///workspace/outputs/report.md).")},
	)
	th, run := r.queued("make a report with a chart")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 5 {
		t.Fatalf("status %s model calls %d", got.Status, len(calls))
	}
	note := lastUserText(calls[3].Messages)
	want := "These linked files don't exist as saved files: /workspace/.work/chart.png, /workspace/outputs/my data.csv; save them under /workspace/outputs/ or fix the links"
	if !strings.HasPrefix(note, want) {
		t.Fatalf("corrective note %q", note)
	}
	for _, typ := range types(r.events(run.ID)) {
		if !knownTypes[typ] {
			t.Fatalf("new event type %s", typ)
		}
	}
	msgs := r.messages(th.ID)
	if last := msgs[len(msgs)-1]; last.Role != store.RoleAssistant || !strings.HasPrefix(last.Content, "Fixed:") {
		t.Fatalf("last message %+v", last)
	}
	for _, m := range msgs {
		if m.Role == store.RoleUser && strings.Contains(m.Content, "These linked files") {
			t.Fatal("the corrective note is shown to the user as a message")
		}
	}
}

func TestAnswerLinksAreCorrectedAtMostOncePerRun(t *testing.T) {
	// An answer that still links a missing file after the corrective step ends the run: there is never a second corrective step.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: llmtest.Text("See [x](workspace-file:///workspace/outputs/x.pdf)")},
		llmtest.Turn{Chunks: llmtest.Text("Still [x](workspace-file:///workspace/outputs/x.pdf)")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected third call")},
	)
	_, run := r.queued("pdf")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || len(r.model.Calls()) != 2 {
		t.Fatalf("status %s model calls %d", got.Status, len(r.model.Calls()))
	}
}

func TestAnswerLinkingSavedFilesOrOnTheLastStepIsNotCorrected(t *testing.T) {
	// Links to saved artifacts need no correction, and an answer given on the last allowed step is never corrected (no steps left; the second run wrote no deliverable, so it ends with max_steps, round 3).
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"outputs/a.md","content":"a"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Saved [a](workspace-file:///workspace/outputs/a.md)")},
		llmtest.Turn{Chunks: streamedCall("call_2", "list_files", `{}`)},
		llmtest.Turn{Chunks: streamedCall("call_3", "list_files", `{}`)},
		llmtest.Turn{Chunks: streamedCall("call_4", "list_files", `{}`)},
		llmtest.Turn{Chunks: streamedCall("call_5", "list_files", `{}`)},
		llmtest.Turn{Chunks: llmtest.Text("Missing [b](workspace-file:///workspace/outputs/b.md)")},
		llmtest.Turn{Chunks: llmtest.Text("unexpected call")},
	)
	th, first := r.queued("a")
	r.process(first)
	second := r.queuedOn(th, "b")

	// Act
	got := r.process(second)

	// Assert
	if got.Status != store.StatusFailed || got.ErrorCode != "max_steps" || len(r.model.Calls()) != 7 {
		t.Fatalf("status %s %s model calls %d", got.Status, got.ErrorCode, len(r.model.Calls()))
	}
}

func TestAnswerMentioningSavedFilesWithoutLinksGetsOneCorrectiveStep(t *testing.T) {
	// A final answer that names a saved file without a workspace-file link (by absolute path, or by the name of a file produced in this run) gets one corrective step asking to link it (4.6).
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"/workspace/outputs/q3-report.md","content":"# Q3"}`)},
		llmtest.Turn{Chunks: streamedCall("call_2", "write_file", `{"path":"/workspace/outputs/data.csv","content":"a,b"}`)},
		llmtest.Turn{Chunks: llmtest.Text("I wrote q3-report.md and the table is at /workspace/outputs/data.csv. Notes: see report.md style.")},
		llmtest.Turn{Chunks: llmtest.Text("Here: [q3-report.md](workspace-file:///workspace/outputs/q3-report.md), [data.csv](workspace-file:///workspace/outputs/data.csv).")},
	)
	_, run := r.queued("write the report")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 4 {
		t.Fatalf("status %s model calls %d", got.Status, len(calls))
	}
	note := lastUserText(calls[3].Messages)
	want := "You mention files without linking them: /workspace/outputs/data.csv, /workspace/outputs/q3-report.md; link each one as [name](workspace-file:///absolute/path)"
	if !strings.HasPrefix(note, want) {
		t.Fatalf("corrective note %q", note)
	}
}

func TestAnswerLinkingEveryFileItMentionsGetsNoNote(t *testing.T) {
	// An answer whose mentioned files are all linked (and mentions of unrelated names) ends the run without a corrective step.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("call_1", "write_file", `{"path":"/workspace/outputs/q3.md","content":"# Q3"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done: [q3.md](workspace-file:///workspace/outputs/q3.md) — the q3.md file follows the template.md style.")},
	)
	_, run := r.queued("write it")

	// Act
	r.process(run)

	// Assert
	if n := len(r.model.Calls()); n != 2 {
		t.Fatalf("model calls %d", n)
	}
}
