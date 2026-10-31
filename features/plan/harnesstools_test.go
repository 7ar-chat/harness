package harnesstools_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/plan"
)

// emitter is the agent's emitter of a tool call: it keeps the events queued after the tool's result.
type emitter struct{ after []agui.Event }

func (e *emitter) ToolOutput(string, string, string) {}
func (e *emitter) EmitAfterResult(ev agui.Event)     { e.after = append(e.after, ev) }

// failingStore refuses to save a plan.
type failingStore struct{}

func (failingStore) SetThreadPlan(context.Context, string, []store.PlanItem) error {
	return errors.New("disk full")
}

type rig struct {
	st    *memstore.Store
	th    *store.Thread
	tools map[string]ai.InvokableTool
	guard tools.StopGuard
	obs   tools.TurnObserver
}

func newRig(t *testing.T, st harnesstools.Store) *rig {
	t.Helper()
	mem := memstore.New()
	th, err := mem.CreateThread(context.Background(), "alice", "t")
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		st = mem
	}
	list, cleanup, err := harnesstools.New(st).Tools(context.Background(), tools.RunContext{ThreadID: th.ID, Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup == nil {
		t.Fatal("no cleanup func")
	}
	cleanup()
	r := &rig{st: mem, th: th, tools: map[string]ai.InvokableTool{}}
	for _, tl := range list {
		info, _ := tl.Info(context.Background())
		r.tools[info.Name] = tl.(ai.InvokableTool)
		if g, ok := tl.(tools.StopGuard); ok {
			r.guard = g
		}
		if o, ok := tl.(tools.TurnObserver); ok {
			r.obs = o
		}
	}
	return r
}

func (r *rig) call(name, args string) (string, error) {
	return r.tools[name].InvokableRun(context.Background(), args)
}

func TestTheProviderOffersUpdatePlanAndAskUser(t *testing.T) {
	// Arrange
	r := newRig(t, nil)

	// Assert
	if harnesstools.New(nil).Name() != "harness" || len(r.tools) != 2 || r.tools["update_plan"] == nil || r.tools["ask_user"] == nil {
		t.Fatalf("tools %v", r.tools)
	}
	if r.guard == nil || r.obs == nil {
		t.Fatal("update_plan is neither a stop guard nor a turn observer")
	}
}

func TestUpdatePlanStoresTheListSnapshotsItAndSummarizesProgress(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	em := &emitter{}
	ctx := tools.WithCall(context.Background(), "call-1", em)
	args := `{"items":[{"id":" 1 ","title":" Read the code ","status":"completed"},{"id":"2","title":"Write the fix","status":"in_progress"},{"id":"3","title":"Test","status":"pending"}]}`

	// Act
	out, err := r.tools["update_plan"].InvokableRun(ctx, args)

	// Assert
	if err != nil || out != "Plan updated: 1/3 completed. In progress: Write the fix." {
		t.Fatalf("out %q err %v", out, err)
	}
	want := []store.PlanItem{{ID: "1", Title: "Read the code", Status: "completed"}, {ID: "2", Title: "Write the fix", Status: "in_progress"}, {ID: "3", Title: "Test", Status: "pending"}}
	th, _ := r.st.GetThread(context.Background(), "alice", r.th.ID)
	if fmt.Sprint(th.Plan) != fmt.Sprint(want) {
		t.Fatalf("stored plan %+v", th.Plan)
	}
	if len(em.after) != 1 || em.after[0].Type() != agui.StateSnapshot {
		t.Fatalf("events after the result: %v", em.after)
	}
	snap := em.after[0]["snapshot"].(map[string]any)["plan"].([]store.PlanItem)
	if fmt.Sprint(snap) != fmt.Sprint(want) {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestUpdatePlanWarnsWhenTheFocusIsNotOneItem(t *testing.T) {
	cases := []struct{ name, items, want string }{
		{"two in progress", `[{"id":"1","title":"A","status":"in_progress"},{"id":"2","title":"B","status":"in_progress"}]`,
			"Plan updated: 0/2 completed. Warning: 2 items are in progress; keep exactly one."},
		{"none in progress", `[{"id":"1","title":"A","status":"completed"},{"id":"2","title":"B","status":"pending"}]`,
			"Plan updated: 1/2 completed. No item is in progress; mark the one you work on."},
		{"all done", `[{"id":"1","title":"A","status":"completed"}]`, "Plan updated: 1/1 completed."},
		{"empty plan", `[]`, "Plan updated: 0/0 completed."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, nil)

			// Act
			out, err := r.call("update_plan", `{"items":`+tc.items+`}`)

			// Assert
			if err != nil || out != tc.want {
				t.Fatalf("out %q err %v, want %q", out, err, tc.want)
			}
		})
	}
}

func TestUpdatePlanRejectsAMalformedListAndStoresNothing(t *testing.T) {
	long := strings.Repeat("x", 301)
	var many []string
	for i := 0; i <= harnesstools.MaxPlanItems; i++ {
		many = append(many, fmt.Sprintf(`{"id":"%d","title":"t","status":"pending"}`, i))
	}
	cases := []struct{ name, items, want string }{
		{"too many items", "[" + strings.Join(many, ",") + "]", "a plan has at most 50 items"},
		{"missing id", `[{"id":" ","title":"A","status":"pending"}]`, "item 1 needs an id and a title"},
		{"missing title", `[{"id":"1","title":"A","status":"pending"},{"id":"2","title":"","status":"pending"}]`, "item 2 needs an id and a title"},
		{"duplicate id", `[{"id":"1","title":"A","status":"pending"},{"id":"1","title":"B","status":"pending"}]`, `item id "1" is used twice`},
		{"title too long", `[{"id":"1","title":"` + long + `","status":"pending"}]`, `item "1": the title is longer than 300 characters`},
		{"unknown status", `[{"id":"1","title":"A","status":"done"}]`, `item "1": status must be pending, in_progress or completed`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, nil)
			if _, err := r.call("update_plan", `{"items":[{"id":"0","title":"kept","status":"pending"}]}`); err != nil {
				t.Fatal(err)
			}

			// Act
			_, err := r.call("update_plan", `{"items":`+tc.items+`}`)

			// Assert
			var ue *tools.UserError
			if err == nil || !errors.As(err, &ue) || ue.Msg != tc.want {
				t.Fatalf("err %v, want user error %q", err, tc.want)
			}
			th, _ := r.st.GetThread(context.Background(), "alice", r.th.ID)
			if len(th.Plan) != 1 || th.Plan[0].Title != "kept" {
				t.Fatalf("a rejected update changed the stored plan: %+v", th.Plan)
			}
		})
	}
}

