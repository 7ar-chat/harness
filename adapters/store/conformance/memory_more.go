package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func memVec(entryID, model, hash string, vec ...float32) store.EntryEmbedding {
	return store.EntryEmbedding{EntryID: entryID, Model: model, ContentHash: hash, Vec: vec}
}

func memoryEmbeddings(t *testing.T, f Factory) {
	memRun(t, f, "embeddings round-trip per entry and are scoped to their user", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "a1", "memories/semantic/notes/a1.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "a2", "memories/semantic/notes/a2.md", store.MemoryPending))
		memCreate(t, st, memEntry("bob", "b1", "memories/semantic/notes/b1.md", store.MemoryActive))

		// Act
		aliceErr := st.PutMemoryEmbeddings(ctx, "alice", []store.EntryEmbedding{memVec("a1", "m-1", "h1", 0.25, -1, 3), memVec("a2", "m-1", "h2", 1, 2, 3)})
		bobErr := st.PutMemoryEmbeddings(ctx, "bob", []store.EntryEmbedding{memVec("b1", "m-2", "hb", 9)})
		alice, aliceGet := st.GetMemoryEmbeddings(ctx, "alice")
		bob, _ := st.GetMemoryEmbeddings(ctx, "bob")
		carol, carolErr := st.GetMemoryEmbeddings(ctx, "carol")

		// Assert
		if aliceErr != nil || bobErr != nil || aliceGet != nil || carolErr != nil {
			t.Fatalf("errors: %v %v %v %v", aliceErr, bobErr, aliceGet, carolErr)
		}
		wantAlice := map[string]store.EntryEmbedding{"a1": memVec("a1", "m-1", "h1", 0.25, -1, 3), "a2": memVec("a2", "m-1", "h2", 1, 2, 3)}
		if !reflect.DeepEqual(alice, wantAlice) {
			t.Fatalf("alice = %+v, want %+v", alice, wantAlice)
		}
		if !reflect.DeepEqual(bob, map[string]store.EntryEmbedding{"b1": memVec("b1", "m-2", "hb", 9)}) {
			t.Fatalf("bob = %+v", bob)
		}
		if carol == nil || len(carol) != 0 {
			t.Fatalf("a user without vectors gets %#v, want an empty map", carol)
		}
	})

	memRun(t, f, "putting an entry's embedding again replaces model, hash and vector", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "a1", "memories/semantic/notes/a1.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "a2", "memories/semantic/notes/a2.md", store.MemoryActive))
		_ = st.PutMemoryEmbeddings(ctx, "alice", []store.EntryEmbedding{memVec("a1", "old", "h1", 1, 2, 3), memVec("a2", "old", "h2", 7)})

		// Act
		err := st.PutMemoryEmbeddings(ctx, "alice", []store.EntryEmbedding{memVec("a1", "new", "h1b", 4, 5)})
		got, _ := st.GetMemoryEmbeddings(ctx, "alice")

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]store.EntryEmbedding{"a1": memVec("a1", "new", "h1b", 4, 5), "a2": memVec("a2", "old", "h2", 7)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	memRun(t, f, "putting no embeddings is a no-op", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store

		// Act
		err := st.PutMemoryEmbeddings(ctx, "alice", nil)
		got, getErr := st.GetMemoryEmbeddings(ctx, "alice")

		// Assert
		if err != nil || getErr != nil || len(got) != 0 {
			t.Fatalf("put: %v, get: %v %v", err, got, getErr)
		}
	})

	memRun(t, f, "embeddings are returned only for live entries", func(t *testing.T, b Backend) {
		// Arrange: an active, a pending, a soft-deleted and a hard-deleted entry, and a vector for an entry that never existed.
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "act", "memories/semantic/notes/act.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "pend", "memories/semantic/notes/pend.md", store.MemoryPending))
		soft := memCreate(t, st, memEntry("alice", "soft", "memories/semantic/notes/soft.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "hard", "memories/semantic/notes/hard.md", store.MemoryActive))
		memCreate(t, st, memEntry("bob", "bobs", "memories/semantic/notes/bobs.md", store.MemoryActive))
		all := []store.EntryEmbedding{memVec("act", "m", "h", 1), memVec("pend", "m", "h", 2), memVec("soft", "m", "h", 3), memVec("hard", "m", "h", 4),
			memVec("ghost", "m", "h", 5), memVec("bobs", "m", "h", 6)}
		if err := st.PutMemoryEmbeddings(ctx, "alice", all); err != nil {
			t.Fatal(err)
		}
		memSoftDelete(t, st, soft, memAt(5))
		if err := st.DeleteMemoryEntry(ctx, "alice", "hard", ""); err != nil {
			t.Fatal(err)
		}

		// Act
		got, err := st.GetMemoryEmbeddings(ctx, "alice")

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]store.EntryEmbedding{"act": memVec("act", "m", "h", 1), "pend": memVec("pend", "m", "h", 2)}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	memRun(t, f, "a restored entry gets its embedding back", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memCreate(t, st, memEntry("alice", "a1", "memories/semantic/notes/a1.md", store.MemoryActive))
		_ = st.PutMemoryEmbeddings(ctx, "alice", []store.EntryEmbedding{memVec("a1", "m", "h", 1, 2)})
		deleted := memSoftDelete(t, st, e, memAt(5))
		hidden, _ := st.GetMemoryEmbeddings(ctx, "alice")
		restore := deleted.Clone()
		restore.Status, restore.DeletedAt = store.MemoryActive, nil

		// Act
		_, err := st.UpdateMemoryEntry(ctx, restore, store.MemoryLimits{})
		back, _ := st.GetMemoryEmbeddings(ctx, "alice")

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if len(hidden) != 0 || !reflect.DeepEqual(back["a1"], memVec("a1", "m", "h", 1, 2)) {
			t.Fatalf("while deleted %+v, once restored %+v", hidden, back)
		}
	})

	memRun(t, f, "stored vectors are not aliased to the caller's slices", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "a1", "memories/semantic/notes/a1.md", store.MemoryActive))
		vec := []float32{1, 2, 3}
		_ = st.PutMemoryEmbeddings(ctx, "alice", []store.EntryEmbedding{{EntryID: "a1", Model: "m", ContentHash: "h", Vec: vec}})
		vec[0] = 99

		// Act
		first, _ := st.GetMemoryEmbeddings(ctx, "alice")
		first["a1"].Vec[1] = 77
		second, _ := st.GetMemoryEmbeddings(ctx, "alice")

		// Assert
		if !reflect.DeepEqual(second["a1"].Vec, []float32{1, 2, 3}) {
			t.Fatalf("vector = %v, want [1 2 3]", second["a1"].Vec)
		}
	})
}

