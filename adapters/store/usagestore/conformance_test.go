package usagestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/usagestore"
	"github.com/7ar-chat/harness/core/usage"
)

var ctx = context.Background()

func newPG(t *testing.T) usage.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	schema := fmt.Sprintf("work_test_usage_%d", time.Now().UnixNano())
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
	return usagestore.NewPG(pool, nil)
}

func eachStore(t *testing.T, fn func(t *testing.T, s usage.Store)) {
	t.Helper()
	t.Run("mem", func(t *testing.T) { fn(t, usagestore.NewMem()) })
	t.Run("file", func(t *testing.T) {
		f, err := usagestore.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		fn(t, f)
	})
	t.Run("pg", func(t *testing.T) { fn(t, newPG(t)) })
}

var day1 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

func entry(run, model, user, project, source string, at time.Time, cost float64) usage.Entry {
	return usage.Entry{RunID: run, ThreadID: "th-" + run, ProjectID: project, User: user, Source: source, Model: model,
		InputTokens: 1000, OutputTokens: 100, CacheReadTokens: 600, CacheWriteTokens: 50, CostUSD: cost, At: at}
}

func TestReportTotalsSplitsAndGroups(t *testing.T) {
	eachStore(t, func(t *testing.T, s usage.Store) {
		// Arrange: run r1 used two models; r2 is a schedule's; r3 is somebody else's in another project.
		must(t, s.Record(ctx, []usage.Entry{
			entry("r1", "gpt-a", "ann", "p1", usage.SourceSession, day1, 0.5),
			entry("r1", "gpt-b", "ann", "p1", usage.SourceSession, day1, 0.25),
			entry("r2", "gpt-a", "bob", "p1", usage.SourceSchedule, day1.Add(24*time.Hour), 1),
			entry("r3", "gpt-a", "cy", "p2", usage.SourceTask, day1, 2),
		}))

		// Act
		rep, err := s.Report(ctx, usage.Query{ProjectID: "p1", Group: usage.GroupDay})

		// Assert
		must(t, err)
		if rep.Total.Runs != 2 || rep.Total.CostUSD != 1.75 || rep.Total.InputTokens != 3000 || rep.Total.CacheReadTokens != 1800 || rep.Total.CacheWriteTokens != 150 {
			t.Fatalf("total = %+v", rep.Total)
		}
		if got := rep.BySource[usage.SourceSchedule]; got.Runs != 1 || got.CostUSD != 1 {
			t.Fatalf("schedule = %+v", got)
		}
		if got := rep.BySource[usage.SourceSession]; got.Runs != 1 || got.CostUSD != 0.75 {
			t.Fatalf("session = %+v", got)
		}
		if len(rep.Rows) != 2 || rep.Rows[0].Key != "2026-11-03" || rep.Rows[1].Key != "2026-11-04" {
			t.Fatalf("day rows = %+v", rep.Rows)
		}
		byModel, _ := s.Report(ctx, usage.Query{ProjectID: "p1", Group: usage.GroupModel})
		if len(byModel.Rows) != 2 || byModel.Rows[0].Key != "gpt-a" || byModel.Rows[0].Runs != 2 {
			t.Fatalf("model rows = %+v", byModel.Rows)
		}
		byUser, _ := s.Report(ctx, usage.Query{Group: usage.GroupUser})
		if len(byUser.Rows) != 3 || byUser.Rows[0].Key != "cy" {
			t.Fatalf("user rows = %+v", byUser.Rows)
		}
		byThread, _ := s.Report(ctx, usage.Query{Group: usage.GroupThread, Limit: 2})
		if len(byThread.Rows) != 2 {
			t.Fatalf("thread rows = %+v", byThread.Rows)
		}
	})
}

func TestReportFiltersByUserAndRange(t *testing.T) {
	eachStore(t, func(t *testing.T, s usage.Store) {
		must(t, s.Record(ctx, []usage.Entry{
			entry("a", "m", "ann", "p1", usage.SourceSession, day1, 1),
			entry("b", "m", "ann", "", usage.SourceSession, day1.Add(48*time.Hour), 2),
			entry("c", "m", "bob", "p1", usage.SourceSession, day1.Add(48*time.Hour), 4),
		}))

		rep, err := s.Report(ctx, usage.Query{User: "ann", From: day1.Add(time.Hour), To: day1.Add(72 * time.Hour)})

		must(t, err)
		if rep.Total.Runs != 1 || rep.Total.CostUSD != 2 {
			t.Fatalf("total = %+v", rep.Total)
		}
		if _, err := s.Report(ctx, usage.Query{Group: "bogus"}); !errors.Is(err, usage.ErrInvalid) {
			t.Fatalf("bad group: %v", err)
		}
	})
}

func TestARunIsBookedOnce(t *testing.T) {
	eachStore(t, func(t *testing.T, s usage.Store) {
		e := entry("r1", "m", "ann", "p1", usage.SourceSession, day1, 1)
		must(t, s.Record(ctx, []usage.Entry{e}))

		must(t, s.Record(ctx, []usage.Entry{e}))

		rep, _ := s.Report(ctx, usage.Query{})
		if rep.Total.Runs != 1 || rep.Total.CostUSD != 1 {
			t.Fatalf("booked twice: %+v", rep.Total)
		}
		if err := s.Record(ctx, []usage.Entry{{Model: "m"}}); !errors.Is(err, usage.ErrInvalid) {
			t.Fatalf("no run id: %v", err)
		}
	})
}

