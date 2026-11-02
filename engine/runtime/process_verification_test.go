package runtime_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/verify"
)

// fixtureChecker is the testdata checker (checker contract) for .docx files.
func fixtureChecker(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "features", "verify", "testdata", "checkers", "check_doc.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// verifyRig runs real bash (ShellExec) for commands and checks .docx and
// .pptx deliverables with the fixture checker.
func verifyRig(t *testing.T, vision bool, turns ...llmtest.Turn) *scriptedRig {
	t.Helper()
	r := newScriptedRig(t, vision, turns...)
	r.strictOrder()
	check := "bash " + fixtureChecker(t) + " {file}"
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) }
	r.agent.Harness.Checker = verify.New(verify.Options{Sandbox: r.mgr, Checkers: map[string]verify.Checker{
		".docx": {Name: "inspect_docx", Command: check},
		".pptx": {Name: "fit_text", Command: check},
		".txt":  {Name: "broken_checker", Command: "exit 1"},
	}})
	r.agent.Harness.Vision = func(string) bool { return vision }
	return r
}

func lastAnswer(t *testing.T, r *scriptedRig, threadID string) *store.Message {
	t.Helper()
	msgs := r.messages(threadID)
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == store.RoleAssistant && len(msgs[i].ToolCalls) == 0 {
			return msgs[i]
		}
	}
	t.Fatal("no answer")
	return nil
}

func TestWritingADeliverableRunsItsCheckerInTheSameStep(t *testing.T) {
	// A deliverable written under /workspace/outputs/ is checked right away: the checker's summary is appended to the tool result, work.verification follows the result, and the answer carries metadata.verification with the file verified.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Report ready.")},
	)
	th, run := r.queued("write the report")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	res := find(evs, "TOOL_CALL_RESULT")[0]["content"].(string)
	if !strings.Contains(res, `"verification":[{"checker":"inspect_docx","ok":true,"path":"/workspace/outputs/report.docx","summary":"inspect_docx: ok (2 checks passed)"}]`) {
		t.Fatalf("result %q", res)
	}
	tt := types(evs)
	vi := index(tt, "CUSTOM:work.verification")
	if vi < index(tt, "TOOL_CALL_RESULT") {
		t.Fatalf("types %v", tt)
	}
	ev := evs[vi]["value"].(map[string]any)
	if ev["toolCallId"] != "w1" || ev["path"] != "/workspace/outputs/report.docx" || ev["checker"] != "inspect_docx" || ev["ok"] != true {
		t.Fatalf("event %v", ev)
	}
	ans := lastAnswer(t, r, th.ID)
	v, _ := json.Marshal(ans.Metadata["verification"])
	if string(v) != `{"failed":[],"unverified":[],"verified":["/workspace/outputs/report.docx"]}` {
		t.Fatalf("answer verification %s", v)
	}
}

func TestStoppingWithAFailingDeliverableIsRefusedOnceAndTheAnswerSaysFailed(t *testing.T) {
	// A deliverable whose checker failed blocks the first stop with a reminder naming it; a second stop ends the run and the answer's metadata.verification lists it under failed with the checker's summary.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"BROKEN"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Done, but the headings could not be fixed.")},
	)
	th, run := r.queued("write the report")

	// Act
	got := r.process(run)

	// Assert
	calls := r.model.Calls()
	if got.Status != store.StatusCompleted || len(calls) != 3 {
		t.Fatalf("status %s calls %d", got.Status, len(calls))
	}
	res := find(r.events(run.ID), "TOOL_CALL_RESULT")[0]["content"].(string)
	if !strings.Contains(res, "inspect_docx: NOT OK — headings: Heading 3 directly after Heading 1") || !strings.Contains(res, `"nextStep":"fix the heading levels`) {
		t.Fatalf("result %q", res)
	}
	reminder := calls[2].Messages[len(calls[2].Messages)-1].Content
	if !strings.Contains(reminder, "not verified yet") || !strings.Contains(reminder, "/workspace/outputs/report.docx: its checker failed") {
		t.Fatalf("reminder %q", reminder)
	}
	ans := lastAnswer(t, r, th.ID)
	v := ans.Metadata["verification"].(map[string]any)
	fl := v["failed"].([]any)
	if len(fl) != 1 || fl[0].(map[string]any)["path"] != "/workspace/outputs/report.docx" || fl[0].(map[string]any)["checker"] != "inspect_docx" ||
		!strings.Contains(fl[0].(map[string]any)["summary"].(string), "Heading 3 directly after Heading 1") || len(v["unverified"].([]any)) != 0 {
		t.Fatalf("verification %v", v)
	}
	live := find(r.events(run.ID), "CUSTOM:work.verification")
	if last := live[len(live)-1]["value"].(map[string]any); last["messageId"] != ans.ID || len(last["failed"].([]any)) != 1 {
		t.Fatalf("answer event %v", last)
	}
}

