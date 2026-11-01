package forge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/factory"
)

// Loop defaults.
const (
	DefaultEvery        = 30 * time.Second
	DefaultSettle       = 60 * time.Second
	DefaultMaxFixRounds = 5
	// DefaultMaxGap is the longest a task that shows no change goes without a poll.
	DefaultMaxGap = 5 * time.Minute
	// DefaultMaxPerTick bounds the evaluations (about three GitHub calls each) of one tick.
	DefaultMaxPerTick = 25

	// pendingMaxGap caps the gap of a task that only waits for CI (or for GitHub to compute mergeability): CI ending is what it waits for.
	pendingMaxGap = 2 * time.Minute

	// ReadyReason is the block reason of a task whose pull request is green and approved but left to a human to merge.
	ReadyReason = "ready to merge: awaiting a human"

	eventLimit = 300
	taskLimit  = 1000
	// maxGateErrors consecutive gate failures on one head escalate the task.
	maxGateErrors = 3
	// noChecksFactor: with RequireChecks, a head that reports no check for this many settle windows is escalated.
	noChecksFactor = 10
	// blockedEvery: a task parked for a human to merge is polled this much less often than a task in review.
	blockedEvery = 5

	maxFeedback    = 9000
	maxFailedLogs  = 4
	logPerCheck    = 3000
	maxHandledKeys = 400
)

// LoopOptions configures the pull-request loop.
type LoopOptions struct {
	Forge      factory.Forge
	Store      factory.Store
	Controller factory.Controller
	// Gate judges a pull request whose checks are green (nil: no review gate).
	Gate factory.Gate
	// Every is the polling interval of Run (default 30 s).
	Every time.Duration
	// Now is the clock (default time.Now). Task events are stamped with it, so it must be the store's clock too.
	Now func() time.Time
	// MergeMethod is how a pull request merges (default squash).
	MergeMethod factory.MergeMethod
	// RequireChecks keeps a pull request from merging while its head reports no
	// check at all. A bool cannot say "default true", so the zero options
	// already require checks: a repository without CI sets AllowNoChecks (and
	// leaves RequireChecks false) to let a head with no check merge.
	RequireChecks bool
	AllowNoChecks bool
	// Settle is how long the checks must stay green after the last head change
	// before the pull request is judged, so a check that registers late cannot
	// be skipped (default 60 s).
	Settle time.Duration
	// MaxFixRounds caps the automatic new attempts on one pull request (CI,
	// conflicts, review findings, requested changes); then the task blocks (default 5).
	MaxFixRounds int
	// MaxGap caps the adaptive polling: an evaluation that found nothing new
	// doubles the task's gap from Every up to this (default 5 min). A webhook
	// event or any change brings it back to Every.
	MaxGap time.Duration
	// MaxPerTick caps the tasks one tick evaluates, the least recently
	// evaluated first, so a burst of pull requests spreads over ticks instead
	// of spiking the GitHub API (default 25).
	MaxPerTick int
}

// Loop drives the pull requests of the tasks in review: it waits for CI, feeds
// failures, conflicts and review findings back to the agent, asks the gate,
// and merges or hands the pull request to a human. All progress is kept in the
// task's events and fields, so a restarted process continues where the last one
// stopped.
type Loop struct {
	LoopOptions
	require bool

	mu       sync.Mutex
	inflight map[string]bool
	polled   map[string]time.Time // last poll of a task parked for a human (an optimisation only)
	poll     map[string]*pollState
}

// pollState is the adaptive polling of one task in review. It lives in memory
// only: a restart evaluates every task once and the events hold the progress.
type pollState struct {
	last time.Time // when the last evaluation started
	next time.Time // not due before this (zero: due at the next tick)
	n    int       // consecutive evaluations that found nothing new
}

