package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

const docx = "/workspace/outputs/r.docx"

func TestVerdictNeedsACheckAfterTheLastChangeThatRanToCompletion(t *testing.T) {
	for name, tc := range map[string]struct {
		d       deliverable
		checked bool
		ok      bool
	}{
		"never checked":         {deliverable{changed: 1}, false, false},
		"checked then changed":  {deliverable{changed: 3, checked: 2, hasLast: true, last: CheckResult{OK: true}}, false, false},
		"checked after change":  {deliverable{changed: 1, checked: 2, hasLast: true, last: CheckResult{OK: true}}, true, true},
		"failing after change":  {deliverable{changed: 1, checked: 2, hasLast: true, last: CheckResult{OK: false}}, true, false},
		"checker could not run": {deliverable{changed: 1, checked: 2, hasLast: true, last: CheckResult{Failed: true}}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			if checked, ok := tc.d.verdict(); checked != tc.checked || ok != tc.ok {
				t.Fatalf("verdict = %v %v", checked, ok)
			}
		})
	}
}

func TestAPassingModelRunCheckNeverOverridesTheRuntimesVerdictOnTheSameContent(t *testing.T) {
	// Arrange
	v := &verifyState{}
	v.changed(docx, store.ChangeUpdated)
	v.result(CheckResult{Path: docx, OK: false, Summary: "runtime: title missing"}, true)

	// Act
	v.result(CheckResult{Path: docx, OK: true, Summary: "model-run: ok"}, false)

	// Assert
	d := v.files[docx]
	if d.last.Summary != "runtime: title missing" || !d.runtime {
		t.Fatalf("last %+v", d.last)
	}
}

func TestAFailingModelRunCheckAndAnyLaterChangeStillCount(t *testing.T) {
	// Arrange
	v := &verifyState{}
	v.changed(docx, store.ChangeUpdated)
	v.result(CheckResult{Path: docx, OK: true, Summary: "runtime ok"}, true)

	// Act
	v.result(CheckResult{Path: docx, OK: false, Summary: "model-run: broken"}, false)

	// Assert
	if checked, ok := v.files[docx].verdict(); !checked || ok {
		t.Fatalf("a failing model-run check was ignored: %v %v", checked, ok)
	}

	// A change after the runtime check makes it stale: a model-run pass then counts.
	v.changed(docx, store.ChangeUpdated)
	v.result(CheckResult{Path: docx, OK: true, Summary: "model-run ok"}, false)
	if checked, ok := v.files[docx].verdict(); !checked || !ok {
		t.Fatalf("verdict after rewrite: %v %v", checked, ok)
	}
}

func TestChangedClearsPendingAndDeleteForgetsTheFile(t *testing.T) {
	// Arrange
	v := &verifyState{}
	v.file(docx).pending = true

	// Act
	v.changed(docx, store.ChangeCreated)

	// Assert
	if v.files[docx].pending || v.files[docx].changed == 0 {
		t.Fatalf("%+v", v.files[docx])
	}
	v.changed(docx, store.ChangeDeleted)
	if _, ok := v.files[docx]; ok {
		t.Fatal("deleted file still tracked")
	}
	v.changed("/workspace/outputs/never.md", store.ChangeDeleted) // no panic on unknown files
}

func TestViewedOnlyMovesForwardAndOnlyForTrackedFiles(t *testing.T) {
	// Arrange
	v := &verifyState{}
	v.file(docx)

	// Act
	v.viewed(docx, 5)
	v.viewed(docx, 3)
	v.viewed("/workspace/outputs/other.pdf", 9)

	// Assert
	if v.files[docx].viewed != 5 || len(v.files) != 1 {
		t.Fatalf("%+v", v.files)
	}
}

func TestViewsCountOnlyOnceEveryPageImageWasSent(t *testing.T) {
	// Arrange
	v := &verifyState{}
	v.file(docx)
	v.file("/workspace/outputs/deck.pptx")
	v.views = []pendingView{
		{path: docx, seq: 4, images: []string{"/p1.png", "/p2.png"}},
		{path: "/workspace/outputs/deck.pptx", seq: 6, images: []string{"/q1.png", "/q2.png"}},
	}

	// Act: the per-request image cap left one page of the deck out.
	v.viewsSent(map[string]bool{"/p1.png": true, "/p2.png": true, "/q1.png": true})

	// Assert
	if v.files[docx].viewed != 4 || v.files["/workspace/outputs/deck.pptx"].viewed != 0 || v.views != nil {
		t.Fatalf("docx viewed %d deck viewed %d views %v", v.files[docx].viewed, v.files["/workspace/outputs/deck.pptx"].viewed, v.views)
	}
}

