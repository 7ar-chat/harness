package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestRunInjectsSteersBeforeTheNextStepAndStoresThem(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "t", "{}"), textTurn("done"))
	r.withTools(tool("t", "r"))
	r.a.Steers = &scriptedSteers{drains: [][]queue.Steer{nil, {{ID: "s1", Text: "use the blue theme", Author: "bob", AuthorKind: store.AuthorUser}, {Text: "and be brief"}}}}

	// Act
	_, err := r.do("make slides")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	second := r.model.Calls()[1].Messages
	var steers []string
	for _, m := range second {
		if m.Role == ai.RoleUser && strings.HasPrefix(m.Content, SteerPrefix) {
			steers = append(steers, strings.TrimPrefix(m.Content, SteerPrefix))
		}
	}
	if fmt.Sprint(steers) != "[[bob] use the blue theme and be brief]" {
		t.Fatalf("steers in the model input: %q", steers)
	}
	var rows []*store.Message
	for _, m := range r.messages() {
		if m.IsSteer() {
			rows = append(rows, m)
		}
	}
	if len(rows) != 2 || rows[0].ID != "s1" || rows[0].AuthorLogin != "bob" || rows[1].ID == "" || rows[1].Metadata["delivered"] != true {
		t.Fatalf("stored steers %+v", rows)
	}
	ev := r.events(agui.Custom + ":" + agui.CustomSteer)
	if len(ev) != 2 || ev[0]["value"].(map[string]any)["delivered"] != true {
		t.Fatalf("steer events %v", ev)
	}
}

func TestRunGivesASteerThatArrivesDuringTheFinalAnswerItsOwnStep(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("first answer"), textTurn("second answer"))
	steers := &scriptedSteers{finals: [][]queue.Steer{{{ID: "late", Text: "one more thing"}}, nil}}
	r.a.Steers = steers

	// Act
	_, err := r.do("hi")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(r.model.Calls()) != 2 || steers.reopened != 1 {
		t.Fatalf("calls %d reopened %d", len(r.model.Calls()), steers.reopened)
	}
	second := r.model.Calls()[1].Messages
	if m := second[len(second)-1]; m.Content != SteerPrefix+"one more thing" {
		t.Fatalf("last message %q", m.Content)
	}
	if a := r.lastMessage(store.RoleAssistant); a.Content != "second answer" {
		t.Fatalf("answer %q", a.Content)
	}
}

func TestRunClosesTheSteerListOnTheLastStep(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("ok"))
	steers := &scriptedSteers{finals: [][]queue.Steer{{{ID: "s", Text: "last words"}}}}
	r.a.Steers = steers
	r.a.MaxSteps = 1

	// Act
	_, err := r.do("hi")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if steers.finalized == 0 {
		t.Fatal("the steer list was drained, not closed, on the final step")
	}
	msgs := r.model.Calls()[0].Messages
	if m := msgs[len(msgs)-2]; m.Content != SteerPrefix+"last words" {
		t.Fatalf("steer not given to the model: %q", m.Content)
	}
}

func TestRunSurvivesSteerQueueFailures(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("fine"))
	r.a.Steers = &scriptedSteers{drainErr: errors.New("redis down"), finalErr: errors.New("redis down"), reopenErr: errors.New("redis down")}

	// Act
	_, err := r.do("hi")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsRepeatingACallThatKeepsReturningTheSameFailure(t *testing.T) {
	// Arrange: the same call four times; the third and fourth are refused without running.
	same := callTurn("c", "t", `{"a":1}`)
	r := newRunRig(t, same, callTurn("c2", "t", `{"a":1}`), callTurn("c3", "t", `{"a":1}`), callTurn("c4", "t", `{"a":1}`), textTurn("giving up"))
	r.a.MaxSteps = 10
	tl := tool("t", "same result")
	r.withTools(tl)

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if n := len(tl.calls()); n != 2 {
		t.Fatalf("tool ran %d times", n)
	}
	res := r.events(agui.ToolCallResult)
	if len(res) != 4 || res[2]["content"] != fmt.Sprintf(LoopGuardResult, "t") {
		t.Fatalf("results %v", res)
	}
}

