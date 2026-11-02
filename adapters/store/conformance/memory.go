package conformance

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// Memory runs the memory suite: store.MemoryStore and store.MemoryEmbeddingStore.
func Memory(t *testing.T, f Factory) {
	t.Helper()
	memorySettings(t, f)
	memoryReads(t, f)
	memoryCreate(t, f)
	memoryUpdate(t, f)
	memoryDelete(t, f)
	memoryUsersAndPurge(t, f)
	memoryEmbeddings(t, f)
	memoryConcurrent(t, f)
	memoryRestart(t, f)
}

// memBase is the creation time of the first test entry; memAt(i) is i minutes later (whole seconds: every backend keeps them).
var memBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func memAt(i int) time.Time { return memBase.Add(time.Duration(i) * time.Minute) }

// memRun runs one subtest on a fresh backend.
func memRun(t *testing.T, f Factory, name string, fn func(t *testing.T, b Backend)) {
	t.Helper()
	t.Run(name, func(t *testing.T) { fn(t, f(t)) })
}

// memEntry is a valid entry of login (created at memBase, active, a fact path derived from id).
func memEntry(login, id, path, status string) *store.MemoryEntry {
	imp := 0.5
	return &store.MemoryEntry{ID: id, UserLogin: login, Path: path, Title: "Title " + id, Content: "Content of " + id,
		Tags: []string{"t1"}, Importance: &imp, Metadata: map[string]any{"trigger": "durable_preference"}, Scope: "personal",
		Repository: "default", Source: store.MemorySourceProxy, Status: status, ContentHash: "hash-" + id, CreatedAt: memBase}
}

// memCreate creates e without limits and fails the test on an error.
func memCreate(t *testing.T, st Store, e *store.MemoryEntry) *store.MemoryEntry {
	t.Helper()
	out, err := st.CreateMemoryEntry(context.Background(), e, store.MemoryLimits{})
	if err != nil {
		t.Fatalf("create %s: %v", e.ID, err)
	}
	return out
}

// memSoftDelete moves e to the deleted status at deletedAt, as the memory service's soft delete does.
func memSoftDelete(t *testing.T, st Store, e *store.MemoryEntry, deletedAt time.Time) *store.MemoryEntry {
	t.Helper()
	d := e.Clone()
	d.Status, d.DeletedAt, d.UpdatedAt, d.ContentUpdatedAt = store.MemoryDeleted, &deletedAt, deletedAt, deletedAt
	out, err := st.UpdateMemoryEntry(context.Background(), d, store.MemoryLimits{})
	if err != nil {
		t.Fatalf("soft delete %s: %v", e.ID, err)
	}
	return out
}

func memIDs(es []*store.MemoryEntry) []string {
	ids := make([]string, 0, len(es))
	for _, e := range es {
		ids = append(ids, e.ID)
	}
	return ids
}

// memSameTime compares instants (the location a backend returns is not part of the contract).
func memSameTime(a, b time.Time) bool { return a.Equal(b) }

