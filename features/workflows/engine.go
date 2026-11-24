// Package workflows runs the workflows of core/workflows: it turns each step of a run into a factory task (or a
// human gate, or a child run), in dependency order, and moves the run on whenever a task changes state. It adds
// no scheduler of its own: the factory's dispatcher starts the tasks, the factory's OnTaskState hook (TaskMoved)
// and a slow poll (Run) tell the engine they moved.
package workflows

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	core "github.com/7ar-chat/harness/core/workflows"
)

// Tasks is the part of the factory's task service the engine uses.
type Tasks interface {
	Create(ctx context.Context, t factory.Task) (*factory.Task, bool, error)
	Get(ctx context.Context, id string) (*factory.Task, error)
	Cancel(ctx context.Context, id string) error
}

// Options configures an Engine. Store and Tasks are required.
type Options struct {
	Store core.Store
	Tasks Tasks
	// Answer returns the last assistant message of a thread: the report of a step whose task reached review
	// (its pull request is open) without being done. Nil: such a step has no result text.
	Answer func(ctx context.Context, threadID string) (string, error)
	// Requirement returns the title and text of a requirement a run is linked to (nil: not shown to the agents).
	Requirement func(ctx context.Context, projectID, id string) (title, body string, ok bool)
	// Sandbox returns the sandbox of a run for a repository ("" none) checked out on branch: command steps run in it
	// directly, without a model. Nil: they run as tasks whose agent runs the command.
	Sandbox func(ctx context.Context, r core.Run, repo, branch string) (sandbox.Sandbox, error)
	// SandboxEnabled says whether a sandbox backend exists (nil: yes when Sandbox is set).
	SandboxEnabled func() bool
	// Judge applies the factory policy to a command before it runs directly (nil: allowed).
	Judge func(ctx context.Context, r core.Run, n core.Node, command string) Verdict
	// Preview starts the app of a preview step (the host's preview mechanism) and returns its URL and the log.
	// It returns ErrNoPreview when it cannot (the step then runs its command like a script).
	Preview func(ctx context.Context, r core.Run, n core.Node, sb sandbox.Sandbox) (url, log string, err error)
	// OnFinished is called once a run reached a final state (the host destroys the run's sandboxes).
	OnFinished func(r core.Run)
	// ExecTimeout bounds a command step (0: DefaultExecTimeout); RepoDir is the checkout directory (default
	// /workspace/repo); FlushEvery is how often a running step's output is saved (0: 2s).
	ExecTimeout time.Duration
	RepoDir     string
	FlushEvery  time.Duration
	// PollEvery is how often Run looks at the active runs (0: 15s).
	PollEvery time.Duration
	Now       func() time.Time
}

// Engine implements core.Engine.
type Engine struct {
	o  Options
	mu sync.Mutex
	// locks serialize the changes to one run in this process; another process is stopped by the store's version.
	locks     map[string]*sync.Mutex
	observers []func(core.Change)
	wg        sync.WaitGroup
	execs     execs
}

var _ core.Engine = (*Engine)(nil)

// New returns an Engine. It panics without a Store or Tasks: that is a wiring error.
func New(o Options) *Engine {
	if o.Store == nil || o.Tasks == nil {
		panic("workflows: Options.Store and Options.Tasks are required")
	}
	if o.PollEvery <= 0 {
		o.PollEvery = 15 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Engine{o: o, locks: map[string]*sync.Mutex{}}
}

// Observe implements core.Engine.
func (e *Engine) Observe(fn func(core.Change)) {
	e.mu.Lock()
	e.observers = append(e.observers, fn)
	e.mu.Unlock()
}

// Wait blocks until the advances started by TaskMoved and by finished child runs are over (tests, shutdown).
func (e *Engine) Wait() { e.wg.Wait() }

func (e *Engine) lock(id string) *sync.Mutex {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.locks[id]
	if m == nil {
		m = &sync.Mutex{}
		e.locks[id] = m
	}
	return m
}

func (e *Engine) emit(cs []core.Change) {
	e.mu.Lock()
	obs := slices.Clone(e.observers)
	e.mu.Unlock()
	for _, c := range cs {
		for _, fn := range obs {
			fn(c)
		}
	}
}

// Run polls the active runs until ctx ends: it catches what a hook missed (a restart, a lost notification).
func (e *Engine) Run(ctx context.Context) error {
	tick := time.NewTicker(e.o.PollEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			e.Sweep(ctx)
		}
	}
}

