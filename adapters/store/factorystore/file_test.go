package factorystore_test

// Persistence of the file-backed store (factorystore.Open). The shared behavioural cases run against
// it in conformance_test.go; these cover what only a file can get wrong.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/factory"
)

func openAt(t *testing.T, path string, clk *clock) *factorystore.Mem {
	t.Helper()
	m, err := factorystore.Open(path, clk.Now)
	must(t, err)
	return m
}

func closeStore(t *testing.T, m *factorystore.Mem) {
	t.Helper()
	must(t, m.Close())
}

// readOnlyDir makes dir unwritable for the test and restores it on cleanup; it skips as root, for
// whom permissions do not apply.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	must(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

func TestStateSurvivesCloseAndOpen(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	dep := newTask(t, s, factory.Task{ProjectID: "p1", Title: "dep", Owner: "alice"})
	main := newTask(t, s, factory.Task{
		ProjectID: "p1", Title: "main", Owner: "alice", DependsOn: []string{dep.ID}, Labels: []string{"bug"},
		Source: factory.Source{Kind: factory.SourceGitHub, Key: "github:issue:acme/app#1"},
	})
	other := newTask(t, s, factory.Task{ProjectID: "p2", Title: "other"})
	for _, fp := range []string{"fp-a", "fp-b"} {
		_, err := s.UpdateTask(ctx, main.ID, factory.Patch{AddFingerprint: fp, Branch: new("factory/x")})
		must(t, err)
	}
	clk.Advance(time.Minute)
	_, err := s.AddSpend(ctx, main.ID, factory.Spend{USD: 1.25, Tokens: 900, Runs: 1})
	must(t, err)
	clk.Advance(time.Minute)
	_, err = s.AddSpend(ctx, other.ID, factory.Spend{USD: 4, Tokens: 10, Runs: 2, Unpriced: 1})
	must(t, err)
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: main.ID, Kind: factory.EventNote, Text: "first", Data: map[string]any{"sha": "abc"}}))
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: main.ID, Kind: factory.EventNote, Text: "second"}))
	next := clk.Now().Add(time.Hour)
	trig, err := s.PutTrigger(ctx, factory.Trigger{
		ProjectID: "p1", Owner: "alice", Name: "nightly", Kind: factory.TriggerCron, Schedule: "@every 1h", Enabled: true,
		NextFireAt: next, Template: factory.Template{Title: "t {{.Time}}", Labels: []string{"cron"}, Budget: factory.Budget{MaxUSD: 3}},
	})
	must(t, err)
	hook, err := s.PutTrigger(ctx, factory.Trigger{Owner: "bob", Name: "hook", Kind: factory.TriggerWebhook, Event: "issues.labeled", Match: map[string]string{"label": "agent"}})
	must(t, err)
	ti := factory.ThreadInfo{ThreadID: "th1", Login: "alice", TaskID: main.ID, Role: factory.RoleCoder, Repo: "acme/app", Branch: "factory/x"}
	must(t, s.SetThreadInfo(ctx, ti))
	_, err = s.SeenDelivery(ctx, "delivery-1")
	must(t, err)
	wantTasks, err := s.ListTasks(ctx, factory.Filter{})
	must(t, err)
	wantEvents, _ := s.ListEvents(ctx, main.ID, 0)
	wantSpend, _ := s.SpendSince(ctx, "", time.Time{})
	wantP1, _ := s.SpendSince(ctx, "p1", clk.Now().Add(-90*time.Second))
	wantTriggers, _ := s.ListTriggers(ctx, "")
	closeStore(t, s)

	// Act
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	gotTasks, _ := r.ListTasks(ctx, factory.Filter{})
	gotEvents, _ := r.ListEvents(ctx, main.ID, 0)
	gotSpend, _ := r.SpendSince(ctx, "", time.Time{})
	gotP1, _ := r.SpendSince(ctx, "p1", clk.Now().Add(-90*time.Second))
	gotTriggers, _ := r.ListTriggers(ctx, "")
	gotTI, tiErr := r.GetThreadInfo(ctx, "th1")
	seen, _ := r.SeenDelivery(ctx, "delivery-1")
	fresh, _ := r.SeenDelivery(ctx, "delivery-2")
	again, created, err := r.CreateTask(ctx, factory.Task{ProjectID: "p1", Title: "dup", Source: main.Source})
	must(t, err)
	post := factory.TaskEvent{TaskID: main.ID, Kind: factory.EventNote, Text: "third"}
	must(t, r.AppendEvent(ctx, post))
	after, _ := r.ListEvents(ctx, main.ID, 0)

	// Assert
	if !reflect.DeepEqual(gotTasks, wantTasks) {
		t.Fatalf("tasks:\n got %+v\nwant %+v", gotTasks, wantTasks)
	}
	got, _ := r.GetTask(ctx, main.ID)
	if got.State != factory.StatePending || !reflect.DeepEqual(got.DependsOn, []string{dep.ID}) || !reflect.DeepEqual(got.Fingerprints, []string{"fp-a", "fp-b"}) {
		t.Fatalf("dependencies / fingerprints lost: %+v", got)
	}
	if !reflect.DeepEqual(gotEvents, wantEvents) || len(gotEvents) != 2 {
		t.Fatalf("events:\n got %+v\nwant %+v", gotEvents, wantEvents)
	}
	if gotSpend != wantSpend || gotSpend.USD != 5.25 || gotP1 != wantP1 || gotP1.USD != 1.25 {
		t.Fatalf("spend ledger: all %+v (want %+v), p1 %+v (want %+v)", gotSpend, wantSpend, gotP1, wantP1)
	}
	if !reflect.DeepEqual(gotTriggers, wantTriggers) || len(gotTriggers) != 2 {
		t.Fatalf("triggers:\n got %+v\nwant %+v", gotTriggers, wantTriggers)
	}
	gt, _ := r.GetTrigger(ctx, trig.ID)
	gh, _ := r.GetTrigger(ctx, hook.ID)
	if !gt.NextFireAt.Equal(next) || gh.Match["label"] != "agent" {
		t.Fatalf("trigger fields lost: %+v %+v", gt, gh)
	}
	if tiErr != nil || *gotTI != ti {
		t.Fatalf("thread info = %+v, %v", gotTI, tiErr)
	}
	if !seen || fresh {
		t.Fatalf("deliveries: delivery-1 seen=%v, delivery-2 seen=%v", seen, fresh)
	}
	if created || again.ID != main.ID {
		t.Fatalf("source key no longer deduplicates: created=%v id=%s", created, again.ID)
	}
	if len(after) != 3 || after[2].ID <= after[1].ID {
		t.Fatalf("event ids must keep increasing across a restart: %+v", after)
	}
}

