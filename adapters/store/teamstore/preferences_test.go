package teamstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/core/store"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestTeamPreferencesAreKeptPerPersonAndMerged(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		ctx := context.Background()
		addUser(t, s, "alice", "")
		addUser(t, s, "bob", "")
		none, err := s.Preferences(ctx, "alice")
		if err != nil || none.Values == nil || len(none.Values) != 0 || !none.UpdatedAt.IsZero() && none.UpdatedAt.Year() > 1970 {
			t.Fatalf("nothing set yet: %#v, %v", none, err)
		}

		first, err := s.PatchPreferences(ctx, "alice", map[string]json.RawMessage{"onboarding": raw(`{"welcomed":true}`), "panel": raw(`{"h":240}`)})
		if err != nil || len(first.Values) != 2 || first.UpdatedAt.IsZero() {
			t.Fatalf("first patch = %#v, %v", first, err)
		}
		// A patch changes only its keys; null removes one.
		second, err := s.PatchPreferences(ctx, "alice", map[string]json.RawMessage{"onboarding": raw(`{"dismissed":true}`), "panel": raw(`null`)})
		if err != nil || len(second.Values) != 1 || string(second.Values["onboarding"]) != `{"dismissed":true}` || !second.UpdatedAt.After(first.UpdatedAt) {
			t.Fatalf("second patch = %#v, %v", second, err)
		}
		got, _ := s.Preferences(ctx, "alice")
		if string(got.Values["onboarding"]) != `{"dismissed":true}` || !got.UpdatedAt.Equal(second.UpdatedAt) {
			t.Fatalf("stored = %#v, want %#v", got, second)
		}
		// Only the caller's own document.
		if other, _ := s.Preferences(ctx, "bob"); len(other.Values) != 0 {
			t.Fatalf("bob sees %#v", other)
		}
		if _, err := s.PatchPreferences(ctx, "nobody", map[string]json.RawMessage{"a": raw(`1`)}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown login: %v", err)
		}
	})
}

func TestTeamPreferencesRefuseBadKeysAndTooMuchData(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		ctx := context.Background()
		addUser(t, s, "alice", "")
		must2 := func(p map[string]json.RawMessage) {
			t.Helper()
			if _, err := s.PatchPreferences(ctx, "alice", p); err != nil {
				t.Fatal(err)
			}
		}
		must2(map[string]json.RawMessage{"keep": raw(`"x"`)})
		if _, err := s.PatchPreferences(ctx, "alice", map[string]json.RawMessage{"Bad Key": raw(`1`)}); !errors.Is(err, store.ErrPreferencesKey) {
			t.Fatalf("bad key: %v", err)
		}
		big := raw(`"` + strings.Repeat("x", store.MaxPreferencesBytes) + `"`)
		if _, err := s.PatchPreferences(ctx, "alice", map[string]json.RawMessage{"big": big}); !errors.Is(err, store.ErrPreferencesTooLarge) {
			t.Fatalf("too large: %v", err)
		}
		// The refused patches changed nothing; just under the limit fits.
		if got, _ := s.Preferences(ctx, "alice"); len(got.Values) != 1 {
			t.Fatalf("a refused patch changed the document: %#v", got)
		}
		fits := raw(`"` + strings.Repeat("x", store.MaxPreferencesBytes-len(`{"keep":"x","fits":""}`)) + `"`)
		must2(map[string]json.RawMessage{"fits": fits})
	})
}

func TestTeamPreferencesSurviveRestartAndRacingDevices(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.TeamStore) {
		ctx := context.Background()
		addUser(t, s, "alice", "")
		var wg sync.WaitGroup
		keys := []string{"a", "b", "c", "d", "e", "f"}
		for _, k := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.PatchPreferences(ctx, "alice", map[string]json.RawMessage{k: raw(`true`)}); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		if got, _ := s.Preferences(ctx, "alice"); len(got.Values) != len(keys) {
			t.Fatalf("racing patches lost keys: %#v", got.Values)
		}
	})
}

func TestTeamPreferencesAreWrittenToTheFile(t *testing.T) {
	path := t.TempDir() + "/team.json"
	m, err := teamstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	addUser(t, m, "alice", "")
	want, err := m.PatchPreferences(ctx, "alice", map[string]json.RawMessage{"onboarding": raw(`{"toured":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := teamstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Preferences(ctx, "alice")
	if string(got.Values["onboarding"]) != `{"toured":true}` || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("after a restart: %#v, want %#v", got, want)
	}
}
