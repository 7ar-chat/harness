package forge_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/forge"
)

type loopEnv struct {
	g     *fakeGitHub
	clk   *clock
	store factory.Store
	c     *ctrl
	gate  *gate
	task  string
}

type loopCfg struct {
	autoMerge bool
	noGate    bool
	mutate    func(o *forge.LoopOptions)
}

// newLoopEnv has a task in review whose pull request #7 is at sha1 with the given checks.
func newLoopEnv(t *testing.T, cfg loopCfg, checks ...map[string]any) *loopEnv {
	t.Helper()
	clk := newClock()
	st := newStore(clk)
	g := newFakeGitHub(t)
	g.set(func() { g.runs[sha1] = checks })
	task, _, err := st.CreateTask(context.Background(), factory.Task{Title: "T", Owner: "u", Repo: repo, BaseBranch: "main", Branch: "har/abc", AutoMerge: cfg.autoMerge})
	if err != nil {
		t.Fatal(err)
	}
	e := &loopEnv{g: g, clk: clk, store: st, c: &ctrl{store: st}, gate: &gate{verdict: factory.Verdict{Approve: true, Summary: "Looks good."}}, task: task.ID}
	e.toReview(t, sha1)
	return e
}

func (e *loopEnv) toReview(t *testing.T, sha string) {
	t.Helper()
	if err := e.c.Review(context.Background(), e.task, factory.PullRequest{Number: 7, URL: "https://github.com/acme/app/pull/7", HeadSHA: sha}); err != nil {
		t.Fatal(err)
	}
}

func (e *loopEnv) loop(cfg loopCfg) *forge.Loop {
	o := forge.LoopOptions{Forge: e.g.client(), Store: e.store, Controller: e.c, Now: e.clk.Now, Every: time.Second}
	if !cfg.noGate {
		o.Gate = e.gate
	}
	if cfg.mutate != nil {
		cfg.mutate(&o)
	}
	return forge.NewLoop(o)
}

func (e *loopEnv) get(t *testing.T) *factory.Task {
	t.Helper()
	got, err := e.store.GetTask(context.Background(), e.task)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *loopEnv) events(t *testing.T, kind string) []factory.TaskEvent {
	t.Helper()
	all, _ := e.store.ListEvents(context.Background(), e.task, 0)
	var out []factory.TaskEvent
	for _, ev := range all {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

func tick(t *testing.T, l *forge.Loop) {
	t.Helper()
	if err := l.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func green() map[string]any { return run("build", "completed", "success", "") }

const (
	mergeReq = "PUT /repos/acme/app/pulls/7/merge"
	reviews  = "POST /repos/acme/app/pulls/7/reviews"
	comments = "POST /repos/acme/app/issues/7/comments"
)

func TestPendingChecksWait(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green(), run("test", "in_progress", "", ""))
	l := e.loop(cfg)
	// Act
	tick(t, l)
	e.clk.Advance(10 * time.Minute)
	tick(t, l)
	// Assert
	if got := e.c.names(); len(got) != 1 || got[0] != "review" {
		t.Fatalf("controller calls = %v, want only the setup review", got)
	}
	if e.get(t).State != factory.StateReview || e.gate.n() != 0 || e.g.count(mergeReq) != 0 {
		t.Fatal("a pending check must not be judged or merged, however long it takes")
	}
	ci := e.events(t, factory.EventCI)
	if len(ci) != 1 || ci[0].Data["state"] != "pending" || ci[0].Data["sha"] != sha1 {
		t.Fatalf("ci events = %+v", ci)
	}
}

func TestFailedChecksResumeWithLog(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, green(), run("unit tests", "completed", "failure", "11"), run("lint", "completed", "failure", "12"))
	e.g.set(func() {
		e.g.logs["11"] = "=== RUN TestAdd\n--- FAIL: TestAdd (0.00s)\n    add_test.go:12: got 3 want 4\nFAIL\n"
		e.g.logs["12"] = "lint: unused import \"os\"\n"
	})
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	task := e.get(t)
	if task.State != factory.StateReady {
		t.Fatalf("state = %s, want ready (new attempt)", task.State)
	}
	for _, want := range []string{"unit tests", "lint", "--- FAIL: TestAdd", "got 3 want 4", "unused import", "same branch", "commit them, then call open_pr again: it pushes your branch"} {
		if !strings.Contains(task.Feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, task.Feedback)
		}
	}
	ci := e.events(t, factory.EventCI)
	last := ci[len(ci)-1]
	if last.Data["state"] != "failed" || last.Data["sha"] != sha1 {
		t.Fatalf("ci event = %+v", last)
	}
	if len(task.Fingerprints) != 1 || task.Fingerprints[0] == "" {
		t.Fatalf("fingerprints = %v, want the signature of the failure", task.Fingerprints)
	}
	if e.gate.n() != 0 {
		t.Fatal("a red head must not reach the gate")
	}
}