func TestChangedOutputsListsOnlyDeliverablesAndKeepsDeletions(t *testing.T) {
	// Arrange
	md := map[string]any{"artifacts": []any{
		map[string]any{"path": "/workspace/outputs/a.md", "change": store.ChangeCreated},
		map[string]any{"path": "/workspace/outputs/b.md", "change": store.ChangeDeleted},
		map[string]any{"path": "/workspace/data/c.md", "change": store.ChangeCreated},
		"junk",
	}}

	// Act
	changed, all := changedOutputs(md)

	// Assert
	if fmt.Sprint(changed) != "[/workspace/outputs/a.md]" || fmt.Sprint(all) != "[[/workspace/outputs/a.md created] [/workspace/outputs/b.md deleted]]" {
		t.Fatalf("changed %v all %v", changed, all)
	}
}

func TestAbsPathResolvesRelativePathsUnderTheWorkspace(t *testing.T) {
	for in, want := range map[string]string{"outputs/a.md": "/workspace/outputs/a.md", "/workspace/outputs//a.md": "/workspace/outputs/a.md", "./x/../y": "/workspace/y"} {
		if got := absPath(in); got != want {
			t.Errorf("absPath(%q) = %q", in, got)
		}
	}
}

func stateWithChecker(chk *fakeChecker, vision bool) *runState {
	st := newState(10)
	st.a.Harness.Checker = chk
	st.a.Harness.Vision = func(string) bool { return vision }
	return st
}

func TestVerificationListsSortDeliverablesIntoVerifiedFailedAndUnverified(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx", ".xlsx": "inspect_xlsx", ".pptx": "inspect_pptx"}}
	st := stateWithChecker(chk, false)
	v := &st.v3.verify
	for _, p := range []string{"/workspace/outputs/ok.xlsx", "/workspace/outputs/bad.docx", "/workspace/outputs/unchecked.pptx", "/workspace/outputs/crashed.docx", "/workspace/outputs/pending.xlsx", "/workspace/outputs/notes.md"} {
		v.changed(p, store.ChangeCreated)
	}
	v.file("/workspace/outputs/untouched.docx") // not changed in this turn: ignored
	v.result(CheckResult{Path: "/workspace/outputs/ok.xlsx", Checker: "inspect_xlsx", OK: true}, true)
	v.result(CheckResult{Path: "/workspace/outputs/bad.docx", Checker: "inspect_docx", Summary: "no title"}, true)
	v.result(CheckResult{Path: "/workspace/outputs/crashed.docx", Failed: true, Summary: "timeout"}, true)
	v.files["/workspace/outputs/pending.xlsx"].pending = true

	// Act
	verified, failed, unverified := st.verificationLists()

	// Assert
	if fmt.Sprint(verified) != "[/workspace/outputs/ok.xlsx]" {
		t.Errorf("verified %v", verified)
	}
	if len(failed) != 1 || failed[0]["path"] != "/workspace/outputs/bad.docx" || failed[0]["summary"] != "no title" || failed[0]["checker"] != "inspect_docx" {
		t.Errorf("failed %v", failed)
	}
	reasons := map[string]string{}
	for _, u := range unverified {
		reasons[u["path"].(string)] = u["reason"].(string)
	}
	want := map[string]string{
		"/workspace/outputs/crashed.docx":   "its checker could not run (timeout)",
		"/workspace/outputs/pending.xlsx":   "its checker has not run yet",
		"/workspace/outputs/unchecked.pptx": "no checker ran after its last change",
	}
	if fmt.Sprint(reasons) != fmt.Sprint(want) {
		t.Errorf("unverified %v\nwant %v", reasons, want)
	}
}

func TestVerificationListsRequireAViewOfDocumentsForVisionModels(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx"}}
	st := stateWithChecker(chk, true)
	v := &st.v3.verify
	v.changed(docx, store.ChangeCreated)
	v.result(CheckResult{Path: docx, OK: true}, true)

	// Act
	_, _, notViewed := st.verificationLists()
	v.viewed(docx, v.next())
	verified, _, _ := st.verificationLists()
	v.changed(docx, store.ChangeUpdated) // a rewrite makes the view stale
	v.result(CheckResult{Path: docx, OK: true}, true)
	_, _, stale := st.verificationLists()

	// Assert
	if len(notViewed) != 1 || !strings.Contains(notViewed[0]["reason"].(string), "rendered pages were not viewed") {
		t.Errorf("not viewed: %v", notViewed)
	}
	if fmt.Sprint(verified) != "["+docx+"]" {
		t.Errorf("verified %v", verified)
	}
	if len(stale) != 1 {
		t.Errorf("stale view accepted: %v", stale)
	}
}