// Sweep advances every active run once.
func (e *Engine) Sweep(ctx context.Context) {
	runs, err := e.o.Store.ListRuns(ctx, core.RunFilter{States: []core.RunState{core.RunRunning, core.RunPaused}, Limit: 500})
	if err != nil {
		slog.Warn("workflows: listing the active runs failed", "error", err)
		return
	}
	for _, r := range runs {
		if _, err := e.Advance(ctx, r.ID); err != nil && !errors.Is(err, core.ErrNotFound) {
			slog.Warn("workflows: advancing a run failed", "run", r.ID, "error", err)
		}
	}
}

// TaskMoved implements core.Engine.
func (e *Engine) TaskMoved(t factory.Task) {
	var run string
	for _, l := range t.Labels {
		if id, ok := strings.CutPrefix(l, factory.LabelWorkflow); ok {
			run = id
		}
	}
	if run == "" {
		return
	}
	e.advanceAsync(run)
}

func (e *Engine) advanceAsync(runID string) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := e.Advance(ctx, runID); err != nil && !errors.Is(err, core.ErrNotFound) {
			slog.Warn("workflows: advancing a run failed", "run", runID, "error", err)
		}
	}()
}

// Advance syncs the steps of a run with their tasks and starts what became ready.
func (e *Engine) Advance(ctx context.Context, runID string) (*core.Run, error) {
	return e.mutate(ctx, runID, func(*pass) error { return nil })
}

// pass is one change of a run: the control (fn) and the settling that follows it.
type pass struct {
	e       *Engine
	ctx     context.Context
	r       *core.Run
	changes []core.Change
}

func (p *pass) log(kind, node, actor, text string) {
	p.r.Events = append(p.r.Events, core.RunEvent{At: p.e.o.Now().UTC(), Kind: kind, NodeID: node, Actor: actor, Text: text})
	if n := len(p.r.Events); n > core.MaxEvents {
		p.r.Events = slices.Clone(p.r.Events[n-core.MaxEvents:])
	}
}

func (p *pass) note(kind, node string) {
	p.changes = append(p.changes, core.Change{Kind: kind, NodeID: node})
}

