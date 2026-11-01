package forge_test

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/forge"
)

const getPR = "GET /repos/acme/app/pulls/7"

func pending() map[string]any { return run("test", "in_progress", "", "") }

// ticks runs n ticks, advancing the clock by one Every (the env loops poll every second) after each.
func ticks(t *testing.T, e *loopEnv, l *forge.Loop, n int) {
	t.Helper()
	for range n {
		tick(t, l)
		e.clk.Advance(time.Second)
	}
}

func TestUnchangedPullRequestIsPolledLessAndLess(t *testing.T) {
	// Arrange
	e := newLoopEnv(t, loopCfg{}, pending())
	l := e.loop(loopCfg{})
	// Act
	ticks(t, e, l, 40)
	atForty := e.g.count(getPR)
	ticks(t, e, l, 360)
	atFourHundred := e.g.count(getPR)
	// Assert: gaps of 1, 2, 4 ... 32 s, then 64 s and the 2 min cap for a task that only waits for CI.
	if atForty < 5 || atForty > 10 {
		t.Fatalf("GetPR calls over 40 ticks = %d, want 5..10", atForty)
	}
	if more := atFourHundred - atForty; more > 6 {
		t.Fatalf("GetPR calls over the next 360 ticks = %d, want logarithmic growth (<= 6)", more)
	}
	if e.get(t).State != factory.StateReview {
		t.Fatalf("state = %s", e.get(t).State)
	}
}

func TestMaxGapCapsTheBackoff(t *testing.T) {
	// Arrange: a draft shows nothing new, and has no CI-wait cap, so only MaxGap limits the gap.
	cfg := loopCfg{mutate: func(o *forge.LoopOptions) { o.MaxGap = 4 * time.Second }}
	e := newLoopEnv(t, cfg, pending())
	e.g.set(func() { e.g.draft = true })
	l := e.loop(cfg)
	// Act
	ticks(t, e, l, 40)
	// Assert: 0, 2, 6, 10 ... 38 s.
	if n := e.g.count(getPR); n < 9 || n > 13 {
		t.Fatalf("GetPR calls = %d, want about one per 4 s (9..13)", n)
	}
}

func TestHeadChangeResetsTheBackoff(t *testing.T) {
	// Arrange: evaluations at 0, 1, 3, 7 and 15 s; the next is due at 31 s.
	e := newLoopEnv(t, loopCfg{}, pending())
	l := e.loop(loopCfg{})
	ticks(t, e, l, 31)
	if n := e.g.count(getPR); n != 5 {
		t.Fatalf("GetPR calls before the push = %d, want 5", n)
	}
	e.g.set(func() { e.g.headSHA = sha2; e.g.runs[sha2] = []map[string]any{pending()} })
	// Act: the push is seen at the first due tick, and the task is back at Every.
	tick(t, l)
	atDue := e.g.count(getPR)
	e.clk.Advance(time.Second)
	tick(t, l)
	afterOne := e.g.count(getPR)
	// Assert
	if atDue != 6 {
		t.Fatalf("GetPR calls at the due tick = %d, want 6", atDue)
	}
	if afterOne != 7 {
		t.Fatalf("GetPR calls one Every after the new head = %d, want 7 (backoff reset)", afterOne)
	}
	if got := e.get(t).PR.HeadSHA; got != sha2 {
		t.Fatalf("recorded head = %s, want the new one", got)
	}
}

func TestWebhookEventEvaluatesInTheMiddleOfABackoffAndResetsIt(t *testing.T) {
	// Arrange: after 20 s the task sleeps until 31 s.
	e := newLoopEnv(t, loopCfg{}, pending())
	l := e.loop(loopCfg{})
	ticks(t, e, l, 20)
	idle := e.g.count(getPR)
	tick(t, l)
	if n := e.g.count(getPR); n != idle {
		t.Fatalf("a tick in the middle of the backoff evaluated the task (%d -> %d calls)", idle, n)
	}
	// Act
	l.OnEvent(context.Background(), factory.Event{Name: "check_run.completed", Repo: repo, Fields: map[string]string{"number": "7"}})
	afterEvent := e.g.count(getPR)
	e.clk.Advance(time.Second)
	tick(t, l)
	afterTick := e.g.count(getPR)
	// Assert
	if afterEvent != idle+1 {
		t.Fatalf("GetPR calls after the event = %d, want %d", afterEvent, idle+1)
	}
	if afterTick != afterEvent+1 {
		t.Fatalf("GetPR calls one Every after the event = %d, want %d (backoff reset)", afterTick, afterEvent+1)
	}
}

