package teamstore_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GhaziBenDahmane/harness/adapters/store/pgstore/migrate"
	"github.com/GhaziBenDahmane/harness/adapters/store/teamstore"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// newPG migrates a fresh schema of HARNESS_TEST_DATABASE_URL; skipped when unset.
func newPG(t *testing.T) *teamstore.PG {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_team_%d", time.Now().UnixNano())
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
	return teamstore.NewPG(pool)
}

func TestPGSeenThreadsKeepTheLatestVisitPerUser(t *testing.T) {
	// Arrange
	ctx := context.Background()
	s := newPG(t)
	for _, l := range []string{"alice", "bob"} {
		if _, err := s.UpsertUser(ctx, store.User{Login: l}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.CreateProject(ctx, store.Project{Name: "api"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetThreadMeta(ctx, store.ThreadMeta{ThreadID: "t2", ProjectID: p.ID, Owner: "alice", CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}
	t1, t2 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	// Act
	for _, c := range []struct {
		login, thread string
		at            time.Time
	}{{"alice", "t1", t2}, {"alice", "t1", t1}, {"bob", "t1", t1}, {"alice", "t2", t1}} {
		if err := s.SetThreadSeen(ctx, c.login, c.thread, c.at); err != nil {
			t.Fatal(err)
		}
	}

	// Assert
	alice, _ := s.SeenThreads(ctx, "alice")
	bob, _ := s.SeenThreads(ctx, "bob")
	if !alice["t1"].Equal(t2) || !bob["t1"].Equal(t1) || len(alice) != 2 {
		t.Fatalf("alice %v, bob %v", alice, bob)
	}
	if err := s.DeleteProject(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SeenThreads(ctx, "alice"); len(got) != 1 {
		t.Fatalf("seen state of the deleted project's thread kept: %v", got)
	}
	if err := s.DeleteThreadMeta(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SeenThreads(ctx, "alice"); len(got) != 0 {
		t.Fatalf("seen state of a deleted thread kept: %v", got)
	}
}