// NewLoop returns a loop; the zero options are filled with the defaults.
func NewLoop(o LoopOptions) *Loop {
	if o.Every <= 0 {
		o.Every = DefaultEvery
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MergeMethod == "" {
		o.MergeMethod = factory.MergeSquash
	}
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.MaxFixRounds <= 0 {
		o.MaxFixRounds = DefaultMaxFixRounds
	}
	if o.MaxGap <= 0 {
		o.MaxGap = DefaultMaxGap
	}
	if o.MaxPerTick <= 0 {
		o.MaxPerTick = DefaultMaxPerTick
	}
	return &Loop{LoopOptions: o, require: o.RequireChecks || !o.AllowNoChecks,
		inflight: map[string]bool{}, polled: map[string]time.Time{}, poll: map[string]*pollState{}}
}

// Run ticks every Every until ctx ends.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(l.Every)
	defer t.Stop()
	for {
		if err := l.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("forge loop: tick", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick evaluates the tasks in review that are due, at most MaxPerTick of them,
// the least recently evaluated first, then the parked tasks whose turn has
// come. A failing task does not stop the others; the errors come back joined.
func (l *Loop) Tick(ctx context.Context) error {
	tasks, err := l.Store.ListTasks(ctx, factory.Filter{States: []factory.State{factory.StateReview}, Limit: taskLimit})
	if err != nil {
		return fmt.Errorf("list tasks in review: %w", err)
	}
	var errs []error
	for _, t := range l.dueReview(tasks) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := l.evaluate(ctx, t, false); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, err))
		}
	}
	parked, err := l.Store.ListTasks(ctx, factory.Filter{States: []factory.State{factory.StateBlocked}, Limit: taskLimit})
	if err != nil {
		return errors.Join(append(errs, fmt.Errorf("list blocked tasks: %w", err))...)
	}
	budget := l.MaxPerTick
	for _, t := range parked {
		if err := ctx.Err(); err != nil {
			return err
		}
		if budget == 0 {
			break
		}
		if !awaitsMerge(t) || !l.dueParked(t.ID) {
			continue
		}
		budget--
		if err := l.evaluate(ctx, t, false); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", t.ID, err))
		}
	}
	return errors.Join(errs...)
}

// dueReview forgets the polling of tasks that left review (a task that comes
// back is looked at right away) and returns the due ones, least recently
// evaluated first, up to MaxPerTick. The rest stay due for the next tick.
func (l *Loop) dueReview(tasks []factory.Task) []factory.Task {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	inReview := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		inReview[t.ID] = true
	}
	for id := range l.poll {
		if !inReview[id] {
			delete(l.poll, id)
		}
	}
	due := make([]factory.Task, 0, len(tasks))
	for _, t := range tasks {
		// Half an Every of slack keeps a late ticker from costing a whole tick.
		if p := l.poll[t.ID]; p == nil || !now.Before(p.next.Add(-l.Every/2)) {
			due = append(due, t)
		}
	}
	last := func(t factory.Task) time.Time {
		if p := l.poll[t.ID]; p != nil {
			return p.last
		}
		return time.Time{}
	}
	sort.SliceStable(due, func(i, j int) bool { return last(due[i]).Before(last(due[j])) })
	if len(due) > l.MaxPerTick {
		due = due[:l.MaxPerTick]
	}
	return due
}

func awaitsMerge(t factory.Task) bool {
	return t.State == factory.StateBlocked && t.PR.Number > 0 && t.Repo != "" && t.BlockedReason == ReadyReason
}

func (l *Loop) dueParked(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	if last, ok := l.polled[id]; ok && now.Sub(last) < blockedEvery*l.Every {
		return false
	}
	l.polled[id] = now
	return true
}

// wakes are the webhook events that can change what the loop would do.
var wakes = []string{"pull_request", "check_suite", "check_run", "workflow_run", "status", "issue_comment"}