func TestFailedWithoutLogStillResumes(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, run("ext", "completed", "failure", ""))
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	if fb := e.get(t).Feedback; !strings.Contains(fb, "ext") || !strings.Contains(fb, "no log available") {
		t.Fatalf("feedback = %q", fb)
	}
}

func TestPendingBeatsFailureSoLateGreenIsNotMissed(t *testing.T) {
	// Arrange: one check failed but another is still running: wait for the full picture.
	e := newLoopEnv(t, loopCfg{}, run("a", "completed", "failure", "11"), run("b", "in_progress", "", ""))
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	if e.get(t).State != factory.StateReview {
		t.Fatal("must wait for pending checks before resuming")
	}
}

func TestFixRoundsAreCapped(t *testing.T) {
	// Arrange
	cfg := loopCfg{mutate: func(o *forge.LoopOptions) { o.MaxFixRounds = 2 }}
	e := newLoopEnv(t, cfg, run("test", "completed", "failure", ""))
	l := e.loop(cfg)
	// Act: two rounds resume, the third blocks.
	for range 2 {
		tick(t, l)
		if e.get(t).State != factory.StateReady {
			t.Fatalf("state = %s, want ready", e.get(t).State)
		}
		e.toReview(t, sha1)
	}
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateBlocked || !strings.Contains(task.BlockedReason, "2 fix rounds") {
		t.Fatalf("state = %s reason = %q", task.State, task.BlockedReason)
	}
	if n := countOf(e.c.names(), "resume"); n != 2 {
		t.Fatalf("resumes = %d, want 2", n)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

func TestGreenApprovedAutoMergeMergesWithShaGuard(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	// Act
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateDone || !strings.Contains(task.Result, "#7") {
		t.Fatalf("state = %s result = %q", task.State, task.Result)
	}
	sent := e.g.body(mergeReq)
	if len(sent) != 1 || sent[0]["sha"] != sha1 || sent[0]["merge_method"] != "squash" {
		t.Fatalf("merge body = %v", sent)
	}
	if rv := e.g.body(reviews); len(rv) != 1 || rv[0]["event"] != "APPROVE" || !strings.Contains(rv[0]["body"].(string), "Looks good.") {
		t.Fatalf("approval = %v", rv)
	}
	if rev := e.events(t, factory.EventReview); len(rev) != 1 || rev[0].Data["verdict"] != "approve" || rev[0].Data["sha"] != sha1 {
		t.Fatalf("review events = %+v", rev)
	}
}

func TestMergeMethodIsConfigurable(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true, noGate: true, mutate: func(o *forge.LoopOptions) { o.MergeMethod = factory.MergeRebase; o.Settle = time.Second }}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	// Act
	tick(t, l)
	e.clk.Advance(2 * time.Second)
	tick(t, l)
	// Assert
	if sent := e.g.body(mergeReq); len(sent) != 1 || sent[0]["merge_method"] != "rebase" {
		t.Fatalf("merge body = %v", sent)
	}
	if e.get(t).State != factory.StateDone || e.g.count(reviews) != 0 {
		t.Fatal("without a gate nothing is reviewed or approved")
	}
}

func TestSettleWindowDelaysTheJudgement(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	// Act + Assert
	tick(t, l)
	if e.gate.n() != 0 {
		t.Fatal("judged at the first green sight")
	}
	e.clk.Advance(59 * time.Second)
	tick(t, l)
	if e.gate.n() != 0 || e.g.count(mergeReq) != 0 {
		t.Fatal("judged before the settle window ended")
	}
	e.clk.Advance(2 * time.Second)
	tick(t, l)
	if e.get(t).State != factory.StateDone {
		t.Fatalf("state = %s after the window", e.get(t).State)
	}
}

func TestLateRegisteringCheckRestartsTheSettleWindow(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(50 * time.Second)
	// Act: a second workflow registers late and is still running.
	e.g.set(func() { e.g.runs[sha1] = append(e.g.runs[sha1], run("e2e", "queued", "", "")) })
	tick(t, l)
	e.clk.Advance(30 * time.Second) // 80 s after the first green sight, but the window restarts at the next green
	e.g.set(func() { e.g.runs[sha1][1] = run("e2e", "completed", "success", "") })
	tick(t, l)
	// Assert
	if e.g.count(mergeReq) != 0 || e.gate.n() != 0 {
		t.Fatal("merged although a check registered late and only just turned green")
	}
	e.clk.Advance(61 * time.Second)
	tick(t, l)
	if e.get(t).State != factory.StateDone {
		t.Fatalf("state = %s", e.get(t).State)
	}
}

func TestNewCommitsResetTheSettleTimer(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(50 * time.Second)
	// Act: the agent pushed; the new head has green checks already.
	e.g.set(func() { e.g.headSHA = sha2; e.g.runs[sha2] = []map[string]any{green()} })
	tick(t, l)
	e.clk.Advance(30 * time.Second)
	tick(t, l)
	// Assert
	if e.g.count(mergeReq) != 0 {
		t.Fatal("the new head merged 30 s after it appeared")
	}
	if got := e.get(t).PR.HeadSHA; got != sha2 {
		t.Fatalf("recorded head = %s, want the new one", got)
	}
	e.clk.Advance(31 * time.Second)
	tick(t, l)
	if sent := e.g.body(mergeReq); len(sent) != 1 || sent[0]["sha"] != sha2 {
		t.Fatalf("merge body = %v, want the new head as guard", sent)
	}
}

func TestGreenWithoutAutoMergeBlocksForAHuman(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: false}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	// Act
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateBlocked || task.BlockedReason != forge.ReadyReason || !strings.Contains(task.BlockedReason, "awaiting a human") {
		t.Fatalf("state = %s reason = %q", task.State, task.BlockedReason)
	}
	if e.g.count(mergeReq) != 0 {
		t.Fatal("merged without AutoMerge")
	}
	if rv := e.g.body(reviews); len(rv) != 1 || rv[0]["event"] != "APPROVE" {
		t.Fatalf("approval = %v", rv)
	}
}

