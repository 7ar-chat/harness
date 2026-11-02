// Package tasks is the factory's task service: it creates tasks, dispatches the
// ready ones to agent runs, and moves a task through its states as runs end,
// pull requests open, CI and the reviewer speak. It implements
// factory.Controller, which is how the other factory features act on a task.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/factory"
)

// Defaults of Options.
const (
	defaultMaxAttempts = 3
	defaultConcurrent  = 4
	defaultPollEvery   = 5 * time.Second
	defaultRecoverAge  = 5 * time.Minute
	backoffBase        = 30 * time.Second
	backoffCap         = 15 * time.Minute
	// repeatLimit is how many identical failure signatures in a row block a task.
	repeatLimit = 3
	// listLimit bounds the task lists the dispatcher and the sweeper read.
	listLimit = 1000
)

// Options configures a Service. Store and Spawner are required to dispatch.
type Options struct {
	Store   factory.Store
	Spawner factory.Spawner
	// Messages returns the last assistant text of a thread: the result of a run
	// that needs no pull request.
	Messages func(ctx context.Context, threadID string) (lastAssistantText string, err error)

	// Models, when not empty, lists the models a task may name; DefaultModel is
	// used when it names none ("" lets the spawner choose).
	Models       []string
	DefaultModel string

	// Escalator is told when a task blocks (nil: nobody).
	Escalator factory.Escalator
	// Observers are told when a task finishes or blocks.
	Observers []factory.Observer
	// Roles resolves a role name to its prompt text and whether it runs read-only
	// (nil: no role text, nothing read-only; ok false: an unknown role).
	Roles func(role string) (instructions string, readOnly bool, ok bool)

	DefaultPolicy      string
	DefaultMaxAttempts int // 0: 3
	DefaultBudget      factory.Budget
	MaxConcurrent      int // Running tasks at once, across processes; 0: 4

	// Backoff is the delay before the next attempt; attempt is 1 after the first
	// failed attempt. Default 30s * 2^(attempt-1), capped at 15m.
	Backoff   func(attempt int) time.Duration
	Now       func() time.Time
	PollEvery time.Duration // 0: 5s

	// CancelRun stops a run (nil: Cancel only marks the task).
	CancelRun func(ctx context.Context, runID string) error
	// RunStatus is the status of a run for Sweep: queued, running, completed,
	// failed, interrupted, cancelled, or "" when the run does not exist (nil:
	// Sweep does not recover running tasks that have a run).
	RunStatus func(ctx context.Context, runID string) (status string, err error)
	// RunError is the error text of a failed run (the hook only learns its
	// status); nil: the failure is fingerprinted by status alone.
	RunError func(ctx context.Context, runID string) (string, error)
	// Admit is asked before every dispatch of a ready task. When it refuses, the task is blocked with the
	// reason and escalated instead of started: this is where the day's budget and a failure pattern stop a
	// task from burning more (budget.Guard.Check has this shape). Nil admits everything.
	Admit func(ctx context.Context, taskID string) (ok bool, reason string)
	// RecoverAfter is how long a Running task may stay without a recorded run
	// (a dispatch that died between claiming and spawning) before Sweep treats
	// the attempt as failed. 0: 5m.
	RecoverAfter time.Duration
}

// Service is the task service. Several Services (processes) may share a Store.
type Service struct {
	opts Options
	st   factory.Store
	kick chan struct{}

	dmu sync.Mutex // serializes Dispatch within the process
	wg  sync.WaitGroup
	// nmu orders the notifications: they are delivered one at a time in the order the transitions
	// happened, so an observer never sees a task's later state before its earlier one.
	nmu  sync.Mutex
	tail chan struct{}
}

var _ factory.Controller = (*Service)(nil)

// New returns a Service. It panics without a Store: that is a wiring error.
func New(o Options) *Service {
	if o.Store == nil {
		panic("tasks: Options.Store is required")
	}
	if o.DefaultMaxAttempts <= 0 {
		o.DefaultMaxAttempts = defaultMaxAttempts
	}
	if o.MaxConcurrent <= 0 {
		o.MaxConcurrent = defaultConcurrent
	}
	if o.PollEvery <= 0 {
		o.PollEvery = defaultPollEvery
	}
	if o.RecoverAfter <= 0 {
		o.RecoverAfter = defaultRecoverAge
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Backoff == nil {
		o.Backoff = defaultBackoff
	}
	return &Service{opts: o, st: o.Store, kick: make(chan struct{}, 1)}
}

func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 { // 30s * 2^5 is already past the cap
		return backoffCap
	}
	return min(backoffBase<<(attempt-1), backoffCap)
}

