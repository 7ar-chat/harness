package capstore_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/core/capabilities"
)

// clock is a settable clock shared by the store and the test. Times stay on whole seconds: Postgres keeps
// microseconds, and equality of what comes back is part of the assertions.
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

// eachStore runs fn against a fresh store of every backend (Postgres is skipped when unconfigured). The
// file-backed store runs every case too: persisting must not change what the port guarantees.
func eachStore(t *testing.T, fn func(t *testing.T, s capabilities.Store, clk *clock)) {
	t.Helper()
	backends := []struct {
		name string
		open func(t *testing.T, clk *clock) capabilities.Store
	}{
		{"mem", func(_ *testing.T, clk *clock) capabilities.Store { return capstore.NewMem(clk.Now) }},
		{"file", func(t *testing.T, clk *clock) capabilities.Store {
			f, err := capstore.Open(filepath.Join(t.TempDir(), "capabilities.json"), clk.Now)
			must(t, err)
			return f
		}},
		{"pg", func(t *testing.T, clk *clock) capabilities.Store { return newPG(t, clk.Now) }},
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

var ctx = context.Background()

func entry(id string) capabilities.Entry {
	return capabilities.Entry{
		ID: id, Name: "Server " + id, Description: "d", Category: "dev", Homepage: "https://example.com/" + id,
		URL: "https://mcp.example.com/" + id, Transport: capabilities.TransportHTTP,
		Headers: map[string]string{"Authorization": "Bearer ${api_key}"},
		Fields:  []capabilities.Field{{Key: "api_key", Label: "API key", Secret: true, Required: true}},
		Tools:   []string{"search", "read"}, DefaultEnabled: true,
	}
}

func catalogInstall(scope capabilities.Scope, scopeID, catalogID string) capabilities.Install {
	return capabilities.Install{
		Scope: scope, ScopeID: scopeID, CatalogID: catalogID, Enabled: true,
		Secrets: []byte("sealed"), SecretKeys: []string{"b", "a"}, CreatedBy: "alice",
	}
}

func customInstall(scope capabilities.Scope, scopeID, name string) capabilities.Install {
	return capabilities.Install{
		Scope: scope, ScopeID: scopeID, Name: name, URL: "https://example.com/mcp", Transport: capabilities.TransportSSE,
		Tools: []string{"t1"}, Enabled: true, Secrets: []byte("sealed-headers"), SecretKeys: []string{"X-Key"}, CreatedBy: "alice",
	}
}

// sameInstall compares installs with times by instant: Postgres returns them in the session's zone.
func sameInstall(a, b capabilities.Install) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) {
		return false
	}
	a.CreatedAt, a.UpdatedAt, b.CreatedAt, b.UpdatedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

func put(t *testing.T, s capabilities.Store, in capabilities.Install) *capabilities.Install {
	t.Helper()
	got, err := s.PutInstall(ctx, in)
	must(t, err)
	return got
}

func TestSeedCatalogStoresEntriesAsBuiltinAndIsIdempotent(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		fixture := []capabilities.Entry{entry("zeta"), entry("alpha")}

		// Act
		must(t, s.SeedCatalog(ctx, fixture))
		must(t, s.SeedCatalog(ctx, fixture))

		// Assert
		got, err := s.ListCatalog(ctx)
		must(t, err)
		want := []capabilities.Entry{entry("alpha"), entry("zeta")}
		for i := range want {
			want[i].Builtin = true
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("catalog = %+v, want %+v", got, want)
		}
		one, err := s.GetCatalogEntry(ctx, "zeta")
		must(t, err)
		if !reflect.DeepEqual(*one, want[1]) {
			t.Fatalf("entry = %+v, want %+v", *one, want[1])
		}
		_, err = s.GetCatalogEntry(ctx, "nope")
		wantErr(t, "unknown entry", err, capabilities.ErrNotFound)
	})
}

func TestSeedCatalogUpdatesChangedEntriesAndRemovesUnlistedBuiltinOnes(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{entry("keep"), entry("drop")}))
		changed := entry("keep")
		changed.URL = "https://mcp.example.com/v2"
		changed.Fields = nil
		changed.Headers = nil
		changed.DefaultEnabled = false

		// Act
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{changed}))

		// Assert
		got, err := s.ListCatalog(ctx)
		must(t, err)
		if len(got) != 1 || got[0].ID != "keep" || got[0].URL != changed.URL || got[0].DefaultEnabled || len(got[0].Fields) != 0 || len(got[0].Headers) != 0 {
			t.Fatalf("catalog = %+v, want only the changed keep entry", got)
		}
		_, err = s.GetCatalogEntry(ctx, "drop")
		wantErr(t, "removed entry", err, capabilities.ErrNotFound)
	})
}