func TestParkedTaskIsCompletedWhenTheHumanMerges(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, green())
	l := e.loop(loopCfg{})
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	tick(t, l)
	if e.get(t).State != factory.StateBlocked {
		t.Fatal("setup: not parked")
	}
	// Act: a human merges on GitHub; the next poll of parked tasks notices.
	e.g.set(func() { e.g.merged, e.g.state = true, "closed" })
	e.clk.Advance(time.Minute)
	tick(t, l)
	// Assert
	if got := e.get(t); got.State != factory.StateDone {
		t.Fatalf("state = %s", got.State)
	}
}

func TestGateRejectCommentsAndResumes(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{autoMerge: true}, green())
	e.gate.verdict = factory.Verdict{Summary: "Missing tests.", Findings: []string{"add a test for Add", "handle nil input"}}
	l := e.loop(loopCfg{autoMerge: true})
	// Act
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateReady {
		t.Fatalf("state = %s", task.State)
	}
	for _, want := range []string{"Missing tests.", "add a test for Add", "handle nil input", "commit, then call open_pr again: it pushes your branch"} {
		if !strings.Contains(task.Feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, task.Feedback)
		}
	}
	cm := e.g.body(comments)
	if len(cm) != 1 || !strings.Contains(cm[0]["body"].(string), "handle nil input") {
		t.Fatalf("PR comment = %v", cm)
	}
	if e.g.count(mergeReq) != 0 || e.g.count(reviews) != 0 {
		t.Fatal("a rejected pull request was merged or approved")
	}
	if rev := e.events(t, factory.EventReview); len(rev) != 1 || rev[0].Data["verdict"] != "changes" {
		t.Fatalf("review events = %+v", rev)
	}
}