func TestAFailedRenderIsReportedWithItsReasonAndDoesNotBlockTheStop(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx"}}
	st := stateWithChecker(chk, true)
	v := &st.v3.verify
	v.changed(docx, store.ChangeCreated)
	v.result(CheckResult{Path: docx, OK: true}, true)

	// Act
	note := st.noteViewFailed(`{"path":"outputs/r.docx"}`, "Error: libreoffice\n  crashed")
	_, unverified := st.verification()
	reminder := st.verificationReminder()

	// Assert
	if note != ViewFailedNote {
		t.Errorf("note %q", note)
	}
	if len(unverified) != 1 || unverified[0]["reason"] != RenderFailedPrefix+"libreoffice crashed" {
		t.Errorf("unverified %v", unverified)
	}
	if reminder != "" {
		t.Errorf("a file that cannot be rendered held the run back: %q", reminder)
	}
}

func TestNoteViewFailedIgnoresFilesThatAreNotDeliverablesOfTheTurn(t *testing.T) {
	st := newState(5)
	st.v3.verify.file(docx) // known but not changed
	for _, args := range []string{`{"path":"/workspace/data/in.docx"}`, `{"path":"` + docx + `"}`, `not json`} {
		if note := st.noteViewFailed(args, "Error: x"); note != "" {
			t.Errorf("%s: note %q", args, note)
		}
	}
}

func TestVerificationReminderComesOnlyOnce(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx"}, commands: map[string]string{docx: "python3 inspect_docx.py " + docx}}
	st := stateWithChecker(chk, false)
	st.v3.verify.changed(docx, store.ChangeCreated)

	// Act
	first, second := st.verificationReminder(), st.verificationReminder()

	// Assert
	if !strings.HasPrefix(first, "Before you finish: these deliverables changed in this task are not verified yet:") ||
		!strings.Contains(first, "- "+docx+": no checker ran after its last change (check it with: python3 inspect_docx.py "+docx+")") {
		t.Fatalf("first %q", first)
	}
	if second != "" {
		t.Fatalf("second %q", second)
	}
}

func TestFailedChecksAreListedFirstInTheUnverifiedSummary(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx", ".xlsx": "x"}}
	st := stateWithChecker(chk, false)
	v := &st.v3.verify
	v.changed("/workspace/outputs/a.xlsx", store.ChangeCreated)
	v.changed("/workspace/outputs/z.docx", store.ChangeCreated)
	v.result(CheckResult{Path: "/workspace/outputs/z.docx", Summary: "no title"}, true)

	// Act
	_, unverified := st.verification()

	// Assert
	if len(unverified) != 2 || unverified[0]["path"] != "/workspace/outputs/z.docx" || unverified[0]["reason"] != "its checker failed (no title)" {
		t.Fatalf("%v", unverified)
	}
}

func TestAnswerMetadataSummarisesTheVerificationOnlyOnAnswersOfRunsThatChangedDeliverables(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".xlsx": "x"}}
	st := stateWithChecker(chk, false)
	answer := ai.AssistantMessage("done", nil)
	withCalls := ai.AssistantMessage("", []ai.ToolCall{call("t", "{}")})

	// Assert: nothing changed
	if st.answerMetadata(answer) != nil || st.answerMetadata(nil) != nil {
		t.Error("metadata without deliverables")
	}

	// Act
	st.v3.verify.changed("/workspace/outputs/a.xlsx", store.ChangeCreated)
	st.v3.verify.result(CheckResult{Path: "/workspace/outputs/a.xlsx", OK: true}, true)
	md := st.answerMetadata(answer)

	// Assert
	v, _ := md["verification"].(map[string]any)
	if fmt.Sprint(v["verified"]) != "[/workspace/outputs/a.xlsx]" || len(v["failed"].([]any)) != 0 || len(v["unverified"].([]any)) != 0 {
		t.Fatalf("metadata %v", md)
	}
	if st.answerMetadata(withCalls) != nil {
		t.Error("an intermediate step carries a verification summary")
	}
}