func TestSeedCatalogKeepsInstallsOfARemovedEntry(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{entry("gone")}))
		in := put(t, s, catalogInstall(capabilities.ScopeProject, "p1", "gone"))

		// Act
		must(t, s.SeedCatalog(ctx, nil))

		// Assert
		got, err := s.GetInstall(ctx, in.ID)
		must(t, err)
		if got.CatalogID != "gone" {
			t.Fatalf("install = %+v, want it kept", got)
		}
	})
}

func TestSeedCatalogRejectsABadFixtureWithoutChangingTheCatalog(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{entry("a")}))

		// Act
		errNoID := s.SeedCatalog(ctx, []capabilities.Entry{{Name: "x"}})
		errDup := s.SeedCatalog(ctx, []capabilities.Entry{entry("b"), entry("b")})

		// Assert
		if errNoID == nil || errDup == nil {
			t.Fatalf("errors = %v, %v; want both rejected", errNoID, errDup)
		}
		got, err := s.ListCatalog(ctx)
		must(t, err)
		if len(got) != 1 || got[0].ID != "a" {
			t.Fatalf("catalog = %+v, want it untouched", got)
		}
	})
}

func TestConcurrentSeedingOfTheSameFixtureIsSafe(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		fixture := []capabilities.Entry{entry("a"), entry("b"), entry("c")}
		var wg sync.WaitGroup
		errs := make(chan error, 16)

		// Act
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs <- s.SeedCatalog(ctx, fixture)
			}()
		}
		wg.Wait()
		close(errs)

		// Assert
		for err := range errs {
			must(t, err)
		}
		got, err := s.ListCatalog(ctx)
		must(t, err)
		if len(got) != 3 {
			t.Fatalf("catalog has %d entries, want 3", len(got))
		}
	})
}

func TestPutInstallCreatesWithFreshIDAndTimestampsAndSortedSecretKeys(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		in := catalogInstall(capabilities.ScopeProject, "p1", "alpha")
		in.ID, in.CreatedAt, in.UpdatedAt = "", time.Unix(1, 0), time.Unix(2, 0)

		// Act
		got := put(t, s, in)

		// Assert
		if got.ID == "" {
			t.Fatal("no id assigned")
		}
		if !got.CreatedAt.Equal(clk.Now()) || !got.UpdatedAt.Equal(clk.Now()) {
			t.Fatalf("timestamps = %v / %v, want the store clock %v", got.CreatedAt, got.UpdatedAt, clk.Now())
		}
		if !reflect.DeepEqual(got.SecretKeys, []string{"a", "b"}) {
			t.Fatalf("secret keys = %v, want sorted", got.SecretKeys)
		}
		if got.Name != "alpha" {
			t.Fatalf("catalog install name = %q, want its entry id", got.Name)
		}
		back, err := s.GetInstall(ctx, got.ID)
		must(t, err)
		if !sameInstall(*back, *got) || string(back.Secrets) != "sealed" || back.CreatedBy != "alice" {
			t.Fatalf("stored = %+v, want %+v", back, got)
		}
	})
}

func TestCustomInstallRoundTripsEveryField(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		in := customInstall(capabilities.ScopeUser, "bob", "mine")

		// Act
		got := put(t, s, in)

		// Assert
		in.ID, in.CreatedAt, in.UpdatedAt = got.ID, clk.Now(), clk.Now()
		if !sameInstall(*got, in) {
			t.Fatalf("got %+v, want %+v", *got, in)
		}
		back, err := s.GetInstall(ctx, got.ID)
		must(t, err)
		if !sameInstall(*back, in) {
			t.Fatalf("stored %+v, want %+v", *back, in)
		}
	})
}