func TestGateHardRejectBlocks(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{autoMerge: true}, green())
	e.gate.verdict = factory.Verdict{Summary: "Touches .github/workflows.", Hard: true}
	l := e.loop(loopCfg{autoMerge: true})
	// Act
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateBlocked || !strings.Contains(task.BlockedReason, "workflows") {
		t.Fatalf("state = %s reason = %q", task.State, task.BlockedReason)
	}
	if e.g.count(comments) != 1 {
		t.Fatal("the findings must still be posted for the human")
	}
}

func TestGateIsAskedOncePerHead(t *testing.T) {
	// Arrange: a merge that keeps failing with a conflict (head moved) is retried; the verdict is reused.
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	e.g.set(func() { e.g.mergeStatus = 409 })
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	// Act
	tick(t, l)
	tick(t, l)
	// Assert
	if e.gate.n() != 1 {
		t.Fatalf("gate called %d times for one head, want 1", e.gate.n())
	}
	if e.get(t).State != factory.StateReview {
		t.Fatalf("state = %s, want review (head moved, try again)", e.get(t).State)
	}
}

func TestGateErrorsEscalateAfterRetries(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{autoMerge: true}, green())
	e.gate.err = errors.New("model unavailable")
	l := e.loop(loopCfg{autoMerge: true})
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	// Act
	for range 3 {
		if err := l.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "model unavailable") {
			t.Fatalf("tick error = %v", err)
		}
	}
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateBlocked || !strings.Contains(task.BlockedReason, "gate failed 3 times") {
		t.Fatalf("state = %s reason = %q", task.State, task.BlockedReason)
	}
	if e.g.count(mergeReq) != 0 {
		t.Fatal("merged although the gate never answered")
	}
}

func TestTaskThatLeftReviewDuringTheGateIsNotMerged(t *testing.T) {
	// Arrange: the task is cancelled while the (slow) gate thinks.
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	cancelled := factory.StateCancelled
	l := forge.NewLoop(forge.LoopOptions{Forge: e.g.client(), Store: e.store, Controller: e.c, Now: e.clk.Now,
		Gate: gateFunc(func(ctx context.Context) {
			_, _ = e.store.UpdateTask(ctx, e.task, factory.Patch{State: &cancelled})
		})})
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	// Act
	tick(t, l)
	// Assert
	if e.g.count(mergeReq) != 0 {
		t.Fatal("merged a task that was cancelled")
	}
	if got := e.get(t); got.State != factory.StateCancelled {
		t.Fatalf("state = %s", got.State)
	}
}

type gateFunc func(ctx context.Context)

// Judge runs the hook (the task changes while the gate thinks) and approves.
func (f gateFunc) Judge(ctx context.Context, t factory.Task, pr factory.PRInfo, c []factory.Check) (factory.Verdict, error) {
	f(ctx)
	return factory.Verdict{Approve: true}, nil
}

func TestClosedWithoutMergeFails(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, green())
	e.g.set(func() { e.g.state = "closed" })
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	task := e.get(t)
	if task.State != factory.StateFailed || task.Error != "pull request closed without merging" {
		t.Fatalf("state = %s error = %q", task.State, task.Error)
	}
}

func TestMergedCompletes(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, run("test", "completed", "failure", ""))
	e.g.set(func() { e.g.merged, e.g.state = true, "closed" })
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	task := e.get(t)
	if task.State != factory.StateDone || !strings.Contains(task.Result, "merged") {
		t.Fatalf("state = %s result = %q", task.State, task.Result)
	}
}

func TestConflictResumesWithRebaseInstructions(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{autoMerge: true}, green())
	no := false
	e.g.set(func() { e.g.mergeable = &no })
	// Act
	tick(t, e.loop(loopCfg{autoMerge: true}))
	// Assert
	task := e.get(t)
	if task.State != factory.StateReady || !strings.Contains(task.Feedback, "git rebase origin/main") || !strings.Contains(task.Feedback, "conflicts") {
		t.Fatalf("state = %s feedback = %q", task.State, task.Feedback)
	}
	if !strings.Contains(task.Feedback, "call open_pr again: it pushes the rebased branch with --force-with-lease") || strings.Contains(task.Feedback, "git push") {
		t.Fatalf("the conflict feedback must hand the push to open_pr: %q", task.Feedback)
	}
}

