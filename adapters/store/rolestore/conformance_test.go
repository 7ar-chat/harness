package rolestore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/rolestore"
	"github.com/7ar-chat/harness/core/roles"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newPG(t *testing.T) roles.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_roles_%d", time.Now().UnixNano())
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
	return rolestore.NewPG(pool, func() time.Time { return t0 })
}

func eachStore(t *testing.T, fn func(t *testing.T, s roles.Store)) {
	t.Helper()
	now := func() time.Time { return t0 }
	backends := []struct {
		name string
		open func(t *testing.T) roles.Store
	}{
		{"mem", func(*testing.T) roles.Store { return rolestore.NewMem(now) }},
		{"file", func(t *testing.T) roles.Store {
			f, err := rolestore.Open(filepath.Join(t.TempDir(), "roles.json"), now)
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

func role(name string) roles.Role {
	return roles.Role{Scope: roles.ScopeProject, ScopeID: "p1", Name: name, Title: "T " + name, Prompt: "be " + name, Enabled: true,
		Skills: []string{"review"}, AllowTools: []string{"read_file"}}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestCreateGetListRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s roles.Store) {
		ctx := context.Background()
		// Act
		created := must(s.Create(ctx, role("b-role"), "alice"))
		must(s.Create(ctx, role("a-role"), "alice"))
		other := role("a-role")
		other.ScopeID = "p2"
		must(s.Create(ctx, other, "bob"))
		got := must(s.Get(ctx, roles.ScopeProject, "p1", "b-role"))
		list := must(s.List(ctx, roles.ScopeProject, "p1"))

		// Assert
		if created.Version != 1 || created.CreatedBy != "alice" || !created.CreatedAt.Equal(t0) {
			t.Fatalf("created = %+v", created)
		}
		if !reflect.DeepEqual(got, created) {
			t.Fatalf("get = %+v, want %+v", got, created)
		}
		if len(list) != 2 || list[0].Name != "a-role" || list[1].Name != "b-role" {
			t.Fatalf("list = %+v", list)
		}
		if l := must(s.List(ctx, roles.ScopeUser, "p1")); len(l) != 0 {
			t.Fatalf("a user scope sees project roles: %+v", l)
		}
	})
}

func TestCreateRefusesDuplicateAndInvalid(t *testing.T) {
	eachStore(t, func(t *testing.T, s roles.Store) {
		ctx := context.Background()
		must(s.Create(ctx, role("dup"), "a"))
		if _, err := s.Create(ctx, role("dup"), "a"); !errors.Is(err, roles.ErrConflict) {
			t.Fatalf("duplicate: %v", err)
		}
		bad := role("Bad Name")
		if _, err := s.Create(ctx, bad, "a"); !errors.Is(err, roles.ErrInvalid) {
			t.Fatalf("bad name: %v", err)
		}
		empty := role("noprompt")
		empty.Prompt = " "
		if _, err := s.Create(ctx, empty, "a"); !errors.Is(err, roles.ErrInvalid) {
			t.Fatalf("empty prompt: %v", err)
		}
	})
}

func TestUpdateBumpsVersionAndKeepsHistory(t *testing.T) {
	eachStore(t, func(t *testing.T, s roles.Store) {
		ctx := context.Background()
		must(s.Create(ctx, role("r"), "alice"))
		r := role("r")
		r.Prompt = "second"
		r.Version = 1

		// Act
		u := must(s.Update(ctx, r, "bob"))
		hist := must(s.History(ctx, roles.ScopeProject, "p1", "r"))

		// Assert
		if u.Version != 2 || u.Prompt != "second" || u.UpdatedBy != "bob" || u.CreatedBy != "alice" {
			t.Fatalf("updated = %+v", u)
		}
		if len(hist) != 1 || hist[0].Version != 1 || hist[0].Role.Prompt != "be r" || hist[0].ChangedBy != "alice" {
			t.Fatalf("history = %+v", hist)
		}
		stale := role("r")
		stale.Version = 1
		if _, err := s.Update(ctx, stale, "x"); !errors.Is(err, roles.ErrStale) {
			t.Fatalf("stale update: %v", err)
		}
		missing := role("ghost")
		if _, err := s.Update(ctx, missing, "x"); !errors.Is(err, roles.ErrNotFound) {
			t.Fatalf("update of a missing role: %v", err)
		}
	})
}

func TestHistoryKeepsTheLastTen(t *testing.T) {
	eachStore(t, func(t *testing.T, s roles.Store) {
		ctx := context.Background()
		must(s.Create(ctx, role("r"), "a"))
		for i := 2; i <= 15; i++ {
			r := role("r")
			r.Prompt = fmt.Sprintf("v%d", i)
			must(s.Update(ctx, r, "a"))
		}
		hist := must(s.History(ctx, roles.ScopeProject, "p1", "r"))
		if len(hist) != roles.MaxHistory || hist[0].Version != 14 || hist[len(hist)-1].Version != 5 {
			t.Fatalf("history versions: %d entries, first %d, last %d", len(hist), hist[0].Version, hist[len(hist)-1].Version)
		}
		if cur := must(s.Get(ctx, roles.ScopeProject, "p1", "r")); cur.Version != 15 || cur.Prompt != "v15" {
			t.Fatalf("current = %+v", cur)
		}
	})
}

func TestDeleteAndDeleteScope(t *testing.T) {
	eachStore(t, func(t *testing.T, s roles.Store) {
		ctx := context.Background()
		must(s.Create(ctx, role("a"), "x"))
		must(s.Create(ctx, role("b"), "x"))
		r := role("a")
		r.Prompt = "again"
		must(s.Update(ctx, r, "x"))

		if err := s.Delete(ctx, roles.ScopeProject, "p1", "a"); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, roles.ScopeProject, "p1", "a"); !errors.Is(err, roles.ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
		if _, err := s.History(ctx, roles.ScopeProject, "p1", "a"); !errors.Is(err, roles.ErrNotFound) {
			t.Fatalf("history of a deleted role: %v", err)
		}
		if err := s.DeleteScope(ctx, roles.ScopeProject, "p1"); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteScope(ctx, roles.ScopeProject, "p1"); err != nil {
			t.Fatalf("deleting an empty scope: %v", err)
		}
		if l := must(s.List(ctx, roles.ScopeProject, "p1")); len(l) != 0 {
			t.Fatalf("list after DeleteScope: %+v", l)
		}
	})
}

func TestFilePersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "roles.json")
	ctx := context.Background()
	f := must(rolestore.Open(path, func() time.Time { return t0 }))
	must(f.Create(ctx, role("keep"), "a"))
	r := role("keep")
	r.Prompt = "two"
	must(f.Update(ctx, r, "a"))

	g := must(rolestore.Open(path, nil))

	got := must(g.Get(ctx, roles.ScopeProject, "p1", "keep"))
	if got.Version != 2 || got.Prompt != "two" {
		t.Fatalf("reloaded = %+v", got)
	}
	if h := must(g.History(ctx, roles.ScopeProject, "p1", "keep")); len(h) != 1 {
		t.Fatalf("reloaded history = %+v", h)
	}
}

func TestFileRefusesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roles.json")
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rolestore.Open(path, nil); err == nil {
		t.Fatal("opened a corrupt file")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "{nope" {
		t.Fatal("the corrupt file was touched")
	}
	if err := os.WriteFile(path, []byte(`{"version":9,"roles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rolestore.Open(path, nil); err == nil {
		t.Fatal("opened an unknown version")
	}
}

func TestStoredValuesAreNotAliased(t *testing.T) {
	s := rolestore.NewMem(nil)
	ctx := context.Background()
	in := role("alias")
	created := must(s.Create(ctx, in, "a"))
	created.Skills[0] = "mutated"
	in.AllowTools[0] = "mutated"
	got := must(s.Get(ctx, roles.ScopeProject, "p1", "alias"))
	if got.Skills[0] != "review" || got.AllowTools[0] != "read_file" {
		t.Fatalf("store aliased the caller's slices: %+v", got)
	}
}