func TestASecondCatalogInstallReplacesTheFirstAndKeepsItsID(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		first := put(t, s, catalogInstall(capabilities.ScopeProject, "p1", "alpha"))
		clk.Advance(time.Hour)
		second := catalogInstall(capabilities.ScopeProject, "p1", "alpha")
		second.Enabled = false
		second.Secrets = []byte("newer")
		second.SecretKeys = []string{"c"}
		second.Tools = []string{"only"}
		second.CreatedBy = "mallory"

		// Act
		got := put(t, s, second)

		// Assert
		if got.ID != first.ID {
			t.Fatalf("id = %q, want the first install's %q", got.ID, first.ID)
		}
		if got.Enabled || string(got.Secrets) != "newer" || !reflect.DeepEqual(got.SecretKeys, []string{"c"}) || !reflect.DeepEqual(got.Tools, []string{"only"}) {
			t.Fatalf("content not replaced: %+v", got)
		}
		if !got.CreatedAt.Equal(first.CreatedAt) || got.CreatedBy != "alice" || !got.UpdatedAt.Equal(clk.Now()) {
			t.Fatalf("created %v by %q updated %v; want creation kept, update stamped", got.CreatedAt, got.CreatedBy, got.UpdatedAt)
		}
		list, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		if len(list) != 1 {
			t.Fatalf("%d installs, want 1", len(list))
		}
	})
}

func TestCatalogInstallsOfDifferentScopesAreIndependent(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange / Act
		a := put(t, s, catalogInstall(capabilities.ScopeProject, "x", "alpha"))
		b := put(t, s, catalogInstall(capabilities.ScopeUser, "x", "alpha"))
		c := put(t, s, catalogInstall(capabilities.ScopeProject, "y", "alpha"))

		// Assert
		if a.ID == b.ID || a.ID == c.ID || b.ID == c.ID {
			t.Fatalf("ids %q %q %q must differ: one project's install is not another scope's", a.ID, b.ID, c.ID)
		}
	})
}

func TestReplacingByIDUpdatesThatInstallAndRejectsUnknownOrForeignIDs(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		mine := put(t, s, customInstall(capabilities.ScopeProject, "p1", "mine"))
		clk.Advance(time.Minute)
		upd := *mine
		upd.URL = "https://example.com/other"
		upd.Enabled = false
		upd.CreatedBy = "mallory"

		// Act
		got, err := s.PutInstall(ctx, upd)

		// Assert
		must(t, err)
		if got.ID != mine.ID || got.URL != "https://example.com/other" || got.Enabled || got.CreatedBy != "alice" || !got.CreatedAt.Equal(mine.CreatedAt) {
			t.Fatalf("replaced = %+v", got)
		}
		ghost := customInstall(capabilities.ScopeProject, "p1", "ghost")
		ghost.ID = "no-such-id"
		_, err = s.PutInstall(ctx, ghost)
		wantErr(t, "unknown id", err, capabilities.ErrNotFound)
		foreign := upd
		foreign.ScopeID = "p2"
		_, err = s.PutInstall(ctx, foreign)
		wantErr(t, "another scope's id", err, capabilities.ErrNotFound)
		back, err := s.GetInstall(ctx, mine.ID)
		must(t, err)
		if back.ScopeID != "p1" {
			t.Fatalf("install moved to scope %q", back.ScopeID)
		}
	})
}

func TestCustomNamesAreUniquePerScopeAndNeverTakeACatalogID(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{entry("alpha")}))
		first := put(t, s, customInstall(capabilities.ScopeProject, "p1", "mine"))
		other := put(t, s, customInstall(capabilities.ScopeProject, "p1", "other"))

		// Act
		_, dup := s.PutInstall(ctx, customInstall(capabilities.ScopeProject, "p1", "mine"))
		_, catalogName := s.PutInstall(ctx, customInstall(capabilities.ScopeProject, "p1", "alpha"))
		rename := *other
		rename.Name = "mine"
		_, renameDup := s.PutInstall(ctx, rename)
		samePlace := *first
		samePlace.URL = "https://example.com/changed"
		_, sameName := s.PutInstall(ctx, samePlace)
		_, otherScope := s.PutInstall(ctx, customInstall(capabilities.ScopeUser, "p1", "mine"))
		_, otherScopeID := s.PutInstall(ctx, customInstall(capabilities.ScopeProject, "p2", "mine"))

		// Assert
		wantErr(t, "duplicate name", dup, capabilities.ErrConflict)
		wantErr(t, "catalog id as name", catalogName, capabilities.ErrConflict)
		wantErr(t, "rename onto a taken name", renameDup, capabilities.ErrConflict)
		must(t, sameName)
		must(t, otherScope)
		must(t, otherScopeID)
		list, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		if len(list) != 2 {
			t.Fatalf("%d installs, want the 2 that did not conflict", len(list))
		}
	})
}