// mutate loads the run, applies fn, settles the run and saves it, retrying when another process saved first.
func (e *Engine) mutate(ctx context.Context, runID string, fn func(*pass) error) (*core.Run, error) {
	l := e.lock(runID)
	l.Lock()
	defer l.Unlock()
	for attempt := 0; ; attempt++ {
		r, err := e.o.Store.GetRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		wasTerminal := r.State.Terminal()
		p := &pass{e: e, ctx: ctx, r: r}
		before := fingerprint(r)
		if err := fn(p); err != nil {
			return nil, err
		}
		e.settle(p)
		if fingerprint(r) == before && len(p.changes) == 0 {
			return r, nil // nothing changed: no write
		}
		saved, err := e.o.Store.UpdateRun(ctx, *r)
		if errors.Is(err, core.ErrStale) && attempt < 5 {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := range p.changes {
			p.changes[i].Run = *saved
		}
		if len(p.changes) == 0 {
			p.changes = []core.Change{{Run: *saved, Kind: "step"}}
		}
		if saved.State.Terminal() && !wasTerminal {
			if e.o.OnFinished != nil {
				run := *saved
				e.wg.Add(1)
				go func() { defer e.wg.Done(); e.o.OnFinished(run) }()
			}
			p.changes = append(p.changes, core.Change{Run: *saved, Kind: "finished"})
			if saved.ParentRunID != "" {
				e.advanceAsync(saved.ParentRunID)
			}
		}
		e.emit(p.changes)
		return saved, nil
	}
}

func fingerprint(r *core.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%d", r.State, r.Error, len(r.Events))
	if n := len(r.Events); n > 0 {
		fmt.Fprintf(&b, "|%s|%s", r.Events[n-1].At.Format(time.RFC3339Nano), r.Events[n-1].Text)
	}
	for _, s := range r.Steps {
		fmt.Fprintf(&b, "|%s:%s:%d:%s:%s:%s:%d:%s:%t", s.NodeID, s.State, s.Round, s.TaskID, s.TaskState, s.Note, len(s.Context), s.Feedback, s.Manual)
	}
	return b.String()
}

// ---- settling ----

func (e *Engine) settle(p *pass) {
	r := p.r
	if r.State.Terminal() {
		return
	}
	for range 12 {
		changed := e.syncSteps(p)
		if e.handleFailures(p) {
			changed = true
		}
		if r.State.Terminal() {
			return
		}
		if r.State == core.RunRunning && e.startReady(p) {
			changed = true
		}
		if !changed {
			break
		}
	}
	if r.State == core.RunRunning || r.State == core.RunPaused {
		e.finish(p)
	}
}

// satisfied reports whether the step of node id lets its dependents start.
func satisfied(r *core.Run, id string) bool {
	s := r.Step(id)
	if s == nil {
		return false
	}
	if s.State == core.NodeDone {
		return true
	}
	n, _ := r.Node(id)
	return s.State == core.NodeFailed && n.OnFailure != nil && n.OnFailure.Action == core.FailContinue
}

// finish ends the run when every step is settled.
func (e *Engine) finish(p *pass) {
	r := p.r
	for _, s := range r.Steps {
		if !s.State.Settled() {
			return
		}
	}
	for _, s := range r.Steps {
		if s.State == core.NodeFailed || s.State == core.NodeCancelled {
			if n, _ := r.Node(s.NodeID); n.OnFailure == nil || n.OnFailure.Action != core.FailContinue {
				return // handleFailures decides; a cancelled step is for a person to retry
			}
		}
		if s.State == core.NodeSkipped {
			return
		}
	}
	r.State, r.FinishedAt = core.RunDone, e.o.Now().UTC()
	p.log("finished", "", "", "the run is done")
}

var exitRe = regexp.MustCompile(`(?m)^\s*EXIT_CODE:\s*(-?\d+)\s*$`)

// syncSteps reads the tasks and child runs of the running steps.
func (e *Engine) syncSteps(p *pass) bool {
	r, changed := p.r, false
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.State != core.NodeRunning {
			continue
		}
		n, _ := r.Node(s.NodeID)
		switch {
		case s.Direct && s.TaskID == "":
			e.resumeDirect(p, s)
		case s.ChildRunID != "":
			child, err := e.o.Store.GetRun(p.ctx, s.ChildRunID)
			if err != nil {
				if errors.Is(err, core.ErrNotFound) {
					e.fail(p, s, "the child run is gone")
					changed = true
				}
				continue
			}
			s.TaskState = string(child.State)
			switch child.State {
			case core.RunDone:
				e.complete(p, s, "workflow "+child.WorkflowName+" finished", nil, "")
				changed = true
			case core.RunFailed, core.RunCancelled:
				msg := child.Error
				if msg == "" {
					msg = "the child run " + string(child.State)
				}
				e.fail(p, s, msg)
				changed = true
			}
		case s.TaskID != "":
			t, err := e.o.Tasks.Get(p.ctx, s.TaskID)
			if errors.Is(err, factory.ErrNotFound) {
				e.fail(p, s, "the task is gone")
				changed = true
				continue
			}
			if err != nil {
				continue
			}
			state, note := string(t.State), ""
			if t.State == factory.StateBlocked {
				note = t.BlockedReason
			}
			if state != s.TaskState || note != s.Note {
				s.TaskState, s.Note = state, note
				changed = true
			}
			switch {
			case t.State == factory.StateDone:
				e.taskEnded(p, s, n, t.Result, t)
				changed = true
			case t.State == factory.StateReview && n.UseRepo && !n.WaitMerge && (t.PR.Number > 0 || t.PR.URL != ""):
				answer := t.Result
				if answer == "" && e.o.Answer != nil && t.ThreadID != "" {
					if a, err := e.o.Answer(p.ctx, t.ThreadID); err == nil {
						answer = a
					}
				}
				e.taskEnded(p, s, n, answer, t)
				changed = true
			case t.State == factory.StateFailed:
				msg := firstNonEmpty(t.Error, "the task failed")
				e.fail(p, s, msg)
				changed = true
			case t.State == factory.StateCancelled:
				e.fail(p, s, "the task was cancelled")
				changed = true
			}
		}
	}
	return changed
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// taskEnded settles a step whose task finished: a command step needs its exit code to be 0.
func (e *Engine) taskEnded(p *pass, s *core.NodeRun, n core.Node, result string, t *factory.Task) {
	pr := t.PR.URL
	if n.Kind.Command() {
		m := exitRe.FindAllStringSubmatch(result, -1)
		switch {
		case len(m) == 0:
			e.failWith(p, s, "the command did not report an exit code:\n"+tail(result, 3000), result)
			return
		case m[len(m)-1][1] != "0":
			e.failWith(p, s, "the command exited with status "+m[len(m)-1][1]+":\n"+tail(result, 3000), result)
			return
		}
	}
	e.complete(p, s, result, parseOutputs(result, n.Outputs), pr)
}

func (e *Engine) complete(p *pass, s *core.NodeRun, result string, outputs map[string]string, pr string) {
	s.State, s.Result, s.Outputs, s.PRURL, s.Error = core.NodeDone, result, outputs, pr, ""
	s.FinishedAt = e.o.Now().UTC()
	p.log("step", s.NodeID, "", "done")
	p.note("step", s.NodeID)
}

func (e *Engine) fail(p *pass, s *core.NodeRun, msg string) { e.failWith(p, s, msg, "") }

func (e *Engine) failWith(p *pass, s *core.NodeRun, msg, result string) {
	s.State, s.Error, s.Result = core.NodeFailed, msg, result
	s.FinishedAt = e.o.Now().UTC()
	p.log("step", s.NodeID, "", "failed: "+firstLine(msg))
	p.note("step", s.NodeID)
}

// handleFailures applies the failure policy of each failed step: rework sends the run back, continue lets it go
// on, anything else fails the run.
func (e *Engine) handleFailures(p *pass) bool {
	r, changed := p.r, false
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.State != core.NodeFailed {
			continue
		}
		n, _ := r.Node(s.NodeID)
		f := n.OnFailure
		switch {
		case f != nil && f.Action == core.FailContinue:
			continue
		case f != nil && f.Action == core.FailRework:
			max := f.MaxRounds
			if max <= 0 {
				max = core.DefaultRounds
			}
			if r.Rework[n.ID] < max {
				e.rework(p, n, s.Error, "sent back")
				changed = true
				continue
			}
			e.failRun(p, fmt.Sprintf("step %s failed after %d rework rounds: %s", label(n), max, firstLine(s.Error)))
			return true
		default:
			e.failRun(p, fmt.Sprintf("step %s failed: %s", label(n), firstLine(s.Error)))
			return true
		}
	}
	return changed
}

