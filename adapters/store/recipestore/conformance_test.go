package recipestore_test

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
	"github.com/7ar-chat/harness/adapters/store/recipestore"
	"github.com/7ar-chat/harness/core/recipes"
)

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func newPG(t *testing.T) recipes.Store {
	t.Helper()
	dsn := os.Getenv("HARNESS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("HARNESS_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("work_test_recipes_%d", time.Now().UnixNano())
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
	return recipestore.NewPG(pool, func() time.Time { return t0 })
}

func eachStore(t *testing.T, fn func(t *testing.T, s recipes.Store)) {
	t.Helper()
	now := func() time.Time { return t0 }
	backends := []struct {
		name string
		open func(t *testing.T) recipes.Store
	}{
		{"mem", func(*testing.T) recipes.Store { return recipestore.NewMem(now) }},
		{"file", func(t *testing.T) recipes.Store {
			f, err := recipestore.Open(filepath.Join(t.TempDir(), "recipes.json"), now)
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

func recipe(name string) recipes.Recipe {
	return recipes.Recipe{Name: name, Summary: "s", Category: recipes.CatReporting, Kind: recipes.KindSchedule, Scope: recipes.ScopeProject, ScopeID: "p1",
		Body:   recipes.Body{Name: name, Prompt: "Report on {{topic}}", Cron: "0 2 * * *"},
		Params: []recipes.Param{{Key: "topic", Label: "Topic", Type: recipes.TypeText, Required: true}}}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestCreateGetListRoundTrip(t *testing.T) {
	eachStore(t, func(t *testing.T, s recipes.Store) {
		ctx := context.Background()
		created := must(s.Create(ctx, recipe("b"), "alice"))
		must(s.Create(ctx, recipe("a"), "alice"))
		other := recipe("c")
		other.Scope, other.ScopeID = recipes.ScopePersonal, "bob"
		must(s.Create(ctx, other, "bob"))
		got := must(s.Get(ctx, created.ID))
		list := must(s.List(ctx, recipes.ScopeProject, "p1"))

		if created.Version != 1 || created.Author != "alice" || !created.CreatedAt.Equal(t0) || created.ID == "" {
			t.Fatalf("created = %+v", created)
		}
		if !reflect.DeepEqual(got, created) {
			t.Fatalf("get = %+v, want %+v", got, created)
		}
		if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
			t.Fatalf("list = %+v", list)
		}
		if all := must(s.List(ctx, "", "")); len(all) != 3 {
			t.Fatalf("all = %d", len(all))
		}
		if l := must(s.List(ctx, recipes.ScopeInstance, "")); len(l) != 0 {
			t.Fatalf("instance sees project recipes: %+v", l)
		}
		if _, err := s.Get(ctx, "nope"); !errors.Is(err, recipes.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestCreateRefusesInvalidAndBuiltinScope(t *testing.T) {
	eachStore(t, func(t *testing.T, s recipes.Store) {
		ctx := context.Background()
		bad := recipe("x")
		bad.Body.Prompt = "uses {{undeclared}}"
		if _, err := s.Create(ctx, bad, "a"); !errors.Is(err, recipes.ErrInvalid) {
			t.Fatalf("undeclared placeholder: %v", err)
		}
		b := recipe("y")
		b.Scope = recipes.ScopeBuiltin
		if _, err := s.Create(ctx, b, "a"); !errors.Is(err, recipes.ErrInvalid) {
			t.Fatalf("builtin scope: %v", err)
		}
	})
}

func TestUpdateBumpsVersionAndKeepsHistory(t *testing.T) {
	eachStore(t, func(t *testing.T, s recipes.Store) {
		ctx := context.Background()
		c := must(s.Create(ctx, recipe("r"), "alice"))
		r := recipe("r")
		r.ID, r.Version = c.ID, 1
		r.Body.Prompt = "second {{topic}}"
		r.Scope, r.ScopeID, r.Author = recipes.ScopeInstance, "", "mallory" // not editable

		u := must(s.Update(ctx, r, "bob"))
		hist := must(s.History(ctx, c.ID))

		if u.Version != 2 || u.Scope != recipes.ScopeProject || u.ScopeID != "p1" || u.Author != "alice" || u.Body.Prompt != "second {{topic}}" {
			t.Fatalf("updated = %+v", u)
		}
		if len(hist) != 1 || hist[0].Version != 1 || hist[0].Recipe.Body.Prompt != "Report on {{topic}}" || hist[0].ChangedBy != "bob" {
			t.Fatalf("history = %+v", hist)
		}
		stale := r
		stale.Version = 1
		if _, err := s.Update(ctx, stale, "bob"); !errors.Is(err, recipes.ErrStale) {
			t.Fatalf("stale: %v", err)
		}
		missing := r
		missing.ID = "nope"
		if _, err := s.Update(ctx, missing, "bob"); !errors.Is(err, recipes.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})
}

func TestHistoryKeepsTheLastTen(t *testing.T) {
	eachStore(t, func(t *testing.T, s recipes.Store) {
		ctx := context.Background()
		c := must(s.Create(ctx, recipe("r"), "a"))
		for i := 0; i < 14; i++ {
			r := recipe("r")
			r.ID = c.ID
			r.Summary = fmt.Sprintf("v%d", i+2)
			must(s.Update(ctx, r, "a"))
		}
		hist := must(s.History(ctx, c.ID))
		if len(hist) != recipes.MaxHistory || hist[0].Version != 14 || hist[9].Version != 5 {
			t.Fatalf("history = %d versions, first %d", len(hist), hist[0].Version)
		}
	})
}

func TestUsesCountDeleteAndDeleteScope(t *testing.T) {
	eachStore(t, func(t *testing.T, s recipes.Store) {
		ctx := context.Background()
		a := must(s.Create(ctx, recipe("a"), "x"))
		b := must(s.Create(ctx, recipe("b"), "x"))
		keep := recipe("k")
		keep.ScopeID = "p2"
		k := must(s.Create(ctx, keep, "x"))
		for _, id := range []string{a.ID, a.ID, "weekly-status-digest", k.ID} {
			if err := s.AddUse(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
		uses := must(s.Uses(ctx))
		if uses[a.ID] != 2 || uses["weekly-status-digest"] != 1 || uses[k.ID] != 1 {
			t.Fatalf("uses = %v", uses)
		}
		if err := s.Delete(ctx, b.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, b.ID); !errors.Is(err, recipes.ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
		if err := s.DeleteScope(ctx, recipes.ScopeProject, "p1"); err != nil {
			t.Fatal(err)
		}
		if l := must(s.List(ctx, "", "")); len(l) != 1 || l[0].ID != k.ID {
			t.Fatalf("after DeleteScope: %+v", l)
		}
		if u := must(s.Uses(ctx)); u[a.ID] != 0 || u[k.ID] != 1 {
			t.Fatalf("uses after delete = %v", u)
		}
	})
}

func TestFilePersistsAcrossOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipes.json")
	f := must(recipestore.Open(path, func() time.Time { return t0 }))
	c := must(f.Create(context.Background(), recipe("r"), "alice"))
	r := recipe("r")
	r.ID = c.ID
	must(f.Update(context.Background(), r, "bob"))
	_ = f.AddUse(context.Background(), c.ID)

	g := must(recipestore.Open(path, nil))
	got := must(g.Get(context.Background(), c.ID))
	hist := must(g.History(context.Background(), c.ID))
	if got.Version != 2 || len(hist) != 1 || must(g.Uses(context.Background()))[c.ID] != 1 {
		t.Fatalf("reopened: %+v %+v", got, hist)
	}
}

func TestFileRefusesACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipes.json")
	for _, body := range []string{`{`, `{"version":9,"recipes":[]}`, `{"version":1,"recipes":[{"id":"x","scope":"galaxy"}]}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := recipestore.Open(path, nil); err == nil {
			t.Fatalf("%s: want an error", body)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Fatal("a corrupt file must be left untouched")
		}
	}
}

func TestStoredValuesAreNotAliased(t *testing.T) {
	s := recipestore.NewMem(nil)
	in := recipe("r")
	in.Params[0].Choices = nil
	c := must(s.Create(context.Background(), in, "a"))
	c.Params[0].Label = "changed"
	if got := must(s.Get(context.Background(), c.ID)); got.Params[0].Label != "Topic" {
		t.Fatalf("a returned value aliases the store: %+v", got.Params)
	}
}