func TestRunForcesAnAnswerAfterEightFailedCallsInARow(t *testing.T) {
	// Arrange
	var turns []llmtest.Turn
	for i := range 8 {
		turns = append(turns, callTurn(fmt.Sprintf("c%d", i), "bad", fmt.Sprintf(`{"n":%d}`, i)))
	}
	turns = append(turns, textTurn("I could not do it"))
	r := newRunRig(t, turns...)
	r.a.MaxSteps = 20
	r.withTools(&funcTool{name: "bad", run: func(_ context.Context, args string) (string, error) {
		return "", errors.New("kind " + args[:6] + string(rune('a'+len(args)%7)))
	}})

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	calls := r.model.Calls()
	if len(calls) != 9 {
		t.Fatalf("%d calls", len(calls))
	}
	last := calls[8]
	if last.ToolChoice == nil || *last.ToolChoice != ai.ToolChoiceForbidden || !strings.Contains(last.Messages[len(last.Messages)-1].Content, "Too many tool calls in a row failed (8)") {
		t.Fatalf("last call not forced: %v / %q", last.ToolChoice, last.Messages[len(last.Messages)-1].Content)
	}
	if res := r.events(agui.ToolCallResult); !strings.Contains(res[4]["content"].(string), "5 tool calls in a row have failed") {
		t.Fatalf("5th failure result %q", res[4]["content"])
	}
}

func TestRunReminderOfStepBudgetAndPlanEveryEighthStep(t *testing.T) {
	// Arrange
	var turns []llmtest.Turn
	for i := range 9 {
		turns = append(turns, callTurn(fmt.Sprintf("c%d", i), "t", fmt.Sprintf(`{"n":%d}`, i)))
	}
	turns = append(turns, textTurn("done"))
	r := newRunRig(t, turns...)
	r.a.MaxSteps = 12
	r.withTools(tool("t", "r"), tool("update_plan", "plan saved"))
	r.model = llmtest.New(append([]llmtest.Turn{callTurn("p", "update_plan", `{"items":[{"id":"1","title":"Draft","status":"completed"},{"id":"2","title":"Review","status":"pending"}]}`)}, turns...)...)
	r.a.Models = llmtest.Factory{M: r.model}

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, c := range r.model.Calls() {
		for _, m := range c.Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Content, "Step 9 of 12 (4 left).") && strings.Contains(m.Content, `Plan: 1 of 2 items open: "Review" (pending).`) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no step-9 reminder with the open plan item")
	}
}

type fixedStop struct{ msg string }

func (f fixedStop) Info(context.Context) (*ai.ToolInfo, error) {
	return &ai.ToolInfo{Name: "plan"}, nil
}
func (f fixedStop) InvokableRun(context.Context, string) (string, error) {
	return "", nil
}
func (f fixedStop) StopReminder(context.Context) string { return f.msg }

func TestRunHoldsBackAnAnswerWhileAStopGuardHasNews(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("I'm done"), textTurn("really done"))
	r.withTools(fixedStop{"item 2 of your plan is still open"})

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	calls := r.model.Calls()
	if len(calls) != 3 { // the guard speaks after each of the first two answers
		t.Fatalf("%d calls", len(calls))
	}
	second := calls[1].Messages
	if m := second[len(second)-1]; !strings.Contains(m.Content, "item 2 of your plan is still open") || !strings.HasPrefix(m.Content, "<system-reminder>") {
		t.Fatalf("reminder %q", m.Content)
	}
	for _, m := range r.messages() {
		if strings.Contains(m.Content, "still open") {
			t.Fatal("the reminder was persisted")
		}
	}
}

func TestRunHooksSeeEveryToolResultAndTheirMetadataIsStored(t *testing.T) {
	// Arrange
	r := newRunRig(t, callTurn("c1", "t", "{}"), textTurn("ok"))
	r.withTools(tool("t", "r"))
	hook := &recordHook{md: map[string]any{"artifacts": []any{map[string]any{"path": "/workspace/outputs/a.md", "change": store.ChangeCreated}}}}
	panicky := &recordHook{panics: true}
	r.a.Harness.ToolHooks = []ToolHook{panicky, hook}
	started := &startHook{}
	r.a.Harness.RunStart = []RunStartHook{&startHook{panics: true}, started}

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(hook.seen) != "[c1/t]" || started.runs != 1 {
		t.Fatalf("hook saw %v, start hook ran %d times", hook.seen, started.runs)
	}
	tm := r.lastMessage(store.RoleTool)
	if got := store.ArtifactPaths(tm.Metadata); fmt.Sprint(got) != "[/workspace/outputs/a.md]" {
		t.Fatalf("stored artifacts %v", got)
	}
}

