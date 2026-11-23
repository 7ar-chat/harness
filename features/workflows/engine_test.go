package workflows_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/workflowstore"
	"github.com/7ar-chat/harness/core/factory"
	core "github.com/7ar-chat/harness/core/workflows"
	wf "github.com/7ar-chat/harness/features/workflows"
)

// fakeTasks is the factory's task service reduced to what the engine uses: tasks that move when the test says so.
type fakeTasks struct {
	mu     sync.Mutex
	tasks  map[string]*factory.Task
	byKey  map[string]string
	nextID int
	// cancelled lists the ids of the tasks the engine cancelled.
	cancelled []string
}

func newFake() *fakeTasks {
	return &fakeTasks{tasks: map[string]*factory.Task{}, byKey: map[string]string{}}
}

func (f *fakeTasks) Create(_ context.Context, t factory.Task) (*factory.Task, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.byKey[t.Source.Key]; ok {
		c := *f.tasks[id]
		return &c, false, nil
	}
	if t.Role == "ghost" {
		return nil, false, fmt.Errorf("%w: unknown role", factory.ErrInvalid)
	}
	f.nextID++
	t.ID = fmt.Sprintf("t%d", f.nextID)
	t.State = factory.StateReady
	f.tasks[t.ID] = &t
	f.byKey[t.Source.Key] = t.ID
	c := t
	return &c, true, nil
}

func (f *fakeTasks) Get(_ context.Context, id string) (*factory.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return nil, factory.ErrNotFound
	}
	c := *t
	return &c, nil
}

func (f *fakeTasks) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tasks[id]
	if !ok {
		return factory.ErrNotFound
	}
	if t.State.Terminal() {
		return factory.ErrConflict
	}
	t.State = factory.StateCancelled
	f.cancelled = append(f.cancelled, id)
	return nil
}

// find returns the newest task created for the node of a run.
func (f *fakeTasks) find(run, node string) *factory.Task {
	f.mu.Lock()
	defer f.mu.Unlock()
	var best *factory.Task
	for _, t := range f.tasks {
		if slicesContains(t.Labels, factory.LabelWorkflow+run) && slicesContains(t.Labels, factory.LabelWorkflowNode+node) {
			if best == nil || t.Source.Key > best.Source.Key {
				best = t
			}
		}
	}
	if best == nil {
		return nil
	}
	c := *best
	return &c
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func (f *fakeTasks) set(id string, st factory.State, mut func(*factory.Task)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tasks[id].State = st
	if mut != nil {
		mut(f.tasks[id])
	}
}

type rig struct {
	t     *testing.T
	ctx   context.Context
	store *workflowstore.Mem
	tasks *fakeTasks
	eng   *wf.Engine
	mu    sync.Mutex
	seen  []core.Change
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, ctx: context.Background(), store: workflowstore.NewMem(nil), tasks: newFake()}
	r.eng = wf.New(wf.Options{
		Store: r.store, Tasks: r.tasks,
		Answer:      func(_ context.Context, thread string) (string, error) { return "answer of " + thread, nil },
		Requirement: func(_ context.Context, _, id string) (string, string, bool) { return "Req " + id, "req body", true },
	})
	r.eng.Observe(func(c core.Change) { r.mu.Lock(); r.seen = append(r.seen, c); r.mu.Unlock() })
	return r
}

func agent(id string, deps ...string) core.Node {
	return core.Node{ID: id, Kind: core.KindAgent, Prompt: "do " + id, DependsOn: deps}
}

