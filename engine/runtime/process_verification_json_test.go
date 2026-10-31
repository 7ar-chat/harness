package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/verify"
)

// Verification in JSON
// results, the latest result decides, strict model-run checks, checks past the
// per-call cap, JSON-preserving spills.

// failingCheck writes a script printing a failing checker report for file (exit 2).
func failingCheck(file string) string {
	return `{"path":"/workspace/.work/strict.sh","content":"echo '{\"ok\": false, \"file\": \"` + file +
		`\", \"checks\": [{\"name\": \"headings\", \"ok\": false, \"detail\": \"Heading 3 after Heading 1\"}]}'; exit 2"}`
}

func verificationOf(t *testing.T, m *store.Message) map[string]any {
	t.Helper()
	v, ok := m.Metadata["verification"].(map[string]any)
	if !ok {
		t.Fatalf("no metadata.verification on %q", m.Content)
	}
	return v
}

func paths(list any) []string {
	var out []string
	items, _ := list.([]any)
	for _, it := range items {
		switch x := it.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			out = append(out, x["path"].(string))
		}
	}
	return out
}

func TestAJSONToolResultCarriesItsVerificationAsAField(t *testing.T) {
	// The checker result of a deliverable written by a JSON-result tool is a "verification" field of that JSON (no text suffix): the result stays valid JSON.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Report ready.")},
	)
	_, run := r.queued("write the report")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	content := find(evs, "TOOL_CALL_RESULT")[0]["content"].(string)
	var res struct {
		Path         string `json:"path"`
		Verification []struct {
			Path, Checker, Summary string
			OK                     bool
		} `json:"verification"`
	}
	if err := json.Unmarshal([]byte(content), &res); err != nil || strings.Contains(content, "[verification]") {
		t.Fatalf("not a JSON result with a verification field: %v %q", err, content)
	}
	v := res.Verification
	if res.Path != "/workspace/outputs/report.docx" || len(v) != 1 || v[0].Path != "/workspace/outputs/report.docx" || v[0].Checker != "inspect_docx" ||
		!v[0].OK || v[0].Summary != "inspect_docx: ok (2 checks passed)" {
		t.Fatalf("verification %+v", res)
	}
}

func TestAFailingCheckAfterAPassingOneLeavesTheDeliverableUnverified(t *testing.T) {
	// The latest check after the last change decides: a passing automatic check followed by a failing one is not verified, the stop is refused and the answer lists the file under failed.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/report.docx","content":"fine"}`)},
		llmtest.Turn{Chunks: streamedCall("w2", "write_file", failingCheck("/workspace/outputs/report.docx"))},
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"bash /workspace/.work/strict.sh"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Done.")},
		llmtest.Turn{Chunks: llmtest.Text("Done, but the headings still fail.")},
	)
	r.agent.MaxSteps = 8
	th, run := r.queued("write the report")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if len(calls) != 5 || !strings.Contains(calls[4].Messages[len(calls[4].Messages)-1].Content, "/workspace/outputs/report.docx: its checker failed") {
		t.Fatalf("calls %d (no reminder for the failing check?)", len(calls))
	}
	v := verificationOf(t, lastAnswer(t, r, th.ID))
	if got := paths(v["failed"]); len(got) != 1 || got[0] != "/workspace/outputs/report.docx" || len(paths(v["verified"])) != 0 {
		t.Fatalf("verification %v", v)
	}
	failed := v["failed"].([]any)[0].(map[string]any)
	if failed["checker"] != "strict" || !strings.Contains(failed["summary"].(string), "Heading 3 after Heading 1") {
		t.Fatalf("failed item %v", failed)
	}
	live := find(r.events(run.ID), "CUSTOM:work.verification")
	if ev := live[len(live)-1]["value"].(map[string]any); len(paths(ev["failed"])) != 1 {
		t.Fatalf("answer event %v", ev)
	}
}

func TestACheckTheModelRanBeforeChangingTheFileInTheSameCommandDoesNotCount(t *testing.T) {
	// A model-run checker followed, in the same command, by a change of the file it checked says nothing about the new content: the file stays unverified.
	// Arrange
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/notes.txt","content":"fine"}`)},
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"bash `+fixtureChecker(t)+` /workspace/outputs/notes.txt; printf tweaked >> /workspace/outputs/notes.txt"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Checked and done.")},
		llmtest.Turn{Chunks: llmtest.Text("Done; the notes could not be verified.")},
	)
	th, run := r.queued("write notes")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	if n := len(r.model.Calls()); n != 4 {
		t.Fatalf("calls %d (the stop was not refused)", n)
	}
	v := verificationOf(t, lastAnswer(t, r, th.ID))
	if got := paths(v["verified"]); len(got) != 0 {
		t.Fatalf("verified %v", got)
	}
}