func memoryConcurrent(t *testing.T, f Factory) {
	memRun(t, f, "concurrent creates never overshoot the active limit", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		const racers, limit = 8, 3
		lim := store.MemoryLimits{Active: limit}
		errs := make([]error, racers)

		// Act
		var wg sync.WaitGroup
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = st.CreateMemoryEntry(ctx, memEntry("alice", fmt.Sprintf("m%d", i), fmt.Sprintf("memories/semantic/notes/%d.md", i), store.MemoryActive), lim)
			}()
		}
		wg.Wait()

		// Assert
		created, limited := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				created++
			case errors.Is(err, store.ErrMemoryLimit):
				limited++
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if all, _ := st.ListMemoryEntries(ctx, "alice"); created != limit || limited != racers-limit || len(all) != limit {
			t.Fatalf("created %d, limited %d, stored %d; want %d, %d, %d", created, limited, len(all), limit, racers-limit, limit)
		}
	})

	memRun(t, f, "concurrent updates from the same version: exactly one wins", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		const racers = 6
		errs := make([]error, racers)

		// Act
		var wg sync.WaitGroup
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				u := created.Clone()
				u.Content = fmt.Sprintf("writer %d", i)
				_, errs[i] = st.UpdateMemoryEntry(ctx, u, store.MemoryLimits{})
			}()
		}
		wg.Wait()

		// Assert
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, store.ErrMemoryConflict):
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if got, _ := st.GetMemoryEntry(ctx, "alice", "m1"); won != 1 || got.Version != 1 {
			t.Fatalf("%d writers won, stored version %d; want 1 and 1", won, got.Version)
		}
	})
}