func TestMergeabilityNotYetKnownWaits(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	e.g.set(func() { e.g.mergeable = nil })
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(2 * time.Minute)
	// Act
	tick(t, l)
	// Assert
	if e.g.count(mergeReq) != 0 || e.get(t).State != factory.StateReview {
		t.Fatal("must wait until GitHub has computed mergeability")
	}
}

func TestRequestedChangesResumeOnce(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	e.g.set(func() {
		e.g.reviews = []map[string]any{{"user": user("bob"), "body": "Please extract a helper.", "state": "CHANGES_REQUESTED"}}
		e.g.inline = []map[string]any{{"user": user("bob"), "body": "this is duplicated", "path": "x.go", "line": 9}}
	})
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	// Act
	tick(t, l)
	// Assert
	task := e.get(t)
	if task.State != factory.StateReady {
		t.Fatalf("state = %s", task.State)
	}
	for _, want := range []string{"bob requested changes: Please extract a helper.", "x.go:9", "this is duplicated", "commit, then call open_pr again: it pushes your branch"} {
		if !strings.Contains(task.Feedback, want) {
			t.Errorf("feedback lacks %q:\n%s", want, task.Feedback)
		}
	}
	// The same review does not send the task round again once the agent answered it.
	e.toReview(t, sha1)
	e.clk.Advance(2 * time.Minute)
	tick(t, l)
	tick(t, l)
	if n := countOf(e.c.names(), "resume"); n != 1 {
		t.Fatalf("resumes = %d, want 1 for one review", n)
	}
	if e.get(t).State != factory.StateDone {
		t.Fatalf("state = %s, want merged after the handled review", e.get(t).State)
	}
}

func TestApprovalSupersedesEarlierRequestedChanges(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true, noGate: true, mutate: func(o *forge.LoopOptions) { o.Settle = time.Second }}
	e := newLoopEnv(t, cfg, green())
	e.g.set(func() {
		e.g.reviews = []map[string]any{
			{"user": user("bob"), "body": "fix", "state": "CHANGES_REQUESTED"},
			{"user": user("bob"), "body": "", "state": "APPROVED"},
		}
	})
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(2 * time.Second)
	// Act
	tick(t, l)
	// Assert
	if e.get(t).State != factory.StateDone {
		t.Fatalf("state = %s, want done: bob approved later", e.get(t).State)
	}
}

func TestNoChecksAtAll(t *testing.T) {
	t.Run("required: waits, then escalates", func(t *testing.T) {
		// Arrange
		cfg := loopCfg{autoMerge: true, noGate: true}
		e := newLoopEnv(t, cfg)
		l := e.loop(cfg)
		// Act
		tick(t, l)
		e.clk.Advance(5 * time.Minute)
		tick(t, l)
		if e.g.count(mergeReq) != 0 || e.get(t).State != factory.StateReview {
			t.Fatal("merged a head that reports no check")
		}
		e.clk.Advance(6 * time.Minute)
		tick(t, l)
		// Assert
		if got := e.get(t); got.State != factory.StateBlocked || !strings.Contains(got.BlockedReason, "no CI check") {
			t.Fatalf("state = %s reason = %q", got.State, got.BlockedReason)
		}
	})
	t.Run("allowed: merges after the settle window", func(t *testing.T) {
		// Arrange
		cfg := loopCfg{autoMerge: true, noGate: true, mutate: func(o *forge.LoopOptions) { o.AllowNoChecks = true }}
		e := newLoopEnv(t, cfg)
		l := e.loop(cfg)
		// Act
		tick(t, l)
		e.clk.Advance(61 * time.Second)
		tick(t, l)
		// Assert
		if e.get(t).State != factory.StateDone {
			t.Fatalf("state = %s", e.get(t).State)
		}
	})
}