// Kick asks the dispatcher to look for work now.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Flush waits for the observer and escalation notifications in flight.
func (s *Service) Flush() { s.wg.Wait() }

// Get returns a task.
func (s *Service) Get(ctx context.Context, id string) (*factory.Task, error) {
	return s.st.GetTask(ctx, id)
}

// List returns the tasks f selects.
func (s *Service) List(ctx context.Context, f factory.Filter) ([]factory.Task, error) {
	return s.st.ListTasks(ctx, f)
}

// Events returns a task's history (limit 0: all).
func (s *Service) Events(ctx context.Context, id string, limit int) ([]factory.TaskEvent, error) {
	return s.st.ListEvents(ctx, id, limit)
}

func (s *Service) maxAttempts(t *factory.Task) int {
	if t.MaxAttempts > 0 {
		return t.MaxAttempts
	}
	return s.opts.DefaultMaxAttempts
}

// load reads a task for a state change.
func (s *Service) load(ctx context.Context, id string) (*factory.Task, error) {
	t, err := s.st.GetTask(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("tasks: load %s: %w", id, err)
	}
	return t, nil
}

func conflict(t *factory.Task, want string) error {
	return fmt.Errorf("%w: task %s is %s, cannot %s", factory.ErrConflict, t.ID, t.State, want)
}

// move changes t's state if it is still in the state the caller read, and
// records the transition.
func (s *Service) move(ctx context.Context, t *factory.Task, to factory.State, p factory.Patch, text string) (*factory.Task, error) {
	p.IfState = []factory.State{t.State}
	p.State = &to
	u, err := s.st.UpdateTask(ctx, t.ID, p)
	if err != nil {
		return nil, fmt.Errorf("tasks: %s %s -> %s: %w", t.ID, t.State, to, err)
	}
	s.event(ctx, t.ID, factory.EventState, text, map[string]any{"from": string(t.State), "to": string(to)})
	return u, nil
}

func (s *Service) event(ctx context.Context, id, kind, text string, data map[string]any) {
	e := factory.TaskEvent{TaskID: id, Kind: kind, Text: text, Data: data, At: s.opts.Now()}
	if err := s.st.AppendEvent(ctx, e); err != nil {
		slog.Warn("tasks: recording an event failed", "task", id, "kind", kind, "error", err)
	}
}

// Create adds a task: it validates the role and model, applies the service
// defaults, and returns the existing task when t.Source.Key is known.
func (s *Service) Create(ctx context.Context, t factory.Task) (*factory.Task, bool, error) {
	if strings.TrimSpace(t.Title) == "" {
		return nil, false, fmt.Errorf("%w: task title", factory.ErrInvalid)
	}
	if t.Role != "" && s.opts.Roles != nil {
		if _, _, ok := s.opts.Roles(t.Role); !ok {
			return nil, false, fmt.Errorf("%w: unknown role %q", factory.ErrInvalid, t.Role)
		}
	}
	if t.Model != "" && len(s.opts.Models) > 0 && !slices.Contains(s.opts.Models, t.Model) {
		return nil, false, fmt.Errorf("%w: unknown model %q", factory.ErrInvalid, t.Model)
	}
	if t.Source.Kind == "" {
		t.Source.Kind = factory.SourceManual
	}
	if t.Budget == (factory.Budget{}) {
		t.Budget = s.opts.DefaultBudget
	}
	t.Attempts = 0
	created, isNew, err := s.st.CreateTask(ctx, t)
	if err != nil {
		return nil, false, fmt.Errorf("tasks: create: %w", err)
	}
	if !isNew {
		return created, false, nil
	}
	s.event(ctx, created.ID, factory.EventCreated, created.Title, map[string]any{
		"source": created.Source.Kind, "key": created.Source.Key, "state": string(created.State),
	})
	if created.State == factory.StatePending {
		if err := s.releasePending(ctx); err != nil {
			slog.Warn("tasks: checking dependencies failed", "task", created.ID, "error", err)
		}
		if u, gerr := s.st.GetTask(ctx, created.ID); gerr == nil {
			created = u
		}
	}
	s.Kick()
	return created, true, nil
}