type recordHook struct {
	seen   []string
	md     map[string]any
	panics bool
}

func (h *recordHook) AfterTool(_ context.Context, _ *store.Run, id, name string) map[string]any {
	if h.panics {
		panic("hook bug")
	}
	h.seen = append(h.seen, id+"/"+name)
	return h.md
}

type startHook struct {
	runs   int
	panics bool
}

func (h *startHook) BeforeRun(context.Context, *store.Run) {
	if h.panics {
		panic("start bug")
	}
	h.runs++
}

func TestRunPlanModeOffersOnlyReadOnlyToolsAndTellsTheModel(t *testing.T) {
	// Arrange
	r := newRunRig(t, textTurn("here is the plan"))
	r.withTools(&funcTool{name: "read_file", ro: true}, &funcTool{name: "write_file"}, &funcTool{name: "ask_user"}, &funcTool{name: "update_plan"}, &funcTool{name: "execute_command"})
	run := r.newRun(store.NewRun{Mode: store.ModePlan, Message: store.Message{Role: store.RoleUser, Content: "plan it"}})

	// Act
	_, err := r.run(run)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ti := range r.model.Calls()[0].Tools {
		names = append(names, ti.Name)
	}
	if fmt.Sprint(names) != "[read_file ask_user update_plan]" {
		t.Fatalf("tools offered: %v", names)
	}
	if sys := r.model.Calls()[0].Messages[0].Content; !strings.Contains(sys, "Plan mode: you are read-only in this run") {
		t.Fatal("the system prompt does not announce plan mode")
	}
}

func TestRunRunsReadOnlyCallsOfAStepInParallelAndReportsThemInOrder(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "c1", "r1", `{"i":1}`), llmtest.ToolCall(1, "c2", "r2", `{"i":2}`), llmtest.ToolCall(2, "c3", "w", `{"i":3}`)}}, textTurn("ok"))
	gate := make(chan struct{})
	started := make(chan string, 2)
	r1 := &funcTool{name: "r1", ro: true, run: func(ctx context.Context, _ string) (string, error) {
		started <- "r1"
		<-gate
		return "R1", nil
	}}
	r2 := &funcTool{name: "r2", ro: true, run: func(ctx context.Context, _ string) (string, error) {
		started <- "r2"
		close(gate) // only reachable if r1 is running at the same time
		return "R2", nil
	}}
	r.withTools(r1, r2, tool("w", "W"))

	// Act
	_, err := r.do("go")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, e := range r.events(agui.ToolCallResult) {
		order = append(order, e["content"].(string))
	}
	if fmt.Sprint(order) != "[R1 R2 W]" {
		t.Fatalf("results %v", order)
	}
}

func TestRunOnlyTheFirstAskUserOfAStepInterruptsTheRun(t *testing.T) {
	// Arrange
	r := newRunRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, "q1", "ask_user", `{"question":"A?"}`), llmtest.ToolCall(1, "q2", "ask_user", `{"question":"B?"}`)}})
	r.withTools(&funcTool{name: "ask_user", run: func(_ context.Context, args string) (string, error) {
		return "", &tools.Interrupt{Reason: "ask_user", Question: args}
	}})

	// Act
	res, err := r.do("ask me")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if res.Interrupt == nil || res.Interrupt.ToolCallID != "q1" || res.Interrupt.ID == "" {
		t.Fatalf("interrupt %+v", res.Interrupt)
	}
	res2 := r.events(agui.ToolCallResult)
	if len(res2) != 1 || res2[0]["toolCallId"] != "q2" || res2[0]["content"] != OneQuestionResult {
		t.Fatalf("results %v", res2)
	}
	if len(r.model.Calls()) != 1 {
		t.Fatal("the run went on after asking the user")
	}
}
