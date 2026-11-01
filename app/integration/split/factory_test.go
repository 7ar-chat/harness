package split_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/factory"
)

// The factory over the split: the web tier dispatches a task (its factory runs the dispatcher), a harness-worker
// process started with FACTORY=1 executes the run, and the end-of-run hooks of the worker move the task on.

// factoryWeb is the web tier of these tests: it dispatches at once and never waits to retry a failed attempt.
func factoryWeb(fo *harness.FactoryOptions) {
	fo.Backoff = func(int) time.Duration { return 0 }
}

// startFactoryWorker is a worker with the factory on and the dispatcher off (the default of a worker).
func (r *rig) startFactoryWorker(id string) *worker {
	r.t.Helper()
	return r.startWorkerEnv(id, "FACTORY=1", "HARNESS_MODEL_PRICES=off")
}

// createTask creates a task through the web tier's service, as the HTTP API does.
func (r *rig) createTask(title, spec string) *factory.Task {
	r.t.Helper()
	f := r.web.FactoryOf()
	if f == nil {
		r.t.Fatal("the web tier has no factory")
	}
	task, created, err := f.Tasks.Create(context.Background(), factory.Task{Owner: user, Title: title, Spec: spec})
	if err != nil || !created {
		r.t.Fatalf("Tasks.Create = %v, %v", created, err)
	}
	return task
}

func (r *rig) task(id string) *factory.Task {
	r.t.Helper()
	task, err := r.web.FactoryOf().Store.GetTask(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return task
}

// waitTask waits until cond holds for the task and returns it.
func (r *rig) waitTask(id, what string, cond func(*factory.Task) bool) *factory.Task {
	r.t.Helper()
	var got *factory.Task
	eventually(r.t, what, func() bool {
		got = r.task(id)
		return cond(got)
	})
	return got
}

func (r *rig) taskEvents(id string) []factory.TaskEvent {
	r.t.Helper()
	evs, err := r.web.FactoryOf().Store.ListEvents(context.Background(), id, 0)
	if err != nil {
		r.t.Fatal(err)
	}
	return evs
}

// eventOf returns the first event of the kind whose Data[key] is value ("" key: any).
func eventOf(evs []factory.TaskEvent, kind, key, value string) (factory.TaskEvent, bool) {
	for _, e := range evs {
		if e.Kind != kind {
			continue
		}
		if key == "" {
			return e, true
		}
		if v, _ := e.Data[key].(string); v == value {
			return e, true
		}
	}
	return factory.TaskEvent{}, false
}

func TestATaskDispatchedByTheWebTierIsRunAndFinishedByTheFactoryWorker(t *testing.T) {
	// Arrange: the web tier dispatches but cannot execute (it never consumes the queue); worker-1 is the only
	// process that can, and it has the factory installed.
	r := newRigWith(t, factoryWeb)
	w := r.startFactoryWorker("worker-1")

	// Act
	task := r.createTask("Summarise the changelog", "Say what changed in the last release.")
	done := r.waitTask(task.ID, "the task to be done", func(t *factory.Task) bool { return t.State == factory.StateDone })

	// Assert: one attempt, one metered run with its tokens (10 in, 5 out per model turn of the fake).
	if done.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", done.Attempts)
	}
	if done.Spent.Runs != 1 || done.Spent.Tokens <= 0 {
		t.Errorf("Spent = %+v, want one run with its tokens counted", done.Spent)
	}
	if !strings.Contains(done.Result, "Summarise the changelog") {
		t.Errorf("Result = %q, want the agent's answer to the task", done.Result)
	}

	// Assert: the history holds what only the worker can write: the spend of the run, then the move to done
	// (the service records the event just after the state, so wait for it).
	eventually(t, "the history to record the move to done", func() bool {
		_, ok := eventOf(r.taskEvents(task.ID), factory.EventState, "to", string(factory.StateDone))
		return ok
	})
	evs := r.taskEvents(task.ID)
	budget, ok := eventOf(evs, factory.EventBudget, "", "")
	if !ok {
		t.Fatalf("no budget event in the history: %+v", evs)
	}

	// Assert: the run really executed in the worker process, not in the web tier.
	runID, _ := budget.Data["runId"].(string)
	if runID == "" {
		t.Fatalf("the budget event names no run: %+v", budget)
	}
	r.waitStatus(runID, "completed")
	if owner := r.runtimeOf(runID); owner != w.id {
		t.Errorf("the run was executed by %q, want the worker %q", owner, w.id)
	}
}

func TestATaskWhoseWorkerDiesMidRunIsRecoveredAndFinishedByAnotherWorker(t *testing.T) {
	// Arrange: worker-1 holds the first attempt on a model reply the fake never sends.
	r := newRigWith(t, factoryWeb)
	hold := r.llm.Hold("slow")
	w1 := r.startFactoryWorker("worker-1")
	task := r.createTask("A slow question", "Take your time over this one.")
	<-hold.arrived
	running := r.waitTask(task.ID, "the first attempt to be recorded", func(t *factory.Task) bool {
		return t.State == factory.StateRunning && t.RunID != ""
	})
	firstRun := running.RunID
	r.waitStatus(firstRun, "running")

	// Act: the worker dies without any chance to clean up; its heartbeat goes stale; a live runtime sweeps. The
	// sweep interrupts the run and runs the tasks job, which settles the attempt the dead worker never finished.
	w1.Kill()
	r.ageHeartbeat(firstRun)
	r.web.Runtime.Sweep(context.Background())

	// Assert: the attempt is recorded as failed and the task is back for another one.
	r.waitStatus(firstRun, "interrupted")
	retried := r.waitTask(task.ID, "the task to be given another attempt", func(t *factory.Task) bool { return t.Attempts == 2 })
	if retried.State.Terminal() {
		t.Fatalf("the task ended after the worker died: %+v", retried)
	}
	if owner := r.runtimeOf(firstRun); owner != w1.id {
		t.Errorf("the first run was executed by %q, want %q", owner, w1.id)
	}

	// Act: a second worker joins (the fake now answers) and executes the retry.
	hold.Release()
	w2 := r.startFactoryWorker("worker-2")
	done := r.waitTask(task.ID, "the task to be done", func(t *factory.Task) bool { return t.State == factory.StateDone })

	// Assert: two attempts, one metered run (the dead one never reached its end-of-run hook), run by worker-2.
	if done.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", done.Attempts)
	}
	if done.Spent.Runs != 1 || done.Spent.Tokens <= 0 {
		t.Errorf("Spent = %+v, want the finished run only", done.Spent)
	}
	if done.RunID == "" || done.RunID == firstRun {
		t.Fatalf("RunID = %q after the retry (first run %q)", done.RunID, firstRun)
	}
	r.waitStatus(done.RunID, "completed")
	if owner := r.runtimeOf(done.RunID); owner != w2.id {
		t.Errorf("the retry was executed by %q, want %q", owner, w2.id)
	}
	evs := r.taskEvents(task.ID)
	if _, ok := eventOf(evs, factory.EventRun, "", ""); !ok {
		t.Errorf("no run event in the history: %+v", evs)
	}
	var failed bool
	for _, e := range evs {
		failed = failed || e.Kind == factory.EventRun && strings.HasPrefix(e.Text, "attempt 1 failed")
	}
	if !failed {
		t.Errorf("the history does not record the failure of attempt 1: %+v", evs)
	}
}