func TestOnlyAStrictModelRunCheckerReportCounts(t *testing.T) {
	// A model-run "checker" report with an empty checks list, or one printed without running any script (cat of an old report), does not verify a deliverable.
	for name, command := range map[string]string{
		"empty checks": `{"command":"bash /workspace/.work/strict.sh"}`,
		"old report":   `{"command":"cat /workspace/.work/report.json"}`,
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			report := `{\"ok\": true, \"file\": \"/workspace/outputs/notes.txt\", \"checks\": [{\"name\": \"opens\", \"ok\": true, \"detail\": \"\"}]}`
			r := verifyRig(t, false,
				llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/notes.txt","content":"fine"}`)},
				llmtest.Turn{Chunks: streamedCall("w2", "write_file", `{"path":"/workspace/.work/strict.sh","content":"echo '{\"ok\": true, \"file\": \"/workspace/outputs/notes.txt\", \"checks\": []}'"}`)},
				llmtest.Turn{Chunks: streamedCall("w3", "write_file", `{"path":"/workspace/.work/report.json","content":"`+report+`"}`)},
				llmtest.Turn{Chunks: streamedCall("c1", "execute_command", command)},
				llmtest.Turn{Chunks: llmtest.Text("Checked.")},
				llmtest.Turn{Chunks: llmtest.Text("Checked; the notes are unverified.")},
			)
			r.agent.MaxSteps = 8
			th, run := r.queued("write notes")

			// Act
			r.process(run)

			// Assert
			checkStrictOrder(t, r.events(run.ID))
			v := verificationOf(t, lastAnswer(t, r, th.ID))
			if got := paths(v["verified"]); len(r.model.Calls()) != 6 || len(got) != 0 {
				t.Fatalf("calls %d verified %v", len(r.model.Calls()), got)
			}
		})
	}
}

func TestAModelRunCheckerReportWithARelativePathCounts(t *testing.T) {
	// A checker run from /workspace or /workspace/outputs that prints a relative "file" verifies the deliverable it names.
	for name, command := range map[string]string{
		"relative to /workspace": "cd /workspace && bash " + "%s" + " outputs/notes.txt",
		"bare file name":         "cd /workspace/outputs && bash " + "%s" + " notes.txt",
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			cmd := strings.Replace(command, "%s", fixtureChecker(t), 1)
			r := verifyRig(t, false,
				llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/notes.txt","content":"fine"}`)},
				llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"`+cmd+`"}`)},
				llmtest.Turn{Chunks: llmtest.Text("Checked and done.")},
			)
			th, run := r.queued("write notes")

			// Act
			r.process(run)

			// Assert
			checkStrictOrder(t, r.events(run.ID))
			v := verificationOf(t, lastAnswer(t, r, th.ID))
			if got := paths(v["verified"]); len(r.model.Calls()) != 3 || len(got) != 1 || got[0] != "/workspace/outputs/notes.txt" {
				t.Fatalf("calls %d verification %v", len(r.model.Calls()), v)
			}
		})
	}
}

func TestFilesPastThePerCallCheckCapAreCheckedInTheNextStep(t *testing.T) {
	// A command that changes five deliverables gets three checks in its own result; the other two are checked before the next model call, which sees their results.
	// Arrange
	write := "for f in a b c d; do printf fine > /workspace/outputs/$f.docx; done; printf BROKEN > /workspace/outputs/e.docx"
	r := verifyRig(t, false,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"`+write+`"}`)},
		llmtest.Turn{Chunks: llmtest.Text("All five written.")},
		llmtest.Turn{Chunks: llmtest.Text("Four are fine; the fifth still fails its checker.")},
	)
	th, run := r.queued("write five reports")

	// Act
	r.process(run)

	// Assert
	evs := r.events(run.ID)
	checkStrictOrder(t, evs)
	calls := r.model.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	var seen strings.Builder
	for _, m := range calls[1].Messages {
		seen.WriteString(m.Content + "\n")
	}
	if !strings.Contains(seen.String(), "/workspace/outputs/d.docx — inspect_docx: ok") || !strings.Contains(seen.String(), "/workspace/outputs/e.docx — inspect_docx: NOT OK") {
		t.Fatalf("the next call does not see the deferred checks:\n%s", seen.String())
	}
	checked := map[string]bool{}
	for _, e := range find(evs, "CUSTOM:work.verification") {
		if v := e["value"].(map[string]any); v["toolCallId"] == "c1" {
			checked[v["path"].(string)] = true
		}
	}
	if len(checked) != 5 {
		t.Fatalf("work.verification for %v", checked)
	}
	v := verificationOf(t, lastAnswer(t, r, th.ID))
	if got := paths(v["verified"]); len(got) != 4 || paths(v["failed"])[0] != "/workspace/outputs/e.docx" {
		t.Fatalf("verification %v", v)
	}
}

