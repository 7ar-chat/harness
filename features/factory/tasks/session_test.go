package tasks_test

import (
	"context"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/factory/tasks"
)

func adoptSession(t *testing.T, e *env) factory.Task {
	t.Helper()
	got, err := e.svc.Adopt(e.ctx, factory.Task{Title: "Fix login", Owner: "alice", ProjectID: "p1", Repo: "acme/app", ThreadID: "sess"})
	if err != nil {
		t.Fatal(err)
	}
	return *got
}

func sessionRunEnds(t *testing.T, e *env, status string) {
	t.Helper()
	f := &runtime.Finish{Run: &store.Run{ID: "r2", ThreadID: "sess", Status: status}, Status: status, Result: agent.Result{}}
	if next := e.svc.Hook().Finishing(e.ctx, f); next != nil {
		next(e.ctx)
	}
}

func TestAnAdoptedSessionIsRunningBoundToItsThreadAndNeverDispatched(t *testing.T) {
	// Arrange
	e := newEnv(t, nil)
	// Act
	task := adoptSession(t, e)
	started := e.dispatch(t)
	// Assert
	if task.State != factory.StateRunning || task.ThreadID != "sess" || task.Source.Kind != factory.SourceSession {
		t.Fatalf("task = %+v", task)
	}
	if started != 0 || len(e.sp.requests()) != 0 {
		t.Fatalf("a session task must not spawn a run: started %d, spawns %d", started, len(e.sp.requests()))
	}
}

func TestAdoptingTheSameSessionTwiceReturnsTheSameTask(t *testing.T) {
	e := newEnv(t, nil)
	a := adoptSession(t, e)
	b := adoptSession(t, e)
	if a.ID != b.ID {
		t.Fatalf("ids %s and %s differ", a.ID, b.ID)
	}
}

func TestFeedbackOfASessionTaskIsDeliveredToItsChatNotASpawnedRun(t *testing.T) {
	// Arrange
	var got []string
	e := newEnv(t, func(o *tasks.Options) {
		o.Deliver = func(_ context.Context, tk factory.Task, fb string) error {
			got = append(got, tk.ThreadID+": "+fb)
			return nil
		}
	})
	task := adoptSession(t, e)
	if err := e.svc.Review(e.ctx, task.ID, factory.PullRequest{Number: 7}); err != nil {
		t.Fatal(err)
	}
	// Act
	if err := e.svc.Resume(e.ctx, task.ID, "CI failed: TestLogin"); err != nil {
		t.Fatal(err)
	}
	// Assert
	if len(got) != 1 || !strings.HasPrefix(got[0], "sess: CI failed") {
		t.Fatalf("delivered = %v", got)
	}
	if s := e.state(t, task.ID); s != factory.StateRunning {
		t.Fatalf("state = %s, want running", s)
	}
	if len(e.sp.requests()) != 0 {
		t.Fatal("no run may be spawned for a session")
	}
}

func TestFeedbackOfASessionTaskBlocksItWhenTheChatCannotBeReached(t *testing.T) {
	e := newEnv(t, nil) // no Deliver
	task := adoptSession(t, e)
	_ = e.svc.Review(e.ctx, task.ID, factory.PullRequest{Number: 7})
	if err := e.svc.Resume(e.ctx, task.ID, "CI failed"); err != nil {
		t.Fatal(err)
	}
	if s := e.state(t, task.ID); s != factory.StateBlocked {
		t.Fatalf("state = %s, want blocked", s)
	}
}

func TestASessionRunThatEndsAfterFeedbackHandsThePullRequestBackToReview(t *testing.T) {
	// Arrange
	e := newEnv(t, func(o *tasks.Options) {
		o.Deliver = func(context.Context, factory.Task, string) error { return nil }
	})
	task := adoptSession(t, e)
	_ = e.svc.Review(e.ctx, task.ID, factory.PullRequest{Number: 7})
	_ = e.svc.Resume(e.ctx, task.ID, "CI failed")
	// Act
	sessionRunEnds(t, e, store.StatusCompleted)
	// Assert
	if s := e.state(t, task.ID); s != factory.StateReview {
		t.Fatalf("state = %s, want review", s)
	}
}

func TestAChatTurnOfAReviewedSessionLeavesItAlone(t *testing.T) {
	e := newEnv(t, nil)
	task := adoptSession(t, e)
	_ = e.svc.Review(e.ctx, task.ID, factory.PullRequest{Number: 7})
	sessionRunEnds(t, e, store.StatusFailed)
	if s := e.state(t, task.ID); s != factory.StateReview {
		t.Fatalf("state = %s, want review", s)
	}
}

func TestOnStateHearsEveryMoveOfATask(t *testing.T) {
	// Arrange
	var moves []string
	e := newEnv(t, func(o *tasks.Options) {
		o.OnState = func(_ factory.Task, from, to factory.State) { moves = append(moves, string(from)+">"+string(to)) }
	})
	// Act
	task := adoptSession(t, e)
	_ = e.svc.Review(e.ctx, task.ID, factory.PullRequest{Number: 7})
	// Assert
	want := []string{">running", "running>review"}
	if len(moves) != 2 || moves[0] != want[0] || moves[1] != want[1] {
		t.Fatalf("moves = %v, want %v", moves, want)
	}
}
