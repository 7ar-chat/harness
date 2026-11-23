package workflowstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/workflowstore"
	"github.com/7ar-chat/harness/core/workflows"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newPG(t *testing.T) workflows.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_workflows_%d", time.Now().UnixNano())
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		pool.Close()
	})
	if err := migrate.Run(ctx, pool, schema); err != nil {
		t.Fatal(err)
	}
	return workflowstore.NewPG(pool, func() time.Time { return t0 })
}

func eachStore(t *testing.T, fn func(t *testing.T, s workflows.Store)) {
	t.Helper()
	now := func() time.Time { return t0 }
	backends := []struct {
		name string
		open func(t *testing.T) workflows.Store
	}{
		{"mem", func(*testing.T) workflows.Store { return workflowstore.NewMem(now) }},
		{"file", func(t *testing.T) workflows.Store {
			f, err := workflowstore.Open(filepath.Join(t.TempDir(), "workflows.json"), now)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}},
		{"pg", newPG},
	}
	for _, b := range backends {
		t.Run(b.name, func(t *testing.T) { fn(t, b.open(t)) })
	}
}

func wf(name string) workflows.Workflow {
	return workflows.Workflow{ProjectID: "p1", Name: name, Description: "d " + name, Nodes: []workflows.Node{
		{ID: "a", Kind: workflows.KindAgent, Prompt: "do " + name, Pos: workflows.Pos{X: 1.5, Y: 2}},
		{ID: "b", Kind: workflows.KindGate, DependsOn: []string{"a"}, Approvers: []string{"role:member"},
			OnFailure: &workflows.OnFailure{Action: workflows.FailRework, Target: "a", MaxRounds: 2}},
	}}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// same compares two values by their JSON, which is what every backend stores.
func same(t *testing.T, got, want any) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Fatalf("got  %s\nwant %s", g, w)
	}
}

func TestCreateGetListRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		// Act
		created := must(s.Create(ctx, wf("b-flow"), "alice"))
		must(s.Create(ctx, wf("a-flow"), "alice"))
		other := wf("a-flow")
		other.ProjectID = "p2"
		must(s.Create(ctx, other, "bob"))
		got := must(s.Get(ctx, created.ID))
		list := must(s.List(ctx, "p1"))

		// Assert
		if created.ID == "" || created.Version != 1 || created.CreatedBy != "alice" || !created.CreatedAt.Equal(t0) || created.Status != workflows.StatusDraft {
			t.Fatalf("created = %+v", created)
		}
		same(t, got, created)
		if len(list) != 2 || list[0].Name != "a-flow" || list[1].Name != "b-flow" {
			t.Fatalf("list = %+v", list)
		}
		if _, err := s.Get(ctx, "nope"); !errors.Is(err, workflows.ErrNotFound) {
			t.Fatalf("get of a missing workflow: %v", err)
		}
	})
}

func TestCreateRefusesADuplicateNameInTheProject(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		must(s.Create(ctx, wf("dup"), "a"))
		if _, err := s.Create(ctx, wf("dup"), "a"); !errors.Is(err, workflows.ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		bad := wf("")
		if _, err := s.Create(ctx, bad, "a"); !errors.Is(err, workflows.ErrInvalid) {
			t.Fatalf("no name: %v", err)
		}
	})
}

