package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

func call(name, args string) ai.ToolCall {
	return ai.ToolCall{ID: "id-" + name, Type: "function", Function: ai.FunctionCall{Name: name, Arguments: args}}
}

// newState is a runState for the decision functions: no store, no event log.
func newState(maxSteps int) *runState {
	return &runState{a: &Agent{MaxSteps: maxSteps}, in: Input{Run: &store.Run{ID: "run-1", ThreadID: "th-1", Model: "m"}}, usage: agui.Usage{Model: "m"}}
}

func TestCallSignatureIgnoresKeyOrderAndSpacingOfTheArguments(t *testing.T) {
	// Arrange
	a := call("search", `{"q": "x",  "limit": 10, "opts": {"b":1,"a":2}}`)
	b := call("search", "{\"opts\":{\"a\":2,\"b\":1},\"limit\":10,\"q\":\"x\"}\n")
	other := call("search", `{"q":"x","limit":11,"opts":{"a":2,"b":1}}`)

	// Assert
	if callSignature(a) != callSignature(b) {
		t.Errorf("same call, different signature:\n%q\n%q", callSignature(a), callSignature(b))
	}
	if callSignature(a) == callSignature(other) {
		t.Error("different arguments share a signature")
	}
	if callSignature(call("x", `{"a":1}`)) == callSignature(call("y", `{"a":1}`)) {
		t.Error("different tools share a signature")
	}
}

func TestCallSignatureKeepsLargeNumbersExactAndTreatsGarbageAsText(t *testing.T) {
	// Arrange
	big1, big2 := call("t", `{"n":12345678901234567890}`), call("t", `{"n":12345678901234567891}`)

	// Assert
	if callSignature(big1) == callSignature(big2) {
		t.Error("numbers beyond float64 precision were merged")
	}
	if got := callSignature(call("t", "  not json ")); got != "t\x00not json" {
		t.Errorf("garbage signature %q", got)
	}
}

func TestLoopingRefusesTheThirdIdenticalCallInARow(t *testing.T) {
	// Arrange
	g := &guardState{}
	same := call("read_file", `{"path":"/a"}`)

	// Act
	first, second, third := g.looping(same), g.looping(same), g.looping(same)

	// Assert
	if first != "" || second != "" {
		t.Fatalf("early refusal: %q %q", first, second)
	}
	if third != fmt.Sprintf(LoopGuardResult, "read_file") {
		t.Fatalf("third call: %q", third)
	}
	if len(g.recent) != 2 {
		t.Errorf("a refused call was recorded: %d recent", len(g.recent))
	}
}

func TestLoopingLetsADifferentCallBreakTheStreak(t *testing.T) {
	// Arrange
	g := &guardState{}
	a, b := call("read_file", `{"path":"/a"}`), call("read_file", `{"path":"/b"}`)

	// Act
	var refusals []string
	for _, c := range []ai.ToolCall{a, a, b, a, a, b, a} {
		refusals = append(refusals, g.looping(c))
	}

	// Assert
	for i, r := range refusals {
		if r != "" {
			t.Errorf("call %d refused: %q", i, r)
		}
	}
}

func TestLoopingRefusesTheSixthCallOfAnABABABAlternation(t *testing.T) {
	// Arrange
	g := &guardState{}
	a, b := call("read_file", `{"path":"/a"}`), call("list_files", `{"path":"/"}`)

	// Act
	var got []string
	for _, c := range []ai.ToolCall{a, b, a, b, a, b} {
		got = append(got, g.looping(c))
	}

	// Assert
	for i, r := range got[:5] {
		if r != "" {
			t.Fatalf("call %d refused early: %q", i, r)
		}
	}
	if want := fmt.Sprintf(AlternationGuardResult, "read_file", "list_files"); got[5] != want {
		t.Fatalf("sixth call: %q, want %q", got[5], want)
	}
}