func memoryRestart(t *testing.T, f Factory) {
	memRun(t, f, "entries, settings and deletions survive a restart", func(t *testing.T, b Backend) {
		if b.Reopen == nil {
			t.Skip("the backend does not persist between instances")
		}
		// Arrange
		ctx := context.Background()
		st := b.Store
		lim := store.MemoryLimits{Active: 100, Pending: 100}
		imp := 0.7
		a := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
		a.Importance, a.Metadata = &imp, map[string]any{"pinned": true, "nested": map[string]any{"k": "v"}}
		a.SourceThreadID = "0b6f6d1e-5d3f-4a0a-9a52-0c1d5f3a7b11"
		created, _ := st.CreateMemoryEntry(ctx, a, lim)
		bEntry, _ := st.CreateMemoryEntry(ctx, memEntry("alice", "m2", "memories/semantic/notes/b.md", store.MemoryPending), lim)
		hard := memCreate(t, st, memEntry("alice", "m3", "memories/semantic/notes/c.md", store.MemoryActive))
		softSrc := memCreate(t, st, memEntry("alice", "m4", "memories/semantic/notes/d.md", store.MemoryActive))
		purged := memCreate(t, st, memEntry("alice", "m5", "memories/semantic/notes/e.md", store.MemoryActive))
		bobs := memCreate(t, st, memEntry("bob", "b1", "memories/semantic/notes/b1.md", store.MemoryActive))
		upd := created.Clone()
		upd.Content, upd.UpdatedAt = "revised", memAt(3)
		upd.Proposal = &store.MemoryProposal{Path: upd.Path, Title: "P", Content: "proposed", Tags: []string{"x"}, ProposedAt: memAt(2)}
		updated, err := st.UpdateMemoryEntry(ctx, upd, lim)
		if err != nil {
			t.Fatal(err)
		}
		soft := memSoftDelete(t, st, softSrc, memAt(40))
		memSoftDelete(t, st, purged, memAt(10))
		if err := st.DeleteMemoryEntry(ctx, "alice", hard.ID, ""); err != nil {
			t.Fatal(err)
		}
		if n, err := st.PurgeDeletedMemoryEntries(ctx, memAt(20)); err != nil || n != 1 {
			t.Fatalf("purge before the restart: %d, %v", n, err)
		}
		if err := st.SetMemoryEnabled(ctx, "alice", false); err != nil {
			t.Fatal(err)
		}
		_ = bEntry

		// Act
		st = b.Reopen(t)
		all, listErr := st.ListMemoryEntries(ctx, "alice")
		gotA, aErr := st.GetMemoryEntry(ctx, "alice", "m1")
		gotSoft, softErr := st.GetMemoryEntry(ctx, "alice", "m4")
		gotBob, bobErr := st.GetMemoryEntry(ctx, "bob", "b1")
		_, hardErr := st.GetMemoryEntry(ctx, "alice", "m3")
		_, purgedErr := st.GetMemoryEntry(ctx, "alice", "m5")
		aliceOn, _ := st.MemoryEnabled(ctx, "alice")
		bobOn, _ := st.MemoryEnabled(ctx, "bob")
		users, _ := st.ListMemoryUsers(ctx)
		_, clash := st.CreateMemoryEntry(ctx, memEntry("alice", "m6", "memories/semantic/notes/a.md", store.MemoryActive), lim)
		stale := upd.Clone()
		_, staleErr := st.UpdateMemoryEntry(ctx, stale, lim)

		// Assert
		if listErr != nil || aErr != nil || softErr != nil || bobErr != nil {
			t.Fatalf("reads: %v %v %v %v", listErr, aErr, softErr, bobErr)
		}
		if got := memIDs(all); !reflect.DeepEqual(got, []string{"m1", "m2", "m4"}) {
			t.Fatalf("alice's entries = %v, want m1 m2 m4 (m3 deleted, m5 purged)", got)
		}
		memSameEntry(t, "updated entry with its proposal", gotA, updated)
		memSameEntry(t, "soft-deleted entry", gotSoft, soft)
		memSameEntry(t, "another user's entry", gotBob, bobs)
		if !memNotFound(hardErr) || !memNotFound(purgedErr) {
			t.Fatalf("deleted entry: %v, purged entry: %v; want ErrNotFound", hardErr, purgedErr)
		}
		if aliceOn || !bobOn {
			t.Fatalf("memory enabled: alice %v, bob %v; want false and true", aliceOn, bobOn)
		}
		if !reflect.DeepEqual(users, []string{"alice", "bob"}) {
			t.Fatalf("users = %v", users)
		}
		if !errors.Is(clash, store.ErrMemoryPathExists) || !errors.Is(staleErr, store.ErrMemoryConflict) {
			t.Fatalf("path clash after restart: %v, stale version after restart: %v", clash, staleErr)
		}
	})

	memRun(t, f, "memory enabled can be turned back on and survives a restart", func(t *testing.T, b Backend) {
		if b.Reopen == nil {
			t.Skip("the backend does not persist between instances")
		}
		// Arrange
		ctx := context.Background()
		st := b.Store
		_ = st.SetMemoryEnabled(ctx, "alice", false)
		_ = st.SetMemoryEnabled(ctx, "bob", false)
		_ = st.SetMemoryEnabled(ctx, "alice", true)

		// Act
		st = b.Reopen(t)
		alice, _ := st.MemoryEnabled(ctx, "alice")
		bob, _ := st.MemoryEnabled(ctx, "bob")

		// Assert
		if !alice || bob {
			t.Fatalf("after restart alice %v bob %v; want true and false", alice, bob)
		}
	})
}