func TestUpdateBumpsVersionKeepsHistoryAndChecksStale(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		c := must(s.Create(ctx, wf("w"), "alice"))
		next := *c
		next.Description = "second"
		next.Status = workflows.StatusPublished
		next.PublishedVersion = 2

		// Act
		u := must(s.Update(ctx, next, "bob"))
		hist := must(s.History(ctx, c.ID))

		// Assert
		if u.Version != 2 || u.Description != "second" || u.UpdatedBy != "bob" || u.CreatedBy != "alice" || u.Status != workflows.StatusPublished {
			t.Fatalf("updated = %+v", u)
		}
		if len(hist) != 1 || hist[0].Version != 1 || hist[0].Workflow.Description != "d w" || hist[0].ChangedBy != "alice" {
			t.Fatalf("history = %+v", hist)
		}
		stale := *c // version 1 again
		if _, err := s.Update(ctx, stale, "x"); !errors.Is(err, workflows.ErrStale) {
			t.Fatalf("stale update: %v", err)
		}
		ghost := wf("ghost")
		ghost.ID = "ghost"
		if _, err := s.Update(ctx, ghost, "x"); !errors.Is(err, workflows.ErrNotFound) {
			t.Fatalf("update of a missing workflow: %v", err)
		}
		other := must(s.Create(ctx, wf("other"), "a"))
		clash := *other
		clash.Name = "w"
		if _, err := s.Update(ctx, clash, "a"); !errors.Is(err, workflows.ErrConflict) {
			t.Fatalf("rename onto a taken name: %v", err)
		}
	})
}

func TestHistoryKeepsTheLastTen(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		c := must(s.Create(ctx, wf("w"), "a"))
		cur := *c
		for i := 2; i <= 15; i++ {
			cur.Description = fmt.Sprintf("v%d", i)
			cur.Version = 0
			cur = *must(s.Update(ctx, cur, "a"))
		}
		hist := must(s.History(ctx, c.ID))
		if len(hist) != workflows.MaxHistory || hist[0].Version != 14 || hist[len(hist)-1].Version != 5 {
			t.Fatalf("history: %d entries, first %d, last %d", len(hist), hist[0].Version, hist[len(hist)-1].Version)
		}
		if got := must(s.Get(ctx, c.ID)); got.Version != 15 || got.Description != "v15" {
			t.Fatalf("current = %+v", got)
		}
	})
}

func TestDeleteAndDeleteProject(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		a := must(s.Create(ctx, wf("a"), "x"))
		must(s.Create(ctx, wf("b"), "x"))
		keep := wf("c")
		keep.ProjectID = "p2"
		kept := must(s.Create(ctx, keep, "x"))
		must(s.CreateRun(ctx, workflows.Run{ProjectID: "p1", WorkflowID: a.ID, State: workflows.RunRunning}))
		must(s.CreateRun(ctx, workflows.Run{ProjectID: "p2", WorkflowID: kept.ID, State: workflows.RunRunning}))

		if err := s.Delete(ctx, a.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, a.ID); !errors.Is(err, workflows.ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
		if _, err := s.History(ctx, a.ID); !errors.Is(err, workflows.ErrNotFound) {
			t.Fatalf("history of a deleted workflow: %v", err)
		}
		if err := s.DeleteProject(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		if l := must(s.List(ctx, "p1")); len(l) != 0 {
			t.Fatalf("p1 still has %+v", l)
		}
		if r := must(s.ListRuns(ctx, workflows.RunFilter{ProjectID: "p1"})); len(r) != 0 {
			t.Fatalf("p1 still has runs %+v", r)
		}
		if l := must(s.List(ctx, "p2")); len(l) != 1 {
			t.Fatalf("p2 lost its workflow: %+v", l)
		}
		if r := must(s.ListRuns(ctx, workflows.RunFilter{ProjectID: "p2"})); len(r) != 1 {
			t.Fatalf("p2 lost its run: %+v", r)
		}
	})
}

func run(wfID string) workflows.Run {
	return workflows.Run{
		ProjectID: "p1", WorkflowID: wfID, WorkflowName: "w", Owner: "alice", State: workflows.RunRunning,
		Graph:  wf("w").Nodes,
		Inputs: map[string]string{"task": "x"},
		Steps:  []workflows.NodeRun{{NodeID: "a", State: workflows.NodePending}, {NodeID: "b", State: workflows.NodePending}},
		Events: []workflows.RunEvent{{At: t0, Kind: "started", Text: "go"}},
	}
}