func TestLoopingNeverRefusesPollingARunningJob(t *testing.T) {
	// Arrange
	g := &guardState{}
	poll := call("job_output", `{"jobId":"j1"}`)
	g.observed(poll, `{"status":"running"}`, false)

	// Act
	var refusals []string
	for range 6 {
		refusals = append(refusals, g.looping(poll))
	}
	g.observed(poll, `{"status":"done"}`, false) // finished: polling is a plain call again
	for range 3 {
		refusals = append(refusals, g.looping(poll))
	}

	// Assert
	for i, r := range refusals[:6] {
		if r != "" {
			t.Errorf("poll %d refused: %q", i, r)
		}
	}
	if refusals[8] == "" {
		t.Error("polling a finished job was never refused")
	}
}

func TestPeekLoopDoesNotRecordTheCall(t *testing.T) {
	// Arrange
	g := &guardState{}
	c := call("read_file", `{"path":"/a"}`)

	// Act
	for range 5 {
		_ = g.peekLoop(c)
	}

	// Assert
	if len(g.recent) != 0 {
		t.Fatalf("peekLoop recorded %d calls", len(g.recent))
	}
}

func TestRedirectedForgetsTheStreak(t *testing.T) {
	// Arrange
	g := &guardState{}
	c := call("read_file", `{"path":"/a"}`)
	g.looping(c)
	g.looping(c)

	// Act
	g.redirected()

	// Assert
	if r := g.looping(c); r != "" {
		t.Fatalf("refused after the model got new input: %q", r)
	}
}

func TestErrorKeyIgnoresNumbersSpacingAndThePrefix(t *testing.T) {
	// Arrange
	a := "Error: file /tmp/x-123.txt   not found (line 42)"
	b := "file /tmp/x-9.txt not found (line 7)"

	// Assert
	if errorKey(a) != errorKey(b) {
		t.Errorf("%q != %q", errorKey(a), errorKey(b))
	}
	if errorKey("Error: boom") == errorKey("Error: bang") {
		t.Error("different errors share a key")
	}
	if n := len(errorKey(strings.Repeat("x", 1000))); n != 300 {
		t.Errorf("key length %d", n)
	}
}

func TestObservedNotesTheThirdOccurrenceOfTheSameError(t *testing.T) {
	// Arrange
	g := &guardState{}
	c := call("execute_command", `{"command":"make"}`)

	// Act
	notes := []string{
		g.observed(c, "Error: build failed at step 1", true),
		g.observed(c, "Error: build failed at step 2", true),
		g.observed(c, "Error: build failed at step 3", true),
		g.observed(c, "Error: build failed at step 4", true),
	}

	// Assert
	if notes[0] != "" || notes[1] != "" || notes[3] != "" {
		t.Fatalf("unexpected notes: %q", notes)
	}
	if !strings.Contains(notes[2], "3rd time this error occurred") || !strings.Contains(notes[2], "build failed at step 3") {
		t.Fatalf("third occurrence note: %q", notes[2])
	}
}

func TestObservedEscalatesConsecutiveFailuresAndForcesAnAnswer(t *testing.T) {
	// Arrange
	g := &guardState{}
	c := call("execute_command", `{}`)
	var notes []string

	// Act
	for i := 1; i <= 8; i++ {
		notes = append(notes, g.observed(c, fmt.Sprintf("Error: failure kind %c", 'a'+i), true))
		if i < 8 && g.forceAnswer {
			t.Fatalf("forced after only %d failures", i)
		}
	}

	// Assert
	for i, n := range notes {
		if noted := strings.Contains(n, "5 tool calls in a row have failed"); noted != (i == 4) {
			t.Errorf("failure %d: note %q", i+1, n)
		}
	}
	if !g.forceAnswer {
		t.Error("8 failures in a row did not force an answer")
	}
}

func TestObservedSuccessResetsTheFailureStreak(t *testing.T) {
	// Arrange
	g := &guardState{}
	c := call("execute_command", `{}`)
	for i := range 4 {
		g.observed(c, fmt.Sprintf("Error: kind %c", 'a'+i), true)
	}

	// Act
	g.observed(call("read_file", `{}`), "fine", false)
	var notes []string
	for i := range 4 {
		notes = append(notes, g.observed(c, fmt.Sprintf("Error: other %c", 'a'+i), true))
	}

	// Assert
	if g.failedInRow != 4 || g.forceAnswer {
		t.Fatalf("failedInRow %d forced %v", g.failedInRow, g.forceAnswer)
	}
	for _, n := range notes {
		if strings.Contains(n, "in a row") {
			t.Errorf("streak survived a success: %q", n)
		}
	}
}