func TestEmitAnswerVerificationAnnouncesOnlyWhenSomethingWasCheckable(t *testing.T) {
	// Arrange
	st := newState(5)
	w := &recordingWriter{}
	st.w = w
	empty := &store.Message{ID: "m1", Metadata: map[string]any{"verification": map[string]any{"verified": []any{}, "failed": []any{}, "unverified": []any{}}}}
	full := &store.Message{ID: "m2", Metadata: map[string]any{"verification": map[string]any{"verified": []any{"/a"}, "failed": []any{}, "unverified": []any{}}}}

	// Act
	for _, row := range []*store.Message{nil, {ID: "m0"}, empty, full} {
		if err := st.emitAnswerVerification(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}

	// Assert
	if len(w.events) != 1 || w.events[0]["name"] != CustomVerification {
		t.Fatalf("events %v", w.events)
	}
	if v := w.events[0]["value"].(map[string]any); v["messageId"] != "m2" || fmt.Sprint(v["verified"]) != "[/a]" {
		t.Fatalf("value %v", v)
	}
}

func TestWithVerificationAddsAFieldToAJSONResultAndLinesToText(t *testing.T) {
	// Arrange
	results := []CheckResult{{Path: docx, Checker: "inspect_docx", OK: false, Summary: "inspect_docx: NOT OK — title", NextStep: strings.Repeat("n", 500)}}
	pending := []any{"/workspace/outputs/z.pptx"}

	// Act
	asJSON := withVerification(`{"exitCode":0}`, results, pending)
	asText := withVerification("wrote 3 files", results, pending)
	untouched := withVerification("same", nil, nil)

	// Assert
	if !strings.HasPrefix(asJSON, `{"exitCode":0,"verification":[{`) || !strings.Contains(asJSON, `"paths":["/workspace/outputs/z.pptx"]`) || !strings.Contains(asJSON, `"ok":false`) {
		t.Errorf("json: %s", asJSON)
	}
	if strings.Contains(asJSON, strings.Repeat("n", 401)) {
		t.Error("next step not truncated")
	}
	for _, want := range []string{"wrote 3 files\n\n[verification] " + docx + " — inspect_docx: NOT OK — title\nNext step: nnn", "[verification] /workspace/outputs/z.pptx — not checked yet (more than 3 files changed in one call)"} {
		if !strings.Contains(asText, want) {
			t.Errorf("text missing %q:\n%s", want, asText)
		}
	}
	if untouched != "same" {
		t.Errorf("untouched %q", untouched)
	}
}

func TestWithVerificationFallsBackToTextWhenTheJSONAlreadyHasTheField(t *testing.T) {
	// Act
	got := withVerification(`{"verification":"mine"}`, []CheckResult{{Path: docx, OK: true, Summary: "ok"}}, nil)

	// Assert
	if !strings.HasPrefix(got, `{"verification":"mine"}`) || !strings.HasSuffix(got, "[verification] "+docx+" — ok") {
		t.Fatalf("%s", got)
	}
}

func TestVerifyAfterToolChecksChangedDeliverablesAndAppendsTheSummary(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx", ".xlsx": "inspect_xlsx"}}
	st := stateWithChecker(chk, false)
	em := &toolEmitter{st: st}
	md := map[string]any{"artifacts": []any{
		map[string]any{"path": docx, "change": store.ChangeCreated},
		map[string]any{"path": "/workspace/outputs/notes.md", "change": store.ChangeCreated}, // no checker
	}}

	// Act
	content, meta := st.verifyAfterTool(context.Background(), call("write_file", "{}"), `{"ok":true}`, false, md, em)

	// Assert
	if !strings.Contains(content, `"verification":[{"checker":"inspect_docx"`) {
		t.Errorf("content %s", content)
	}
	items, _ := meta["verification"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["failed"] != false || fmt.Sprint(chk.checked) != "["+docx+"]" {
		t.Errorf("meta %v checked %v", meta, chk.checked)
	}
	if len(em.after) != 1 || em.after[0]["name"] != CustomVerification {
		t.Errorf("events %v", em.after)
	}
	if checked, ok := st.v3.verify.files[docx].verdict(); !checked || !ok {
		t.Error("the result was not recorded")
	}
}

func TestVerifyAfterToolChecksAtMostThreeFilesAndDefersTheRestToTheNextStep(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".xlsx": "x"}}
	st := stateWithChecker(chk, false)
	var arts []any
	for i := range 5 {
		arts = append(arts, map[string]any{"path": fmt.Sprintf("/workspace/outputs/f%d.xlsx", i), "change": store.ChangeCreated})
	}
	call1 := ai.ToolCall{ID: "c9", Function: ai.FunctionCall{Name: "execute_command", Arguments: "{}"}}

	// Act
	content, meta := st.verifyAfterTool(context.Background(), call1, "built 5 files", false, map[string]any{"artifacts": arts}, &toolEmitter{st: st})

	// Assert
	if len(chk.checked) != 3 {
		t.Fatalf("checked %v", chk.checked)
	}
	if got := fmt.Sprint(meta["verificationPending"]); got != "[/workspace/outputs/f3.xlsx /workspace/outputs/f4.xlsx]" {
		t.Errorf("pending %s", got)
	}
	if !strings.Contains(content, "not checked yet (more than 3 files changed in one call)") {
		t.Errorf("content %s", content)
	}

	// Act: the next step checks them before the model call and stores the results on the origin call.
	w := &recordingWriter{}
	st.w = w
	if err := st.checkPending(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Assert
	if len(chk.checked) != 5 || len(w.events) != 2 || w.events[0]["value"].(map[string]any)["toolCallId"] != "c9" {
		t.Fatalf("checked %v events %v", chk.checked, w.events)
	}
	reminder := userMessages(st.history)
	if len(reminder) != 1 || !strings.Contains(reminder[0], "Checks of files changed earlier") || !strings.Contains(reminder[0], "f3.xlsx") {
		t.Fatalf("reminder %v", reminder)
	}
	if err := st.checkPending(context.Background()); err != nil || len(chk.checked) != 5 {
		t.Fatalf("pending checks ran twice: %v %v", err, chk.checked)
	}
}

func TestCheckPendingChecksAtMostThreePerStepAndSaysHowManyAreLeft(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".xlsx": "x"}}
	st := stateWithChecker(chk, false)
	st.w = &recordingWriter{}
	for i := range 5 {
		d := st.v3.verify.file(fmt.Sprintf("/workspace/outputs/f%d.xlsx", i))
		d.changed, d.pending, d.origin = 1, true, "c1"
	}

	// Act
	if err := st.checkPending(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Assert
	if len(chk.checked) != 3 {
		t.Fatalf("checked %v", chk.checked)
	}
	if msg := userMessages(st.history)[0]; !strings.Contains(msg, "2 more files are checked before your next step.") {
		t.Fatalf("reminder %q", msg)
	}
}