func TestUpdatePlanReportsAStoreFailureAndDoesNotGuardTheUnsavedPlan(t *testing.T) {
	// Arrange
	r := newRig(t, failingStore{})

	// Act
	_, err := r.call("update_plan", `{"items":[{"id":"1","title":"A","status":"pending"}]}`)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "saving the plan failed: disk full") {
		t.Fatalf("err %v", err)
	}
	if reminder := r.guard.StopReminder(context.Background()); reminder != "" {
		t.Fatalf("an unsaved plan guards the answer: %q", reminder)
	}
}

func TestStopReminderListsTheUnfinishedItemsOfThePlanThisRunWrote(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	if reminder := r.guard.StopReminder(context.Background()); reminder != "" {
		t.Fatalf("a run without a plan was reminded: %q", reminder)
	}
	if _, err := r.call("update_plan", `{"items":[{"id":"1","title":"Done","status":"completed"},{"id":"2","title":"Fix it","status":"in_progress"},{"id":"3","title":"Ship","status":"pending"}]}`); err != nil {
		t.Fatal(err)
	}

	// Act
	reminder := r.guard.StopReminder(context.Background())

	// Assert
	want := `Your plan still has 2 unfinished item(s): "Fix it" (in_progress), "Ship" (pending). Continue working on them now; if they are done or no longer needed, call update_plan to mark them completed or remove them, then give your final answer.`
	if reminder != want {
		t.Fatalf("reminder %q", reminder)
	}
	if _, err := r.call("update_plan", `{"items":[{"id":"1","title":"Done","status":"completed"}]}`); err != nil {
		t.Fatal(err)
	}
	if reminder := r.guard.StopReminder(context.Background()); reminder != "" {
		t.Fatalf("a finished plan still reminds: %q", reminder)
	}
}

func TestAResumedRunKeepsGuardingThePlanAnEarlierRunOfTheTurnWrote(t *testing.T) {
	// A run resumed after an ask_user answer sees the turn's earlier calls: the last successful update_plan becomes the plan to guard.
	// Arrange
	r := newRig(t, nil)
	calls := []tools.TurnCall{
		{Name: "update_plan", Arguments: `{"items":[{"id":"1","title":"Old","status":"pending"}]}`},
		{Name: "read_file", Arguments: `{}`},
		{Name: "update_plan", Arguments: `{"items":[{"id":"1","title":"Current step","status":"in_progress"}]}`},
		{Name: "update_plan", Arguments: `{"items":[{"id":"9","title":"Failed attempt","status":"pending"}]}`, Failed: true},
	}

	// Act
	r.obs.ObserveTurn(calls)

	// Assert
	if got := r.guard.StopReminder(context.Background()); !strings.Contains(got, `"Current step" (in_progress)`) || strings.Contains(got, "Old") || strings.Contains(got, "Failed attempt") {
		t.Fatalf("reminder %q", got)
	}
}

func TestObserveTurnNeverReplacesThePlanTheRunAlreadyWrote(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	if _, err := r.call("update_plan", `{"items":[{"id":"1","title":"Mine","status":"pending"}]}`); err != nil {
		t.Fatal(err)
	}

	// Act
	r.obs.ObserveTurn([]tools.TurnCall{{Name: "update_plan", Arguments: `{"items":[{"id":"1","title":"Earlier run","status":"pending"}]}`}})

	// Assert
	if got := r.guard.StopReminder(context.Background()); !strings.Contains(got, `"Mine"`) || strings.Contains(got, "Earlier run") {
		t.Fatalf("reminder %q", got)
	}
}