// OnEvent evaluates the pull request an outside event is about right away
// instead of at the next tick. It works synchronously: a host calls it from a
// goroutine, not from the request that delivered the webhook.
func (l *Loop) OnEvent(ctx context.Context, ev factory.Event) {
	if ev.Repo == "" || !wakesLoop(ev.Name) {
		return
	}
	var tasks []factory.Task
	for _, st := range []factory.State{factory.StateReview, factory.StateBlocked} {
		ts, err := l.Store.ListTasks(ctx, factory.Filter{States: []factory.State{st}, Limit: taskLimit})
		if err != nil {
			slog.Warn("forge loop: list tasks for event", "event", ev.Name, "error", err)
			return
		}
		tasks = append(tasks, ts...)
	}
	for _, t := range tasks {
		if t.PR.Number == 0 || !strings.EqualFold(t.Repo, ev.Repo) || t.State == factory.StateBlocked && !awaitsMerge(t) {
			continue
		}
		if !eventConcerns(t, ev) {
			continue
		}
		if err := l.evaluate(ctx, t, true); err != nil && ctx.Err() == nil {
			slog.Warn("forge loop: evaluate on event", "task_id", t.ID, "event", ev.Name, "error", err)
		}
	}
}

func wakesLoop(name string) bool {
	for _, w := range wakes {
		if name == w || strings.HasPrefix(name, w+".") || strings.HasPrefix(name, w+"_") {
			return true
		}
	}
	return false
}

func eventConcerns(t factory.Task, ev factory.Event) bool {
	f := ev.Fields
	if n := f["number"]; n != "" {
		return n == strconv.Itoa(t.PR.Number)
	}
	if b := f["branch"]; b != "" && b == t.Branch {
		return true
	}
	if s := f["sha"]; s != "" && s == t.PR.HeadSHA {
		return true
	}
	return false
}

// claim keeps two evaluations of one task from overlapping in this process.
func (l *Loop) claim(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[id] {
		return false
	}
	l.inflight[id] = true
	return true
}

func (l *Loop) release(id string) {
	l.mu.Lock()
	delete(l.inflight, id)
	l.mu.Unlock()
}

// eval is the state of one evaluation.
type eval struct {
	l      *Loop
	t      factory.Task
	pr     *factory.PRInfo
	events []factory.TaskEvent
	// force: a webhook woke this evaluation (look at the comments now).
	force bool
	// changed: the evaluation saw something new (a new head, a CI transition).
	changed bool
	// acted: the evaluation resumed, blocked or merged the task, or judged it. If the task is back in review
	// later it carries new work, so it is due at the next tick.
	acted bool
	// waiting: the task only waits for CI or for GitHub to compute mergeability.
	waiting bool
	// settleAt is when a settling green head can first be judged.
	settleAt time.Time
}

func (l *Loop) evaluate(ctx context.Context, t factory.Task, force bool) (err error) {
	if t.Repo == "" || t.PR.Number == 0 {
		return nil
	}
	if !l.claim(t.ID) {
		return nil
	}
	defer l.release(t.ID)

	started := l.Now()
	e := &eval{l: l, t: t, force: force}
	if t.State == factory.StateReview {
		defer func() { l.schedule(t.ID, started, e, err) }()
	}

	pr, err := l.Forge.GetPR(ctx, t.Repo, t.PR.Number)
	if err != nil {
		if errors.Is(err, ErrNotFound) && t.State == factory.StateReview && l.current(ctx, t.ID) {
			return l.Controller.Block(ctx, t.ID, fmt.Sprintf("pull request #%d of %s was not found", t.PR.Number, t.Repo))
		}
		return err
	}
	e.pr = pr

	switch pr.State {
	case "merged":
		if !l.open(ctx, t.ID) {
			return nil
		}
		return l.Controller.Complete(ctx, t.ID, fmt.Sprintf("Pull request #%d was merged: %s", pr.Number, pr.URL))
	case "closed":
		if !l.open(ctx, t.ID) {
			return nil
		}
		return l.Controller.Fail(ctx, t.ID, "pull request closed without merging")
	}
	if t.State != factory.StateReview {
		return nil // parked for a human: only the merge or close above matters
	}
	if pr.Draft {
		return nil
	}

	events, err := l.Store.ListEvents(ctx, t.ID, eventLimit)
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	e.events = events

	if pr.HeadSHA != t.PR.HeadSHA {
		// New commits: the checks of the old head say nothing; the settle clock below is per head.
		moved := factory.PullRequest{Number: pr.Number, URL: pr.URL, HeadSHA: pr.HeadSHA}
		if _, err := l.Store.UpdateTask(ctx, t.ID, factory.Patch{IfState: []factory.State{factory.StateReview}, PR: &moved}); err != nil {
			if errors.Is(err, factory.ErrConflict) {
				return nil
			}
			return fmt.Errorf("record head %s: %w", shortSHA(pr.HeadSHA), err)
		}
		e.t.PR = moved
		e.changed = true
		l.note(ctx, t.ID, factory.EventPR, "new commits on the pull request", map[string]any{"sha": pr.HeadSHA})
	}

	if pr.Mergeable != nil && !*pr.Mergeable {
		return e.resume(ctx, "conflict", conflictFeedback(pr), nil)
	}

	checks, err := l.Forge.Checks(ctx, t.Repo, pr.HeadSHA)
	if err != nil {
		return fmt.Errorf("checks of %s: %w", shortSHA(pr.HeadSHA), err)
	}
	return e.judge(ctx, checks)
}