// memSameEntry fails when got differs from want in any field the port exposes (times compared as instants).
func memSameEntry(t *testing.T, what string, got, want *store.MemoryEntry) {
	t.Helper()
	if got == nil || want == nil {
		t.Fatalf("%s: got %v want %v", what, got, want)
	}
	if got.ID != want.ID || got.UserLogin != want.UserLogin || got.Path != want.Path || got.Title != want.Title ||
		got.Content != want.Content || got.Scope != want.Scope || got.Repository != want.Repository || got.Source != want.Source ||
		got.Status != want.Status || got.SourceThreadID != want.SourceThreadID || got.ContentHash != want.ContentHash ||
		got.Version != want.Version {
		t.Fatalf("%s: scalar fields differ:\n got %+v\nwant %+v", what, got, want)
	}
	if !reflect.DeepEqual(got.Tags, want.Tags) || !reflect.DeepEqual(got.Metadata, want.Metadata) {
		t.Fatalf("%s: tags/metadata differ: got %v %v want %v %v", what, got.Tags, got.Metadata, want.Tags, want.Metadata)
	}
	if (got.Importance == nil) != (want.Importance == nil) || (got.Importance != nil && *got.Importance != *want.Importance) {
		t.Fatalf("%s: importance got %v want %v", what, got.Importance, want.Importance)
	}
	if !memSameTime(got.CreatedAt, want.CreatedAt) || !memSameTime(got.UpdatedAt, want.UpdatedAt) ||
		!memSameTime(got.ContentUpdatedAt, want.ContentUpdatedAt) {
		t.Fatalf("%s: times differ: got %v %v %v want %v %v %v", what, got.CreatedAt, got.UpdatedAt, got.ContentUpdatedAt,
			want.CreatedAt, want.UpdatedAt, want.ContentUpdatedAt)
	}
	if (got.DeletedAt == nil) != (want.DeletedAt == nil) || (got.DeletedAt != nil && !memSameTime(*got.DeletedAt, *want.DeletedAt)) {
		t.Fatalf("%s: deleted at got %v want %v", what, got.DeletedAt, want.DeletedAt)
	}
	if (got.Proposal == nil) != (want.Proposal == nil) {
		t.Fatalf("%s: proposal got %+v want %+v", what, got.Proposal, want.Proposal)
	}
	if p, q := got.Proposal, want.Proposal; p != nil {
		if p.Path != q.Path || p.Title != q.Title || p.Content != q.Content || p.ContentHash != q.ContentHash || p.ThreadID != q.ThreadID ||
			!memSameTime(p.ProposedAt, q.ProposedAt) || !reflect.DeepEqual(p.Tags, q.Tags) || !reflect.DeepEqual(p.Metadata, q.Metadata) ||
			(p.Importance == nil) != (q.Importance == nil) || (p.Importance != nil && *p.Importance != *q.Importance) {
			t.Fatalf("%s: proposal differs:\n got %+v\nwant %+v", what, p, q)
		}
	}
}

func memorySettings(t *testing.T, f Factory) {
	memRun(t, f, "memory is enabled by default and the setting is per user", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store

		// Act
		before, err := st.MemoryEnabled(ctx, "alice")
		offErr := st.SetMemoryEnabled(ctx, "alice", false)
		off, _ := st.MemoryEnabled(ctx, "alice")
		bob, _ := st.MemoryEnabled(ctx, "bob")
		onErr := st.SetMemoryEnabled(ctx, "alice", true)
		on, _ := st.MemoryEnabled(ctx, "alice")

		// Assert
		if err != nil || offErr != nil || onErr != nil {
			t.Fatalf("errors: %v %v %v", err, offErr, onErr)
		}
		if !before || off || !bob || !on {
			t.Fatalf("enabled: default %v after off %v other user %v after on %v", before, off, bob, on)
		}
	})
}