func TestObservedNotesAnIdenticalResultOnItsThirdOccurrence(t *testing.T) {
	// Arrange
	g := &guardState{}
	c := call("read_file", `{"path":"/a"}`)

	// Act
	var notes []string
	for range 4 {
		notes = append(notes, g.observed(c, "same text", false))
	}
	other := g.observed(c, "different text", false)

	// Assert
	if notes[0] != "" || notes[1] != "" || notes[3] != "" || other != "" {
		t.Fatalf("unexpected notes %q %q", notes, other)
	}
	if !strings.Contains(notes[2], "ran this exact call 3 times") {
		t.Fatalf("third result: %q", notes[2])
	}
}

func TestObservedRemembersTheLastPlan(t *testing.T) {
	// Arrange
	g := &guardState{}

	// Act
	g.observed(call("update_plan", `{"items":[{"id":"1","title":"A","status":"completed"},{"id":"2","title":"B","status":"pending"}]}`), "ok", false)
	g.observed(call("update_plan", `not json`), "ok", false)
	g.observed(call("update_plan", `{"other":1}`), "ok", false)

	// Assert
	if len(g.plan) != 2 || g.plan[1].Title != "B" {
		t.Fatalf("plan %+v", g.plan)
	}
}

func TestNotePollTracksWhetherTheJobIsStillRunning(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		failed  bool
		polling bool
	}{
		"running":     {`{"status":"running"}`, false, true},
		"finished":    {`{"status":"exited"}`, false, false},
		"failed call": {`{"status":"running"}`, true, false},
		"not json":    {`still going`, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			g := &guardState{polling: map[string]bool{"sig": true}}

			// Act
			g.notePoll("sig", tc.content, tc.failed)

			// Assert
			if g.polling["sig"] != tc.polling {
				t.Fatalf("polling = %v", g.polling["sig"])
			}
		})
	}
}