// rework sends the run back from node n to its target with the failure text.
func (e *Engine) rework(p *pass, n core.Node, feedback, why string) {
	r := p.r
	target := ""
	if n.OnFailure != nil {
		target = n.OnFailure.Target
	}
	if target == "" && len(n.DependsOn) > 0 {
		target = n.DependsOn[0]
	}
	if _, ok := r.Node(target); !ok {
		target = n.ID
	}
	if r.Rework == nil {
		r.Rework = map[string]int{}
	}
	r.Rework[n.ID]++
	e.invalidate(p, target, true)
	if st := r.Step(target); st != nil {
		st.Feedback = feedback
	}
	p.log("rework", n.ID, "", fmt.Sprintf("%s to %s (round %d)", why, target, r.Rework[n.ID]))
	p.note("step", n.ID)
}

func label(n core.Node) string {
	if n.Title != "" {
		return n.Title
	}
	return n.ID
}

// failRun ends the run as failed: running tasks are cancelled, steps not started are skipped.
func (e *Engine) failRun(p *pass, msg string) {
	r := p.r
	for i := range r.Steps {
		s := &r.Steps[i]
		switch s.State {
		case core.NodeRunning, core.NodeWaiting:
			e.cancelStep(p, s)
			s.State, s.FinishedAt = core.NodeCancelled, e.o.Now().UTC()
		case core.NodePending:
			s.State = core.NodeSkipped
		}
	}
	r.State, r.Error, r.FinishedAt = core.RunFailed, msg, e.o.Now().UTC()
	p.log("finished", "", "", "the run failed: "+firstLine(msg))
}