// schedule records when a task in review is due next. An evaluation that
// found nothing new doubles the gap (up to MaxGap, or the shorter pendingMaxGap
// while the task only waits for CI or GitHub); anything new, or a webhook,
// resets it to Every. A failed evaluation is retried at the next tick.
func (l *Loop) schedule(id string, started time.Time, e *eval, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := l.poll[id]
	if p == nil {
		p = &pollState{}
		l.poll[id] = p
	}
	p.last = started
	switch {
	case e.force || e.changed || e.acted:
		p.n = 0
	case err == nil:
		p.n++
	}
	if err != nil || e.acted {
		p.next = time.Time{}
		return
	}
	gap := l.gap(p.n, e.waiting)
	if !e.settleAt.IsZero() {
		// The judgement itself is due when the settle window ends: do not sleep through it.
		gap = min(gap, max(e.settleAt.Sub(started), l.Every))
	}
	p.next = started.Add(gap)
}

// gap is the wait after n consecutive evaluations that found nothing new.
func (l *Loop) gap(n int, waiting bool) time.Duration {
	limit := l.MaxGap
	if waiting {
		limit = min(limit, pendingMaxGap)
	}
	limit = max(limit, l.Every)
	g := l.Every
	for range n {
		if g >= limit {
			break
		}
		g *= 2
	}
	return min(g, limit)
}

// open reports whether the task is still one this loop acts on (in review, or parked for the merge).
func (l *Loop) open(ctx context.Context, id string) bool {
	t, err := l.Store.GetTask(ctx, id)
	return err == nil && (t.State == factory.StateReview || awaitsMerge(*t))
}

// current reports whether the task is still in review.
func (l *Loop) current(ctx context.Context, id string) bool {
	t, err := l.Store.GetTask(ctx, id)
	return err == nil && t.State == factory.StateReview
}

func (l *Loop) note(ctx context.Context, id, kind, text string, data map[string]any) {
	if err := l.Store.AppendEvent(ctx, factory.TaskEvent{TaskID: id, Kind: kind, Text: text, Data: data, At: l.Now()}); err != nil {
		slog.Warn("forge loop: record event", "task_id", id, "kind", kind, "error", err)
	}
}

func summarize(checks []factory.Check) (failed, pending []factory.Check, passed int) {
	for _, c := range checks {
		switch c.State {
		case factory.CheckFailed:
			failed = append(failed, c)
		case factory.CheckPassed:
			passed++
		default:
			pending = append(pending, c)
		}
	}
	return failed, pending, passed
}

func (e *eval) judge(ctx context.Context, checks []factory.Check) error {
	l, sha := e.l, e.pr.HeadSHA
	failed, pending, _ := summarize(checks)

	switch {
	case len(pending) > 0:
		e.waiting = true
		e.transition(ctx, sha, "pending", names(pending))
		if e.force {
			// A review event woke us: a human's request should not wait for CI to end.
			if fb, handled, err := e.requestedChanges(ctx); err != nil {
				return err
			} else if fb != "" {
				return e.resumeHuman(ctx, fb, handled)
			}
		}
		return nil
	case len(failed) > 0:
		e.transition(ctx, sha, "failed", names(failed))
		return e.ciFailed(ctx, failed)
	case len(checks) == 0 && l.require:
		e.waiting = true
		first := e.transition(ctx, sha, "pending", nil)
		if l.Now().Sub(first) >= noChecksFactor*l.Settle && l.current(ctx, e.t.ID) {
			return l.Controller.Block(ctx, e.t.ID, fmt.Sprintf("no CI check was reported on %s", shortSHA(sha)))
		}
		return nil
	}

	greenSince := e.transition(ctx, sha, "passed", nil)
	if l.Now().Sub(greenSince) < l.Settle {
		e.waiting, e.settleAt = true, greenSince.Add(l.Settle)
		return nil
	}
	return e.ready(ctx, checks)
}