func TestUnpricedModelsAreFlaggedNotFree(t *testing.T) {
	eachStore(t, func(t *testing.T, s usage.Store) {
		e := entry("r1", "mystery", "ann", "p1", usage.SourceSession, day1, 0)
		e.Unpriced = true
		must(t, s.Record(ctx, []usage.Entry{e, entry("r2", "m", "ann", "p1", usage.SourceSession, day1, 1)}))

		rep, _ := s.Report(ctx, usage.Query{Group: usage.GroupModel})

		if rep.Total.Unpriced != 1 || rep.Total.CostUSD != 1 {
			t.Fatalf("total = %+v", rep.Total)
		}
		for _, r := range rep.Rows {
			if (r.Key == "mystery") != (r.Unpriced == 1) {
				t.Fatalf("row %+v", r)
			}
		}
	})
}

func TestSpentAndCaps(t *testing.T) {
	eachStore(t, func(t *testing.T, s usage.Store) {
		now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
		must(t, s.Record(ctx, []usage.Entry{
			entry("old", "m", "ann", "p1", usage.SourceSession, time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), 50),
			entry("a", "m", "ann", "p1", usage.SourceSession, now.Add(-24*time.Hour), 3),
			entry("b", "m", "bob", "p1", usage.SourceSession, now.Add(-time.Hour), 4),
			entry("c", "m", "ann", "", usage.SourceSession, now.Add(-time.Hour), 5),
		}))

		spentProject, _ := s.Spent(ctx, "p1", "", usage.MonthStart(now))
		spentAnn, _ := s.Spent(ctx, "", "ann", usage.MonthStart(now))

		if spentProject != 7 || spentAnn != 8 {
			t.Fatalf("project %v user %v", spentProject, spentAnn)
		}
		if c, _ := s.Cap(ctx, usage.ScopeProject, "p1"); c != 0 {
			t.Fatalf("cap before = %v", c)
		}
		must(t, s.SetCap(ctx, usage.ScopeProject, "p1", 7))
		must(t, s.SetCap(ctx, usage.ScopeUser, "ann", 100))
		err := usage.Check(ctx, s, "p1", "ann", now)
		var ce *usage.CapError
		if !errors.Is(err, usage.ErrCapReached) || !errors.As(err, &ce) || ce.Scope != usage.ScopeProject || ce.SpentUSD != 7 {
			t.Fatalf("check = %v", err)
		}
		if err := usage.Check(ctx, s, "", "ann", now); err != nil {
			t.Fatalf("personal under its cap: %v", err)
		}
		must(t, s.SetCap(ctx, usage.ScopeUser, "ann", 8))
		if err := usage.Check(ctx, s, "", "ann", now); !errors.Is(err, usage.ErrCapReached) {
			t.Fatalf("personal over its cap: %v", err)
		}
		// A new month starts again from zero.
		if err := usage.Check(ctx, s, "p1", "ann", now.AddDate(0, 1, 0)); err != nil {
			t.Fatalf("next month: %v", err)
		}
		must(t, s.SetCap(ctx, usage.ScopeProject, "p1", 0))
		if c, _ := s.Cap(ctx, usage.ScopeProject, "p1"); c != 0 {
			t.Fatalf("cap after removal = %v", c)
		}
		if err := s.SetCap(ctx, "team", "x", 1); !errors.Is(err, usage.ErrInvalid) {
			t.Fatalf("bad scope: %v", err)
		}
		if err := s.SetCap(ctx, usage.ScopeUser, "ann", -1); !errors.Is(err, usage.ErrInvalid) {
			t.Fatalf("negative: %v", err)
		}
	})
}

func TestFileStoreSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f, err := usagestore.Open(dir)
	must(t, err)
	must(t, f.Record(ctx, []usage.Entry{entry("r1", "m", "ann", "p1", usage.SourceSession, day1, 2)}))
	must(t, f.SetCap(ctx, usage.ScopeProject, "p1", 10))
	must(t, f.Close())

	g, err := usagestore.Open(dir)
	must(t, err)
	defer g.Close()

	rep, _ := g.Report(ctx, usage.Query{})
	if rep.Total.Runs != 1 || rep.Total.CostUSD != 2 {
		t.Fatalf("after restart: %+v", rep.Total)
	}
	if c, _ := g.Cap(ctx, usage.ScopeProject, "p1"); c != 10 {
		t.Fatalf("cap after restart = %v", c)
	}
	must(t, g.Record(ctx, []usage.Entry{entry("r1", "m", "ann", "p1", usage.SourceSession, day1, 2)}))
	if rep, _ := g.Report(ctx, usage.Query{}); rep.Total.Runs != 1 {
		t.Fatalf("a booked run was booked again after the restart: %+v", rep.Total)
	}
}

func TestFileStoreRefusesACorruptLedger(t *testing.T) {
	dir := t.TempDir()
	must(t, os.WriteFile(dir+"/usage.jsonl", []byte("{not json}\n"), 0o600))

	if _, err := usagestore.Open(dir); err == nil {
		t.Fatal("a corrupt ledger was accepted")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
