package factorystore_test

// The factory.Store conformance suite: the same behavioural cases run against
// every backend (in-memory, file-backed, Postgres). Only what the port guarantees is asserted.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/factory"
)

// clock is a settable clock shared by the store and the test.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// eachStore runs fn against a fresh store of every backend (Postgres is skipped when unconfigured).
// The file-backed store runs every case too: persisting must not change what the port guarantees.
func eachStore(t *testing.T, fn func(t *testing.T, s factory.Store, clk *clock)) {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T, clk *clock) factory.Store
	}{
		{"mem", func(_ *testing.T, clk *clock) factory.Store { return factorystore.NewMem(clk.Now) }},
		{"file", func(t *testing.T, clk *clock) factory.Store {
			m, err := factorystore.Open(filepath.Join(t.TempDir(), "factory.json"), clk.Now)
			must(t, err)
			t.Cleanup(func() { must(t, m.Close()) })
			return m
		}},
		{"pg", func(t *testing.T, clk *clock) factory.Store { return newPG(t, clk.Now) }},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) {
			clk := newClock()
			fn(t, b.open(t, clk), clk)
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, what string, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("%s: err = %v, want %v", what, err, want)
	}
}

func newTask(t *testing.T, s factory.Store, task factory.Task) *factory.Task {
	t.Helper()
	got, created, err := s.CreateTask(context.Background(), task)
	must(t, err)
	if !created {
		t.Fatalf("task %q not created", task.Title)
	}
	return got
}

func setState(t *testing.T, s factory.Store, id string, st factory.State) *factory.Task {
	t.Helper()
	got, err := s.UpdateTask(context.Background(), id, factory.Patch{State: new(st)})
	must(t, err)
	return got
}

func TestCreateTaskStoresItReadyWithFreshIDAndTimestamps(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		in := factory.Task{
			ProjectID: "p1", Owner: "alice", Title: "fix", Spec: "do it", Role: factory.RoleCoder,
			Source: factory.Source{Kind: factory.SourceGitHub, Key: "github:issue:acme/app#1", URL: "https://x"},
			Labels: []string{"bug", "p0"}, Priority: 3, Repo: "acme/app", BaseBranch: "main", AutoMerge: true, MaxAttempts: 4,
			Policy: factory.PolicySandboxed, Budget: factory.Budget{MaxUSD: 2.5, MaxTokens: 1000},
			NotBefore: clk.Now().Add(time.Hour),
		}

		// Act
		got, created, err := s.CreateTask(ctx, in)

		// Assert
		must(t, err)
		if !created || got.ID == "" || got.State != factory.StateReady {
			t.Fatalf("created=%v id=%q state=%s", created, got.ID, got.State)
		}
		if !got.CreatedAt.Equal(clk.Now()) || !got.UpdatedAt.Equal(clk.Now()) || !got.FinishedAt.IsZero() {
			t.Fatalf("timestamps %v %v %v", got.CreatedAt, got.UpdatedAt, got.FinishedAt)
		}
		back, err := s.GetTask(ctx, got.ID)
		must(t, err)
		in.ID, in.State, in.CreatedAt, in.UpdatedAt = got.ID, factory.StateReady, got.CreatedAt, got.UpdatedAt
		if !back.NotBefore.Equal(in.NotBefore) {
			t.Fatalf("notBefore = %v, want %v", back.NotBefore, in.NotBefore)
		}
		back.NotBefore, in.NotBefore = time.Time{}, time.Time{}
		back.CreatedAt, back.UpdatedAt = in.CreatedAt, in.UpdatedAt
		if !reflect.DeepEqual(*back, in) {
			t.Fatalf("round trip:\n got %+v\nwant %+v", *back, in)
		}
		_, err = s.GetTask(ctx, "missing")
		wantErr(t, "GetTask of an unknown id", err, factory.ErrNotFound)
	})
}