func TestACustomNameMayEqualAnOrphanedCatalogInstall(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange: "gone" was a catalog entry once; its install is an orphan now and still owns the name.
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{entry("gone")}))
		put(t, s, catalogInstall(capabilities.ScopeProject, "p1", "gone"))
		must(t, s.SeedCatalog(ctx, nil))

		// Act
		_, err := s.PutInstall(ctx, customInstall(capabilities.ScopeProject, "p1", "gone"))

		// Assert
		wantErr(t, "name of an orphan", err, capabilities.ErrConflict)
	})
}

func TestListInstallsIsScopedAndOrderedByName(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		put(t, s, customInstall(capabilities.ScopeProject, "p1", "zulu"))
		put(t, s, catalogInstall(capabilities.ScopeProject, "p1", "mike"))
		put(t, s, customInstall(capabilities.ScopeProject, "p1", "alpha"))
		put(t, s, customInstall(capabilities.ScopeProject, "p2", "other-project"))
		put(t, s, customInstall(capabilities.ScopeUser, "p1", "other-scope"))

		// Act
		got, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		none, err2 := s.ListInstalls(ctx, capabilities.ScopeProject, "nobody")

		// Assert
		must(t, err)
		must(t, err2)
		var names []string
		for _, in := range got {
			names = append(names, in.Name)
		}
		if !reflect.DeepEqual(names, []string{"alpha", "mike", "zulu"}) {
			t.Fatalf("names = %v", names)
		}
		if len(none) != 0 {
			t.Fatalf("unknown scope lists %d installs", len(none))
		}
	})
}

func TestDeleteInstallRemovesItAndReportsAMissingOne(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		in := put(t, s, customInstall(capabilities.ScopeProject, "p1", "mine"))

		// Act
		err := s.DeleteInstall(ctx, in.ID)
		again := s.DeleteInstall(ctx, in.ID)

		// Assert
		must(t, err)
		wantErr(t, "second delete", again, capabilities.ErrNotFound)
		_, err = s.GetInstall(ctx, in.ID)
		wantErr(t, "get after delete", err, capabilities.ErrNotFound)
		// the name is free again
		put(t, s, customInstall(capabilities.ScopeProject, "p1", "mine"))
	})
}

func TestDeleteInstallsRemovesOnlyThatScope(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		put(t, s, customInstall(capabilities.ScopeProject, "p1", "a"))
		put(t, s, catalogInstall(capabilities.ScopeProject, "p1", "b"))
		keepProject := put(t, s, customInstall(capabilities.ScopeProject, "p2", "a"))
		keepUser := put(t, s, customInstall(capabilities.ScopeUser, "p1", "a"))

		// Act
		err := s.DeleteInstalls(ctx, capabilities.ScopeProject, "p1")
		empty := s.DeleteInstalls(ctx, capabilities.ScopeProject, "nobody")

		// Assert
		must(t, err)
		must(t, empty)
		gone, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		if len(gone) != 0 {
			t.Fatalf("%d installs left", len(gone))
		}
		for _, in := range []*capabilities.Install{keepProject, keepUser} {
			if _, err := s.GetInstall(ctx, in.ID); err != nil {
				t.Fatalf("install of another scope lost: %v", err)
			}
		}
	})
}