// cancelStep stops the task or child run of a step (best effort: a failure is logged, the step is still left).
func (e *Engine) cancelStep(p *pass, s *core.NodeRun) {
	if s.Direct && s.State == core.NodeRunning {
		e.execs.cancel(execKey{p.r.ID, s.NodeID, s.Round})
	}
	if s.TaskID != "" && (s.State == core.NodeRunning || s.State == core.NodeWaiting) {
		if err := e.o.Tasks.Cancel(p.ctx, s.TaskID); err != nil && !errors.Is(err, factory.ErrConflict) && !errors.Is(err, factory.ErrNotFound) {
			slog.Warn("workflows: cancelling a task failed", "task", s.TaskID, "error", err)
		}
	}
	if s.ChildRunID != "" && s.State == core.NodeRunning {
		if _, err := e.cancelRun(p.ctx, s.ChildRunID, "parent run"); err != nil && !errors.Is(err, core.ErrNotFound) {
			slog.Warn("workflows: cancelling a child run failed", "run", s.ChildRunID, "error", err)
		}
	}
}

// invalidate resets a step, and with downstream everything that depends on it, to pending.
func (e *Engine) invalidate(p *pass, id string, downstream bool) {
	r := p.r
	ids := []string{id}
	if downstream {
		ids = append(ids, core.Downstream(r.Graph, id)...)
	}
	for _, x := range ids {
		s := r.Step(x)
		if s == nil {
			continue
		}
		if s.State == core.NodeRunning || s.State == core.NodeWaiting {
			e.cancelStep(p, s)
		}
		*s = core.NodeRun{NodeID: x, State: core.NodePending, Round: s.Round}
	}
}

// ---- starting steps ----

