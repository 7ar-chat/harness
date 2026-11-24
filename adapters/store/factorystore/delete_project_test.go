package factorystore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/factory"
)

func TestDeleteProjectRemovesItsTasksEventsThreadsAndTriggersOnly(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange: two projects with a task each (history, a learning record, a thread), a trigger, and some spend.
		ctx := context.Background()
		seed := func(project string) (taskID string) {
			task := newTask(t, s, factory.Task{ProjectID: project, Owner: "alice", Title: "t-" + project, Spec: "s"})
			setState(t, s, task.ID, factory.StateRunning)
			must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: task.ID, Kind: factory.EventContext, Text: "recalled"}))
			must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: task.ID, Kind: factory.EventOutcome, Text: "ended"}))
			must(t, s.SetThreadInfo(ctx, factory.ThreadInfo{ThreadID: "th-" + project, TaskID: task.ID, Login: "alice"}))
			_, err := s.PutTrigger(ctx, factory.Trigger{ProjectID: project, Owner: "alice", Name: "n-" + project, Kind: factory.TriggerWebhook, Event: "issues.opened"})
			must(t, err)
			_, err = s.AddSpend(ctx, task.ID, factory.Spend{USD: 1, Tokens: 10, Runs: 1})
			must(t, err)
			return task.ID
		}
		gone, kept := seed("p-gone"), seed("p-kept")
		must(t, s.SetThreadInfo(ctx, factory.ThreadInfo{ThreadID: "th-plain", Login: "alice"})) // an ordinary thread of nobody's task

		// Act
		must(t, s.DeleteProject(ctx, "p-gone"))
		must(t, s.DeleteProject(ctx, "p-never-existed")) // nothing to delete is not an error

		// Assert: nothing of the project is left, the other project is whole.
		if list, _ := s.ListTasks(ctx, factory.Filter{ProjectID: "p-gone"}); len(list) != 0 {
			t.Fatalf("tasks left: %v", list)
		}
		_, err := s.GetTask(ctx, gone)
		wantErr(t, "deleted task", err, factory.ErrNotFound)
		if ev, _ := s.ListEvents(ctx, gone, 0); len(ev) != 0 {
			t.Fatalf("events left: %v", ev)
		}
		_, err = s.GetThreadInfo(ctx, "th-p-gone")
		wantErr(t, "thread info of a deleted task", err, factory.ErrNotFound)
		if tr, _ := s.ListTriggers(ctx, "p-gone"); len(tr) != 0 {
			t.Fatalf("triggers left: %v", tr)
		}
		if _, err := s.GetTask(ctx, kept); err != nil {
			t.Fatalf("the other project's task: %v", err)
		}
		if ev, _ := s.ListEvents(ctx, kept, 0); len(ev) != 2 {
			t.Fatalf("the other project's events = %d, want 2", len(ev))
		}
		for _, id := range []string{"th-p-kept", "th-plain"} {
			if _, err := s.GetThreadInfo(ctx, id); err != nil {
				t.Fatalf("thread info %s: %v", id, err)
			}
		}
		if tr, _ := s.ListTriggers(ctx, "p-kept"); len(tr) != 1 {
			t.Fatalf("the other project's triggers = %d, want 1", len(tr))
		}
		wantErr(t, "empty project id", s.DeleteProject(ctx, ""), factory.ErrInvalid)
	})
}

func TestDeleteProjectSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "factory.json")
	ctx := context.Background()
	m, err := factorystore.Open(path, nil)
	must(t, err)
	task := newTask(t, m, factory.Task{ProjectID: "p1", Owner: "alice", Title: "t", Spec: "s"})
	must(t, m.AppendEvent(ctx, factory.TaskEvent{TaskID: task.ID, Kind: factory.EventContext, Text: "x"}))
	must(t, m.DeleteProject(ctx, "p1"))
	must(t, m.Close())
	r, err := factorystore.Open(path, nil)
	must(t, err)
	defer func() { must(t, r.Close()) }()
	if list, _ := r.ListTasks(ctx, factory.Filter{}); len(list) != 0 {
		t.Fatalf("tasks after a restart: %v", list)
	}
}