func TestCreateTaskWithTheSameSourceKeyReturnsTheExistingTask(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		key := factory.Source{Kind: factory.SourceGitHub, Key: "github:issue:acme/app#12"}
		first := newTask(t, s, factory.Task{ProjectID: "p1", Title: "first", Source: key})

		// Act
		again, created, err := s.CreateTask(ctx, factory.Task{ProjectID: "p1", Title: "second", Source: key})
		other, otherCreated, otherErr := s.CreateTask(ctx, factory.Task{ProjectID: "p2", Title: "other project", Source: key})
		a, aCreated, aErr := s.CreateTask(ctx, factory.Task{ProjectID: "p1", Title: "no key a"})
		b, bCreated, bErr := s.CreateTask(ctx, factory.Task{ProjectID: "p1", Title: "no key b"})

		// Assert
		must(t, err)
		if created || again.ID != first.ID || again.Title != "first" {
			t.Fatalf("redelivery created=%v task=%+v", created, again)
		}
		must(t, otherErr)
		if !otherCreated || other.ID == first.ID {
			t.Fatalf("another project shares the key: created=%v", otherCreated)
		}
		must(t, aErr)
		must(t, bErr)
		if !aCreated || !bCreated || a.ID == b.ID {
			t.Fatalf("tasks without a key must not collide: %v %v", aCreated, bCreated)
		}
		if got, _ := s.ListTasks(ctx, factory.Filter{ProjectID: "p1", SourceKey: key.Key}); len(got) != 1 {
			t.Fatalf("tasks with the key in p1 = %d", len(got))
		}
	})
}

func TestCreateTaskIsIdempotentUnderConcurrentRedelivery(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		const n = 16
		ctx := context.Background()
		var wg sync.WaitGroup
		ids := make([]string, n)
		created := make([]bool, n)
		errs := make([]error, n)

		// Act
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tk, c, err := s.CreateTask(ctx, factory.Task{ProjectID: "p1", Title: fmt.Sprint("t", i), Source: factory.Source{Key: "cron:nightly:2026-10-31"}})
				if err == nil {
					ids[i], created[i] = tk.ID, c
				}
				errs[i] = err
			}()
		}
		wg.Wait()

		// Assert
		wins := 0
		for i := range n {
			must(t, errs[i])
			if created[i] {
				wins++
			}
			if ids[i] != ids[0] {
				t.Fatalf("callers got different tasks: %v", ids)
			}
		}
		if wins != 1 {
			t.Fatalf("%d callers created the task, want exactly 1", wins)
		}
		if got, _ := s.ListTasks(ctx, factory.Filter{ProjectID: "p1"}); len(got) != 1 {
			t.Fatalf("%d tasks stored", len(got))
		}
	})
}

func TestCreateTaskRejectsBadDependenciesAndDuplicateIDs(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		existing := newTask(t, s, factory.Task{ID: "fixed", Title: "a"})
		cases := []struct {
			name string
			task factory.Task
		}{
			{"unknown dependency", factory.Task{Title: "x", DependsOn: []string{"nope"}}},
			{"depends on itself", factory.Task{ID: "me", Title: "x", DependsOn: []string{"me"}}},
			{"duplicate id", factory.Task{ID: existing.ID, Title: "x"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				// Act
				got, created, err := s.CreateTask(ctx, tc.task)

				// Assert
				wantErr(t, tc.name, err, factory.ErrInvalid)
				if got != nil || created {
					t.Fatalf("returned %+v created=%v with the error", got, created)
				}
			})
		}
		if all, _ := s.ListTasks(ctx, factory.Filter{}); len(all) != 1 {
			t.Fatalf("rejected creates left %d tasks", len(all))
		}
	})
}

func TestTasksWaitForTheirDependenciesAndAreReleasedWhenTheLastIsDone(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		a := newTask(t, s, factory.Task{Title: "a"})
		b := newTask(t, s, factory.Task{Title: "b"})
		c := newTask(t, s, factory.Task{Title: "c", DependsOn: []string{a.ID, b.ID}})
		d := newTask(t, s, factory.Task{Title: "d", DependsOn: []string{c.ID}})
		if c.State != factory.StatePending || d.State != factory.StatePending {
			t.Fatalf("states c=%s d=%s, want pending", c.State, d.State)
		}

		// Act + Assert: one of two dependencies done keeps c waiting
		setState(t, s, a.ID, factory.StateDone)
		if got, _ := s.GetTask(ctx, c.ID); got.State != factory.StatePending {
			t.Fatalf("c = %s after one dependency, want pending", got.State)
		}
		// a failed dependency never releases
		setState(t, s, b.ID, factory.StateFailed)
		if got, _ := s.GetTask(ctx, c.ID); got.State != factory.StatePending {
			t.Fatalf("c = %s after a failed dependency, want pending", got.State)
		}
		setState(t, s, b.ID, factory.StateDone)
		if got, _ := s.GetTask(ctx, c.ID); got.State != factory.StateReady {
			t.Fatalf("c = %s after both dependencies, want ready", got.State)
		}
		if got, _ := s.GetTask(ctx, d.ID); got.State != factory.StatePending {
			t.Fatalf("d = %s while c is not done, want pending", got.State)
		}
		setState(t, s, c.ID, factory.StateDone)
		if got, _ := s.GetTask(ctx, d.ID); got.State != factory.StateReady {
			t.Fatalf("d = %s after c is done, want ready", got.State)
		}
		// a task created on done dependencies starts ready
		if e := newTask(t, s, factory.Task{Title: "e", DependsOn: []string{a.ID, b.ID}}); e.State != factory.StateReady {
			t.Fatalf("e = %s, want ready", e.State)
		}
	})
}