func (e *Engine) startReady(p *pass) bool {
	r, changed := p.r, false
	for _, n := range r.Graph {
		s := r.Step(n.ID)
		if s == nil || s.State != core.NodePending {
			continue
		}
		ready := true
		for _, d := range n.DependsOn {
			if !satisfied(r, d) {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		e.start(p, n, s)
		changed = true
		if r.State.Terminal() {
			return true
		}
	}
	return changed
}

func (e *Engine) start(p *pass, n core.Node, s *core.NodeRun) {
	s.Round++
	s.StartedAt, s.FinishedAt = e.o.Now().UTC(), time.Time{}
	s.Error, s.Result, s.Note, s.TaskState = "", "", "", ""
	s.Direct, s.Log, s.URL, s.ExitCode = false, "", "", nil
	switch {
	case n.Kind == core.KindGate:
		s.State = core.NodeWaiting
		p.log("gate", n.ID, "", "waiting for "+strings.Join(n.Approvers, ", "))
		p.note("gate", n.ID)
	case n.Kind == core.KindSubworkflow:
		child, err := e.startChild(p, n, s)
		if err != nil {
			e.fail(p, s, "could not start the workflow: "+err.Error())
			return
		}
		s.State, s.ChildRunID = core.NodeRunning, child.ID
		p.log("step", n.ID, "", "started workflow "+child.WorkflowName)
		p.note("step", n.ID)
	case n.Kind.Command() && e.startDirect(p, n, s):
		// the command runs in the run's sandbox (or waits for approval, or was refused)
	default:
		t, err := e.createTask(p, n, s)
		if err != nil {
			e.fail(p, s, "could not start the task: "+err.Error())
			return
		}
		s.State, s.TaskID, s.TaskState = core.NodeRunning, t.ID, string(t.State)
		p.log("step", n.ID, "", "started")
		p.note("step", n.ID)
	}
}

func (e *Engine) createTask(p *pass, n core.Node, s *core.NodeRun) (*factory.Task, error) {
	r := p.r
	role := n.Role
	if role == "" && n.Kind.Command() {
		role = factory.RoleTester
	}
	t := factory.Task{
		ProjectID: r.ProjectID, Owner: r.Owner,
		Title: fmt.Sprintf("%s: %s", r.WorkflowName, label(n)),
		Spec:  e.spec(p, n, s), Role: role, Model: n.Model, Policy: n.Policy, Budget: n.Budget,
		Labels: []string{factory.LabelWorkflow + r.ID, factory.LabelWorkflowNode + n.ID},
		Source: factory.Source{Kind: factory.SourceWorkflow, Key: fmt.Sprintf("workflow:%s:%s:%d", r.ID, n.ID, s.Round)},
	}
	if repo := core.RepoOf(*r, n); repo != "" {
		// One branch (and pull request) per repository per run.
		t.Repo, t.Branch = repo, core.BranchFor(*r, repo)
		if repo == r.Repo {
			t.BaseBranch = r.BaseBranch
		}
	}
	created, _, err := e.o.Tasks.Create(p.ctx, t)
	return created, err
}

func (e *Engine) startChild(p *pass, n core.Node, s *core.NodeRun) (*core.Run, error) {
	r := p.r
	existing, err := e.o.Store.ListRuns(p.ctx, core.RunFilter{ParentRunID: r.ID, Limit: 500})
	if err != nil {
		return nil, err
	}
	for i := range existing {
		if existing[i].ParentNodeID == n.ID && existing[i].ParentRound == s.Round {
			return &existing[i], nil // a retry of this pass already started it
		}
	}
	inputs := map[string]string{}
	for k, v := range n.Inputs {
		inputs[k] = core.Render(v, e.lookup(r, n))
	}
	return e.start1(p.ctx, core.StartRequest{
		ProjectID: r.ProjectID, WorkflowID: n.WorkflowID, Inputs: inputs, RequirementID: r.RequirementID,
		Owner: r.Owner, Repo: r.Repo, BaseBranch: r.BaseBranch,
		ParentRunID: r.ID, ParentNodeID: n.ID, ParentRound: s.Round,
	}, r)
}

// ---- prompts ----

func (e *Engine) lookup(r *core.Run, n core.Node) func(string) (string, bool) {
	return func(name string) (string, bool) {
		parts := strings.Split(name, ".")
		switch parts[0] {
		case "inputs":
			if len(parts) == 2 {
				v, ok := r.Inputs[parts[1]]
				return v, ok
			}
		case "nodes":
			if len(parts) >= 3 {
				s := r.Step(parts[1])
				if s == nil {
					return "", false
				}
				switch {
				case parts[2] == "result":
					return s.Result, true
				case parts[2] == "error":
					return s.Error, true
				case parts[2] == "outputs" && len(parts) == 4:
					v, ok := s.Outputs[parts[3]]
					return v, ok
				}
			}
		case "feedback":
			if s := r.Step(n.ID); s != nil {
				return s.Feedback, true
			}
		case "branch":
			return r.Branch, true
		case "repo":
			return r.Repo, true
		}
		return "", false
	}
}

const maxCarried = 4000

// spec is the text of the task of a step.
func (e *Engine) spec(p *pass, n core.Node, s *core.NodeRun) string {
	r := p.r
	look := e.lookup(r, n)
	var b strings.Builder
	if n.Kind.Command() {
		cmd := core.Render(n.Command, look)
		b.WriteString("Run this command in a shell, in the repository checkout (/workspace/repo) when there is one, and change no file:\n\n```sh\n" + cmd + "\n```\n\n")
		b.WriteString("Report whether it succeeded and the last lines of its output. End your final message with a line of the form `EXIT_CODE: <number>` carrying the command's exit status.")
		if n.UseRepo && r.Repo != "" {
			b.WriteString(" When you are done, call open_pr (it returns the pull request that is already open for the branch).")
		}
	} else {
		b.WriteString(core.Render(n.Prompt, look))
	}
	if t := e.requirementText(p); t != "" {
		b.WriteString("\n\n## Linked requirement\n" + t)
	}
	var carried strings.Builder
	for _, d := range n.DependsOn {
		ds := r.Step(d)
		dn, _ := r.Node(d)
		if ds == nil || ds.State != core.NodeDone && ds.State != core.NodeFailed {
			continue
		}
		text := ds.Result
		if ds.State == core.NodeFailed {
			text = "(failed) " + ds.Error
		}
		for _, c := range ds.Context {
			text += "\n\nNote from a reviewer: " + c
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		fmt.Fprintf(&carried, "\n### %s (%s)\n%s\n", label(dn), dn.ID, clip(text, maxCarried))
	}
	if carried.Len() > 0 {
		b.WriteString("\n\n## Results of the earlier steps" + carried.String())
	}
	if fb := strings.TrimSpace(s.Feedback); fb != "" {
		b.WriteString("\n\n## Feedback to address first\nThis step ran before and was sent back:\n\n" + clip(fb, maxCarried))
	}
	if len(n.Outputs) > 0 && !n.Kind.Command() {
		b.WriteString("\n\n## Deliverables\nEnd your final report with one section per deliverable, each starting with a heading line `### <name>`: " + strings.Join(n.Outputs, ", ") + ".")
	}
	return b.String()
}

func (e *Engine) requirementText(p *pass) string {
	r := p.r
	if r.RequirementID == "" || e.o.Requirement == nil {
		return ""
	}
	title, body, ok := e.o.Requirement(p.ctx, r.ProjectID, r.RequirementID)
	if !ok {
		return ""
	}
	return title + "\n\n" + clip(body, maxCarried)
}

// parseOutputs finds the "### name" sections of a report for the declared outputs.
func parseOutputs(result string, names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	out := map[string]string{}
	var cur string
	var buf []string
	flush := func() {
		if cur != "" {
			out[cur] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}
	for _, line := range strings.Split(result, "\n") {
		if h, ok := strings.CutPrefix(strings.TrimSpace(line), "### "); ok {
			flush()
			cur = ""
			if name := strings.ToLower(strings.TrimSpace(h)); slices.Contains(names, name) {
				cur = name
			}
			continue
		}
		if cur != "" {
			buf = append(buf, line)
		}
	}
	flush()
	for _, n := range names {
		if _, ok := out[n]; !ok {
			out[n] = ""
		}
	}
	return out
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "..." + string(r[len(r)-n:])
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return clip(s, 200)
}