// Resume implements factory.Controller: the next attempt starts as soon as the
// dispatcher has a slot.
func (s *Service) Resume(ctx context.Context, taskID, feedback string) error {
	t, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	return s.resume(ctx, t, feedback, 0)
}

// resume puts t back to Ready after delay, or blocks it when its attempts ran out.
func (s *Service) resume(ctx context.Context, t *factory.Task, feedback string, delay time.Duration) error {
	switch t.State {
	case factory.StateRunning, factory.StateReview, factory.StateBlocked:
	default:
		return conflict(t, "resume")
	}
	if max := s.maxAttempts(t); t.Attempts >= max {
		reason := fmt.Sprintf("attempts exhausted (%d of %d)", t.Attempts, max)
		if f := firstLine(feedback); f != "" {
			reason += ": " + f
		}
		return s.block(ctx, t, reason, factory.Patch{Feedback: &feedback})
	}
	notBefore := time.Time{}
	if delay > 0 {
		notBefore = s.opts.Now().Add(delay)
	}
	empty := ""
	text := "ready"
	if f := firstLine(feedback); f != "" {
		text += ": " + f
	}
	if _, err := s.move(ctx, t, factory.StateReady, factory.Patch{
		Feedback: &feedback, NotBefore: &notBefore, BlockedReason: &empty,
	}, text); err != nil {
		return err
	}
	s.Kick()
	return nil
}

// Block implements factory.Controller.
func (s *Service) Block(ctx context.Context, taskID, reason string) error {
	t, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	return s.block(ctx, t, reason, factory.Patch{})
}

func (s *Service) block(ctx context.Context, t *factory.Task, reason string, p factory.Patch) error {
	if t.State.Terminal() {
		return conflict(t, "block")
	}
	p.BlockedReason = &reason
	u, err := s.move(ctx, t, factory.StateBlocked, p, "blocked: "+clip(reason, 300))
	if err != nil {
		return err
	}
	if s.opts.Escalator != nil {
		s.event(ctx, t.ID, factory.EventEscalated, clip(reason, 300), map[string]any{"reason": reason})
	}
	s.notify(ctx, *u, reason)
	return nil
}

// Complete implements factory.Controller.
func (s *Service) Complete(ctx context.Context, taskID, result string) error {
	t, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	if t.State.Terminal() {
		return conflict(t, "complete")
	}
	return s.complete(ctx, t, result)
}

// Fail implements factory.Controller.
func (s *Service) Fail(ctx context.Context, taskID, reason string) error {
	t, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	return s.fail(ctx, t, factory.StateFailed, reason)
}

// fail ends t as Failed or Cancelled with reason.
func (s *Service) fail(ctx context.Context, t *factory.Task, to factory.State, reason string) error {
	if t.State.Terminal() {
		return conflict(t, string(to))
	}
	empty := ""
	u, err := s.move(ctx, t, to, factory.Patch{Error: &reason, BlockedReason: &empty}, string(to)+": "+clip(reason, 300))
	if err != nil {
		return err
	}
	s.afterTerminal(ctx, *u)
	return nil
}

// Review implements factory.Controller.
func (s *Service) Review(ctx context.Context, taskID string, pr factory.PullRequest) error {
	t, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	switch t.State {
	case factory.StateRunning, factory.StateReview, factory.StateBlocked:
	default:
		return conflict(t, "review")
	}
	empty := ""
	if _, err := s.move(ctx, t, factory.StateReview, factory.Patch{PR: &pr, BlockedReason: &empty},
		fmt.Sprintf("review: pull request #%d", pr.Number)); err != nil {
		return err
	}
	s.event(ctx, t.ID, factory.EventPR, pr.URL, map[string]any{"number": pr.Number, "url": pr.URL, "sha": pr.HeadSHA})
	return nil
}

// Cancel stops a task for good and its run, if the task has one.
func (s *Service) Cancel(ctx context.Context, taskID string) error {
	t, err := s.load(ctx, taskID)
	if err != nil {
		return err
	}
	wasRunning, runID := t.State == factory.StateRunning, t.RunID
	if err := s.fail(ctx, t, factory.StateCancelled, "cancelled"); err != nil {
		return err
	}
	if wasRunning && runID != "" && s.opts.CancelRun != nil {
		if err := s.opts.CancelRun(ctx, runID); err != nil {
			return fmt.Errorf("tasks: cancelling run %s of task %s: %w", runID, taskID, err)
		}
	}
	return nil
}