// publish stores the workflow as published and returns it.
func (r *rig) publish(name string, nodes ...core.Node) *core.Workflow {
	r.t.Helper()
	w, err := r.store.Create(r.ctx, core.Workflow{ProjectID: "p1", Name: name, Nodes: nodes, Status: core.StatusPublished, Inputs: []core.Input{{Name: "task", Default: "the task"}}}, "alice")
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) start(w *core.Workflow) *core.Run {
	r.t.Helper()
	run, err := r.eng.Start(r.ctx, core.StartRequest{ProjectID: "p1", WorkflowID: w.ID, Owner: "alice", Inputs: map[string]string{"task": "ship it"}})
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

// done finishes the task of a step and lets the engine notice.
func (r *rig) done(run *core.Run, node, result string) *core.Run {
	r.t.Helper()
	t := r.tasks.find(run.ID, node)
	if t == nil {
		r.t.Fatalf("no task for step %s", node)
	}
	r.tasks.set(t.ID, factory.StateDone, func(x *factory.Task) { x.Result = result })
	return r.advance(run)
}

func (r *rig) advance(run *core.Run) *core.Run {
	r.t.Helper()
	got, err := r.eng.Advance(r.ctx, run.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

func state(run *core.Run, node string) core.NodeState { return run.Step(node).State }

func wantStates(t *testing.T, run *core.Run, want map[string]core.NodeState) {
	t.Helper()
	for id, w := range want {
		if got := state(run, id); got != w {
			t.Errorf("step %s is %s, want %s", id, got, w)
		}
	}
}

func TestParallelBranchesRunTogetherAndJoin(t *testing.T) {
	// Arrange: a, then b and c in parallel, then d after both.
	r := newRig(t)
	w := r.publish("diamond", agent("a"), agent("b", "a"), agent("c", "a"), agent("d", "b", "c"))

	// Act
	run := r.start(w)
	wantStates(t, run, map[string]core.NodeState{"a": core.NodeRunning, "b": core.NodePending, "c": core.NodePending, "d": core.NodePending})
	run = r.done(run, "a", "A result")
	wantStates(t, run, map[string]core.NodeState{"b": core.NodeRunning, "c": core.NodeRunning, "d": core.NodePending})
	run = r.done(run, "b", "B result")
	wantStates(t, run, map[string]core.NodeState{"c": core.NodeRunning, "d": core.NodePending})
	run = r.done(run, "c", "C result")
	wantStates(t, run, map[string]core.NodeState{"d": core.NodeRunning})
	run = r.done(run, "d", "D result")

	// Assert
	if run.State != core.RunDone || run.FinishedAt.IsZero() {
		t.Fatalf("run = %s, want done", run.State)
	}
	dSpec := r.tasks.find(run.ID, "d").Spec
	if !strings.Contains(dSpec, "B result") || !strings.Contains(dSpec, "C result") || strings.Contains(dSpec, "A result") {
		t.Fatalf("the task of d must carry the results of its dependencies only:\n%s", dSpec)
	}
	task := r.tasks.find(run.ID, "a")
	if task.Source.Kind != factory.SourceWorkflow || task.Owner != "alice" || task.ProjectID != "p1" ||
		!slicesContains(task.Labels, factory.LabelWorkflow+run.ID) {
		t.Fatalf("task = %+v", task)
	}
	var finished int
	for _, c := range r.seen {
		if c.Kind == "finished" {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("observers saw %d finished changes, want 1", finished)
	}
}

func TestPromptTemplatingInputsResultsOutputsAndRequirement(t *testing.T) {
	// Arrange
	r := newRig(t)
	a := core.Node{ID: "a", Kind: core.KindAgent, Prompt: "plan {{inputs.task}}", Outputs: []string{"plan", "risks"}}
	b := core.Node{ID: "b", Kind: core.KindAgent, DependsOn: []string{"a"}, Prompt: "build from {{nodes.a.outputs.plan}} (risks: {{nodes.a.outputs.risks}})"}
	w := r.publish("tpl", a, b)

	// Act
	run, err := r.eng.Start(r.ctx, core.StartRequest{ProjectID: "p1", WorkflowID: w.ID, Owner: "alice", RequirementID: "REQ-1", Inputs: map[string]string{"task": "ship it"}})
	if err != nil {
		t.Fatal(err)
	}
	aSpec := r.tasks.find(run.ID, "a").Spec
	run = r.done(run, "a", "intro\n### plan\nstep one\nstep two\n### risks\nnone")
	bSpec := r.tasks.find(run.ID, "b").Spec

	// Assert
	for _, want := range []string{"plan ship it", "Req REQ-1", "### plan, risks"} {
		if !strings.Contains(aSpec, strings.TrimPrefix(want, "### ")) && !strings.Contains(aSpec, want) {
			t.Errorf("a's task misses %q:\n%s", want, aSpec)
		}
	}
	if !strings.Contains(bSpec, "build from step one\nstep two (risks: none)") {
		t.Errorf("b's task:\n%s", bSpec)
	}
	if got := run.Step("a").Outputs["plan"]; got != "step one\nstep two" {
		t.Errorf("outputs = %v", run.Step("a").Outputs)
	}
}

func TestScriptStepNeedsExitCodeZero(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("s", core.Node{ID: "t", Kind: core.KindScript, Command: "make test"}, agent("after", "t"))

	// Act: no exit code reported
	run := r.start(w)
	task := r.tasks.find(run.ID, "t")
	if !strings.Contains(task.Spec, "make test") || task.Role != factory.RoleTester {
		t.Fatalf("script task = %+v", task)
	}
	run = r.done(run, "t", "all good I think")

	// Assert
	if run.State != core.RunFailed || !strings.Contains(run.Step("t").Error, "did not report an exit code") {
		t.Fatalf("run = %s step = %+v", run.State, run.Step("t"))
	}
	if state(run, "after") != core.NodeSkipped {
		t.Fatalf("the step after a failed one is %s, want skipped", state(run, "after"))
	}

	// Act: exit code 0 passes, 2 fails
	r2 := newRig(t)
	w2 := r2.publish("s", core.Node{ID: "t", Kind: core.KindScript, Command: "make test"})
	run2 := r2.start(w2)
	run2 = r2.done(run2, "t", "ok\nEXIT_CODE: 0")
	if run2.State != core.RunDone {
		t.Fatalf("exit 0: run = %s", run2.State)
	}
	r3 := newRig(t)
	w3 := r3.publish("s", core.Node{ID: "t", Kind: core.KindBuild, Command: "make build"})
	run3 := r3.start(w3)
	run3 = r3.done(run3, "t", "boom\nEXIT_CODE: 2")
	if run3.State != core.RunFailed || !strings.Contains(run3.Step("t").Error, "status 2") {
		t.Fatalf("exit 2: run = %s step = %+v", run3.State, run3.Step("t"))
	}
}

func reworkFlow(r *rig, rounds int) *core.Workflow {
	return r.publish("rework",
		agent("impl"),
		core.Node{ID: "test", Kind: core.KindScript, Command: "go test", DependsOn: []string{"impl"},
			OnFailure: &core.OnFailure{Action: core.FailRework, Target: "impl", MaxRounds: rounds}},
		agent("ship", "test"))
}

func TestReworkEdgeSendsTheFailureBackThenGoesOn(t *testing.T) {
	// Arrange
	r := newRig(t)
	run := r.start(reworkFlow(r, 2))
	run = r.done(run, "impl", "first try")

	// Act: the test fails once
	run = r.done(run, "test", "FAIL TestX\nEXIT_CODE: 1")

	// Assert: impl runs again with the failure text, test and ship wait
	wantStates(t, run, map[string]core.NodeState{"impl": core.NodeRunning, "test": core.NodePending, "ship": core.NodePending})
	if run.State != core.RunRunning || run.Rework["test"] != 1 || run.Step("impl").Round != 2 {
		t.Fatalf("run = %s rework = %v impl round %d", run.State, run.Rework, run.Step("impl").Round)
	}
	spec := r.tasks.find(run.ID, "impl").Spec
	if !strings.Contains(spec, "Feedback to address first") || !strings.Contains(spec, "FAIL TestX") {
		t.Fatalf("the reworked task must carry the failure:\n%s", spec)
	}

	// Act: second round passes
	run = r.done(run, "impl", "second try")
	run = r.done(run, "test", "ok\nEXIT_CODE: 0")
	run = r.done(run, "ship", "shipped")
	if run.State != core.RunDone {
		t.Fatalf("run = %s, want done", run.State)
	}
}

func TestReworkStopsAtTheMaximumRounds(t *testing.T) {
	// Arrange
	r := newRig(t)
	run := r.start(reworkFlow(r, 1))

	// Act: fail twice; one round is allowed
	run = r.done(run, "impl", "try 1")
	run = r.done(run, "test", "EXIT_CODE: 1")
	run = r.done(run, "impl", "try 2")
	run = r.done(run, "test", "EXIT_CODE: 1")

	// Assert
	if run.State != core.RunFailed || !strings.Contains(run.Error, "rework") {
		t.Fatalf("run = %s error = %q", run.State, run.Error)
	}
}

func TestContinuePolicyLetsDependentsRun(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("c",
		core.Node{ID: "lint", Kind: core.KindScript, Command: "lint", OnFailure: &core.OnFailure{Action: core.FailContinue}},
		agent("next", "lint"))

	// Act
	run := r.start(w)
	run = r.done(run, "lint", "warnings\nEXIT_CODE: 1")

	// Assert
	if state(run, "lint") != core.NodeFailed || state(run, "next") != core.NodeRunning || run.State != core.RunRunning {
		t.Fatalf("run = %s lint = %s next = %s", run.State, state(run, "lint"), state(run, "next"))
	}
	run = r.done(run, "next", "fine")
	if run.State != core.RunDone {
		t.Fatalf("run = %s, want done despite the tolerated failure", run.State)
	}
}

func gateFlow(r *rig) *core.Workflow {
	return r.publish("gated",
		agent("draft"),
		core.Node{ID: "review", Kind: core.KindGate, DependsOn: []string{"draft"}, Approvers: []string{"role:member"}},
		agent("publish", "review"))
}

func TestGateWaitsForAHumanAndCarriesTheirContext(t *testing.T) {
	// Arrange
	r := newRig(t)
	run := r.start(gateFlow(r))
	run = r.done(run, "draft", "the draft")

	// Assert: the gate waits and has no task
	if state(run, "review") != core.NodeWaiting || run.Step("review").TaskID != "" || state(run, "publish") != core.NodePending {
		t.Fatalf("gate = %+v", run.Step("review"))
	}
	if run.State != core.RunRunning {
		t.Fatalf("run = %s", run.State)
	}
	gates := 0
	for _, c := range r.seen {
		if c.Kind == "gate" && c.NodeID == "review" {
			gates++
		}
	}
	if gates == 0 {
		t.Fatal("no gate change was reported to the observers")
	}
	// A gate does not move on its own.
	run = r.advance(run)
	if state(run, "review") != core.NodeWaiting {
		t.Fatal("the gate moved without a decision")
	}

	// Act: add context, then approve
	run, err := r.eng.AddContext(r.ctx, run.ID, "review", "bob", "mind the migration")
	if err != nil || state(run, "review") != core.NodeWaiting || len(run.Step("review").Context) != 1 {
		t.Fatalf("add context: %v %+v", err, run.Step("review"))
	}
	run, err = r.eng.Approve(r.ctx, run.ID, "review", "bob", "looks good")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	if state(run, "review") != core.NodeDone || state(run, "publish") != core.NodeRunning {
		t.Fatalf("after approval: review %s publish %s", state(run, "review"), state(run, "publish"))
	}
	spec := r.tasks.find(run.ID, "publish").Spec
	if !strings.Contains(spec, "looks good") || !strings.Contains(spec, "mind the migration") {
		t.Fatalf("the step after the gate must see the approval and the context:\n%s", spec)
	}
	if _, err := r.eng.Approve(r.ctx, run.ID, "review", "bob", ""); !errors.Is(err, core.ErrState) {
		t.Fatalf("second approval: %v", err)
	}
}

func TestGateSendBackRunsTheEarlierStepAgainWithFeedback(t *testing.T) {
	// Arrange
	r := newRig(t)
	run := r.start(gateFlow(r))
	run = r.done(run, "draft", "the draft")

	// Act
	if _, err := r.eng.SendBack(r.ctx, run.ID, "review", "bob", ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("a send-back needs feedback: %v", err)
	}
	run, err := r.eng.SendBack(r.ctx, run.ID, "review", "bob", "too short")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	wantStates(t, run, map[string]core.NodeState{"draft": core.NodeRunning, "review": core.NodePending, "publish": core.NodePending})
	spec := r.tasks.find(run.ID, "draft").Spec
	if !strings.Contains(spec, "too short") || !strings.Contains(spec, "bob") {
		t.Fatalf("draft's task:\n%s", spec)
	}
	run = r.done(run, "draft", "a longer draft")
	if state(run, "review") != core.NodeWaiting {
		t.Fatalf("the gate must wait again, is %s", state(run, "review"))
	}
}

func TestRerunFromANodeInvalidatesDownstreamOnly(t *testing.T) {
	// Arrange: a finished diamond
	r := newRig(t)
	w := r.publish("diamond", agent("a"), agent("b", "a"), agent("c", "a"), agent("d", "b", "c"))
	run := r.start(w)
	run = r.done(run, "a", "A")
	run = r.done(run, "b", "B")
	run = r.done(run, "c", "C")
	run = r.done(run, "d", "D")
	if run.State != core.RunDone {
		t.Fatalf("setup: %s", run.State)
	}
	oldB := r.tasks.find(run.ID, "b").ID

	// Act
	run, err := r.eng.Rerun(r.ctx, run.ID, "b", "alice", "use another approach")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	wantStates(t, run, map[string]core.NodeState{"a": core.NodeDone, "b": core.NodeRunning, "c": core.NodeDone, "d": core.NodePending})
	if run.State != core.RunRunning || !run.FinishedAt.IsZero() {
		t.Fatalf("the run must be active again: %s", run.State)
	}
	nb := r.tasks.find(run.ID, "b")
	if nb.ID == oldB || !strings.Contains(nb.Spec, "use another approach") || run.Step("b").Round != 2 {
		t.Fatalf("b was not restarted: %+v", nb)
	}
	if run.Step("c").Result != "C" {
		t.Fatal("c, which is not downstream of b, lost its result")
	}
	run = r.done(run, "b", "B2")
	run = r.done(run, "d", "D2")
	if run.State != core.RunDone {
		t.Fatalf("run = %s", run.State)
	}
}

func TestRerunCancelsTheRunningTasksItInvalidates(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("two", agent("a"), agent("b", "a"))
	run := r.start(w)
	run = r.done(run, "a", "A")
	running := r.tasks.find(run.ID, "b").ID

	// Act
	run, err := r.eng.Rerun(r.ctx, run.ID, "a", "alice", "")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !slicesContains(r.tasks.cancelled, running) {
		t.Fatalf("the task of b was not cancelled: %v", r.tasks.cancelled)
	}
	wantStates(t, run, map[string]core.NodeState{"a": core.NodeRunning, "b": core.NodePending})
}

func TestRetryTakesOverPauseResumeAndCancel(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("flow", agent("a"), agent("b", "a"))
	run := r.start(w)
	taskA := r.tasks.find(run.ID, "a")
	r.tasks.set(taskA.ID, factory.StateFailed, func(x *factory.Task) { x.Error = "compile error" })
	run = r.advance(run)
	if run.State != core.RunFailed || run.Step("a").Error != "compile error" || state(run, "b") != core.NodeSkipped {
		t.Fatalf("setup: run %s a=%+v", run.State, run.Step("a"))
	}

	// Act: retry a failed step
	if _, err := r.eng.Retry(r.ctx, run.ID, "b", "alice"); !errors.Is(err, core.ErrState) {
		t.Fatalf("retry of a skipped step: %v", err)
	}
	run, err := r.eng.Retry(r.ctx, run.ID, "a", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != core.RunRunning || state(run, "a") != core.NodeRunning || state(run, "b") != core.NodePending {
		t.Fatalf("after retry: %s a=%s b=%s", run.State, state(run, "a"), state(run, "b"))
	}
	if !strings.Contains(r.tasks.find(run.ID, "a").Spec, "compile error") {
		t.Fatal("a retry must carry the failure to the next attempt")
	}

	// Act: pause stops new steps, resume starts them
	run, err = r.eng.Pause(r.ctx, run.ID, "alice")
	if err != nil || run.State != core.RunPaused {
		t.Fatalf("pause: %v %v", err, run)
	}
	run = r.done(run, "a", "fixed")
	if state(run, "a") != core.NodeDone || state(run, "b") != core.NodePending {
		t.Fatalf("paused run: a=%s b=%s (a finishes, b must wait)", state(run, "a"), state(run, "b"))
	}
	if _, err := r.eng.Pause(r.ctx, run.ID, "alice"); !errors.Is(err, core.ErrState) {
		t.Fatalf("second pause: %v", err)
	}
	run, err = r.eng.Resume(r.ctx, run.ID, "alice")
	if err != nil || state(run, "b") != core.NodeRunning {
		t.Fatalf("resume: %v b=%s", err, state(run, "b"))
	}

	// Act: take over b with a human result
	bTask := r.tasks.find(run.ID, "b").ID
	if _, err := r.eng.Takeover(r.ctx, run.ID, "b", "alice", " ", nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("takeover without a result: %v", err)
	}
	run, err = r.eng.Takeover(r.ctx, run.ID, "b", "alice", "I did it by hand", map[string]string{"note": "n"})
	if err != nil {
		t.Fatal(err)
	}
	if state(run, "b") != core.NodeDone || !run.Step("b").Manual || run.Step("b").Result != "I did it by hand" || run.State != core.RunDone {
		t.Fatalf("after takeover: %+v run %s", run.Step("b"), run.State)
	}
	if !slicesContains(r.tasks.cancelled, bTask) {
		t.Fatal("a taken over step's task must be cancelled")
	}

	// Act: cancel a live run
	w2 := r.publish("flow2", agent("x"), agent("y", "x"))
	live := r.start(w2)
	liveTask := r.tasks.find(live.ID, "x").ID
	live, err = r.eng.Cancel(r.ctx, live.ID, "alice")
	if err != nil || live.State != core.RunCancelled || state(live, "x") != core.NodeCancelled || state(live, "y") != core.NodeCancelled {
		t.Fatalf("cancel: %v %+v", err, live)
	}
	if !slicesContains(r.tasks.cancelled, liveTask) {
		t.Fatal("cancel must stop the running task")
	}
	if _, err := r.eng.Cancel(r.ctx, live.ID, "alice"); !errors.Is(err, core.ErrState) {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestSubworkflowRunsAChildRunAndWaitsForIt(t *testing.T) {
	// Arrange
	r := newRig(t)
	child := r.publish("child", core.Node{ID: "work", Kind: core.KindAgent, Prompt: "child does {{inputs.task}}"})
	parent := r.publish("parent",
		agent("first"),
		core.Node{ID: "sub", Kind: core.KindSubworkflow, WorkflowID: child.ID, DependsOn: []string{"first"}, Inputs: map[string]string{"task": "{{nodes.first.result}}"}},
		agent("last", "sub"))

	// Act
	run := r.start(parent)
	run = r.done(run, "first", "from first")
	childID := run.Step("sub").ChildRunID

	// Assert: a child run exists, started by the step, with the rendered input
	if state(run, "sub") != core.NodeRunning || childID == "" {
		t.Fatalf("sub = %+v", run.Step("sub"))
	}
	cr, err := r.store.GetRun(r.ctx, childID)
	if err != nil || cr.ParentRunID != run.ID || cr.ParentNodeID != "sub" || cr.Inputs["task"] != "from first" {
		t.Fatalf("child run = %+v err %v", cr, err)
	}
	if spec := r.tasks.find(childID, "work").Spec; !strings.Contains(spec, "child does from first") {
		t.Fatalf("child task spec: %s", spec)
	}

	// Act: the child finishes; its end advances the parent by itself
	childTask := r.tasks.find(childID, "work")
	r.tasks.set(childTask.ID, factory.StateDone, func(x *factory.Task) { x.Result = "child done" })
	if _, err := r.eng.Advance(r.ctx, childID); err != nil {
		t.Fatal(err)
	}
	r.eng.Wait()

	// Assert
	run, _ = r.store.GetRun(r.ctx, run.ID)
	if state(run, "sub") != core.NodeDone || state(run, "last") != core.NodeRunning {
		t.Fatalf("parent after the child: sub=%s last=%s", state(run, "sub"), state(run, "last"))
	}
}

func TestSubworkflowFailureFailsTheStepAndCancelRemovesTheChild(t *testing.T) {
	// Arrange
	r := newRig(t)
	child := r.publish("child", agent("work"))
	parent := r.publish("parent", core.Node{ID: "sub", Kind: core.KindSubworkflow, WorkflowID: child.ID})
	run := r.start(parent)
	childID := run.Step("sub").ChildRunID
	task := r.tasks.find(childID, "work")

	// Act: cancelling the parent cancels the child and its task
	run, err := r.eng.Cancel(r.ctx, run.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}

	// Assert
	cr, _ := r.store.GetRun(r.ctx, childID)
	if cr.State != core.RunCancelled || !slicesContains(r.tasks.cancelled, task.ID) {
		t.Fatalf("child = %s cancelled tasks %v", cr.State, r.tasks.cancelled)
	}
	_ = run
}

func TestSubworkflowRecursionIsRefused(t *testing.T) {
	// Arrange: a published workflow that runs a published workflow that runs the first one again (stored directly)
	r := newRig(t)
	a := r.publish("a", agent("x"))
	b := r.publish("b", core.Node{ID: "go", Kind: core.KindSubworkflow, WorkflowID: a.ID})
	a.Nodes = []core.Node{{ID: "go", Kind: core.KindSubworkflow, WorkflowID: b.ID}}
	if _, err := r.store.Update(r.ctx, *a, "x"); err != nil {
		t.Fatal(err)
	}

	// Act
	run := r.start(a)

	// Assert: a -> b -> a is refused at the second level, so b's step fails and the runs fail
	b1 := run.Step("go")
	if b1.State != core.NodeFailed && b1.State != core.NodeRunning {
		t.Fatalf("step = %+v", b1)
	}
	got, _ := r.store.GetRun(r.ctx, run.ID)
	if got.State != core.RunFailed {
		child, _ := r.store.GetRun(r.ctx, got.Step("go").ChildRunID)
		t.Fatalf("run = %s, child = %+v", got.State, child)
	}
	if !strings.Contains(got.Step("go").Error, "cannot run itself") {
		t.Fatalf("error = %q", got.Step("go").Error)
	}
}

func TestStartRefusesADraftMissingInputsAndAnotherProject(t *testing.T) {
	// Arrange
	r := newRig(t)
	draft, _ := r.store.Create(r.ctx, core.Workflow{ProjectID: "p1", Name: "draft", Nodes: []core.Node{agent("a")}}, "x")
	req, _ := r.store.Create(r.ctx, core.Workflow{ProjectID: "p1", Name: "req", Status: core.StatusPublished, Nodes: []core.Node{agent("a")},
		Inputs: []core.Input{{Name: "needed", Required: true}}}, "x")

	// Act and assert
	if _, err := r.eng.Start(r.ctx, core.StartRequest{ProjectID: "p1", WorkflowID: draft.ID, Owner: "a"}); !errors.Is(err, core.ErrState) {
		t.Fatalf("draft: %v", err)
	}
	if _, err := r.eng.Start(r.ctx, core.StartRequest{ProjectID: "p1", WorkflowID: req.ID, Owner: "a"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("missing input: %v", err)
	}
	if _, err := r.eng.Start(r.ctx, core.StartRequest{ProjectID: "p2", WorkflowID: req.ID, Owner: "a", Inputs: map[string]string{"needed": "x"}}); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("other project: %v", err)
	}
}

func TestRepoStepsShareABranchAndFinishWhenThePullRequestOpens(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("repo",
		core.Node{ID: "impl", Kind: core.KindAgent, Prompt: "build", UseRepo: true},
		core.Node{ID: "plan", Kind: core.KindAgent, Prompt: "think"},
		core.Node{ID: "land", Kind: core.KindAgent, Prompt: "land", UseRepo: true, WaitMerge: true, DependsOn: []string{"impl"}})

	// Act
	run, err := r.eng.Start(r.ctx, core.StartRequest{ProjectID: "p1", WorkflowID: w.ID, Owner: "alice", Repo: "acme/app", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	impl, plan := r.tasks.find(run.ID, "impl"), r.tasks.find(run.ID, "plan")

	// Assert: repo tasks share the run's branch, a step without UseRepo has no repository
	if run.Branch == "" || impl.Repo != "acme/app" || impl.Branch != run.Branch || impl.BaseBranch != "main" || plan.Repo != "" {
		t.Fatalf("branch %q impl %+v plan %+v", run.Branch, impl, plan)
	}

	// Act: impl opens its PR (review): done for the workflow; land waits for the merge
	r.tasks.set(impl.ID, factory.StateReview, func(x *factory.Task) {
		x.PR = factory.PullRequest{Number: 7, URL: "https://x/pull/7"}
		x.ThreadID = "th1"
	})
	run = r.advance(run)
	if state(run, "impl") != core.NodeDone || run.Step("impl").PRURL != "https://x/pull/7" || run.Step("impl").Result != "answer of th1" {
		t.Fatalf("impl = %+v", run.Step("impl"))
	}
	land := r.tasks.find(run.ID, "land")
	r.tasks.set(land.ID, factory.StateReview, func(x *factory.Task) { x.PR = factory.PullRequest{Number: 7, URL: "https://x/pull/7"} })
	run = r.advance(run)
	if state(run, "land") != core.NodeRunning {
		t.Fatalf("a WaitMerge step must wait for done, is %s", state(run, "land"))
	}
	r.tasks.set(land.ID, factory.StateDone, nil)
	r.tasks.set(plan.ID, factory.StateDone, nil)
	run = r.advance(run)
	if run.State != core.RunDone {
		t.Fatalf("run = %s", run.State)
	}
}

func TestBlockedTaskShowsItsReasonAndAnUnknownRoleFailsTheStep(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("b", agent("a"))
	run := r.start(w)
	task := r.tasks.find(run.ID, "a")

	// Act
	r.tasks.set(task.ID, factory.StateBlocked, func(x *factory.Task) { x.BlockedReason = "needs approval" })
	run = r.advance(run)

	// Assert
	if state(run, "a") != core.NodeRunning || run.Step("a").Note != "needs approval" || run.Step("a").TaskState != "blocked" {
		t.Fatalf("step = %+v", run.Step("a"))
	}
	w2 := r.publish("ghost", core.Node{ID: "g", Kind: core.KindAgent, Prompt: "x", Role: "ghost"})
	run2 := r.start(w2)
	if run2.State != core.RunFailed || !strings.Contains(run2.Step("g").Error, "unknown role") {
		t.Fatalf("run = %s step = %+v", run2.State, run2.Step("g"))
	}
}

func TestTaskMovedAdvancesTheRunOfTheTask(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("flow", agent("a"), agent("b", "a"))
	run := r.start(w)
	task := r.tasks.find(run.ID, "a")
	r.tasks.set(task.ID, factory.StateDone, func(x *factory.Task) { x.Result = "A" })

	// Act
	r.eng.TaskMoved(*r.tasks.find(run.ID, "a"))
	r.eng.TaskMoved(factory.Task{ID: "other", Labels: []string{"nolabel"}}) // not a workflow task: ignored
	r.eng.Wait()

	// Assert
	got, _ := r.store.GetRun(r.ctx, run.ID)
	if state(got, "b") != core.NodeRunning {
		t.Fatalf("b = %s", state(got, "b"))
	}
}

func TestSweepAdvancesActiveRuns(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("flow", agent("a"), agent("b", "a"))
	run := r.start(w)
	task := r.tasks.find(run.ID, "a")
	r.tasks.set(task.ID, factory.StateDone, func(x *factory.Task) { x.Result = "A" })

	// Act
	r.eng.Sweep(r.ctx)

	// Assert
	got, _ := r.store.GetRun(r.ctx, run.ID)
	if state(got, "b") != core.NodeRunning {
		t.Fatalf("b = %s", state(got, "b"))
	}
}

func TestAdvanceWritesNothingWhenNothingChanged(t *testing.T) {
	// Arrange
	r := newRig(t)
	w := r.publish("flow", agent("a"))
	run := r.start(w)

	// Act
	again := r.advance(run)

	// Assert
	if again.Version != run.Version {
		t.Fatalf("version moved from %d to %d without a change", run.Version, again.Version)
	}
}

func TestEventsLogTheRun(t *testing.T) {
	// Arrange
	r := newRig(t)
	run := r.start(gateFlow(r))
	run = r.done(run, "draft", "d")
	run, _ = r.eng.Approve(r.ctx, run.ID, "review", "bob", "ok")

	// Assert
	var kinds []string
	for _, e := range run.Events {
		kinds = append(kinds, e.Kind)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"started", "step", "gate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events %s lack %s", joined, want)
		}
	}
}