func TestNoteFileIntentRecognisesTheOutputsDirectoryOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		args string
		want bool
	}{
		"absolute in text":  {"I'll save it to /workspace/outputs/report.docx", "", true},
		"relative in text":  {"writing outputs/report.docx now", "", true},
		"start of text":     {"outputs/a.txt", "", true},
		"in call arguments": {"", `{"command":"mkdir -p /workspace/outputs && cd /workspace/outputs"}`, true},
		"other directory":   {"I'll save it to /workspace/.work/notes.md", `{"path":"/tmp/outputs/x"}`, false},
		"prefixed name":     {"see my_outputs/x and layouts/outputs/y", "", false},
		"nothing":           {"hello", `{}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			g := &guardState{}

			// Act
			g.noteFileIntent(tc.text, []ai.ToolCall{call("t", tc.args)})

			// Assert
			if g.asksFile != tc.want {
				t.Fatalf("asksFile = %v", g.asksFile)
			}
		})
	}
}

func TestNoteFileIntentIsStickyOnceSet(t *testing.T) {
	// Arrange
	g := &guardState{}
	g.noteFileIntent("write /workspace/outputs/a.md", nil)

	// Act
	g.noteFileIntent("just talking", nil)

	// Assert
	if !g.asksFile {
		t.Fatal("a later message cleared the intent")
	}
}

func TestNoteInPlaceOnlyCountsEditsOfExistingWorkspaceFiles(t *testing.T) {
	artifact := func(path, change string) map[string]any {
		return map[string]any{"artifacts": []any{map[string]any{"path": path, "change": change}}}
	}
	for name, tc := range map[string]struct {
		md   map[string]any
		want bool
	}{
		"updated upload":      {artifact("/workspace/data/in.xlsx", store.ChangeUpdated), true},
		"created":             {artifact("/workspace/data/in.xlsx", store.ChangeCreated), false},
		"deleted":             {artifact("/workspace/data/in.xlsx", store.ChangeDeleted), false},
		"deliverable updated": {artifact("/workspace/outputs/r.docx", store.ChangeUpdated), false},
		"scratch updated":     {artifact("/workspace/.work/n.md", store.ChangeUpdated), false},
		"no artifacts":        {map[string]any{}, false},
		"malformed":           {map[string]any{"artifacts": "x"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			g := &guardState{}

			// Act
			g.noteInPlace(tc.md)

			// Assert
			if g.inPlace != tc.want {
				t.Fatalf("inPlace = %v", g.inPlace)
			}
		})
	}
}

func TestMissingFileNeedsASandboxToolCallsAnIntentAndNoProducedFile(t *testing.T) {
	// A question that never asked for a file is not an incomplete run; neither is a run that wrote one or edited an upload in place.
	base := func() *runState {
		st := newState(10)
		st.in.Sandbox, st.g.called, st.g.asksFile = true, 2, true
		return st
	}
	for name, tc := range map[string]struct {
		mutate func(*runState)
		want   bool
	}{
		"nothing produced":      {func(*runState) {}, true},
		"no sandbox":            {func(st *runState) { st.in.Sandbox = false }, false},
		"no calls":              {func(st *runState) { st.g.called = 0 }, false},
		"no intent":             {func(st *runState) { st.g.asksFile = false }, false},
		"deliverable written":   {func(st *runState) { st.v3.verify.file("/workspace/outputs/a.md").changed = 1 }, false},
		"edited in place":       {func(st *runState) { st.g.inPlace = true }, false},
		"deliverable unchanged": {func(st *runState) { st.v3.verify.file("/workspace/outputs/a.md") }, true},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			st := base()
			tc.mutate(st)

			// Assert
			if got := st.missingFile(); got != tc.want {
				t.Fatalf("missingFile = %v", got)
			}
		})
	}
}

func TestLateStepsIsTenPercentOfTheBudgetButAtLeastThree(t *testing.T) {
	for steps, want := range map[int]int{10: 3, 30: 3, 40: 4, 100: 10} {
		if got := newState(steps).lateSteps(); got != want {
			t.Errorf("lateSteps(%d) = %d, want %d", steps, got, want)
		}
	}
}

func userMessages(history []*ai.Message) []string {
	var out []string
	for _, m := range history {
		if m.Role == ai.RoleUser {
			out = append(out, m.Content)
		}
	}
	return out
}

func TestBudgetReminderPushesForAPartialDeliverableOnceBeforeTheLimit(t *testing.T) {
	// Arrange
	st := newState(20)
	st.in.Sandbox = true

	// Act
	var pushedAt []int
	for step := 1; step <= 20; step++ {
		before := len(st.history)
		st.budgetReminder(step)
		for _, m := range st.history[before:] {
			if strings.Contains(m.Content, "no deliverable has been written yet") {
				pushedAt = append(pushedAt, step)
				if !strings.Contains(m.Content, "Only 3 steps left") {
					t.Errorf("push text: %q", m.Content)
				}
			}
		}
	}

	// Assert
	if len(pushedAt) != 1 || pushedAt[0] != 18 {
		t.Fatalf("pushed at steps %v, want [18]", pushedAt)
	}
}

func TestBudgetReminderStaysQuietWhenThePushWouldBeWrong(t *testing.T) {
	for name, mutate := range map[string]func(*runState){
		"small budget":    func(st *runState) { st.a.MaxSteps = 9 },
		"no sandbox":      func(st *runState) { st.in.Sandbox = false },
		"already pushed":  func(st *runState) { st.g.pushed = true },
		"file written":    func(st *runState) { st.v3.verify.file("/workspace/outputs/a.md").changed = 1 },
		"edited in place": func(st *runState) { st.g.inPlace = true },
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			st := newState(20)
			st.in.Sandbox = true
			mutate(st)
			step := st.a.MaxSteps - st.lateSteps() + 1

			// Act
			st.budgetReminder(step)

			// Assert
			for _, m := range userMessages(st.history) {
				if strings.Contains(m, "no deliverable has been written yet") {
					t.Fatalf("pushed: %q", m)
				}
			}
		})
	}
}

func TestBudgetReminderReportsStepsPlanAndUnverifiedDeliverablesEveryEighthStep(t *testing.T) {
	// Arrange
	st := newState(40)
	st.g.plan = []store.PlanItem{{Title: "Draft", Status: store.PlanCompleted}, {Title: "Review", Status: "in_progress"}}
	st.a.Harness.Checker = &fakeChecker{names: map[string]string{".docx": "inspect_docx"}}
	st.v3.verify.file("/workspace/outputs/r.docx").changed = 1

	// Act
	var steps []int
	var text string
	for step := 1; step <= 18; step++ {
		before := len(st.history)
		st.budgetReminder(step)
		if len(st.history) > before {
			steps = append(steps, step)
			text = st.history[len(st.history)-1].Content
		}
	}

	// Assert
	if fmt.Sprint(steps) != "[9 17]" {
		t.Fatalf("reminders at steps %v", steps)
	}
	for _, want := range []string{"Step 17 of 40 (24 left).", `Plan: 1 of 2 items open: "Review" (in_progress).`, "Deliverables not verified yet: /workspace/outputs/r.docx."} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %q", want, text)
		}
	}
	if !strings.HasPrefix(text, "<system-reminder>\n") || !strings.HasSuffix(text, "\n</system-reminder>") {
		t.Errorf("not a system reminder: %q", text)
	}
}

func TestBudgetReminderNeedsAStepLimit(t *testing.T) {
	// Arrange
	st := newState(0)

	// Act
	st.budgetReminder(9)

	// Assert
	if len(st.history) != 0 {
		t.Fatalf("history %+v", st.history)
	}
}

type guardTool struct {
	reminder string
	panics   bool
	calls    int
}

func (g *guardTool) Info(context.Context) (*ai.ToolInfo, error) {
	return &ai.ToolInfo{Name: "guard"}, nil
}

func (g *guardTool) StopReminder(context.Context) string {
	g.calls++
	if g.panics {
		panic("boom")
	}
	return g.reminder
}

func TestStopGuardsAreListedInToolNameOrder(t *testing.T) {
	// Arrange
	b, a := &guardTool{reminder: "b"}, &guardTool{reminder: "a"}
	plain := &fakeChecker{}
	_ = plain
	toolMap := map[string]ai.BaseTool{"b_tool": b, "a_tool": a, "plain": plainTool{}}

	// Act
	got := stopGuards(toolMap)

	// Assert
	if len(got) != 2 || got[0].StopReminder(context.Background()) != "a" || got[1].StopReminder(context.Background()) != "b" {
		t.Fatalf("guards %v", got)
	}
}

type plainTool struct{}

func (plainTool) Info(context.Context) (*ai.ToolInfo, error) { return &ai.ToolInfo{Name: "plain"}, nil }

func TestStopReminderCollectsGuardsAndGivesAtMostTwo(t *testing.T) {
	// Arrange
	st := newState(10)
	st.g.guards = []tools.StopGuard{&guardTool{reminder: "finish the plan"}, &guardTool{reminder: "  "}, &guardTool{panics: true}, &guardTool{reminder: "answer the question"}}

	// Act
	first, second, third := st.stopReminder(context.Background()), st.stopReminder(context.Background()), st.stopReminder(context.Background())

	// Assert
	want := "<system-reminder>\nfinish the plan\n\nanswer the question\n</system-reminder>"
	if first != want || second != want {
		t.Fatalf("reminders %q / %q", first, second)
	}
	if third != "" {
		t.Fatalf("a third reminder was given: %q", third)
	}
}

func TestStopReminderIsEmptyWhenNothingIsUnfinished(t *testing.T) {
	// Arrange
	st := newState(10)
	st.g.guards = []tools.StopGuard{&guardTool{}}

	// Act
	got := st.stopReminder(context.Background())

	// Assert
	if got != "" || st.g.reminders != 0 {
		t.Fatalf("reminder %q (count %d)", got, st.g.reminders)
	}
}

func TestVerificationReminderComesOnceAndNamesTheUnverifiedFiles(t *testing.T) {
	// Arrange
	st := newState(10)
	st.a.Harness.Checker = &fakeChecker{names: map[string]string{".docx": "inspect_docx"}, commands: map[string]string{"/workspace/outputs/r.docx": "python3 inspect_docx.py /workspace/outputs/r.docx"}}
	st.v3.verify.file("/workspace/outputs/r.docx").changed = 1

	// Act
	first, second := st.stopReminder(context.Background()), st.stopReminder(context.Background())

	// Assert
	if !strings.Contains(first, "/workspace/outputs/r.docx: no checker ran after its last change (check it with: python3 inspect_docx.py /workspace/outputs/r.docx)") {
		t.Fatalf("first reminder %q", first)
	}
	if second != "" {
		t.Fatalf("the verification reminder repeated: %q", second)
	}
}

func TestContinueAfterAnswerNudgesAnEmptyAnswerOnce(t *testing.T) {
	// Arrange
	st := newState(10)

	// Act
	first := st.continueAfterAnswer(context.Background(), "  ")
	second := st.continueAfterAnswer(context.Background(), "")

	// Assert
	if first != EmptyAnswerNudge {
		t.Fatalf("first: %q", first)
	}
	if second != "" {
		t.Fatalf("second empty answer was nudged again: %q", second)
	}
}

func TestContinueAfterAnswerEndsTheRunWhenCancelledInterruptedOrForced(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		ctx    context.Context
		mutate func(*runState)
	}{
		"cancelled":   {cancelled, func(*runState) {}},
		"interrupted": {context.Background(), func(st *runState) { st.h.interrupt = &tools.Interrupt{} }},
		"forced":      {context.Background(), func(st *runState) { st.g.forceAnswer = true }},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			st := newState(10)
			st.g.guards = []tools.StopGuard{&guardTool{reminder: "not done"}}
			tc.mutate(st)

			// Act
			note := st.continueAfterAnswer(tc.ctx, "an answer")

			// Assert
			if note != "" {
				t.Fatalf("note %q", note)
			}
		})
	}
}

type answerChecker struct {
	note  string
	calls int
	panic bool
}

func (c *answerChecker) CheckAnswer(_ context.Context, _ *store.Run, answer string, emit func(agui.Event) error) string {
	c.calls++
	if c.panic {
		panic("boom")
	}
	_ = emit(agui.CustomEvent("work.test", map[string]any{"answer": answer}))
	return c.note
}

func TestContinueAfterAnswerAsksTheAnswerCheckerOnlyOnceAndAfterTheGuards(t *testing.T) {
	// Arrange
	st := newState(10)
	chk := &answerChecker{note: "fix the dangling link"}
	st.a.Harness.Answers = chk
	w := &recordingWriter{}
	st.w = w
	st.h.answer = "done" // set by the policy before it asks
	st.g.guards = []tools.StopGuard{&guardTool{reminder: "finish the plan"}}

	// Act
	first := st.continueAfterAnswer(context.Background(), "done")  // the guard speaks first
	second := st.continueAfterAnswer(context.Background(), "done") // the guard again (2nd reminder)
	third := st.continueAfterAnswer(context.Background(), "done")  // guards exhausted: the checker
	fourth := st.continueAfterAnswer(context.Background(), "done") // the checker already ran

	// Assert
	if !strings.Contains(first, "finish the plan") || !strings.Contains(second, "finish the plan") {
		t.Fatalf("guard reminders: %q %q", first, second)
	}
	if third != "fix the dangling link" || fourth != "" || chk.calls != 1 {
		t.Fatalf("checker: third %q fourth %q calls %d", third, fourth, chk.calls)
	}
	if len(w.events) != 1 || w.events[0]["value"].(map[string]any)["answer"] != "done" {
		t.Fatalf("checker events %+v", w.events)
	}
}

func TestCheckAnswerTreatsAPanicAsNoNote(t *testing.T) {
	// Arrange
	st := newState(10)
	st.a.Harness.Answers = &answerChecker{panic: true}

	// Act
	note := st.checkAnswer(context.Background())

	// Assert
	if note != "" {
		t.Fatalf("note %q", note)
	}
}

func TestContinueAfterAnswerForgetsTheStreakWhenItRedirectsTheModel(t *testing.T) {
	// Arrange
	st := newState(10)
	st.g.push("sig")

	// Act
	st.continueAfterAnswer(context.Background(), "")

	// Assert
	if len(st.g.recent) != 0 {
		t.Fatalf("recent %v", st.g.recent)
	}
}