func TestClaimedTriggerTicksAndDoneDependenciesSurviveARestart(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	trig, err := s.PutTrigger(ctx, factory.Trigger{Owner: "a", Name: "c", Kind: factory.TriggerCron, Enabled: true, NextFireAt: clk.Now()})
	must(t, err)
	fireAt := clk.Now().Add(time.Hour)
	due, err := s.ClaimDueTriggers(ctx, clk.Now(), func(factory.Trigger) time.Time { return fireAt })
	must(t, err)
	dep := newTask(t, s, factory.Task{Title: "dep"})
	child := newTask(t, s, factory.Task{Title: "child", DependsOn: []string{dep.ID}})
	setState(t, s, dep.ID, factory.StateDone)
	closeStore(t, s)

	// Act
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	got, _ := r.GetTrigger(ctx, trig.ID)
	again, _ := r.ClaimDueTriggers(ctx, clk.Now(), func(factory.Trigger) time.Time { return fireAt })
	gotChild, _ := r.GetTask(ctx, child.ID)

	// Assert
	if len(due) != 1 || !got.NextFireAt.Equal(fireAt) || !got.LastFiredAt.Equal(clk.Now()) || len(again) != 0 {
		t.Fatalf("claimed tick refires after a restart: claimed %d, trigger %+v, again %d", len(due), got, len(again))
	}
	if gotChild.State != factory.StateReady {
		t.Fatalf("released dependent = %s, want ready", gotChild.State)
	}
}