func TestResultsShareNothingWithTheStore(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		must(t, s.SeedCatalog(ctx, []capabilities.Entry{entry("alpha")}))
		in := customInstall(capabilities.ScopeProject, "p1", "mine")
		stored := put(t, s, in)

		// Act: scribble over everything the store handed out or was given.
		in.Tools[0], in.Secrets[0], in.SecretKeys[0] = "x", 'x', "x"
		stored.Tools[0], stored.Secrets[0], stored.SecretKeys[0] = "y", 'y', "y"
		got, err := s.GetInstall(ctx, stored.ID)
		must(t, err)
		got.Tools[0], got.Secrets[0], got.SecretKeys[0] = "z", 'z', "z"
		list, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		list[0].Tools[0], list[0].Secrets[0], list[0].SecretKeys[0] = "w", 'w', "w"
		cat, err := s.GetCatalogEntry(ctx, "alpha")
		must(t, err)
		cat.Headers["Authorization"], cat.Tools[0], cat.Fields[0].Key = "x", "x", "x"
		all, err := s.ListCatalog(ctx)
		must(t, err)
		all[0].Headers["Authorization"], all[0].Tools[0], all[0].Fields[0].Key = "w", "w", "w"

		// Assert
		back, err := s.GetInstall(ctx, stored.ID)
		must(t, err)
		if back.Tools[0] != "t1" || string(back.Secrets) != "sealed-headers" || back.SecretKeys[0] != "X-Key" {
			t.Fatalf("install changed through an alias: %+v", back)
		}
		e, err := s.GetCatalogEntry(ctx, "alpha")
		must(t, err)
		if e.Headers["Authorization"] != "Bearer ${api_key}" || e.Tools[0] != "search" || e.Fields[0].Key != "api_key" {
			t.Fatalf("catalog entry changed through an alias: %+v", e)
		}
	})
}

func TestEmptyListsAndSecretsComeBackEmpty(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		in := capabilities.Install{Scope: capabilities.ScopeUser, ScopeID: "bob", CatalogID: "alpha"}

		// Act
		got := put(t, s, in)
		back, err := s.GetInstall(ctx, got.ID)
		must(t, err)

		// Assert
		for _, x := range []*capabilities.Install{got, back} {
			if len(x.Tools) != 0 || len(x.Secrets) != 0 || len(x.SecretKeys) != 0 || x.Enabled {
				t.Fatalf("install = %+v, want empty tools, secrets and keys", x)
			}
		}
	})
}

func TestPutInstallRejectsAnIncompleteInstall(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		for name, in := range map[string]capabilities.Install{
			"no scope":      {ScopeID: "p1", Name: "x"},
			"unknown scope": {Scope: "team", ScopeID: "p1", Name: "x"},
			"no scope id":   {Scope: capabilities.ScopeProject, Name: "x"},
			"no name":       {Scope: capabilities.ScopeProject, ScopeID: "p1"},
		} {
			if _, err := s.PutInstall(ctx, in); err == nil {
				t.Errorf("%s: install accepted", name)
			}
		}
		list, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		if len(list) != 0 {
			t.Fatalf("%d installs stored from rejected puts", len(list))
		}
	})
}

func TestConcurrentPutsOfOneCatalogInstallLeaveExactlyOneRow(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		const n = 24
		ids := make(chan string, n)
		errs := make(chan error, n)
		var wg sync.WaitGroup

		// Act
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				in := catalogInstall(capabilities.ScopeProject, "p1", "alpha")
				in.SecretKeys = []string{string(rune('a' + i))}
				got, err := s.PutInstall(ctx, in)
				if err != nil {
					errs <- err
					return
				}
				ids <- got.ID
			}()
		}
		wg.Wait()
		close(ids)
		close(errs)

		// Assert
		for err := range errs {
			t.Fatalf("put failed: %v", err)
		}
		seen := map[string]bool{}
		for id := range ids {
			seen[id] = true
		}
		if len(seen) != 1 {
			t.Fatalf("callers saw %d distinct ids, want 1", len(seen))
		}
		list, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		if len(list) != 1 {
			t.Fatalf("%d rows, want exactly 1", len(list))
		}
	})
}

func TestConcurrentCreatesOfOneCustomNameHaveExactlyOneWinner(t *testing.T) {
	eachStore(t, func(t *testing.T, s capabilities.Store, clk *clock) {
		// Arrange
		const n = 16
		var won, conflicts, other atomic.Int32
		var wg sync.WaitGroup

		// Act
		for range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.PutInstall(ctx, customInstall(capabilities.ScopeProject, "p1", "mine"))
				switch {
				case err == nil:
					won.Add(1)
				case errors.Is(err, capabilities.ErrConflict):
					conflicts.Add(1)
				default:
					other.Add(1)
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		// Assert
		if won.Load() != 1 || conflicts.Load() != n-1 || other.Load() != 0 {
			t.Fatalf("won=%d conflicts=%d other=%d, want 1 / %d / 0", won.Load(), conflicts.Load(), other.Load(), n-1)
		}
		list, err := s.ListInstalls(ctx, capabilities.ScopeProject, "p1")
		must(t, err)
		if len(list) != 1 {
			t.Fatalf("%d rows, want exactly 1", len(list))
		}
	})
}