func TestMaxPerTickSpreadsABurstOverTicks(t *testing.T) {
	// Arrange: 60 pull requests in review (they all name pull request 7 of the fake).
	e := newLoopEnv(t, loopCfg{}, pending())
	l := e.loop(loopCfg{})
	ids := []string{e.task}
	for range 59 {
		task, _, err := e.store.CreateTask(context.Background(), factory.Task{Title: "T", Owner: "u", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.c.Review(context.Background(), task.ID, factory.PullRequest{Number: 7, HeadSHA: sha1}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	seen := func() int {
		n := 0
		for _, id := range ids {
			evs, _ := e.store.ListEvents(context.Background(), id, 0)
			for _, ev := range evs {
				if ev.Kind == factory.EventCI {
					n++
					break
				}
			}
		}
		return n
	}
	// Act + Assert
	want := []struct{ calls, seen int }{{25, 25}, {50, 50}, {75, 60}}
	for i, w := range want {
		tick(t, l)
		e.clk.Advance(time.Second)
		if got := e.g.count(getPR); got != w.calls {
			t.Fatalf("after tick %d: GetPR calls = %d, want %d", i+1, got, w.calls)
		}
		if got := seen(); got != w.seen {
			t.Fatalf("after tick %d: %d tasks evaluated, want %d (least recently evaluated first, nobody starved)", i+1, got, w.seen)
		}
	}
}

func TestMaxPerTickIsConfigurable(t *testing.T) {
	// Arrange
	cfg := loopCfg{mutate: func(o *forge.LoopOptions) { o.MaxPerTick = 2 }}
	e := newLoopEnv(t, cfg, pending())
	for range 4 {
		task, _, _ := e.store.CreateTask(context.Background(), factory.Task{Title: "T", Owner: "u", Repo: repo, BaseBranch: "main", Branch: "har/abc"})
		if err := e.c.Review(context.Background(), task.ID, factory.PullRequest{Number: 7, HeadSHA: sha1}); err != nil {
			t.Fatal(err)
		}
	}
	// Act
	tick(t, e.loop(cfg))
	// Assert
	if n := e.g.count(getPR); n != 2 {
		t.Fatalf("GetPR calls = %d, want 2", n)
	}
}

func TestFirstEvaluationIsNeverDelayed(t *testing.T) {
	t.Run("red CI resumes the task at the first tick", func(t *testing.T) {
		// Arrange
		e := newLoopEnv(t, loopCfg{}, run("test", "completed", "failure", ""))
		// Act
		tick(t, e.loop(loopCfg{}))
		// Assert
		if e.get(t).State != factory.StateReady {
			t.Fatalf("state = %s, want ready (new attempt)", e.get(t).State)
		}
	})
	t.Run("green CI is recorded at the first tick", func(t *testing.T) {
		// Arrange
		e := newLoopEnv(t, loopCfg{}, green())
		// Act
		tick(t, e.loop(loopCfg{}))
		// Assert
		if ci := e.events(t, factory.EventCI); len(ci) != 1 || ci[0].Data["state"] != "passed" {
			t.Fatalf("ci events = %+v", ci)
		}
	})
}

func TestBackoffDoesNotSleepThroughTheSettleWindow(t *testing.T) {
	// Arrange: green and settling for 60 s while each quiet look doubles the gap.
	cfg := loopCfg{autoMerge: true, noGate: true}
	e := newLoopEnv(t, cfg, green())
	l := e.loop(cfg)
	// Act
	elapsed := 0
	for ; elapsed <= 62 && e.get(t).State != factory.StateDone; elapsed++ {
		tick(t, l)
		e.clk.Advance(time.Second)
	}
	// Assert
	if e.get(t).State != factory.StateDone || elapsed > 62 {
		t.Fatalf("state = %s after %d s, want merged right after the settle window", e.get(t).State, elapsed)
	}
	if n := e.g.count(getPR); n > 12 {
		t.Fatalf("GetPR calls = %d, want a few (backoff)", n)
	}
}