// transition records the CI state of sha when it differs from the last one
// recorded and returns when that state began (for pending: when the head was
// first seen).
func (e *eval) transition(ctx context.Context, sha, state string, failing []string) time.Time {
	var first, last *factory.TaskEvent
	for i := range e.events {
		ev := &e.events[i]
		if ev.Kind != factory.EventCI || dataString(ev.Data, "sha") != sha {
			continue
		}
		if first == nil {
			first = ev
		}
		last = ev
	}
	if last != nil && dataString(last.Data, "state") == state {
		if state == "pending" {
			return first.At
		}
		return last.At
	}
	now := e.l.Now()
	data := map[string]any{"sha": sha, "state": state}
	if len(failing) > 0 {
		data["checks"] = failing
	}
	text := fmt.Sprintf("CI %s on %s", state, shortSHA(sha))
	if len(failing) > 0 {
		text += ": " + strings.Join(failing, ", ")
	}
	ev := factory.TaskEvent{TaskID: e.t.ID, Kind: factory.EventCI, Text: text, Data: data, At: now}
	if err := e.l.Store.AppendEvent(ctx, ev); err != nil {
		slog.Warn("forge loop: record event", "task_id", e.t.ID, "kind", factory.EventCI, "error", err)
	}
	e.events = append(e.events, ev)
	e.changed = true
	if first != nil && state == "pending" {
		return first.At
	}
	return now
}

func names(cs []factory.Check) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func dataString(d map[string]any, key string) string {
	s, _ := d[key].(string)
	return s
}

// ciFailed sends the failing checks and the end of their logs back to the agent.
func (e *eval) ciFailed(ctx context.Context, failed []factory.Check) error {
	l := e.l
	var b strings.Builder
	fmt.Fprintf(&b, "CI failed on pull request #%d (head %s). Failing checks: %s.\n", e.pr.Number, shortSHA(e.pr.HeadSHA), strings.Join(names(failed), ", "))
	var logs strings.Builder
	for i, c := range failed {
		if i >= maxFailedLogs {
			fmt.Fprintf(&b, "(%d more failing checks not shown)\n", len(failed)-maxFailedLogs)
			break
		}
		log, err := l.Forge.FailureLog(ctx, e.t.Repo, c)
		if err != nil {
			slog.Warn("forge loop: failure log", "task_id", e.t.ID, "check", c.Name, "error", err)
		}
		log = strings.TrimSpace(log)
		fmt.Fprintf(&b, "\n## %s\n", c.Name)
		if c.Detail != "" {
			fmt.Fprintf(&b, "%s\n", c.Detail)
		}
		if log == "" {
			if c.URL != "" {
				fmt.Fprintf(&b, "(no log available; see %s)\n", c.URL)
			} else {
				b.WriteString("(no log available)\n")
			}
			continue
		}
		log = tailRunes(log, logPerCheck)
		logs.WriteString(log)
		fmt.Fprintf(&b, "Log (end):\n%s\n", log)
	}
	b.WriteString("\nFix the failures on the same branch, commit them, then call open_pr again: it pushes your branch.")
	return e.resume(ctx, "ci", b.String(), &factory.Patch{AddFingerprint: factory.Fingerprint(logs.String())})
}

func conflictFeedback(pr *factory.PRInfo) string {
	return fmt.Sprintf("Pull request #%d conflicts with %s and cannot be merged. Rebase your branch onto the base and resolve the conflicts: "+
		"`git fetch origin %s` (add `--unshallow` first if the history is shallow), `git rebase origin/%s`, fix the conflicts, build and test, "+
		"commit, then call open_pr again: it pushes the rebased branch with --force-with-lease (you cannot push yourself).", pr.Number, pr.Base, pr.Base, pr.Base)
}