func TestUpdatingDependenciesRevalidatesThem(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		a := newTask(t, s, factory.Task{Title: "a"})
		b := newTask(t, s, factory.Task{Title: "b"})

		// Act
		_, errUnknown := s.UpdateTask(ctx, b.ID, factory.Patch{DependsOn: &[]string{"nope"}})
		_, errSelf := s.UpdateTask(ctx, b.ID, factory.Patch{DependsOn: &[]string{b.ID}})
		got, errOK := s.UpdateTask(ctx, b.ID, factory.Patch{DependsOn: &[]string{a.ID}})

		// Assert
		wantErr(t, "unknown dependency", errUnknown, factory.ErrInvalid)
		wantErr(t, "self dependency", errSelf, factory.ErrInvalid)
		must(t, errOK)
		if !reflect.DeepEqual(got.DependsOn, []string{a.ID}) {
			t.Fatalf("dependsOn = %v", got.DependsOn)
		}
	})
}

func TestUpdateTaskAppliesOnlyTheFieldsOfThePatch(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		tk := newTask(t, s, factory.Task{Title: "t", Spec: "s", Labels: []string{"x"}, Policy: factory.PolicySupervised})
		later := clk.Now().Add(time.Hour)
		clk.Advance(time.Minute)

		// Act
		got, err := s.UpdateTask(ctx, tk.ID, factory.Patch{
			State: new(factory.StateBlocked), BlockedReason: new("needs a human"), Error: new("boom"), Result: new("report"),
			Title: new("t2"), ThreadID: new("th"), RunID: new("run"), Model: new("m"), Branch: new("agent/x"),
			PR: &factory.PullRequest{Number: 7, URL: "https://pr", HeadSHA: "abc"}, Attempts: new(2), MaxAttempts: new(5),
			AutoMerge: new(true), Budget: &factory.Budget{MaxUSD: 1}, NotBefore: &later, Feedback: new("fix ci"),
			Labels: &[]string{"a", "b"},
		})

		// Assert
		must(t, err)
		back, _ := s.GetTask(ctx, tk.ID)
		for _, g := range []*factory.Task{got, back} {
			if g.State != factory.StateBlocked || g.BlockedReason != "needs a human" || g.Error != "boom" || g.Result != "report" ||
				g.Title != "t2" || g.Spec != "s" || g.ThreadID != "th" || g.RunID != "run" || g.Model != "m" || g.Branch != "agent/x" ||
				g.PR != (factory.PullRequest{Number: 7, URL: "https://pr", HeadSHA: "abc"}) || g.Attempts != 2 || g.MaxAttempts != 5 ||
				!g.AutoMerge || g.Budget != (factory.Budget{MaxUSD: 1}) || !g.NotBefore.Equal(later) || g.Feedback != "fix ci" ||
				!reflect.DeepEqual(g.Labels, []string{"a", "b"}) || g.Policy != factory.PolicySupervised {
				t.Fatalf("patched task %+v", g)
			}
			if !g.UpdatedAt.Equal(clk.Now()) || !g.CreatedAt.Before(g.UpdatedAt) || !g.FinishedAt.IsZero() {
				t.Fatalf("timestamps created=%v updated=%v finished=%v", g.CreatedAt, g.UpdatedAt, g.FinishedAt)
			}
		}
		if got, _ := s.ListTasks(ctx, factory.Filter{ThreadID: "th"}); len(got) != 1 || got[0].ID != tk.ID {
			t.Fatalf("list by thread = %+v", got)
		}
	})
}