func TestACheckerTheModelRunsItselfCountsAfterTheLastChange(t *testing.T) {
	// When the automatic checker could not run, the model running the checker itself (its contract JSON on stdout) verifies the deliverable: no reminder, the answer lists it verified.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/notes.txt","content":"fine"}`)},
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"bash `+fixtureChecker(t)+` /workspace/outputs/notes.txt"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Checked and done.")},
	)
	th, run := r.queued("write notes")

	// Act
	r.process(run)

	// Assert
	results := find(r.events(run.ID), "TOOL_CALL_RESULT")
	if !strings.Contains(results[0]["content"].(string), "broken_checker: could not run (exit 1)") {
		t.Fatalf("first result %q", results[0]["content"])
	}
	if len(r.model.Calls()) != 3 {
		t.Fatalf("calls %d (reminded?)", len(r.model.Calls()))
	}
	v := lastAnswer(t, r, th.ID).Metadata["verification"].(map[string]any)
	if ver := v["verified"].([]any); len(ver) != 1 || ver[0] != "/workspace/outputs/notes.txt" {
		t.Fatalf("verification %v", v)
	}
}

func TestVisionModelsMustViewChangedDocumentsBeforeStopping(t *testing.T) {
	// With a vision model a changed deck needs a passing checker and a view_file of it after its last change: the stop without a view is refused, after the view the answer lists it verified.
	// Arrange
	r := verifyRig(t, true,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"true"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Deck ready.")},
		llmtest.Turn{Chunks: streamedCall("v1", "view_file", `{"path":"outputs/deck.pptx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Deck ready, slides look right.")},
	)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb)))
		sb.PutDoc("/workspace/outputs/deck.pptx", sandboxtest.Doc{Parts: []string{"Title"}}) // produced by the command
	}
	th, run := r.queued("make the deck")

	// Act
	r.process(run)

	// Assert
	calls := r.model.Calls()
	if len(calls) != 4 {
		t.Fatalf("calls %d", len(calls))
	}
	reminder := calls[2].Messages[len(calls[2].Messages)-1].Content
	if !strings.Contains(reminder, "/workspace/outputs/deck.pptx: its rendered pages were not viewed") {
		t.Fatalf("reminder %q", reminder)
	}
	v := lastAnswer(t, r, th.ID).Metadata["verification"].(map[string]any)
	if ver := v["verified"].([]any); len(ver) != 1 || ver[0] != "/workspace/outputs/deck.pptx" || len(v["unverified"].([]any)) != 0 {
		t.Fatalf("verification %v", v)
	}
}

func TestDeliverablesOfTheRunCarryTheirLastCheckerResult(t *testing.T) {
	// The verifier lists the turn's deliverables for the post-compaction rehydration: a file checked after its last change has its verdict, a file whose checker could not run after its last change has none.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/a.docx","content":"BROKEN"}`)},
		llmtest.Turn{Chunks: streamedCall("w2", "write_file", `{"path":"outputs/b.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"echo more >> /workspace/outputs/b.docx"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Done anyway.")},
	)
	r.agent.MaxSteps = 10
	r.agent.Harness.Checker = verify.New(verify.Options{Sandbox: r.mgr, Checkers: map[string]verify.Checker{
		".docx": {Name: "inspect_docx", Command: "case {file} in *a.docx) bash " + fixtureChecker(t) + " {file};; *) true;; esac"},
	}})
	_, run := r.queued("write both")
	r.process(run)
	v := verify.New(verify.Options{Messages: r.st})

	// Act
	ds := v.Deliverables(t.Context(), run)

	// Assert
	if len(ds) != 2 || ds[0].Path != "/workspace/outputs/a.docx" || ds[0].OK == nil || *ds[0].OK || ds[0].Checker != "inspect_docx" {
		t.Fatalf("deliverables %+v", ds)
	}
	if ds[1].Path != "/workspace/outputs/b.docx" || ds[1].OK != nil {
		t.Fatalf("b.docx %+v (no runnable check after its last change: no verdict)", ds[1])
	}
}