func TestRunRoundTripAndOptimisticUpdate(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		c := must(s.CreateRun(ctx, run("w1")))

		// Act
		got := must(s.GetRun(ctx, c.ID))
		upd := *got
		upd.Steps = []workflows.NodeRun{{NodeID: "a", State: workflows.NodeDone, Result: "ok", Outputs: map[string]string{"plan": "p"}}, {NodeID: "b", State: workflows.NodeWaiting}}
		u := must(s.UpdateRun(ctx, upd))

		// Assert
		if c.ID == "" || c.Version != 1 || !c.CreatedAt.Equal(t0) {
			t.Fatalf("created run = %+v", c)
		}
		same(t, got, c)
		if u.Version != 2 || u.Steps[0].Result != "ok" {
			t.Fatalf("updated run = %+v", u)
		}
		if _, err := s.UpdateRun(ctx, upd); !errors.Is(err, workflows.ErrStale) {
			t.Fatalf("update from a stale version: %v", err)
		}
		ghost := run("w1")
		ghost.ID = "ghost"
		if _, err := s.UpdateRun(ctx, ghost); !errors.Is(err, workflows.ErrNotFound) {
			t.Fatalf("update of a missing run: %v", err)
		}
		if _, err := s.GetRun(ctx, "ghost"); !errors.Is(err, workflows.ErrNotFound) {
			t.Fatalf("get of a missing run: %v", err)
		}
		fresh := must(s.GetRun(ctx, c.ID))
		same(t, fresh, u)
	})
}

func TestListRunsFiltersAndOrdersNewestFirst(t *testing.T) {
	eachStore(t, func(t *testing.T, s workflows.Store) {
		ctx := context.Background()
		a := run("w1")
		a.ID = "run-a"
		b := run("w2")
		b.ID = "run-b"
		b.State = workflows.RunDone
		c := run("w1")
		c.ID = "run-c"
		c.ParentRunID = "run-a"
		other := run("w1")
		other.ID = "run-d"
		other.ProjectID = "p2"
		for _, r := range []workflows.Run{a, b, c, other} {
			must(s.CreateRun(ctx, r))
		}
		ids := func(rs []workflows.Run) (out []string) {
			for _, r := range rs {
				out = append(out, r.ID)
			}
			return
		}
		cases := []struct {
			name string
			f    workflows.RunFilter
			want []string
		}{
			{"project", workflows.RunFilter{ProjectID: "p1"}, []string{"run-c", "run-b", "run-a"}},
			{"workflow", workflows.RunFilter{ProjectID: "p1", WorkflowID: "w2"}, []string{"run-b"}},
			{"state", workflows.RunFilter{States: []workflows.RunState{workflows.RunRunning}}, []string{"run-d", "run-c", "run-a"}},
			{"parent", workflows.RunFilter{ParentRunID: "run-a"}, []string{"run-c"}},
			{"limit", workflows.RunFilter{ProjectID: "p1", Limit: 1}, []string{"run-c"}},
		}
		for _, tc := range cases {
			got := ids(must(s.ListRuns(ctx, tc.f)))
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	})
}

func TestFileStoreSurvivesAReopen(t *testing.T) {
	// Arrange
	path := filepath.Join(t.TempDir(), "sub", "workflows.json")
	now := func() time.Time { return t0 }
	f := must(workflowstore.Open(path, now))
	w := must(f.Create(context.Background(), wf("keep"), "alice"))
	r := must(f.CreateRun(context.Background(), run(w.ID)))

	// Act
	g := must(workflowstore.Open(path, now))

	// Assert
	same(t, must(g.Get(context.Background(), w.ID)), w)
	same(t, must(g.GetRun(context.Background(), r.ID)), r)
}

func TestFileStoreRefusesAFileItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.json")
	if err := os.WriteFile(path, []byte(`{"version": 99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workflowstore.Open(path, nil); err == nil {
		t.Fatal("a file of another version was accepted")
	}
}