func TestFinishedAtIsSetOnTerminalStatesAndClearedWhenTheTaskReopens(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		tk := newTask(t, s, factory.Task{Title: "t"})
		clk.Advance(time.Minute)

		// Act
		done := setState(t, s, tk.ID, factory.StateFailed)
		clk.Advance(time.Minute)
		reopened := setState(t, s, tk.ID, factory.StateReady)

		// Assert
		if !done.FinishedAt.Equal(clk.Now().Add(-time.Minute)) {
			t.Fatalf("finishedAt = %v", done.FinishedAt)
		}
		if !reopened.FinishedAt.IsZero() {
			t.Fatalf("finishedAt = %v after reopening", reopened.FinishedAt)
		}
	})
}

func TestUpdateTaskIfStateIsAConditionalWrite(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		tk := newTask(t, s, factory.Task{Title: "t"})
		title := "changed"

		// Act
		_, conflict := s.UpdateTask(ctx, tk.ID, factory.Patch{IfState: []factory.State{factory.StateRunning, factory.StateReview}, Title: &title})
		ok, errOK := s.UpdateTask(ctx, tk.ID, factory.Patch{IfState: []factory.State{factory.StateReview, factory.StateReady}, State: new(factory.StateRunning)})
		_, invalid := s.UpdateTask(ctx, tk.ID, factory.Patch{State: new(factory.State("bogus"))})
		_, missing := s.UpdateTask(ctx, "nope", factory.Patch{Title: &title})

		// Assert
		wantErr(t, "IfState mismatch", conflict, factory.ErrConflict)
		must(t, errOK)
		if ok.State != factory.StateRunning || ok.Title != "t" {
			t.Fatalf("task after the winning patch: %+v (the conflicting patch must not have applied)", ok)
		}
		wantErr(t, "unknown state", invalid, factory.ErrInvalid)
		wantErr(t, "unknown task", missing, factory.ErrNotFound)
		if got, _ := s.GetTask(ctx, tk.ID); got.State != factory.StateRunning {
			t.Fatalf("an invalid state was stored: %s", got.State)
		}
	})
}

func TestConcurrentClaimsOfAReadyTaskAdmitExactlyOneWinner(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		const n = 16
		ctx := context.Background()
		tk := newTask(t, s, factory.Task{Title: "t"})
		errs := make([]error, n)
		var wg sync.WaitGroup

		// Act
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				run := fmt.Sprint("run-", i)
				_, errs[i] = s.UpdateTask(ctx, tk.ID, factory.Patch{IfState: []factory.State{factory.StateReady}, State: new(factory.StateRunning), RunID: &run})
			}()
		}
		wg.Wait()

		// Assert
		winners := 0
		for _, err := range errs {
			switch {
			case err == nil:
				winners++
			case !errors.Is(err, factory.ErrConflict):
				t.Fatalf("unexpected error %v", err)
			}
		}
		if winners != 1 {
			t.Fatalf("%d claimers won, want exactly 1", winners)
		}
	})
}

func TestFingerprintsKeepTheLastTen(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		tk := newTask(t, s, factory.Task{Title: "t"})

		// Act
		var got *factory.Task
		for i := range 13 {
			var err error
			got, err = s.UpdateTask(ctx, tk.ID, factory.Patch{AddFingerprint: fmt.Sprintf("fp%02d", i)})
			must(t, err)
		}

		// Assert
		want := []string{"fp03", "fp04", "fp05", "fp06", "fp07", "fp08", "fp09", "fp10", "fp11", "fp12"}
		back, _ := s.GetTask(ctx, tk.ID)
		if !reflect.DeepEqual(got.Fingerprints, want) || !reflect.DeepEqual(back.Fingerprints, want) {
			t.Fatalf("fingerprints %v / %v, want %v", got.Fingerprints, back.Fingerprints, want)
		}
	})
}

func TestListTasksFiltersOrdersAndLimits(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		base := clk.Now()
		mk := func(id, project, owner string, prio int, age time.Duration, extra func(*factory.Task)) {
			tk := factory.Task{ID: id, ProjectID: project, Owner: owner, Title: id, Priority: prio, CreatedAt: base.Add(age)}
			if extra != nil {
				extra(&tk)
			}
			newTask(t, s, tk)
		}
		mk("old-low", "p1", "alice", 0, 0, nil)
		mk("new-low", "p1", "bob", 0, time.Minute, func(t *factory.Task) { t.ParentID = "parent" })
		mk("tie-b", "p2", "alice", 0, 2*time.Minute, nil)
		mk("tie-a", "p2", "alice", 0, 2*time.Minute, nil)
		mk("high", "p2", "bob", 5, 3*time.Minute, func(t *factory.Task) { t.Source.Key = "k" })
		setState(t, s, "old-low", factory.StateDone)
		ids := func(f factory.Filter) []string {
			got, err := s.ListTasks(ctx, f)
			must(t, err)
			out := make([]string, len(got))
			for i, g := range got {
				out[i] = g.ID
			}
			return out
		}

		// Act + Assert
		cases := []struct {
			name string
			f    factory.Filter
			want []string
		}{
			{"order: priority desc, created asc, id asc", factory.Filter{}, []string{"high", "old-low", "new-low", "tie-a", "tie-b"}},
			{"project", factory.Filter{ProjectID: "p2"}, []string{"high", "tie-a", "tie-b"}},
			{"owner", factory.Filter{Owner: "bob"}, []string{"high", "new-low"}},
			{"states", factory.Filter{States: []factory.State{factory.StateDone, factory.StateRunning}}, []string{"old-low"}},
			{"parent", factory.Filter{ParentID: "parent"}, []string{"new-low"}},
			{"source key", factory.Filter{SourceKey: "k"}, []string{"high"}},
			{"fields are ANDed", factory.Filter{ProjectID: "p2", Owner: "alice", States: []factory.State{factory.StateReady}}, []string{"tie-a", "tie-b"}},
			{"limit", factory.Filter{Limit: 2}, []string{"high", "old-low"}},
			{"no match", factory.Filter{Owner: "carol"}, []string{}},
		}
		for _, tc := range cases {
			if got := ids(tc.f); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}

func TestListTasksReturnsAtMost200ByDefault(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		for i := range 205 {
			newTask(t, s, factory.Task{ID: fmt.Sprintf("t%03d", i), Title: "t"})
		}

		// Act
		got, err := s.ListTasks(context.Background(), factory.Filter{})

		// Assert
		must(t, err)
		if len(got) != 200 {
			t.Fatalf("%d tasks, want 200", len(got))
		}
	})
}

func TestConcurrentAddSpendSumsExactly(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		const n = 32
		ctx := context.Background()
		tk := newTask(t, s, factory.Task{ProjectID: "p1", Title: "t"})
		var wg sync.WaitGroup
		errs := make([]error, n)

		// Act
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				d := factory.Spend{USD: 0.25, Tokens: 100, Runs: 1}
				if i%4 == 0 {
					d.Unpriced = 1
				}
				_, errs[i] = s.AddSpend(ctx, tk.ID, d)
			}()
		}
		wg.Wait()

		// Assert
		for _, err := range errs {
			must(t, err)
		}
		got, _ := s.GetTask(ctx, tk.ID)
		if got.Spent != (factory.Spend{USD: 8, Tokens: 3200, Runs: 32, Unpriced: 8}) {
			t.Fatalf("spent = %+v", got.Spent)
		}
		if since, _ := s.SpendSince(ctx, "p1", time.Time{}); since != got.Spent {
			t.Fatalf("ledger = %+v, task = %+v", since, got.Spent)
		}
		_, err := s.AddSpend(ctx, "nope", factory.Spend{Runs: 1})
		wantErr(t, "AddSpend on an unknown task", err, factory.ErrNotFound)
	})
}

func TestSpendSinceCountsTheWindowAndTheProject(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		a := newTask(t, s, factory.Task{ProjectID: "p1", Title: "a"})
		b := newTask(t, s, factory.Task{ProjectID: "p2", Title: "b"})
		start := clk.Now()
		_, err := s.AddSpend(ctx, a.ID, factory.Spend{USD: 1, Tokens: 10, Runs: 1})
		must(t, err)
		clk.Advance(time.Hour)
		mid := clk.Now()
		_, err = s.AddSpend(ctx, a.ID, factory.Spend{USD: 2, Tokens: 20, Runs: 1, Unpriced: 1})
		must(t, err)
		_, err = s.AddSpend(ctx, b.ID, factory.Spend{USD: 4, Tokens: 40, Runs: 1})
		must(t, err)

		cases := []struct {
			name    string
			project string
			since   time.Time
			want    factory.Spend
		}{
			{"project from the start", "p1", start, factory.Spend{USD: 3, Tokens: 30, Runs: 2, Unpriced: 1}},
			{"the window start is inclusive", "p1", mid, factory.Spend{USD: 2, Tokens: 20, Runs: 1, Unpriced: 1}},
			{"other project", "p2", start, factory.Spend{USD: 4, Tokens: 40, Runs: 1}},
			{"all projects", "", start, factory.Spend{USD: 7, Tokens: 70, Runs: 3, Unpriced: 1}},
			{"all projects in the window", "", mid, factory.Spend{USD: 6, Tokens: 60, Runs: 2, Unpriced: 1}},
			{"after everything", "", mid.Add(time.Second), factory.Spend{}},
			{"unknown project", "p9", start, factory.Spend{}},
		}
		for _, tc := range cases {
			// Act
			got, err := s.SpendSince(ctx, tc.project, tc.since)

			// Assert
			must(t, err)
			if got != tc.want {
				t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
			}
		}
	})
}

func TestEventsComeOldestFirstAndALimitKeepsTheNewest(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		tk := newTask(t, s, factory.Task{Title: "t"})
		other := newTask(t, s, factory.Task{Title: "other"})
		for i := range 5 {
			clk.Advance(time.Second)
			e := factory.TaskEvent{TaskID: tk.ID, Kind: factory.EventNote, Text: fmt.Sprint("e", i)}
			if i == 0 {
				e.Data = map[string]any{"sha": "abc", "n": float64(3), "nested": map[string]any{"ok": true}}
			}
			must(t, s.AppendEvent(ctx, e))
		}
		must(t, s.AppendEvent(ctx, factory.TaskEvent{TaskID: other.ID, Kind: factory.EventState, Text: "elsewhere", At: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)}))
		texts := func(ev []factory.TaskEvent) []string {
			out := []string{}
			for _, e := range ev {
				out = append(out, e.Text)
			}
			return out
		}

		// Act
		all, err := s.ListEvents(ctx, tk.ID, 0)
		must(t, err)
		last2, err := s.ListEvents(ctx, tk.ID, 2)
		must(t, err)
		huge, err := s.ListEvents(ctx, tk.ID, 100)
		must(t, err)
		none, err := s.ListEvents(ctx, "nope", 10)
		must(t, err)
		otherEv, _ := s.ListEvents(ctx, other.ID, 0)

		// Assert
		if !reflect.DeepEqual(texts(all), []string{"e0", "e1", "e2", "e3", "e4"}) {
			t.Fatalf("events %v", texts(all))
		}
		if !reflect.DeepEqual(texts(last2), []string{"e3", "e4"}) || len(huge) != 5 || len(none) != 0 {
			t.Fatalf("limit 2: %v, limit 100: %d, unknown task: %d", texts(last2), len(huge), len(none))
		}
		if !sort.SliceIsSorted(all, func(i, j int) bool { return all[i].ID < all[j].ID }) || all[0].ID == 0 || all[0].ID == all[1].ID {
			t.Fatalf("event ids not increasing: %+v", all)
		}
		if !all[0].At.Equal(clk.Now().Add(-4*time.Second)) || all[0].Kind != factory.EventNote || all[0].TaskID != tk.ID {
			t.Fatalf("first event %+v", all[0])
		}
		if !reflect.DeepEqual(all[0].Data, map[string]any{"sha": "abc", "n": float64(3), "nested": map[string]any{"ok": true}}) || all[1].Data != nil {
			t.Fatalf("event data %v / %v", all[0].Data, all[1].Data)
		}
		if len(otherEv) != 1 || !otherEv[0].At.Equal(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("an explicit timestamp must be kept: %+v", otherEv)
		}
		wantErr(t, "event of an unknown task", s.AppendEvent(ctx, factory.TaskEvent{TaskID: "nope", Kind: factory.EventNote}), factory.ErrNotFound)
	})
}

func TestTriggerCRUD(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		tmpl := factory.Template{Title: "Nightly {{.Time}}", Spec: "run", Labels: []string{"cron"}, Budget: factory.Budget{MaxUSD: 3}, AutoMerge: true, Priority: 2}
		cron := factory.Trigger{ProjectID: "p1", Owner: "alice", Name: "nightly", Kind: factory.TriggerCron, Schedule: "@every 1h", Template: tmpl, Enabled: true, NextFireAt: clk.Now().Add(time.Hour)}
		hook := factory.Trigger{ProjectID: "p2", Owner: "bob", Name: "labeled", Kind: factory.TriggerWebhook, Event: "issues.labeled", Match: map[string]string{"label": "agent"}, Template: factory.Template{Title: "{{.Fields.title}}"}}

		// Act
		c1, err := s.PutTrigger(ctx, cron)
		must(t, err)
		clk.Advance(time.Minute)
		h1, err := s.PutTrigger(ctx, hook)
		must(t, err)
		_, badKind := s.PutTrigger(ctx, factory.Trigger{Name: "x", Kind: "carrier-pigeon"})

		// Assert
		if c1.ID == "" || !c1.CreatedAt.Equal(clk.Now().Add(-time.Minute)) || !c1.NextFireAt.Equal(cron.NextFireAt) || !c1.LastFiredAt.IsZero() {
			t.Fatalf("stored cron trigger %+v", c1)
		}
		if got, err := s.GetTrigger(ctx, c1.ID); err != nil || !reflect.DeepEqual(got.Template, tmpl) || got.Name != "nightly" || got.Schedule != "@every 1h" || !got.Enabled {
			t.Fatalf("get cron: %+v %v", got, err)
		}
		if got, err := s.GetTrigger(ctx, h1.ID); err != nil || !reflect.DeepEqual(got.Match, map[string]string{"label": "agent"}) || got.Event != "issues.labeled" || got.Enabled || !got.NextFireAt.IsZero() {
			t.Fatalf("get webhook: %+v %v", got, err)
		}
		wantErr(t, "unknown kind", badKind, factory.ErrInvalid)
		_, err = s.GetTrigger(ctx, "nope")
		wantErr(t, "GetTrigger of an unknown id", err, factory.ErrNotFound)
		if all, _ := s.ListTriggers(ctx, ""); len(all) != 2 || all[0].ID != c1.ID || all[1].ID != h1.ID {
			t.Fatalf("list all (creation order): %+v", all)
		}
		if p2, _ := s.ListTriggers(ctx, "p2"); len(p2) != 1 || p2[0].ID != h1.ID {
			t.Fatalf("list p2: %+v", p2)
		}

		// Act: Put with an existing ID replaces
		cron.ID, cron.Name, cron.Enabled, cron.CreatedAt = c1.ID, "renamed", false, c1.CreatedAt
		again, err := s.PutTrigger(ctx, cron)
		must(t, err)
		if all, _ := s.ListTriggers(ctx, ""); len(all) != 2 || again.Name != "renamed" || again.Enabled {
			t.Fatalf("replace: %+v %+v", again, all)
		}

		// Act: delete
		must(t, s.DeleteTrigger(ctx, c1.ID))
		_, err = s.GetTrigger(ctx, c1.ID)
		wantErr(t, "deleted trigger", err, factory.ErrNotFound)
		wantErr(t, "delete twice", s.DeleteTrigger(ctx, c1.ID), factory.ErrNotFound)
	})
}

func TestClaimDueTriggersClaimsOnlyDueEnabledCronTriggersAndAdvancesThem(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		ctx := context.Background()
		now := clk.Now()
		put := func(id string, tr factory.Trigger) {
			tr.ID, tr.Name = id, id
			if tr.Kind == "" {
				tr.Kind = factory.TriggerCron
			}
			_, err := s.PutTrigger(ctx, tr)
			must(t, err)
		}
		put("due", factory.Trigger{Enabled: true, NextFireAt: now.Add(-time.Minute)})
		put("exactly-now", factory.Trigger{Enabled: true, NextFireAt: now})
		put("future", factory.Trigger{Enabled: true, NextFireAt: now.Add(time.Minute)})
		put("disabled", factory.Trigger{Enabled: false, NextFireAt: now.Add(-time.Minute)})
		put("unscheduled", factory.Trigger{Enabled: true})
		put("webhook", factory.Trigger{Kind: factory.TriggerWebhook, Enabled: true, NextFireAt: now.Add(-time.Minute)})
		next := func(factory.Trigger) time.Time { return now.Add(time.Hour) }

		// Act
		got, err := s.ClaimDueTriggers(ctx, now, next)
		again, errAgain := s.ClaimDueTriggers(ctx, now, next)

		// Assert
		must(t, err)
		must(t, errAgain)
		var ids []string
		for _, tr := range got {
			ids = append(ids, tr.ID)
		}
		if !reflect.DeepEqual(ids, []string{"due", "exactly-now"}) {
			t.Fatalf("claimed %v", ids)
		}
		if got[0].NextFireAt.Equal(now.Add(time.Hour)) {
			t.Fatalf("the claimed copy must carry the tick that fired, got %v", got[0].NextFireAt)
		}
		if len(again) != 0 {
			t.Fatalf("second claim returned %d triggers", len(again))
		}
		due, _ := s.GetTrigger(ctx, "due")
		if !due.NextFireAt.Equal(now.Add(time.Hour)) || !due.LastFiredAt.Equal(now.Add(-time.Minute)) {
			t.Fatalf("after the claim next=%v last=%v", due.NextFireAt, due.LastFiredAt)
		}
		if f, _ := s.GetTrigger(ctx, "future"); !f.NextFireAt.Equal(now.Add(time.Minute)) || !f.LastFiredAt.IsZero() {
			t.Fatalf("a trigger that is not due changed: %+v", f)
		}
	})
}

func TestConcurrentClaimersFireEachTickOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, clk *clock) {
		// Arrange
		const claimers = 8
		ctx := context.Background()
		now := clk.Now()
		for i := range 5 {
			_, err := s.PutTrigger(ctx, factory.Trigger{ID: fmt.Sprint("t", i), Name: "n", Kind: factory.TriggerCron, Enabled: true, NextFireAt: now.Add(-time.Duration(i+1) * time.Minute)})
			must(t, err)
		}
		var mu sync.Mutex
		claimed := map[string]int{}
		errs := make([]error, claimers)
		var wg sync.WaitGroup

		// Act
		for i := range claimers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := s.ClaimDueTriggers(ctx, now, func(factory.Trigger) time.Time { return now.Add(time.Hour) })
				mu.Lock()
				defer mu.Unlock()
				errs[i] = err
				for _, tr := range got {
					claimed[tr.ID]++
				}
			}()
		}
		wg.Wait()

		// Assert
		for _, err := range errs {
			must(t, err)
		}
		if len(claimed) != 5 {
			t.Fatalf("claimed %v, want all 5 triggers", claimed)
		}
		for id, n := range claimed {
			if n != 1 {
				t.Fatalf("trigger %s claimed %d times", id, n)
			}
			if tr, _ := s.GetTrigger(ctx, id); !tr.NextFireAt.Equal(now.Add(time.Hour)) {
				t.Fatalf("trigger %s next fire %v", id, tr.NextFireAt)
			}
		}
	})
}

func TestSeenDeliveryReportsRedeliveries(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		seen := func(id string) bool {
			got, err := s.SeenDelivery(ctx, id)
			must(t, err)
			return got
		}

		// Act + Assert
		if seen("d1") {
			t.Fatal("first delivery reported as seen")
		}
		if !seen("d1") {
			t.Fatal("redelivery not reported")
		}
		if seen("d2") {
			t.Fatal("another delivery reported as seen")
		}
		if seen("") || seen("") {
			t.Fatal("an empty id must never count as seen")
		}
	})
}

func TestConcurrentDeliveriesOfOneIDAreSeenOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		const n = 16
		var wg sync.WaitGroup
		var mu sync.Mutex
		fresh := 0

		// Act
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				seen, err := s.SeenDelivery(context.Background(), "delivery")
				must(t, err)
				if !seen {
					mu.Lock()
					fresh++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()

		// Assert
		if fresh != 1 {
			t.Fatalf("%d callers saw the delivery as new, want 1", fresh)
		}
	})
}

func TestThreadInfoRoundTripsAndReplaces(t *testing.T) {
	eachStore(t, func(t *testing.T, s factory.Store, _ *clock) {
		// Arrange
		ctx := context.Background()
		ti := factory.ThreadInfo{
			ThreadID: "th1", Login: "alice", TaskID: "task1", Role: factory.RoleReviewer, Instructions: "be strict", Policy: factory.PolicyAutonomous,
			ParentThreadID: "th0", ReadOnly: true, Repo: "acme/app", BaseBranch: "main", Branch: "agent/x",
		}

		// Act
		must(t, s.SetThreadInfo(ctx, ti))
		got, err := s.GetThreadInfo(ctx, "th1")
		must(t, err)
		_, missing := s.GetThreadInfo(ctx, "th2")
		replaced := factory.ThreadInfo{ThreadID: "th1", Login: "bob"}
		must(t, s.SetThreadInfo(ctx, replaced))
		after, _ := s.GetThreadInfo(ctx, "th1")

		// Assert
		if *got != ti {
			t.Fatalf("round trip %+v, want %+v", *got, ti)
		}
		wantErr(t, "ordinary thread", missing, factory.ErrNotFound)
		if *after != replaced {
			t.Fatalf("replaced info %+v, want %+v", *after, replaced)
		}
		wantErr(t, "empty thread id", s.SetThreadInfo(ctx, factory.ThreadInfo{}), factory.ErrInvalid)
	})
}