// rounds counts the automatic new attempts already made for the pull request.
func (e *eval) rounds() int {
	n := 0
	for _, ev := range e.events {
		if ev.Kind == factory.EventNote && ev.Data != nil && ev.Data["fix"] != nil {
			n++
		}
	}
	return n
}

// resume gives the next attempt its feedback, or blocks the task when the
// rounds are used up. Nothing happens to a task that left review meanwhile.
func (e *eval) resume(ctx context.Context, why, feedback string, extra *factory.Patch) error {
	l := e.l
	e.acted = true
	if n := e.rounds(); n >= l.MaxFixRounds {
		if !l.current(ctx, e.t.ID) {
			return nil
		}
		return l.Controller.Block(ctx, e.t.ID, fmt.Sprintf("still not mergeable after %d fix rounds (last: %s): needs a human", n, why))
	}
	if len(feedback) > maxFeedback {
		feedback = clipRunes(feedback, maxFeedback)
	}
	if extra != nil {
		p := *extra
		p.IfState = []factory.State{factory.StateReview}
		if _, err := l.Store.UpdateTask(ctx, e.t.ID, p); err != nil {
			if errors.Is(err, factory.ErrConflict) {
				return nil
			}
			return fmt.Errorf("update task: %w", err)
		}
	} else if !l.current(ctx, e.t.ID) {
		return nil
	}
	if err := l.Controller.Resume(ctx, e.t.ID, feedback); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	round := e.rounds() + 1
	l.note(ctx, e.t.ID, factory.EventNote, fmt.Sprintf("fix round %d/%d: %s", round, l.MaxFixRounds, why),
		map[string]any{"fix": round, "why": why, "sha": e.pr.HeadSHA})
	return nil
}

// ready runs once the checks are green and settled: requested changes, the
// gate, then the merge or the hand-over to a human.
func (e *eval) ready(ctx context.Context, checks []factory.Check) error {
	l := e.l
	if fb, handled, err := e.requestedChanges(ctx); err != nil {
		return err
	} else if fb != "" {
		return e.resumeHuman(ctx, fb, handled)
	}
	if e.pr.Mergeable == nil {
		e.waiting = true
		return nil // GitHub is still computing it
	}
	e.acted = true // from here on the evaluation judges, blocks or merges

	if l.Gate != nil {
		if done, err := e.gate(ctx, checks); err != nil || done {
			return err
		}
	}

	if !e.t.AutoMerge {
		if !l.current(ctx, e.t.ID) {
			return nil
		}
		return l.Controller.Block(ctx, e.t.ID, ReadyReason)
	}
	// The task could have left review while the gate ran: merging cannot be undone.
	if !l.current(ctx, e.t.ID) {
		return nil
	}
	if err := l.Forge.Merge(ctx, e.t.Repo, e.pr.Number, e.pr.HeadSHA, l.MergeMethod); err != nil {
		var ae *APIError
		switch {
		case errors.Is(err, ErrConflict):
			return nil // the head moved: the next evaluation judges the new one
		case errors.As(err, &ae) && !ae.RateLimited && (ae.Status == http.StatusMethodNotAllowed || ae.Status == http.StatusForbidden || ae.Status == http.StatusUnprocessableEntity):
			if !l.current(ctx, e.t.ID) {
				return nil
			}
			return l.Controller.Block(ctx, e.t.ID, "GitHub refused the merge: "+Redact(err.Error()))
		}
		return err
	}
	return l.Controller.Complete(ctx, e.t.ID, fmt.Sprintf("Pull request #%d merged (%s): %s", e.pr.Number, l.MergeMethod, e.pr.URL))
}