func TestOpenOfAMissingFileStartsEmptyAndCreatesTheDirectory(t *testing.T) {
	// Arrange
	root := t.TempDir()
	path := filepath.Join(root, "a", "b", "factory.json")

	// Act
	s, err := factorystore.Open(path, nil)
	must(t, err)
	tasks, _ := s.ListTasks(context.Background(), factory.Filter{})
	info, statErr := os.Stat(filepath.Dir(path))
	_, fileErr := os.Stat(path)

	// Assert
	if len(tasks) != 0 {
		t.Fatalf("tasks = %v", tasks)
	}
	if statErr != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory = %v, %v; want a 0700 directory", info, statErr)
	}
	if !os.IsNotExist(fileErr) {
		t.Fatalf("an unused store must not write a file: %v", fileErr)
	}
	closeStore(t, s)
}

func TestOpenRefusesACorruptOrForeignFileAndLeavesItUntouched(t *testing.T) {
	cases := map[string]string{
		"not json":          "{not json",
		"truncated":         `{"version":1,"tasks":{"x":`,
		"wrong version":     `{"version":99}`,
		"no version":        `{"tasks":{}}`,
		"task id mismatch":  `{"version":1,"tasks":{"a":{"id":"b"}}}`,
		"null task":         `{"version":1,"tasks":{"a":null}}`,
		"trigger mismatch":  `{"version":1,"triggers":{"a":{"id":"b"}}}`,
		"thread id mistake": `{"version":1,"threads":{"a":{"threadId":"b"}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			path := filepath.Join(t.TempDir(), "factory.json")
			must(t, os.WriteFile(path, []byte(content), 0o600))

			// Act
			s, err := factorystore.Open(path, nil)

			// Assert
			if err == nil || s != nil {
				t.Fatalf("Open = %v, %v; want an error and no store", s, err)
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name %s", err, path)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || string(after) != content {
				t.Fatalf("file changed: %q, %v", after, readErr)
			}
			if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
				t.Fatalf("a refused file left %s.tmp behind", path)
			}
		})
	}
}

func TestOpenOfAnUnreadablePathNamesIt(t *testing.T) {
	// Arrange: a directory where the file should be
	path := filepath.Join(t.TempDir(), "factory.json")
	must(t, os.Mkdir(path, 0o700))

	// Act
	_, err := factorystore.Open(path, nil)

	// Assert
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
}

func TestFileIsPrivateAndNoTempFileIsLeftBehind(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.json")
	s := openAt(t, path, clk)
	tk := newTask(t, s, factory.Task{Title: "t"})
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote}))
	// A crash mid-write leaves a temp file, possibly with wide permissions.
	must(t, os.WriteFile(path+".tmp", []byte("stale"), 0o644))

	// Act
	setState(t, s, tk.ID, factory.StateRunning)
	closeStore(t, s)
	info, err := os.Stat(path)
	must(t, err)
	entries, err := os.ReadDir(dir)
	must(t, err)

	// Assert
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	for _, e := range entries {
		if e.Name() != "factory.json" {
			t.Fatalf("unexpected file %s left in the state directory", e.Name())
		}
	}
}

func TestAFailedWriteIsReportedAndRolledBack(t *testing.T) {
	// Each case changes one thing while the directory is read-only, then checks the error and that
	// memory still equals the last persisted state.
	type fixture struct {
		task factory.Task
		trig factory.Trigger
		dep  factory.Task
	}
	cases := []struct {
		name string
		do   func(ctx context.Context, s *factorystore.Mem, f fixture) error
		same func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture)
	}{
		{"UpdateTask", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			_, err := s.UpdateTask(ctx, f.task.ID, factory.Patch{State: new(factory.StateRunning), Title: new("changed"), AddFingerprint: "x"})
			return err
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			got, _ := s.GetTask(ctx, f.task.ID)
			if !reflect.DeepEqual(got, &f.task) {
				t.Fatalf("task changed in memory:\n got %+v\nwant %+v", got, &f.task)
			}
		}},
		{"UpdateTask that would release a dependent", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			_, err := s.UpdateTask(ctx, f.dep.ID, factory.Patch{State: new(factory.StateDone)})
			return err
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			dep, _ := s.GetTask(ctx, f.dep.ID)
			waiting, _ := s.ListTasks(ctx, factory.Filter{States: []factory.State{factory.StatePending}})
			if dep.State != factory.StateReady || len(waiting) != 1 {
				t.Fatalf("dependency %s, pending tasks %d: the release was not undone", dep.State, len(waiting))
			}
		}},
		{"CreateTask", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			_, _, err := s.CreateTask(ctx, factory.Task{ID: "new", Title: "new"})
			return err
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			if _, err := s.GetTask(ctx, "new"); err == nil {
				t.Fatal("the task that failed to persist is still in memory")
			}
		}},
		{"AddSpend", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			_, err := s.AddSpend(ctx, f.task.ID, factory.Spend{USD: 2, Tokens: 5, Runs: 1})
			return err
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			got, _ := s.GetTask(ctx, f.task.ID)
			sum, _ := s.SpendSince(ctx, "", time.Time{})
			if got.Spent != (factory.Spend{}) || sum != (factory.Spend{}) {
				t.Fatalf("spend kept: task %+v, ledger %+v", got.Spent, sum)
			}
		}},
		{"PutTrigger (new)", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			_, err := s.PutTrigger(ctx, factory.Trigger{ID: "tr2", Name: "n", Kind: factory.TriggerWebhook})
			return err
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			if _, err := s.GetTrigger(ctx, "tr2"); err == nil {
				t.Fatal("the trigger that failed to persist is still in memory")
			}
		}},
		{"PutTrigger (replace)", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			r := f.trig
			r.Name = "renamed"
			_, err := s.PutTrigger(ctx, r)
			return err
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			got, _ := s.GetTrigger(ctx, f.trig.ID)
			if got.Name != f.trig.Name {
				t.Fatalf("trigger name = %q, want %q", got.Name, f.trig.Name)
			}
		}},
		{"DeleteTrigger", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			return s.DeleteTrigger(ctx, f.trig.ID)
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			if _, err := s.GetTrigger(ctx, f.trig.ID); err != nil {
				t.Fatalf("the trigger whose deletion failed to persist is gone: %v", err)
			}
		}},
		{"SetThreadInfo", func(ctx context.Context, s *factorystore.Mem, f fixture) error {
			return s.SetThreadInfo(ctx, factory.ThreadInfo{ThreadID: "th", Login: "x"})
		}, func(ctx context.Context, t *testing.T, s *factorystore.Mem, f fixture) {
			if _, err := s.GetThreadInfo(ctx, "th"); err == nil {
				t.Fatal("the thread link that failed to persist is still in memory")
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			ctx := context.Background()
			clk := newClock()
			dir := t.TempDir()
			path := filepath.Join(dir, "factory.json")
			s := openAt(t, path, clk)
			var f fixture
			f.task = *newTask(t, s, factory.Task{Title: "t"})
			f.dep = *newTask(t, s, factory.Task{Title: "dep"})
			newTask(t, s, factory.Task{Title: "waits", DependsOn: []string{f.dep.ID}})
			trig, err := s.PutTrigger(ctx, factory.Trigger{Name: "trig", Kind: factory.TriggerCron})
			must(t, err)
			f.trig = *trig
			before, err := os.ReadFile(path)
			must(t, err)
			readOnlyDir(t, dir)

			// Act
			err = c.do(ctx, s, f)

			// Assert
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("err = %v, want a write error naming %s", err, path)
			}
			c.same(ctx, t, s, f)
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("the file changed although the write failed")
			}

			// The store recovers once the disk does.
			must(t, os.Chmod(dir, 0o700))
			if err := c.do(ctx, s, f); err != nil {
				t.Fatalf("retry after the directory is writable again: %v", err)
			}
			closeStore(t, s)
		})
	}
}

func TestAppendEventAndSeenDeliveryDoNotFailOnAFullDisk(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	dir := t.TempDir()
	path := filepath.Join(dir, "factory.json")
	s := openAt(t, path, clk)
	s.SetFlushInterval(time.Millisecond)
	tk := newTask(t, s, factory.Task{Title: "t"})
	readOnlyDir(t, dir)

	// Act
	appendErr := s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: "kept in memory"})
	_, seenErr := s.SeenDelivery(ctx, "d1")
	waitFor(t, "the flush attempt", func() bool { return s.Failures() > 0 })
	ev, _ := s.ListEvents(ctx, tk.ID, 0)
	must(t, os.Chmod(dir, 0o700))
	// The failed flush stays pending: Close writes it once the disk is back.
	closeErr := s.Close()
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	back, _ := r.ListEvents(ctx, tk.ID, 0)
	seen, _ := r.SeenDelivery(ctx, "d1")

	// Assert
	if appendErr != nil || seenErr != nil || len(ev) != 1 {
		t.Fatalf("append err %v, seen err %v, events %d", appendErr, seenErr, len(ev))
	}
	if closeErr != nil || len(back) != 1 || !seen {
		t.Fatalf("close err %v, events after restart %d, delivery seen %v", closeErr, len(back), seen)
	}
}

func TestATaskKeepsAtMost500EventsAndTheNewestSurvive(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	tk := newTask(t, s, factory.Task{Title: "t"})
	other := newTask(t, s, factory.Task{Title: "other"})
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: other.ID, Kind: factory.EventNote, Text: "only"}))

	// Act
	for i := range 620 {
		must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: fmt.Sprint("e", i)}))
	}
	inMemory, _ := s.ListEvents(ctx, tk.ID, 0)
	closeStore(t, s)
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	reloaded, _ := r.ListEvents(ctx, tk.ID, 0)
	otherEv, _ := r.ListEvents(ctx, other.ID, 0)

	// Assert
	for name, ev := range map[string][]factory.TaskEvent{"memory": inMemory, "file": reloaded} {
		if len(ev) != 500 || ev[0].Text != "e120" || ev[499].Text != "e619" {
			t.Fatalf("%s: %d events, first %q, last %q; want the newest 500 (e120..e619)", name, len(ev), ev[0].Text, ev[len(ev)-1].Text)
		}
	}
	if len(otherEv) != 1 {
		t.Fatalf("another task's history was trimmed: %d", len(otherEv))
	}
}

func TestSpendLedgerOlderThanFortyDaysIsDroppedOnLoad(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	tk := newTask(t, s, factory.Task{ProjectID: "p1", Title: "t"})
	_, err := s.AddSpend(ctx, tk.ID, factory.Spend{USD: 1, Runs: 1})
	must(t, err)
	clk.Advance(30 * 24 * time.Hour)
	_, err = s.AddSpend(ctx, tk.ID, factory.Spend{USD: 2, Runs: 1})
	must(t, err)
	closeStore(t, s)

	// Act: 41 days after the first entry, 11 after the second
	clk.Advance(11 * 24 * time.Hour)
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	ledger, _ := r.SpendSince(ctx, "p1", time.Time{})
	got, _ := r.GetTask(ctx, tk.ID)

	// Assert
	if ledger.USD != 2 || ledger.Runs != 1 {
		t.Fatalf("ledger = %+v, want only the entry of the last 40 days", ledger)
	}
	if got.Spent.USD != 3 {
		t.Fatalf("the task's own total must keep everything: %+v", got.Spent)
	}
}

// waitFor polls cond until it holds; there is no fixed sleep to tune.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A burst of AppendEvent calls must cost far fewer rewrites than calls. Writes counts completed
// atomic replacements of the file (a test hook next to the rename), so the assertion does not depend
// on timing: the burst finishes inside one 200 ms window, hence at most the writes the setup made
// plus the trailing flush.
func TestABurstOfEventsIsCoalescedIntoFewFileWrites(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	tasks := make([]*factory.Task, 4)
	for i := range tasks {
		tasks[i] = newTask(t, s, factory.Task{Title: fmt.Sprint("t", i)})
	}
	setupWrites := s.Writes()

	// Act
	for i := range 1000 {
		must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tasks[i%len(tasks)].ID, Kind: factory.EventNote, Text: fmt.Sprint(i)}))
	}
	burstWrites := s.Writes() - setupWrites
	closeStore(t, s)
	total := s.Writes() - setupWrites
	r := openAt(t, path, clk)
	defer closeStore(t, r)

	// Assert
	if burstWrites > 5 {
		t.Fatalf("1000 AppendEvent calls rewrote the file %d times", burstWrites)
	}
	if total > 6 {
		t.Fatalf("burst + Close rewrote the file %d times", total)
	}
	for i, tk := range tasks {
		ev, _ := r.ListEvents(ctx, tk.ID, 0)
		if len(ev) != 250 || ev[len(ev)-1].Text != fmt.Sprint(996+i) {
			t.Fatalf("task %d: %d events after the restart, last %q", i, len(ev), ev[len(ev)-1].Text)
		}
	}
}

func TestPendingEventsAreWrittenByTheTrailingFlushWithoutClose(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	s.SetFlushInterval(5 * time.Millisecond)
	tk := newTask(t, s, factory.Task{Title: "t"})
	base := s.Writes()

	// Act
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: "late"}))
	waitFor(t, "the trailing flush", func() bool { return s.Writes() > base })
	defer closeStore(t, s)
	peek, err := factorystore.Open(path, clk.Now)
	must(t, err)
	defer closeStore(t, peek)
	ev, _ := peek.ListEvents(ctx, tk.ID, 0)

	// Assert
	if len(ev) != 1 || ev[0].Text != "late" {
		t.Fatalf("file holds %+v", ev)
	}
}

func TestCloseFlushesPendingEventsAndIsIdempotent(t *testing.T) {
	// Arrange: a flush window so long that only Close can write the events
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	s.SetFlushInterval(time.Hour)
	tk := newTask(t, s, factory.Task{Title: "t"})
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: "pending"}))
	_, err := s.SeenDelivery(ctx, "d1")
	must(t, err)
	peek := openAt(t, path, clk)
	beforeClose, _ := peek.ListEvents(ctx, tk.ID, 0)
	closeStore(t, peek)

	// Act
	first := s.Close()
	second := s.Close()
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	afterClose, _ := r.ListEvents(ctx, tk.ID, 0)
	seen, _ := r.SeenDelivery(ctx, "d1")

	// Assert
	if len(beforeClose) != 0 {
		t.Fatalf("the event was written before the flush window ended: %+v", beforeClose)
	}
	if first != nil || second != nil {
		t.Fatalf("Close = %v, %v", first, second)
	}
	if len(afterClose) != 1 || afterClose[0].Text != "pending" || !seen {
		t.Fatalf("after Close: events %+v, delivery seen %v", afterClose, seen)
	}
}

func TestAChangeAfterCloseIsStillPersisted(t *testing.T) {
	// Arrange: a late event from a goroutine that outlived shutdown must not be lost
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	tk := newTask(t, s, factory.Task{Title: "t"})
	closeStore(t, s)

	// Act
	must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: "late"}))
	r := openAt(t, path, clk)
	defer closeStore(t, r)
	ev, _ := r.ListEvents(ctx, tk.ID, 0)

	// Assert
	if len(ev) != 1 {
		t.Fatalf("events = %+v", ev)
	}
}

func TestConcurrentMutationsLeaveAFileThatLoadsToTheSameState(t *testing.T) {
	// Arrange
	ctx := context.Background()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "factory.json")
	s := openAt(t, path, clk)
	s.SetFlushInterval(2 * time.Millisecond)
	const workers, rounds = 16, 12

	// Act
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := fmt.Sprint("p", w%3)
			tk, _, err := s.CreateTask(ctx, factory.Task{ProjectID: p, Title: fmt.Sprint("w", w), Source: factory.Source{Kind: factory.SourceManual, Key: fmt.Sprint("k", w)}})
			must(t, err)
			trig, err := s.PutTrigger(ctx, factory.Trigger{ProjectID: p, Name: fmt.Sprint("t", w), Kind: factory.TriggerWebhook, Match: map[string]string{"w": fmt.Sprint(w)}})
			must(t, err)
			for i := range rounds {
				_, err := s.AddSpend(ctx, tk.ID, factory.Spend{USD: 0.5, Tokens: 10, Runs: 1})
				must(t, err)
				must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: fmt.Sprint(i)}))
				_, err = s.UpdateTask(ctx, tk.ID, factory.Patch{Attempts: new(i), AddFingerprint: fmt.Sprint("f", i)})
				must(t, err)
				_, err = s.SeenDelivery(ctx, fmt.Sprint("d", w, "-", i))
				must(t, err)
				must(t, s.SetThreadInfo(ctx, factory.ThreadInfo{ThreadID: fmt.Sprint("th", w), TaskID: tk.ID, Login: fmt.Sprint(i)}))
			}
			if w%2 == 0 {
				must(t, s.DeleteTrigger(ctx, trig.ID))
			}
		}()
	}
	wg.Wait()
	closeStore(t, s)
	r := openAt(t, path, clk)
	defer closeStore(t, r)

	// Assert
	wantTasks, _ := s.ListTasks(ctx, factory.Filter{})
	gotTasks, _ := r.ListTasks(ctx, factory.Filter{})
	if len(wantTasks) != workers || !reflect.DeepEqual(gotTasks, wantTasks) {
		t.Fatalf("tasks differ after a restart: %d vs %d", len(gotTasks), len(wantTasks))
	}
	wantTrig, _ := s.ListTriggers(ctx, "")
	gotTrig, _ := r.ListTriggers(ctx, "")
	if len(wantTrig) != workers/2 || !reflect.DeepEqual(gotTrig, wantTrig) {
		t.Fatalf("triggers differ: %d vs %d", len(gotTrig), len(wantTrig))
	}
	for _, tk := range wantTasks {
		want, _ := s.ListEvents(ctx, tk.ID, 0)
		got, _ := r.ListEvents(ctx, tk.ID, 0)
		if len(want) != rounds || !reflect.DeepEqual(got, want) {
			t.Fatalf("events of %s differ: %d vs %d", tk.ID, len(got), len(want))
		}
	}
	wantSpend, _ := s.SpendSince(ctx, "", time.Time{})
	gotSpend, _ := r.SpendSince(ctx, "", time.Time{})
	if wantSpend.Runs != workers*rounds || gotSpend != wantSpend {
		t.Fatalf("spend %+v vs %+v", gotSpend, wantSpend)
	}
	for w := range workers {
		want, _ := s.GetThreadInfo(ctx, fmt.Sprint("th", w))
		got, err := r.GetThreadInfo(ctx, fmt.Sprint("th", w))
		must(t, err)
		if *got != *want {
			t.Fatalf("thread %d: %+v vs %+v", w, got, want)
		}
		for i := range rounds {
			if seen, _ := r.SeenDelivery(ctx, fmt.Sprint("d", w, "-", i)); !seen {
				t.Fatalf("delivery d%d-%d forgotten", w, i)
			}
		}
	}
}