func memoryReads(t *testing.T, f Factory) {
	memRun(t, f, "lists a user's entries oldest first, filtered by status", func(t *testing.T, b Backend) {
		// Arrange: created in the reverse order of their creation times, one per status, plus another user's entry.
		ctx := context.Background()
		st := b.Store
		for i, spec := range []struct{ id, status string }{{"m3", store.MemoryActive}, {"m2", store.MemoryPending}, {"m1", store.MemoryActive}} {
			e := memEntry("alice", spec.id, "memories/semantic/notes/"+spec.id+".md", spec.status)
			e.CreatedAt = memAt(3 - i)
			memCreate(t, st, e)
		}
		gone := memEntry("alice", "m0", "memories/semantic/notes/m0.md", store.MemoryActive)
		gone.CreatedAt = memAt(0)
		memSoftDelete(t, st, memCreate(t, st, gone), memAt(50))
		memCreate(t, st, memEntry("bob", "b1", "memories/semantic/notes/m1.md", store.MemoryActive))

		// Act
		all, allErr := st.ListMemoryEntries(ctx, "alice")
		active, _ := st.ListMemoryEntries(ctx, "alice", store.MemoryActive)
		pendingDeleted, _ := st.ListMemoryEntries(ctx, "alice", store.MemoryPending, store.MemoryDeleted)
		unknown, _ := st.ListMemoryEntries(ctx, "alice", "bogus")
		nobody, nobodyErr := st.ListMemoryEntries(ctx, "carol")

		// Assert
		if allErr != nil || nobodyErr != nil {
			t.Fatalf("errors: %v %v", allErr, nobodyErr)
		}
		if got, want := memIDs(all), []string{"m0", "m1", "m2", "m3"}; !slices.Equal(got, want) {
			t.Fatalf("all = %v, want %v", got, want)
		}
		if got, want := memIDs(active), []string{"m1", "m3"}; !slices.Equal(got, want) {
			t.Fatalf("active = %v, want %v", got, want)
		}
		if got, want := memIDs(pendingDeleted), []string{"m0", "m2"}; !slices.Equal(got, want) {
			t.Fatalf("pending+deleted = %v, want %v", got, want)
		}
		if len(unknown) != 0 || len(nobody) != 0 {
			t.Fatalf("unknown status %v, unknown user %v: want none", memIDs(unknown), memIDs(nobody))
		}
	})

	memRun(t, f, "gets an entry by id only for its owner", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))

		// Act
		got, err := st.GetMemoryEntry(ctx, "alice", "m1")
		_, otherUser := st.GetMemoryEntry(ctx, "bob", "m1")
		_, missing := st.GetMemoryEntry(ctx, "alice", "nope")

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		memSameEntry(t, "get", got, created)
		if !memNotFound(otherUser) || !memNotFound(missing) {
			t.Fatalf("other user: %v, missing: %v; want ErrNotFound", otherUser, missing)
		}
	})

	memRun(t, f, "returned entries are copies the caller may modify", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
		e.Metadata = map[string]any{"nested": map[string]any{"k": "v"}}
		created := memCreate(t, st, e)
		created.Tags[0], created.Metadata["nested"].(map[string]any)["k"] = "changed", "changed"
		*created.Importance = 0.99

		// Act
		first, _ := st.GetMemoryEntry(ctx, "alice", "m1")
		first.Tags[0], first.Metadata["nested"].(map[string]any)["k"] = "changed again", "changed again"
		listed, _ := st.ListMemoryEntries(ctx, "alice")
		second, _ := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		for what, got := range map[string]*store.MemoryEntry{"list": listed[0], "get": second} {
			if got.Tags[0] != "t1" || got.Metadata["nested"].(map[string]any)["k"] != "v" || *got.Importance != 0.5 {
				t.Fatalf("%s: the store shares memory with a returned entry: %+v", what, got)
			}
		}
	})

	memRun(t, f, "looks an entry up by path: the live one first, else the latest deleted", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		const live, onlyDeleted = "memories/semantic/notes/live.md", "memories/semantic/notes/old.md"
		// The deleted entry of the live path was updated after the live one: liveness wins, not recency.
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "d0", live, store.MemoryActive)), memAt(30))
		pending := memCreate(t, st, memEntry("alice", "p1", live, store.MemoryPending))
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "d1", onlyDeleted, store.MemoryActive)), memAt(10))
		latest := memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "d2", onlyDeleted, store.MemoryActive)), memAt(20))
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "d3", onlyDeleted, store.MemoryActive)), memAt(5))
		memCreate(t, st, memEntry("bob", "b1", "memories/semantic/notes/bob.md", store.MemoryActive))

		// Act
		gotLive, liveErr := st.GetMemoryEntryByPath(ctx, "alice", live)
		gotOld, oldErr := st.GetMemoryEntryByPath(ctx, "alice", onlyDeleted)
		_, otherUser := st.GetMemoryEntryByPath(ctx, "alice", "memories/semantic/notes/bob.md")
		_, missing := st.GetMemoryEntryByPath(ctx, "alice", "memories/semantic/notes/none.md")

		// Assert
		if liveErr != nil || oldErr != nil {
			t.Fatalf("errors: %v %v", liveErr, oldErr)
		}
		memSameEntry(t, "live path", gotLive, pending)
		memSameEntry(t, "deleted-only path", gotOld, latest)
		if !memNotFound(otherUser) || !memNotFound(missing) {
			t.Fatalf("other user's path: %v, unknown path: %v; want ErrNotFound", otherUser, missing)
		}
	})
}

func memNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }
