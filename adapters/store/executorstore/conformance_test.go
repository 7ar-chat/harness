package executorstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/executorstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/core/executors"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newPG(t *testing.T) executors.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_executors_%d", time.Now().UnixNano())
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
	return executorstore.NewPG(pool, func() time.Time { return t0 })
}

func eachStore(t *testing.T, fn func(t *testing.T, s executors.Store)) {
	t.Helper()
	now := func() time.Time { return t0 }
	for _, b := range []struct {
		name string
		open func(t *testing.T) executors.Store
	}{
		{"mem", func(*testing.T) executors.Store { return executorstore.NewMem(now) }},
		{"file", func(t *testing.T) executors.Store {
			f, err := executorstore.Open(filepath.Join(t.TempDir(), "executors.json"), now)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}},
		{"pg", newPG},
	} {
		t.Run(b.name, func(t *testing.T) { fn(t, b.open(t)) })
	}
}

func ex(name string) executors.Executor {
	return executors.Executor{Name: name, ProjectID: "p1", Command: "mycli --prompt {task} {mode}", Binary: "mycli", Enabled: true,
		ModeFlags: map[executors.Level]string{executors.LevelReadOnly: "--ro"}, CredentialEnv: []string{"MYCLI_KEY"}}
}

func TestConformance(t *testing.T) {
	eachStore(t, func(t *testing.T, s executors.Store) {
		ctx := context.Background()
		got, err := s.Create(ctx, ex("mycli"), "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != executors.KindCustom || got.CreatedBy != "alice" || !got.CreatedAt.Equal(t0) {
			t.Fatalf("created %+v", got)
		}
		if _, err := s.Create(ctx, ex("mycli"), "alice"); !errors.Is(err, executors.ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		if _, err := s.Create(ctx, ex("codex"), "alice"); !errors.Is(err, executors.ErrInvalid) {
			t.Fatalf("builtin name: %v", err)
		}
		bad := ex("other")
		bad.Command = "no placeholder"
		if _, err := s.Create(ctx, bad, "alice"); !errors.Is(err, executors.ErrInvalid) {
			t.Fatalf("invalid: %v", err)
		}
		g, err := s.Get(ctx, "p1", "mycli")
		if err != nil || g.ModeFlags[executors.LevelReadOnly] != "--ro" || len(g.CredentialEnv) != 1 {
			t.Fatalf("get: %+v %v", g, err)
		}
		if _, err := s.Get(ctx, "p2", "mycli"); !errors.Is(err, executors.ErrNotFound) {
			t.Fatalf("other project: %v", err)
		}
		up := ex("mycli")
		up.Title = "My CLI"
		if u, err := s.Update(ctx, up, "bob"); err != nil || u.Title != "My CLI" || u.CreatedBy != "alice" {
			t.Fatalf("update: %+v %v", u, err)
		}
		if _, err := s.Update(ctx, ex("ghost"), "bob"); !errors.Is(err, executors.ErrNotFound) {
			t.Fatalf("update unknown: %v", err)
		}
		if _, err := s.Create(ctx, ex("another"), "alice"); err != nil {
			t.Fatal(err)
		}
		l, err := s.List(ctx, "p1")
		if err != nil || len(l) != 2 || l[0].Name != "another" {
			t.Fatalf("list: %+v %v", l, err)
		}
		if err := s.Delete(ctx, "p1", "another"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, "p1", "another"); !errors.Is(err, executors.ErrNotFound) {
			t.Fatalf("delete twice: %v", err)
		}
		if err := s.DeleteProject(ctx, "p1"); err != nil {
			t.Fatal(err)
		}
		if l, _ := s.List(ctx, "p1"); len(l) != 0 {
			t.Fatalf("after DeleteProject: %+v", l)
		}
	})
}

func TestFilePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.json")
	f, err := executorstore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Create(context.Background(), ex("mycli"), "a"); err != nil {
		t.Fatal(err)
	}
	g, err := executorstore.Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Get(context.Background(), "p1", "mycli"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executorstore.Open(path, nil); err == nil {
		t.Fatal("a corrupt file must be refused")
	}
}