// afterTerminal runs what a finished task triggers: observers, and the
// release (or failure) of the tasks that depend on it.
func (s *Service) afterTerminal(ctx context.Context, t factory.Task) {
	s.notify(ctx, t, "")
	if err := s.releasePending(ctx); err != nil {
		slog.Warn("tasks: releasing dependents failed", "task", t.ID, "error", err)
	}
	s.Kick()
}

// notify tells the escalator (reason != "") and the observers, off the caller's
// path: both may call models or the network, and the caller is often a run's
// finishing step.
func (s *Service) notify(ctx context.Context, t factory.Task, escalate string) {
	if s.opts.Escalator == nil && len(s.opts.Observers) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	// The turn is taken here, on the caller's goroutine, so the order of calls is the order of delivery.
	turn := s.takeTurn()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer turn.done()
		turn.wait()
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if escalate != "" && s.opts.Escalator != nil {
			safely("escalator", t.ID, func() { s.opts.Escalator.Escalate(ctx, t, escalate) })
		}
		if len(s.opts.Observers) == 0 {
			return
		}
		history, err := s.st.ListEvents(ctx, t.ID, 0)
		if err != nil {
			slog.Warn("tasks: loading history for observers failed", "task", t.ID, "error", err)
		}
		for _, o := range s.opts.Observers {
			safely("observer", t.ID, func() { o.TaskFinished(ctx, t, history) })
		}
	}()
}

// turn is one place in the queue of notifications: wait returns when the previous one is done.
type turn struct{ prev, mine chan struct{} }

func (t turn) wait() { <-t.prev }
func (t turn) done() { close(t.mine) }

// takeTurn reserves the next place in the notification queue.
func (s *Service) takeTurn() turn {
	s.nmu.Lock()
	defer s.nmu.Unlock()
	if s.tail == nil {
		s.tail = make(chan struct{})
		close(s.tail)
	}
	t := turn{prev: s.tail, mine: make(chan struct{})}
	s.tail = t.mine
	return t
}

func safely(what, task string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("tasks: "+what+" panicked", "task", task, "panic", p)
		}
	}()
	fn()
}

// depState reports the first failed or cancelled dependency of t ("" none),
// and whether every dependency is done.
func (s *Service) depState(ctx context.Context, t *factory.Task) (failed string, allDone bool, err error) {
	allDone = true
	for _, id := range t.DependsOn {
		d, gerr := s.st.GetTask(ctx, id)
		switch {
		case errors.Is(gerr, factory.ErrNotFound):
			return id + " is missing", false, nil
		case gerr != nil:
			return "", false, fmt.Errorf("tasks: dependency %s of %s: %w", id, t.ID, gerr)
		case d.State == factory.StateFailed || d.State == factory.StateCancelled:
			return fmt.Sprintf("%s %s", id, d.State), false, nil
		case d.State != factory.StateDone:
			allDone = false
		}
	}
	return "", allDone, nil
}

// releasePending moves Pending tasks whose dependencies are done to Ready, and
// fails those whose dependency failed or was cancelled.
func (s *Service) releasePending(ctx context.Context) error {
	pending, err := s.st.ListTasks(ctx, factory.Filter{States: []factory.State{factory.StatePending}, Limit: listLimit})
	if err != nil {
		return fmt.Errorf("tasks: listing pending tasks: %w", err)
	}
	var errs []error
	released := false
	for i := range pending {
		t := &pending[i]
		failed, done, err := s.depState(ctx, t)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		switch {
		case failed != "":
			if err := s.fail(ctx, t, factory.StateFailed, "dependency "+failed); err != nil && !errors.Is(err, factory.ErrConflict) {
				errs = append(errs, err)
			}
		case done:
			// The store may have released it already when the last dependency finished.
			switch _, err := s.move(ctx, t, factory.StateReady, factory.Patch{}, "ready: dependencies done"); {
			case err == nil:
				released = true
			case !errors.Is(err, factory.ErrConflict):
				errs = append(errs, err)
			}
		}
	}
	if released {
		s.Kick()
	}
	return errors.Join(errs...)
}

// firstLine is the first non-blank line of s, clipped for an event or a reason.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return clip(l, 200)
		}
	}
	return ""
}

// clip shortens s to n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