// gate asks the review gate about the head, once per head: a verdict already
// recorded for it is reused (a restart or a retried merge does not pay for a
// second review). done reports that the task was resumed or blocked.
func (e *eval) gate(ctx context.Context, checks []factory.Check) (done bool, err error) {
	l, sha := e.l, e.pr.HeadSHA
	verdict, errCount := e.recorded(sha)
	if verdict == "approve" {
		return false, nil
	}
	if errCount >= maxGateErrors {
		if !l.current(ctx, e.t.ID) {
			return true, nil
		}
		return true, l.Controller.Block(ctx, e.t.ID, fmt.Sprintf("the review gate failed %d times on %s: needs a human", errCount, shortSHA(sha)))
	}
	before := e.lastEventID()
	v, jerr := l.Gate.Judge(ctx, e.t, *e.pr, checks)
	if jerr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		e.record(ctx, before, sha, "error", jerr.Error())
		return true, fmt.Errorf("review gate: %w", jerr)
	}
	if v.Approve {
		e.record(ctx, before, sha, "approve", v.Summary)
		e.post(ctx, func() error {
			return l.Forge.Review(ctx, e.t.Repo, e.pr.Number, "APPROVE", "Approved by the automated review: "+v.Summary)
		}, func() error {
			return l.Forge.Comment(ctx, e.t.Repo, e.pr.Number, "Approved by the automated review: "+v.Summary)
		})
		return false, nil
	}
	e.record(ctx, before, sha, "changes", v.Summary)
	findings := reviewText(v)
	if cerr := l.Forge.Comment(ctx, e.t.Repo, e.pr.Number, "Automated review: changes needed.\n\n"+findings); cerr != nil {
		slog.Warn("forge loop: post review findings", "task_id", e.t.ID, "error", cerr)
	}
	if v.Hard {
		if !l.current(ctx, e.t.ID) {
			return true, nil
		}
		return true, l.Controller.Block(ctx, e.t.ID, "the review found a problem another attempt cannot fix: "+clipRunes(v.Summary, 400))
	}
	fb := "The automated review of pull request #" + strconv.Itoa(e.pr.Number) + " found problems:\n\n" + findings +
		"\n\nFix them on the same branch, commit, then call open_pr again: it pushes your branch."
	return true, e.resume(ctx, "review", fb, nil)
}

func reviewText(v factory.Verdict) string {
	var b strings.Builder
	if s := strings.TrimSpace(v.Summary); s != "" {
		b.WriteString(s)
	}
	for _, f := range v.Findings {
		if f = strings.TrimSpace(f); f != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString("- " + f)
		}
	}
	if b.Len() == 0 {
		return "The review did not approve the change and gave no details."
	}
	return b.String()
}

// post tries the first way of posting and falls back to the second when GitHub
// refuses it (an author cannot approve their own pull request); a failure of
// both is only logged: the post is a courtesy, the merge does not depend on it.
func (e *eval) post(ctx context.Context, first, fallback func() error) {
	err := first()
	if err == nil {
		return
	}
	if errors.Is(err, ErrUnprocessable) || errors.Is(err, ErrForbidden) {
		if err = fallback(); err == nil {
			return
		}
	}
	slog.Warn("forge loop: post review", "task_id", e.t.ID, "error", err)
}

// recorded is the latest verdict recorded for sha and the count of gate errors since the last real verdict.
func (e *eval) recorded(sha string) (verdict string, errors int) {
	for _, ev := range e.events {
		if ev.Kind != factory.EventReview || dataString(ev.Data, "sha") != sha {
			continue
		}
		verdict = dataString(ev.Data, "verdict")
		if verdict == "error" {
			errors++
		} else {
			errors = 0
		}
	}
	if verdict == "error" {
		verdict = ""
	}
	return verdict, errors
}

func (e *eval) lastEventID() int64 {
	var id int64
	for _, ev := range e.events {
		id = max(id, ev.ID)
	}
	return id
}

// record writes the verdict as a review event unless the gate wrote one itself.
func (e *eval) record(ctx context.Context, before int64, sha, verdict, text string) {
	if evs, err := e.l.Store.ListEvents(ctx, e.t.ID, eventLimit); err == nil {
		e.events = evs
		for _, ev := range evs {
			if ev.ID > before && ev.Kind == factory.EventReview && dataString(ev.Data, "sha") == sha {
				return
			}
		}
	}
	ev := factory.TaskEvent{TaskID: e.t.ID, Kind: factory.EventReview, Text: clipRunes(text, 600),
		Data: map[string]any{"sha": sha, "verdict": verdict}, At: e.l.Now()}
	if err := e.l.Store.AppendEvent(ctx, ev); err != nil {
		slog.Warn("forge loop: record event", "task_id", e.t.ID, "kind", factory.EventReview, "error", err)
	}
	e.events = append(e.events, ev)
}