func TestObserveTurnIgnoresCallsWithoutAUsablePlan(t *testing.T) {
	// Arrange
	r := newRig(t, nil)

	// Act
	r.obs.ObserveTurn([]tools.TurnCall{
		{Name: "update_plan", Arguments: `not json`},
		{Name: "update_plan", Arguments: `{}`},
		{Name: "ask_user", Arguments: `{"question":"q"}`},
	})

	// Assert
	if got := r.guard.StopReminder(context.Background()); got != "" {
		t.Fatalf("reminder %q", got)
	}
}

func TestAskUserInterruptsTheRunWithTheCleanedQuestionAndOptions(t *testing.T) {
	// Arrange
	r := newRig(t, nil)

	// Act
	out, err := r.call("ask_user", `{"question":"  Which database? ","options":[{"label":" Postgres ","description":" the default "},{"label":"SQLite"}]}`)

	// Assert
	var intr *tools.Interrupt
	if out != "" || !errors.As(err, &intr) {
		t.Fatalf("out %q err %v", out, err)
	}
	want := tools.Interrupt{Reason: "ask_user", Question: "Which database?", AllowFreeText: true,
		Options: []tools.QuestionOption{{Label: "Postgres", Description: "the default"}, {Label: "SQLite"}}}
	if fmt.Sprintf("%+v", *intr) != fmt.Sprintf("%+v", want) {
		t.Fatalf("interrupt %+v, want %+v", *intr, want)
	}
}

func TestAskUserHonoursAllowFreeText(t *testing.T) {
	cases := []struct {
		name, args string
		wantFree   bool
		wantErr    string
	}{
		{"default allows free text", `{"question":"Q?"}`, true, ""},
		{"explicitly allowed", `{"question":"Q?","allowFreeText":true}`, true, ""},
		{"options only", `{"question":"Q?","allowFreeText":false,"options":[{"label":"A"}]}`, false, ""},
		{"no way to answer", `{"question":"Q?","allowFreeText":false}`, false, "give options or allow a free-text answer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, nil)

			// Act
			_, err := r.call("ask_user", tc.args)

			// Assert
			var intr *tools.Interrupt
			var ue *tools.UserError
			switch {
			case tc.wantErr != "":
				if !errors.As(err, &ue) || ue.Msg != tc.wantErr {
					t.Fatalf("err %v, want %q", err, tc.wantErr)
				}
			case !errors.As(err, &intr) || intr.AllowFreeText != tc.wantFree:
				t.Fatalf("err %v, want an interrupt with AllowFreeText=%v", err, tc.wantFree)
			}
		})
	}
}

func TestAskUserRejectsAnInvalidQuestionOrOptions(t *testing.T) {
	var nine []string
	for i := 0; i <= harnesstools.MaxOptions; i++ {
		nine = append(nine, fmt.Sprintf(`{"label":"o%d"}`, i))
	}
	cases := []struct{ name, args, want string }{
		{"blank question", `{"question":"  "}`, "question is required (at most 2000 characters)"},
		{"long question", `{"question":"` + strings.Repeat("q", 2001) + `"}`, "question is required (at most 2000 characters)"},
		{"too many options", `{"question":"Q?","options":[` + strings.Join(nine, ",") + `]}`, "at most 8 options"},
		{"blank label", `{"question":"Q?","options":[{"label":" "}]}`, "each option needs a label (at most 100 characters, description at most 300)"},
		{"long label", `{"question":"Q?","options":[{"label":"` + strings.Repeat("l", 101) + `"}]}`, "each option needs a label (at most 100 characters, description at most 300)"},
		{"long description", `{"question":"Q?","options":[{"label":"A","description":"` + strings.Repeat("d", 301) + `"}]}`, "each option needs a label (at most 100 characters, description at most 300)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, nil)

			// Act
			_, err := r.call("ask_user", tc.args)

			// Assert
			var ue *tools.UserError
			if !errors.As(err, &ue) || ue.Msg != tc.want {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAnInterruptSurvivesTheToolWrapperUnchanged(t *testing.T) {
	// The agent recognises ask_user's interrupt with errors.As even through the tool framework's error wrapping.
	// Arrange
	r := newRig(t, nil)

	// Act
	_, err := r.call("ask_user", `{"question":"Q?"}`)

	// Assert
	var intr *tools.Interrupt
	if !errors.As(err, &intr) || intr.Reason != "ask_user" {
		t.Fatalf("err %T %v", err, err)
	}
	if b, _ := json.Marshal(intr.Options); string(b) != "[]" {
		t.Fatalf("options encode as %s, want an empty list the UI can render", b)
	}
}