func TestAViewWhosePagesWereNeverSentDoesNotCountAsViewed(t *testing.T) {
	// A view_file whose page images were dropped by the per-request image cap was never seen by the model: the deck still needs a view, and the dropped page is stubbed "not shown", not "shown earlier".
	// Arrange
	r := verifyRig(t, true,
		llmtest.Turn{Chunks: streamedCall("c1", "execute_command", `{"command":"true"}`)},
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "v1", "view_file", `{"path":"outputs/deck.pptx","pages":"1"}`), llmtest.ToolCall(1, "v2", "view", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("Deck ready.")},
		llmtest.Turn{Chunks: llmtest.Text("Deck ready; I could not look at slide 1.")},
	)
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb)))
		sb.PutDoc("/workspace/outputs/deck.pptx", sandboxtest.Doc{Parts: []string{"Title"}})
	}
	r.withTools(viewTool(6, 100))
	r.agent.MaxSteps = 6
	th, run := r.queued("make the deck")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if len(calls) != 4 || !strings.Contains(calls[3].Messages[len(calls[3].Messages)-1].Content, "deck.pptx: its rendered pages were not viewed") {
		t.Fatalf("calls %d (the unseen view counted)", len(calls))
	}
	_, stubs := imageParts(calls[3])
	var notShown int
	for _, s := range stubs {
		if strings.Contains(s, "/workspace/.work/renders/") && strings.Contains(s, "not shown") && !strings.Contains(s, "p1.png shown earlier") {
			notShown++
		}
	}
	if notShown != 1 {
		t.Fatalf("stubs %q", stubs)
	}
	if v := verificationOf(t, lastAnswer(t, r, th.ID)); len(paths(v["verified"])) != 0 {
		t.Fatalf("verification %v", v)
	}
}

func TestALargeJSONResultIsSpilledAsValidJSON(t *testing.T) {
	// A JSON-object result over the spill limit keeps its shape: the large string field is cut in place, "spilled": true and "outputPath" are added, and outputPath holds the full result.
	// Arrange
	big := `{"content":"HEAD ` + strings.Repeat("row,1,2,3\\n", 3000) + ` TAIL","pages":7}`
	r := newRig(t,
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c0", "execute_command", `{"command":"true"}`)}},
		llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "dump", `{}`)}},
		llmtest.Turn{Chunks: llmtest.Text("ok")},
	)
	r.withTools(&fakeTool{name: "dump", run: func(context.Context, string) (string, error) { return big, nil }})
	r.agent.Harness.Spill = tools.SandboxSpiller{Sandboxes: r.mgr}
	r.agent.SpillChars = 4000
	_, run := r.queued("dump the table")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	content := find(r.events(run.ID), "TOOL_CALL_RESULT")[1]["content"].(string)
	var res struct {
		Content    string `json:"content"`
		Pages      int    `json:"pages"`
		Spilled    bool   `json:"spilled"`
		OutputPath string `json:"outputPath"`
	}
	if err := json.Unmarshal([]byte(content), &res); err != nil {
		t.Fatalf("spilled result is not JSON: %v (%d chars) %q", err, len(content), content[:200])
	}
	full, ok := r.file(res.OutputPath)
	if !res.Spilled || res.Pages != 7 || !ok || full != big || len([]rune(content)) > 4000 {
		t.Fatalf("spilled %v pages %d path %q kept %v size %d", res.Spilled, res.Pages, res.OutputPath, ok, len(content))
	}
	if !strings.HasPrefix(res.Content, "HEAD ") || !strings.HasSuffix(res.Content, " TAIL") || !strings.Contains(res.Content, res.OutputPath) {
		t.Fatalf("content field %q…", res.Content[:100])
	}
}

func TestTheReminderNamesTheValidatorScriptOfAnUncheckedDataFile(t *testing.T) {
	// A CSV deliverable whose automatic check gave no result is named in the stop reminder with the shipped validator script the model can run itself.
	// Arrange
	r := newScriptedRig(t, false,
		llmtest.Turn{Chunks: streamedCall("w1", "write_file", `{"path":"outputs/data.csv","content":"a,b\n1,2\n"}`)},
		llmtest.Turn{Chunks: llmtest.Text("Data ready.")},
		llmtest.Turn{Chunks: llmtest.Text("Data ready; it could not be checked.")},
	)
	r.agent.Harness.Checker = verify.New(verify.Options{Sandbox: r.mgr})
	_, run := r.queued("export the data")

	// Act
	r.process(run)

	// Assert
	checkStrictOrder(t, r.events(run.ID))
	calls := r.model.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls %d", len(calls))
	}
	reminder := calls[2].Messages[len(calls[2].Messages)-1].Content
	if !strings.Contains(reminder, "/workspace/outputs/data.csv: its checker could not run") ||
		!strings.Contains(reminder, "python3 /skills/xlsx/scripts/validate_file.py '/workspace/outputs/data.csv'") {
		t.Fatalf("reminder %q", reminder)
	}
}