func commentKey(c factory.Comment) string {
	sum := sha256.Sum256([]byte(c.Author + "\x00" + c.Path + "\x00" + strconv.Itoa(c.Line) + "\x00" + c.Review + "\x00" + c.Body))
	return hex.EncodeToString(sum[:8])
}

// handledKeys are the comments an earlier round already passed to the agent.
func (e *eval) handledKeys() map[string]bool {
	out := map[string]bool{}
	for _, ev := range e.events {
		if ev.Kind != factory.EventNote || ev.Data == nil {
			continue
		}
		switch ks := ev.Data["handled"].(type) {
		case []string:
			for _, k := range ks {
				out[k] = true
			}
		case []any:
			for _, k := range ks {
				if s, ok := k.(string); ok {
					out[s] = true
				}
			}
		}
	}
	return out
}

// requestedChanges is the text of the reviews that asked for changes since the
// last round (with the inline comments that came with them), and the keys of
// all comments now present. A review counts while its author's latest verdict
// is still "changes requested".
func (e *eval) requestedChanges(ctx context.Context) (string, []string, error) {
	comments, err := e.l.Forge.Comments(ctx, e.t.Repo, e.pr.Number)
	if err != nil {
		return "", nil, fmt.Errorf("comments of #%d: %w", e.pr.Number, err)
	}
	handled := e.handledKeys()
	latest := map[string]string{}
	keys := make([]string, 0, len(comments))
	for _, c := range comments {
		keys = append(keys, commentKey(c))
		if c.Review != "" {
			latest[c.Author] = c.Review
		}
	}
	var asks, inline []string
	for _, c := range comments {
		if handled[commentKey(c)] {
			continue
		}
		switch {
		case c.Review == "changes_requested" && latest[c.Author] == "changes_requested":
			text := strings.TrimSpace(c.Body)
			if text == "" {
				text = "(no text)"
			}
			asks = append(asks, fmt.Sprintf("%s requested changes: %s", c.Author, clipRunes(text, 2000)))
		case c.Path != "":
			line := c.Path
			if c.Line > 0 {
				line += ":" + strconv.Itoa(c.Line)
			}
			inline = append(inline, fmt.Sprintf("%s on %s: %s", c.Author, line, clipRunes(strings.TrimSpace(c.Body), 1000)))
		}
	}
	if len(asks) == 0 {
		return "", nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "A reviewer asked for changes on pull request #%d:\n\n%s", e.pr.Number, strings.Join(asks, "\n\n"))
	if len(inline) > 0 {
		b.WriteString("\n\nInline comments:\n" + strings.Join(inline, "\n"))
	}
	b.WriteString("\n\nAddress them on the same branch, commit, then call open_pr again: it pushes your branch.")
	if len(keys) > maxHandledKeys {
		keys = keys[len(keys)-maxHandledKeys:]
	}
	return b.String(), keys, nil
}

func (e *eval) resumeHuman(ctx context.Context, feedback string, handled []string) error {
	e.acted = true
	if n := e.rounds(); n >= e.l.MaxFixRounds {
		return e.resume(ctx, "review", feedback, nil)
	}
	if !e.l.current(ctx, e.t.ID) {
		return nil
	}
	if len(feedback) > maxFeedback {
		feedback = clipRunes(feedback, maxFeedback)
	}
	if err := e.l.Controller.Resume(ctx, e.t.ID, feedback); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	round := e.rounds() + 1
	e.l.note(ctx, e.t.ID, factory.EventNote, fmt.Sprintf("fix round %d/%d: reviewer asked for changes", round, e.l.MaxFixRounds),
		map[string]any{"fix": round, "why": "human", "sha": e.pr.HeadSHA, "handled": handled})
	return nil
}
