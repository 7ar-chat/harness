package schedstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/core/schedule"
)

var ctx = context.Background()

var t0 = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// every10 is the next function of the tests: ten minutes after the claim.
func every10(_ schedule.Schedule, now time.Time) time.Time { return now.Add(10 * time.Minute) }

func newPG(t *testing.T) schedule.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	schema := fmt.Sprintf("work_test_sched_%d", time.Now().UnixNano())
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
	return schedstore.NewPG(pool, nil)
}

// eachStore runs fn against every backend; the file store runs every case too (persisting must not change the port).
func eachStore(t *testing.T, fn func(t *testing.T, s schedule.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, schedstore.NewMem()) })
	t.Run("file", func(t *testing.T) {
		f, err := schedstore.Open(filepath.Join(t.TempDir(), "schedules.json"))
		must(t, err)
		fn(t, f)
	})
	t.Run("pg", func(t *testing.T) { fn(t, newPG(t)) })
}

func sched(name string, due time.Time) schedule.Schedule {
	return schedule.Schedule{ProjectID: "p1", Owner: "ann", RunAs: "ann", Name: name, Prompt: "check the build", Cron: "*/10 * * * *",
		Timezone: "UTC", Enabled: true, NextRunAt: due}
}

func TestCreateGetUpdateListDelete(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		a, err := s.Create(ctx, sched("a", t0))
		must(t, err)
		_, err = s.Create(ctx, schedule.Schedule{ProjectID: "p2", Owner: "ann", RunAs: "ann", Name: "other project", Prompt: "x", Cron: "@daily"})
		must(t, err)
		_, err = s.Create(ctx, schedule.Schedule{Owner: "ann", RunAs: "ann", Name: "mine", Prompt: "x", Cron: "@daily"})
		must(t, err)
		_, err = s.Create(ctx, schedule.Schedule{Owner: "bob", RunAs: "bob", Name: "bobs", Prompt: "x", Cron: "@daily"})
		must(t, err)

		if a.TimeoutMinutes != 30 || a.Policy != schedule.PolicyDefault || a.Timezone != "UTC" || a.ID == "" || a.CreatedAt.IsZero() {
			t.Fatalf("defaults: %+v", a)
		}
		got, err := s.Get(ctx, a.ID)
		must(t, err)
		if got.Name != "a" || !got.NextRunAt.Equal(t0) || got.Owner != "ann" {
			t.Fatalf("get: %+v", got)
		}
		proj, _ := s.List(ctx, "p1", "")
		personal, _ := s.List(ctx, "", "ann")
		if len(proj) != 1 || len(personal) != 1 || personal[0].Name != "mine" {
			t.Fatalf("lists: project %d, personal %+v", len(proj), personal)
		}

		upd := *got
		upd.Name, upd.Owner, upd.ProjectID, upd.RunAs, upd.TimeoutMinutes, upd.Enabled = "renamed", "mallory", "elsewhere", "bob", 90, false
		out, err := s.Update(ctx, upd)
		must(t, err)
		if out.Name != "renamed" || out.TimeoutMinutes != 90 || out.Enabled || out.RunAs != "bob" || out.Owner != "ann" || out.ProjectID != "p1" {
			t.Fatalf("update: %+v", out)
		}

		must(t, s.Delete(ctx, a.ID))
		if _, err := s.Get(ctx, a.ID); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("get after delete: %v", err)
		}
		if err := s.Delete(ctx, a.ID); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("delete twice: %v", err)
		}
		if _, err := s.Update(ctx, upd); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("update after delete: %v", err)
		}
	})
}

func TestCreateRefusesInvalidSchedules(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		for name, mutate := range map[string]func(*schedule.Schedule){
			"no name":        func(x *schedule.Schedule) { x.Name = " " },
			"no prompt":      func(x *schedule.Schedule) { x.Prompt = "" },
			"no cron":        func(x *schedule.Schedule) { x.Cron = "" },
			"no runAs":       func(x *schedule.Schedule) { x.RunAs = "" },
			"timeout low":    func(x *schedule.Schedule) { x.TimeoutMinutes = -1 },
			"timeout high":   func(x *schedule.Schedule) { x.TimeoutMinutes = 241 },
			"unknown policy": func(x *schedule.Schedule) { x.Policy = "yolo" },
		} {
			x := sched("a", t0)
			mutate(&x)
			if _, err := s.Create(ctx, x); !errors.Is(err, schedule.ErrInvalid) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})
}

func TestClaimDueStartsOneRunAndSkipsTheMissedOnes(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		// Arrange: due since an hour ago (six missed ticks), another not due yet, a third disabled.
		due, err := s.Create(ctx, sched("due", t0.Add(-time.Hour)))
		must(t, err)
		_, err = s.Create(ctx, sched("later", t0.Add(time.Minute)))
		must(t, err)
		off := sched("off", t0.Add(-time.Hour))
		off.Enabled = false
		_, err = s.Create(ctx, off)
		must(t, err)

		// Act
		claims, err := s.ClaimDue(ctx, t0, every10)

		// Assert: one claim, the missed ticks are not replayed, the schedule moved on.
		must(t, err)
		if len(claims) != 1 || claims[0].Schedule.ID != due.ID {
			t.Fatalf("claims = %+v", claims)
		}
		r := claims[0].Run
		if r.Status != schedule.StatusRunning || r.Trigger != schedule.TriggerSchedule || r.StartedBy != "ann" || !r.FireAt.Equal(t0.Add(-time.Hour)) {
			t.Fatalf("run = %+v", r)
		}
		if !r.Deadline.Equal(t0.Add(35 * time.Minute)) {
			t.Fatalf("deadline = %v", r.Deadline)
		}
		got, _ := s.Get(ctx, due.ID)
		if !got.NextRunAt.Equal(t0.Add(10*time.Minute)) || !got.LastRunAt.Equal(t0) || got.LastStatus != schedule.StatusRunning {
			t.Fatalf("schedule after claim: %+v", got)
		}
		again, err := s.ClaimDue(ctx, t0, every10)
		must(t, err)
		if len(again) != 0 {
			t.Fatalf("claimed twice: %+v", again)
		}
	})
}

func TestOneRunAtATimePerSchedule(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		sc, err := s.Create(ctx, sched("a", t0))
		must(t, err)
		first, err := s.ClaimDue(ctx, t0, every10)
		must(t, err)
		if len(first) != 1 {
			t.Fatalf("first claim: %+v", first)
		}

		// The next tick comes while the first run is still going: a skipped run is recorded, nothing starts.
		second, err := s.ClaimDue(ctx, t0.Add(10*time.Minute), every10)

		must(t, err)
		if len(second) != 0 {
			t.Fatalf("a second run started: %+v", second)
		}
		runs, _ := s.Runs(ctx, sc.ID, 10)
		if len(runs) != 2 || runs[0].Status != schedule.StatusSkipped || runs[1].Status != schedule.StatusRunning {
			t.Fatalf("history: %+v", runs)
		}
		if _, err := s.StartManual(ctx, sc.ID, "ann", t0.Add(11*time.Minute)); !errors.Is(err, schedule.ErrBusy) {
			t.Fatalf("run now while running: %v", err)
		}
		// Once the first run is over a manual run is possible and leaves the schedule's own timetable alone.
		must(t, s.FinishRun(ctx, first[0].Run.ID, schedule.Finish{Status: schedule.StatusSucceeded}, t0.Add(12*time.Minute)))
		before, _ := s.Get(ctx, sc.ID)
		c, err := s.StartManual(ctx, sc.ID, "bob", t0.Add(13*time.Minute))
		must(t, err)
		if c.Run.Trigger != schedule.TriggerManual || c.Run.StartedBy != "bob" {
			t.Fatalf("manual run: %+v", c.Run)
		}
		after, _ := s.Get(ctx, sc.ID)
		if !after.NextRunAt.Equal(before.NextRunAt) {
			t.Fatalf("a manual run moved the timetable: %v -> %v", before.NextRunAt, after.NextRunAt)
		}
		if _, err := s.StartManual(ctx, "nope", "ann", t0); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("unknown schedule: %v", err)
		}
	})
}

func TestFinishRunRecordsTheOutcomeOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		sc, err := s.Create(ctx, sched("a", t0))
		must(t, err)
		sc.ReuseSession = true
		_, err = s.Update(ctx, *sc)
		must(t, err)
		cl, err := s.ClaimDue(ctx, t0, every10)
		must(t, err)
		run := cl[0].Run
		must(t, s.SetThread(ctx, run.ID, "thread-1", "agent-run-1"))
		if got, err := s.RunOfThread(ctx, "thread-1"); err != nil || got.ID != run.ID || got.Status != schedule.StatusRunning {
			t.Fatalf("run of thread while running: %+v %v", got, err)
		}

		usage := schedule.RunUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60, CacheWriteTokens: 5, CostUSD: 0.12, Unpriced: 1}
		must(t, s.FinishRun(ctx, run.ID, schedule.Finish{Status: schedule.StatusTimeout, Error: "too slow", Usage: usage}, t0.Add(time.Minute)))
		must(t, s.FinishRun(ctx, run.ID, schedule.Finish{Status: schedule.StatusSucceeded}, t0.Add(2*time.Minute)))

		runs, _ := s.Runs(ctx, sc.ID, 10)
		if len(runs) != 1 || runs[0].Status != schedule.StatusTimeout || runs[0].Error != "too slow" || runs[0].ThreadID != "thread-1" ||
			runs[0].AgentRun != "agent-run-1" || runs[0].Usage != usage || !runs[0].FinishedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("run: %+v", runs)
		}
		got, _ := s.Get(ctx, sc.ID)
		if got.LastStatus != schedule.StatusTimeout || got.ThreadID != "thread-1" {
			t.Fatalf("schedule: %+v", got)
		}
		if err := s.FinishRun(ctx, "nope", schedule.Finish{Status: schedule.StatusFailed}, t0); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("unknown run: %v", err)
		}
		if _, err := s.RunOfThread(ctx, "other"); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("run of an unknown thread: %v", err)
		}
	})
}

func TestARunThatOutlivedItsDeadlineIsLost(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		sc, err := s.Create(ctx, sched("a", t0))
		must(t, err)
		lost, err := s.ClaimDue(ctx, t0, every10)
		must(t, err)
		// The process that ran it died. 35 minutes later (timeout plus grace) the next tick sweeps it and starts a new run.
		later := t0.Add(40 * time.Minute)

		claims, err := s.ClaimDue(ctx, later, every10)

		must(t, err)
		if len(claims) != 1 || claims[0].Run.ID == lost[0].Run.ID {
			t.Fatalf("claims = %+v", claims)
		}
		runs, _ := s.Runs(ctx, sc.ID, 10)
		var found bool
		for _, r := range runs {
			if r.ID == lost[0].Run.ID {
				found = true
				if r.Status != schedule.StatusFailed || r.Error != schedulestoreLost {
					t.Fatalf("lost run: %+v", r)
				}
			}
		}
		if !found {
			t.Fatalf("history lost the run: %+v", runs)
		}
	})
}

const schedulestoreLost = schedstore.LostError

func TestReplicasNeverClaimTheSameTick(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		for i := range 5 {
			_, err := s.Create(ctx, sched(fmt.Sprintf("s%d", i), t0))
			must(t, err)
		}
		var mu sync.Mutex
		claimed := map[string]int{}
		var wg sync.WaitGroup

		for range 6 { // six replicas ticking at the same moment
			wg.Add(1)
			go func() {
				defer wg.Done()
				cl, err := s.ClaimDue(ctx, t0, every10)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				for _, c := range cl {
					claimed[c.Schedule.ID]++
				}
			}()
		}
		wg.Wait()

		if len(claimed) != 5 {
			t.Fatalf("claimed %d of 5 schedules: %v", len(claimed), claimed)
		}
		for id, n := range claimed {
			if n != 1 {
				t.Fatalf("schedule %s claimed %d times", id, n)
			}
		}
	})
}

func TestRunsAreNewestFirstAndLimited(t *testing.T) {
	eachStore(t, func(t *testing.T, s schedule.Store) {
		sc, err := s.Create(ctx, sched("a", t0))
		must(t, err)
		for i := range 4 {
			c, err := s.StartManual(ctx, sc.ID, "ann", t0.Add(time.Duration(i)*time.Minute))
			must(t, err)
			must(t, s.FinishRun(ctx, c.Run.ID, schedule.Finish{Status: schedule.StatusSucceeded}, t0.Add(time.Duration(i)*time.Minute+time.Second)))
		}

		runs, err := s.Runs(ctx, sc.ID, 3)

		must(t, err)
		if len(runs) != 3 || !runs[0].StartedAt.Equal(t0.Add(3*time.Minute)) || !runs[2].StartedAt.Equal(t0.Add(time.Minute)) {
			t.Fatalf("runs: %+v", runs)
		}
		if _, err := s.Runs(ctx, "nope", 3); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatalf("unknown schedule: %v", err)
		}
		must(t, s.Delete(ctx, sc.ID))
		if _, err := s.RunOfThread(ctx, "x"); !errors.Is(err, schedule.ErrNotFound) {
			t.Fatal(err)
		}
	})
}

func TestFileStoreSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schedules.json")
	f, err := schedstore.Open(path)
	must(t, err)
	sc, err := f.Create(ctx, sched("a", t0))
	must(t, err)
	cl, err := f.ClaimDue(ctx, t0, every10)
	must(t, err)
	must(t, f.FinishRun(ctx, cl[0].Run.ID, schedule.Finish{Status: schedule.StatusSucceeded, ThreadID: "t1"}, t0.Add(time.Minute)))

	g, err := schedstore.Open(path)
	must(t, err)

	runs, _ := g.Runs(ctx, sc.ID, 10)
	if len(runs) != 1 || runs[0].Status != schedule.StatusSucceeded || runs[0].ThreadID != "t1" {
		t.Fatalf("runs after restart: %+v", runs)
	}
	got, _ := g.Get(ctx, sc.ID)
	if !got.NextRunAt.Equal(t0.Add(10 * time.Minute)) {
		t.Fatalf("next after restart: %v", got.NextRunAt)
	}
	must(t, os.WriteFile(path, []byte("{broken"), 0o600))
	if _, err := schedstore.Open(path); err == nil {
		t.Fatal("a corrupt file was accepted")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