func TestCheckPendingStopsOnEventLogFailureAndIsInertWithoutAChecker(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".xlsx": "x"}}
	st := stateWithChecker(chk, false)
	st.w = &recordingWriter{failOn: agui.Custom}
	d := st.v3.verify.file("/workspace/outputs/a.xlsx")
	d.changed, d.pending = 1, true

	// Act
	err := st.checkPending(context.Background())

	// Assert
	if err == nil {
		t.Fatal("event log failure swallowed")
	}
	st.a.Harness.Checker = nil
	if err := st.checkPending(context.Background()); err != nil {
		t.Fatalf("no checker: %v", err)
	}
}

type completer struct {
	fakeChecker
	completed CheckResult
	panics    bool
}

func (c *completer) Complete(_ context.Context, _ *store.Run, path string, base CheckResult) CheckResult {
	if c.panics {
		panic("boom")
	}
	c.completed = base
	return CheckResult{Path: "ignored", Checker: base.Checker, OK: false, Summary: "page citations missing"}
}

func TestVerifyAfterToolTreatsACheckerCommandRunLastAsTheFilesOwnCheck(t *testing.T) {
	// Arrange: the model ran the file's own checker as its command's last step, and the runtime completes it.
	cmd := "python3 /skills/docx/inspect_docx.py " + docx
	chk := &completer{fakeChecker: fakeChecker{names: map[string]string{".docx": "inspect_docx"}, commands: map[string]string{docx: cmd}}}
	st := stateWithChecker(&chk.fakeChecker, false)
	st.a.Harness.Checker = chk
	st.v3.verify.file(docx).changed = 1
	tc := execCall("python3 build.py && " + cmd)
	out := execResult(contract(true, docx, [3]any{"fonts", true, ""}), 0)

	// Act
	content, meta := st.verifyAfterTool(context.Background(), tc, out, false, nil, &toolEmitter{st: st})

	// Assert
	items := meta["verification"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["modelRun"] != nil {
		t.Fatalf("meta %v", meta)
	}
	if chk.completed.Path != docx || len(chk.checked) != 0 {
		t.Errorf("completed %+v, runtime re-checked %v", chk.completed, chk.checked)
	}
	if !strings.Contains(content, "page citations missing") {
		t.Errorf("content %s", content)
	}
	if checked, ok := st.v3.verify.files[docx].verdict(); !checked || ok {
		t.Errorf("verdict %v %v", checked, ok)
	}
}

func TestVerifyAfterToolKeepsAModelRunCheckerResultMarkedAsSuchWhenNotTheLastStep(t *testing.T) {
	// Arrange
	cmd := "python3 /skills/docx/inspect_docx.py " + docx
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx"}, commands: map[string]string{docx: cmd}}
	st := stateWithChecker(chk, false)
	st.v3.verify.file(docx).changed = 1
	tc := execCall(cmd + " && python3 patch.py")
	out := execResult(contract(true, docx, [3]any{"fonts", true, ""}), 0)

	// Act
	_, meta := st.verifyAfterTool(context.Background(), tc, out, false, nil, &toolEmitter{st: st})

	// Assert
	items := meta["verification"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["modelRun"] != true {
		t.Fatalf("meta %v", meta)
	}
}

func TestVerifyAfterToolSurvivesACrashingChecker(t *testing.T) {
	// Arrange
	chk := &fakeChecker{names: map[string]string{".docx": "inspect_docx"}, panics: true}
	st := stateWithChecker(chk, false)
	md := map[string]any{"artifacts": []any{map[string]any{"path": docx, "change": store.ChangeCreated}}}

	// Act
	_, meta := st.verifyAfterTool(context.Background(), call("write_file", "{}"), "ok", false, md, &toolEmitter{st: st})

	// Assert
	r := meta["verification"].([]any)[0].(map[string]any)
	if r["failed"] != true || r["summary"] != "the checker crashed" {
		t.Fatalf("%v", r)
	}
	if checked, _ := st.v3.verify.files[docx].verdict(); checked {
		t.Error("a crashed checker counted as a verdict")
	}
}

func TestSafeCompleteKeepsThePathAndSurvivesAPanic(t *testing.T) {
	// Arrange
	st := newState(5)
	base := CheckResult{Path: docx, Checker: "inspect_docx", OK: true}

	// Act
	got := st.safeComplete(context.Background(), &completer{}, base)
	crashed := st.safeComplete(context.Background(), &completer{panics: true}, base)

	// Assert
	if got.Path != docx || got.Summary != "page citations missing" {
		t.Errorf("got %+v", got)
	}
	if !crashed.Failed || crashed.Path != docx || crashed.Checker != "inspect_docx" {
		t.Errorf("crashed %+v", crashed)
	}
}

func TestVerifyAfterToolRecordsAFailedViewOfADeliverable(t *testing.T) {
	// Arrange
	st := stateWithChecker(&fakeChecker{}, true)
	st.v3.verify.file(docx).changed = 1

	// Act
	content, _ := st.verifyAfterTool(context.Background(), call("view_file", `{"path":"`+docx+`"}`), "Error: cannot render", true, nil, &toolEmitter{st: st})

	// Assert
	if !strings.HasSuffix(content, ViewFailedNote) || st.v3.verify.files[docx].viewFailed == 0 {
		t.Fatalf("content %q", content)
	}
}

func TestASuccessfulViewCountsOnceItsPagesAreSentToTheModel(t *testing.T) {
	// Arrange
	st := stateWithChecker(&fakeChecker{}, true)
	d := st.v3.verify.file(docx)
	d.changed = 1
	pages := mustJSONv(map[string]any{"pages": []any{map[string]any{"imagePath": "/p1.png"}, map[string]any{"imagePath": ""}, map[string]any{"imagePath": "/p2.png"}}})
	st.verifyAfterTool(context.Background(), call("view_file", `{"path":"`+docx+`"}`), pages, false, nil, &toolEmitter{st: st})
	if d.viewed != 0 || len(st.v3.verify.views) != 1 {
		t.Fatalf("viewed before the images were sent: %d", d.viewed)
	}
	history := []*ai.Message{{Role: ai.RoleUser, Parts: []ai.Part{
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "AAAA"}, Extra: map[string]any{imagePathKey: "/p1.png"}},
		{Type: ai.PartImage, Image: &ai.Image{Base64Data: "AAAA"}, Extra: map[string]any{imagePathKey: "/p2.png"}},
	}}}
	st.history = history

	// Act
	st.imagesSent(history)

	// Assert
	if d.viewed == 0 {
		t.Fatal("the view did not count after all pages were sent")
	}
}