func TestProgressSurvivesARestart(t *testing.T) {
	// Arrange: one loop sees the green head; another process continues.
	cfg := loopCfg{autoMerge: true}
	e := newLoopEnv(t, cfg, green())
	tick(t, e.loop(cfg))
	e.clk.Advance(61 * time.Second)
	// Act
	tick(t, e.loop(cfg))
	// Assert
	if e.get(t).State != factory.StateDone {
		t.Fatalf("state = %s: the settle clock must live in the task events", e.get(t).State)
	}
}

func TestDraftIsLeftAlone(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true, noGate: true, mutate: func(o *forge.LoopOptions) { o.Settle = time.Second }}
	e := newLoopEnv(t, cfg, green())
	e.g.set(func() { e.g.draft = true })
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(time.Minute)
	// Act
	tick(t, l)
	// Assert
	if e.g.count(mergeReq) != 0 || e.get(t).State != factory.StateReview {
		t.Fatal("a draft pull request was merged")
	}
}

func TestOnlyTasksInReviewWithAPullRequestAreEvaluated(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, run("test", "completed", "failure", ""))
	running := factory.StateRunning
	_, _ = e.store.UpdateTask(context.Background(), e.task, factory.Patch{State: &running})
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	if len(e.g.requests) != 0 {
		t.Fatalf("requests for a running task: %v", e.g.requests)
	}
}

func TestWebhookEventWakesThePullRequest(t *testing.T) {
	// Arrange: green and settled, but the loop only polls every hour.
	cfg := loopCfg{autoMerge: true, noGate: true, mutate: func(o *forge.LoopOptions) { o.Every = time.Hour }}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	tick(t, l)
	e.clk.Advance(61 * time.Second)
	ev := func(name, r string, fields map[string]string) factory.Event {
		return factory.Event{Name: name, Repo: r, Fields: fields}
	}
	// Act + Assert: events that are not about this pull request do nothing.
	l.OnEvent(context.Background(), ev("check_suite.completed", "other/repo", map[string]string{"number": "7"}))
	l.OnEvent(context.Background(), ev("check_suite.completed", repo, map[string]string{"number": "8"}))
	l.OnEvent(context.Background(), ev("issues.opened", repo, map[string]string{"number": "7"}))
	if e.get(t).State != factory.StateReview {
		t.Fatal("an unrelated event woke the task")
	}
	// The event that names the PR evaluates it now.
	l.OnEvent(context.Background(), ev("check_suite.completed", repo, map[string]string{"number": "7"}))
	if e.get(t).State != factory.StateDone {
		t.Fatalf("state = %s, want done after the wake-up", e.get(t).State)
	}
}

func TestWebhookEventMatchesByBranchWhenNoNumber(t *testing.T) {
	// Arrange
	cfg := loopCfg{autoMerge: true, noGate: true, mutate: func(o *forge.LoopOptions) { o.Settle = time.Second }}
	e := newLoopEnv(t, cfg, run("test", "completed", "failure", ""))
	l := e.loop(cfg)
	// Act
	l.OnEvent(context.Background(), factory.Event{Name: "check_run.completed", Repo: repo, Fields: map[string]string{"branch": "har/abc", "sha": sha1}})
	// Assert
	if e.get(t).State != factory.StateReady {
		t.Fatalf("state = %s, want the red run to resume the task", e.get(t).State)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, green())
	l := e.loop(loopCfg{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// Act
	go func() { l.Run(ctx); close(done) }()
	cancel()
	// Assert
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestTickReportsAPIFailureAndContinues(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, green())
	e.g.set(func() { e.g.failNext["GET /repos/acme/app/pulls/7"] = []failure{{status: 500, message: "boom"}} })
	// Act
	err := e.loop(loopCfg{}).Tick(context.Background())
	// Assert
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if e.get(t).State != factory.StateReview {
		t.Fatal("a transient failure must leave the task in review")
	}
}

func TestMissingPullRequestBlocks(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, green())
	e.g.set(func() { e.g.failNext["GET /repos/acme/app/pulls/7"] = []failure{{status: 404, message: "Not Found"}} })
	// Act
	tick(t, e.loop(loopCfg{}))
	// Assert
	if got := e.get(t); got.State != factory.StateBlocked || !strings.Contains(got.BlockedReason, "not found") {
		t.Fatalf("state = %s reason = %q", got.State, got.BlockedReason)
	}
}
