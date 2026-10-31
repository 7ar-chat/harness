package conformance

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

func memoryCreate(t *testing.T, f Factory) {
	memRun(t, f, "create stores every field and returns the stored entry at version 0", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/preferences/a.md", store.MemoryActive)
		e.SourceThreadID = "0b6f6d1e-5d3f-4a0a-9a52-0c1d5f3a7b11"
		e.Metadata = map[string]any{"trigger": "x", "n": 3, "f": 1.5, "b": true, "none": nil,
			"nested": map[string]any{"a": "b", "list": []any{"x", 2}}}
		e.Tags = []string{"preference", "language"}

		// Act
		created, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})
		got, getErr := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		if err != nil || getErr != nil {
			t.Fatalf("create: %v, get: %v", err, getErr)
		}
		want := e.Clone()
		want.Metadata = map[string]any{"trigger": "x", "n": float64(3), "f": 1.5, "b": true, "none": nil,
			"nested": map[string]any{"a": "b", "list": []any{"x", float64(2)}}}
		want.UpdatedAt, want.ContentUpdatedAt, want.Version = memBase, memBase, 0
		memSameEntry(t, "created", created, want)
		memSameEntry(t, "read back", got, want)
	})

	memRun(t, f, "create keeps a source thread only when it is a uuid", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
		e.SourceThreadID = "thread-that-is-not-a-uuid"

		// Act
		created, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if created.SourceThreadID != "" {
			t.Fatalf("source thread = %q, want it dropped", created.SourceThreadID)
		}
	})

	memRun(t, f, "create without tags or metadata returns empty ones, and no deletion date", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
		e.Tags, e.Metadata, e.Importance = nil, nil, nil
		at := memAt(9)
		e.DeletedAt = &at

		// Act
		created, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})
		got, _ := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		for what, x := range map[string]*store.MemoryEntry{"created": created, "read": got} {
			if x.Tags == nil || len(x.Tags) != 0 || x.Metadata == nil || len(x.Metadata) != 0 || x.Importance != nil || x.DeletedAt != nil {
				t.Fatalf("%s: tags %#v metadata %#v importance %v deleted at %v; want empty tags and metadata, no importance, no deletion date",
					what, x.Tags, x.Metadata, x.Importance, x.DeletedAt)
			}
		}
	})

	memRun(t, f, "create without a creation time stamps the current time on all three timestamps", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
		e.CreatedAt = time.Time{}
		lo := time.Now().Add(-time.Minute)

		// Act
		created, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		hi := time.Now().Add(time.Minute)
		for what, ts := range map[string]time.Time{"created": created.CreatedAt, "updated": created.UpdatedAt, "content updated": created.ContentUpdatedAt} {
			if ts.Before(lo) || ts.After(hi) || !ts.Equal(created.CreatedAt) {
				t.Fatalf("%s at %v, want the current time (%v..%v) on all three", what, ts, lo, hi)
			}
		}
	})

	memRun(t, f, "create enforces the per-status limit of each user", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		lim := store.MemoryLimits{Active: 2, Pending: 1}
		create := func(login, id, status string) error {
			_, err := st.CreateMemoryEntry(ctx, memEntry(login, id, "memories/semantic/notes/"+id+".md", status), lim)
			return err
		}

		// Act
		errs := []error{create("alice", "a1", store.MemoryActive), create("alice", "a2", store.MemoryActive), create("alice", "p1", store.MemoryPending)}
		thirdActive := create("alice", "a3", store.MemoryActive)
		secondPending := create("alice", "p2", store.MemoryPending)
		bob := create("bob", "b1", store.MemoryActive)
		deleted := create("alice", "d1", store.MemoryDeleted)
		deleted2 := create("alice", "d2", store.MemoryDeleted)

		// Assert
		for i, err := range errs {
			if err != nil {
				t.Fatalf("create %d within the limits: %v", i, err)
			}
		}
		if !errors.Is(thirdActive, store.ErrMemoryLimit) || !errors.Is(secondPending, store.ErrMemoryLimit) {
			t.Fatalf("third active: %v, second pending: %v; want ErrMemoryLimit", thirdActive, secondPending)
		}
		if bob != nil || deleted != nil || deleted2 != nil {
			t.Fatalf("another user: %v, deleted entries (no limit): %v %v; want none", bob, deleted, deleted2)
		}
		if all, _ := st.ListMemoryEntries(ctx, "alice"); len(all) != 5 {
			t.Fatalf("alice has %v, want a1 a2 p1 d1 d2 (the refused ones are not stored)", memIDs(all))
		}
	})

	memRun(t, f, "create with a zero limit or no limits is unbounded", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store

		// Act
		var last error
		for i := range 6 {
			_, last = st.CreateMemoryEntry(ctx, memEntry("alice", fmt.Sprintf("m%d", i), fmt.Sprintf("memories/semantic/notes/%d.md", i), store.MemoryActive),
				store.MemoryLimits{Active: 0, Pending: -1})
			if last != nil {
				break
			}
		}

		// Assert
		if last != nil {
			t.Fatal(last)
		}
		if all, _ := st.ListMemoryEntries(ctx, "alice"); len(all) != 6 {
			t.Fatalf("stored %v, want 6", memIDs(all))
		}
	})

	memRun(t, f, "create refuses a path another live entry of the user has", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		const path = "memories/semantic/notes/shared.md"
		memCreate(t, st, memEntry("alice", "m1", path, store.MemoryActive))
		pending := memEntry("alice", "m2", "memories/semantic/notes/other.md", store.MemoryPending)
		memCreate(t, st, pending)
		lim := store.MemoryLimits{}

		// Act
		_, againActive := st.CreateMemoryEntry(ctx, memEntry("alice", "m3", path, store.MemoryActive), lim)
		_, againPending := st.CreateMemoryEntry(ctx, memEntry("alice", "m4", path, store.MemoryPending), lim)
		_, ontoPending := st.CreateMemoryEntry(ctx, memEntry("alice", "m5", pending.Path, store.MemoryActive), lim)
		_, otherUser := st.CreateMemoryEntry(ctx, memEntry("bob", "m6", path, store.MemoryActive), lim)
		_, asDeleted := st.CreateMemoryEntry(ctx, memEntry("alice", "m7", path, store.MemoryDeleted), lim)

		// Assert
		for what, err := range map[string]error{"active": againActive, "pending": againPending, "onto pending": ontoPending} {
			if !errors.Is(err, store.ErrMemoryPathExists) {
				t.Fatalf("%s: %v, want ErrMemoryPathExists", what, err)
			}
		}
		if otherUser != nil || asDeleted != nil {
			t.Fatalf("another user's entry: %v, a deleted entry: %v; want both accepted", otherUser, asDeleted)
		}
	})

	memRun(t, f, "a soft-deleted entry frees its path", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		const path = "memories/semantic/notes/a.md"
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "m1", path, store.MemoryActive)), memAt(5))

		// Act
		created, err := st.CreateMemoryEntry(ctx, memEntry("alice", "m2", path, store.MemoryActive), store.MemoryLimits{})

		// Assert
		if err != nil || created.ID != "m2" {
			t.Fatalf("create on a freed path: %v, %v", created, err)
		}
	})

	memRun(t, f, "create refuses an id that exists, whoever owns it", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))

		// Act
		_, same := st.CreateMemoryEntry(ctx, memEntry("alice", "m1", "memories/semantic/notes/b.md", store.MemoryActive), store.MemoryLimits{})
		_, other := st.CreateMemoryEntry(ctx, memEntry("bob", "m1", "memories/semantic/notes/b.md", store.MemoryActive), store.MemoryLimits{})

		// Assert
		if !store.IsDataError(same) || !store.IsDataError(other) {
			t.Fatalf("same owner: %v, other owner: %v; want a permanent data error", same, other)
		}
		if got, _ := st.GetMemoryEntry(ctx, "alice", "m1"); got == nil || got.Path != "memories/semantic/notes/a.md" {
			t.Fatalf("the existing entry was changed: %+v", got)
		}
	})

	for name, mutate := range map[string]func(e *store.MemoryEntry){
		"an unknown status":     func(e *store.MemoryEntry) { e.Status = "archived" },
		"an empty path":         func(e *store.MemoryEntry) { e.Path = "" },
		"a path over 512 chars": func(e *store.MemoryEntry) { e.Path = strings.Repeat("a", 513) },
		"an importance below 0": func(e *store.MemoryEntry) { v := -0.1; e.Importance = &v },
		"an importance above 1": func(e *store.MemoryEntry) { v := 1.1; e.Importance = &v },
		"a path of 513 runes":   func(e *store.MemoryEntry) { e.Path = strings.Repeat("é", 513) },
	} {
		memRun(t, f, "create rejects "+name+" and stores nothing", func(t *testing.T, b Backend) {
			// Arrange
			ctx := context.Background()
			st := b.Store
			e := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
			mutate(e)

			// Act
			_, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})

			// Assert
			if !store.IsDataError(err) {
				t.Fatalf("err = %v, want a permanent data error", err)
			}
			if all, _ := st.ListMemoryEntries(ctx, "alice"); len(all) != 0 {
				t.Fatalf("stored %v", memIDs(all))
			}
		})
	}

	memRun(t, f, "create accepts the boundary path lengths and importances", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		zero, one := 0.0, 1.0
		cases := []*store.MemoryEntry{
			memEntry("alice", "m1", "a", store.MemoryActive),
			memEntry("alice", "m2", strings.Repeat("é", 512), store.MemoryActive),
			memEntry("alice", "m3", "memories/semantic/notes/zero.md", store.MemoryActive),
			memEntry("alice", "m4", "memories/semantic/notes/one.md", store.MemoryActive),
		}
		cases[2].Importance, cases[3].Importance = &zero, &one

		// Act
		var errs []error
		for _, e := range cases {
			_, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})
			errs = append(errs, err)
		}

		// Assert
		for i, err := range errs {
			if err != nil {
				t.Fatalf("case %d: %v", i, err)
			}
		}
	})

	memRun(t, f, "create sanitizes NUL and invalid UTF-8 like Postgres", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/notes/n\x00.md", store.MemoryActive)
		e.Title, e.Content, e.Tags = "t\x00itle", "a\x00b\xffc", []string{"x\x00"}
		e.Metadata = map[string]any{"k\x00": "v\x00", "arr": []any{"\x00", "ok"}, "nested": map[string]any{"deep": "d\xff"}}
		e.Proposal = &store.MemoryProposal{Path: "memories/semantic/notes/p\x00.md", Title: "p\x00", Content: "c\xff", Tags: []string{"q\x00"},
			Metadata: map[string]any{"m\x00": "n"}, ContentHash: "ph", ProposedAt: memAt(1)}

		// Act
		created, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{})
		got, _ := st.GetMemoryEntry(ctx, "alice", "m1")
		byPath, byPathErr := st.GetMemoryEntryByPath(ctx, "alice", "memories/semantic/notes/n\ufffd.md")

		// Assert
		if err != nil || byPathErr != nil {
			t.Fatalf("create: %v, by path: %v", err, byPathErr)
		}
		const r = "\ufffd"
		for what, x := range map[string]*store.MemoryEntry{"created": created, "read": got, "by path": byPath} {
			if x.Path != "memories/semantic/notes/n"+r+".md" || x.Title != "t"+r+"itle" || x.Content != "a"+r+"b"+r+"c" || !reflect.DeepEqual(x.Tags, []string{"x" + r}) {
				t.Fatalf("%s: text fields = %q %q %q %q", what, x.Path, x.Title, x.Content, x.Tags)
			}
			wantMeta := map[string]any{"k" + r: "v" + r, "arr": []any{r, "ok"}, "nested": map[string]any{"deep": "d" + r}}
			if !reflect.DeepEqual(x.Metadata, wantMeta) {
				t.Fatalf("%s: metadata = %#v, want %#v", what, x.Metadata, wantMeta)
			}
			p := x.Proposal
			if p == nil || p.Path != "memories/semantic/notes/p"+r+".md" || p.Title != "p"+r || p.Content != "c"+r || !reflect.DeepEqual(p.Tags, []string{"q" + r}) ||
				!reflect.DeepEqual(p.Metadata, map[string]any{"m" + r: "n"}) {
				t.Fatalf("%s: proposal = %+v", what, p)
			}
		}
	})
}

func memoryUpdate(t *testing.T, f Factory) {
	memRun(t, f, "update replaces the mutable fields, bumps the version and keeps the immutable ones", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		e := memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)
		e.SourceThreadID = "0b6f6d1e-5d3f-4a0a-9a52-0c1d5f3a7b11"
		created := memCreate(t, st, e)
		upd := created.Clone()
		upd.Path, upd.Title, upd.Content, upd.Tags = "memories/semantic/facts/b.md", "New title", "New content", []string{"fact", "x"}
		upd.Importance, upd.Metadata, upd.ContentHash = nil, map[string]any{"pinned": true, "n": 2}, "hash-new"
		upd.UpdatedAt, upd.ContentUpdatedAt = memAt(5), memAt(4)
		upd.Scope, upd.Repository, upd.Source, upd.SourceThreadID, upd.CreatedAt = "shared", "other", store.MemorySourceUser, "", memAt(99)

		// Act
		updated, err := st.UpdateMemoryEntry(ctx, upd, store.MemoryLimits{})
		got, getErr := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		if err != nil || getErr != nil {
			t.Fatalf("update: %v, get: %v", err, getErr)
		}
		want := created.Clone()
		want.Path, want.Title, want.Content, want.Tags = upd.Path, upd.Title, upd.Content, upd.Tags
		want.Importance, want.Metadata, want.ContentHash = nil, map[string]any{"pinned": true, "n": float64(2)}, "hash-new"
		want.UpdatedAt, want.ContentUpdatedAt, want.Version = memAt(5), memAt(4), 1
		memSameEntry(t, "updated", updated, want)
		memSameEntry(t, "read back", got, want)
		if created.Version != 0 {
			t.Fatalf("the entry returned by create changed: version %d", created.Version)
		}
		if byPath, err := st.GetMemoryEntryByPath(ctx, "alice", "memories/semantic/facts/b.md"); err != nil || byPath.ID != "m1" {
			t.Fatalf("by the new path: %v, %v", byPath, err)
		}
		if _, err := st.GetMemoryEntryByPath(ctx, "alice", "memories/semantic/notes/a.md"); !memNotFound(err) {
			t.Fatalf("by the old path: %v, want ErrNotFound", err)
		}
	})

	memRun(t, f, "update is guarded by the version that was read", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		stale := created.Clone()
		first := created.Clone()
		first.Content, first.UpdatedAt = "first", memAt(1)
		stale.Content, stale.UpdatedAt = "stale", memAt(2)

		// Act
		one, oneErr := st.UpdateMemoryEntry(ctx, first, store.MemoryLimits{})
		_, staleErr := st.UpdateMemoryEntry(ctx, stale, store.MemoryLimits{})
		next := one.Clone()
		next.Content, next.UpdatedAt = "second", memAt(3)
		two, twoErr := st.UpdateMemoryEntry(ctx, next, store.MemoryLimits{})
		ahead := two.Clone()
		ahead.Version += 5
		_, aheadErr := st.UpdateMemoryEntry(ctx, ahead, store.MemoryLimits{})
		got, _ := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		if oneErr != nil || twoErr != nil || one.Version != 1 || two.Version != 2 {
			t.Fatalf("sequential updates: %v %v, versions %d %d", oneErr, twoErr, one.Version, two.Version)
		}
		if !errors.Is(staleErr, store.ErrMemoryConflict) || !errors.Is(aheadErr, store.ErrMemoryConflict) {
			t.Fatalf("stale: %v, ahead: %v; want ErrMemoryConflict", staleErr, aheadErr)
		}
		if got.Content != "second" || got.Version != 2 {
			t.Fatalf("a refused write was applied: %+v", got)
		}
	})

	memRun(t, f, "update of a missing entry or of another user's entry is not found", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		missing := created.Clone()
		missing.ID = "nope"
		foreign := created.Clone()
		foreign.UserLogin, foreign.Content = "bob", "hijacked"

		// Act
		_, missingErr := st.UpdateMemoryEntry(ctx, missing, store.MemoryLimits{})
		_, foreignErr := st.UpdateMemoryEntry(ctx, foreign, store.MemoryLimits{})
		got, _ := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		if !memNotFound(missingErr) || !memNotFound(foreignErr) {
			t.Fatalf("missing: %v, foreign: %v; want ErrNotFound", missingErr, foreignErr)
		}
		if got.Content != created.Content || got.Version != 0 {
			t.Fatalf("the entry changed: %+v", got)
		}
	})

	memRun(t, f, "update refuses a path another live entry has, but not its own", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		a := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		other := memCreate(t, st, memEntry("alice", "m2", "memories/semantic/notes/b.md", store.MemoryPending))
		bobs := memCreate(t, st, memEntry("bob", "m3", "memories/semantic/notes/c.md", store.MemoryActive))
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "m4", "memories/semantic/notes/old.md", store.MemoryActive)), memAt(5))
		rename := func(e *store.MemoryEntry, path string) error {
			c := mustGetMem(t, st, e.UserLogin, e.ID)
			c.Path = path
			_, err := st.UpdateMemoryEntry(ctx, c, store.MemoryLimits{})
			return err
		}

		// Act
		ontoPending := rename(a, other.Path)
		onto := rename(other, a.Path)
		ontoBobs := rename(a, bobs.Path)
		ontoDeleted := rename(a, "memories/semantic/notes/old.md")
		self := rename(bobs, bobs.Path)

		// Assert
		if !errors.Is(ontoPending, store.ErrMemoryPathExists) || !errors.Is(onto, store.ErrMemoryPathExists) {
			t.Fatalf("onto another live path: %v, %v; want ErrMemoryPathExists", ontoPending, onto)
		}
		if ontoBobs != nil || ontoDeleted != nil || self != nil {
			t.Fatalf("another user's path: %v, a deleted entry's path: %v, its own path: %v; want all accepted", ontoBobs, ontoDeleted, self)
		}
	})

	memRun(t, f, "update checks the limit of the new status only when the status changes", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		lim := store.MemoryLimits{Active: 1, Pending: 1}
		active := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		pending := memCreate(t, st, memEntry("alice", "m2", "memories/semantic/notes/b.md", store.MemoryPending))
		deleted := memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "m3", "memories/semantic/notes/c.md", store.MemoryActive)), memAt(5))
		move := func(e *store.MemoryEntry, status string) (*store.MemoryEntry, error) {
			c := mustGetMem(t, st, e.UserLogin, e.ID)
			c.Status, c.DeletedAt, c.UpdatedAt = status, nil, memAt(7)
			if status == store.MemoryDeleted {
				at := memAt(7)
				c.DeletedAt = &at
			}
			return st.UpdateMemoryEntry(ctx, c, lim)
		}

		// Act
		_, confirm := move(pending, store.MemoryActive)
		_, restore := move(deleted, store.MemoryActive)
		_, restoreAsPending := move(deleted, store.MemoryPending)
		sameStatus, sameErr := move(active, store.MemoryActive)
		_, demote := move(active, store.MemoryPending)
		freed, freedErr := move(active, store.MemoryDeleted)
		_, confirmAfter := move(pending, store.MemoryActive)

		// Assert
		for what, err := range map[string]error{"confirm a pending entry": confirm, "restore a deleted entry": restore, "restore as pending": restoreAsPending,
			"demote to pending": demote} {
			if !errors.Is(err, store.ErrMemoryLimit) {
				t.Fatalf("%s: %v, want ErrMemoryLimit", what, err)
			}
		}
		if sameErr != nil || sameStatus.Version != 1 {
			t.Fatalf("update at the limit without a status change: %v, %+v", sameErr, sameStatus)
		}
		if freedErr != nil || freed.Status != store.MemoryDeleted {
			t.Fatalf("delete: %v, %+v", freedErr, freed)
		}
		if confirmAfter != nil {
			t.Fatalf("confirm once the active slot is free: %v", confirmAfter)
		}
		if p, _ := st.GetMemoryEntry(ctx, "alice", "m2"); p.Status != store.MemoryActive {
			t.Fatalf("pending entry status = %s, want active", p.Status)
		}
		if d, _ := st.GetMemoryEntry(ctx, "alice", "m3"); d.Status != store.MemoryDeleted {
			t.Fatalf("a refused restore changed the entry: %+v", d)
		}
	})

	memRun(t, f, "soft delete and restore through update", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))

		// Act
		deleted := memSoftDelete(t, st, created, memAt(10))
		listed, _ := st.ListMemoryEntries(ctx, "alice", store.MemoryActive)
		restore := deleted.Clone()
		restore.Status, restore.DeletedAt, restore.UpdatedAt = store.MemoryActive, nil, memAt(20)
		restored, err := st.UpdateMemoryEntry(ctx, restore, store.MemoryLimits{})

		// Assert
		if deleted.Status != store.MemoryDeleted || deleted.DeletedAt == nil || !deleted.DeletedAt.Equal(memAt(10)) || deleted.Version != 1 {
			t.Fatalf("deleted = %+v", deleted)
		}
		if len(listed) != 0 {
			t.Fatalf("active list holds the deleted entry: %v", memIDs(listed))
		}
		if err != nil || restored.Status != store.MemoryActive || restored.DeletedAt != nil || restored.Version != 2 {
			t.Fatalf("restored = %+v, %v", restored, err)
		}
	})

	memRun(t, f, "update stores and clears a proposal", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		imp := 0.9
		proposal := &store.MemoryProposal{Path: created.Path, Title: "New", Content: "New content", Tags: []string{"fact"}, Importance: &imp,
			Metadata: map[string]any{"trigger": "stale_correction", "n": 1}, ContentHash: "hp", ThreadID: "t1", ProposedAt: memAt(3)}
		withProposal := created.Clone()
		withProposal.Proposal = proposal

		// Act
		set, setErr := st.UpdateMemoryEntry(ctx, withProposal, store.MemoryLimits{})
		got, _ := st.GetMemoryEntry(ctx, "alice", "m1")
		clear := got.Clone()
		clear.Proposal = nil
		cleared, clearErr := st.UpdateMemoryEntry(ctx, clear, store.MemoryLimits{})

		// Assert
		if setErr != nil || clearErr != nil {
			t.Fatalf("set: %v, clear: %v", setErr, clearErr)
		}
		want := created.Clone()
		want.Version = 1
		want.Proposal = proposal.Clone()
		want.Proposal.Metadata = map[string]any{"trigger": "stale_correction", "n": float64(1)}
		memSameEntry(t, "with proposal", set, want)
		memSameEntry(t, "with proposal, read back", got, want)
		if set.Content != created.Content {
			t.Fatalf("the proposal replaced the confirmed content: %q", set.Content)
		}
		if cleared.Proposal != nil || cleared.Version != 2 {
			t.Fatalf("cleared = %+v", cleared)
		}
	})

	memRun(t, f, "a proposal without tags, metadata or importance reads back empty", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		upd := created.Clone()
		upd.Proposal = &store.MemoryProposal{Path: created.Path, Title: "T", Content: "C", ProposedAt: memAt(1)}

		// Act
		updated, err := st.UpdateMemoryEntry(ctx, upd, store.MemoryLimits{})

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		p := updated.Proposal
		if p == nil || p.Tags == nil || len(p.Tags) != 0 || p.Metadata == nil || len(p.Metadata) != 0 || p.Importance != nil {
			t.Fatalf("proposal = %+v", p)
		}
	})

	for name, mutate := range map[string]func(e *store.MemoryEntry){
		"an unknown status":     func(e *store.MemoryEntry) { e.Status = "archived" },
		"an empty path":         func(e *store.MemoryEntry) { e.Path = "" },
		"a path over 512 chars": func(e *store.MemoryEntry) { e.Path = strings.Repeat("a", 513) },
		"an importance over 1":  func(e *store.MemoryEntry) { v := 2.0; e.Importance = &v },
	} {
		memRun(t, f, "update rejects "+name+" and leaves the entry unchanged", func(t *testing.T, b Backend) {
			// Arrange
			ctx := context.Background()
			st := b.Store
			created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
			upd := created.Clone()
			upd.Content = "changed"
			mutate(upd)

			// Act
			_, err := st.UpdateMemoryEntry(ctx, upd, store.MemoryLimits{})

			// Assert
			if !store.IsDataError(err) {
				t.Fatalf("err = %v, want a permanent data error", err)
			}
			got, _ := st.GetMemoryEntry(ctx, "alice", "m1")
			memSameEntry(t, "after the refused update", got, created)
		})
	}

	memRun(t, f, "update sanitizes NUL and invalid UTF-8 like Postgres", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		created := memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		upd := created.Clone()
		upd.Path, upd.Title, upd.Content, upd.Tags = "memories/semantic/notes/b\x00.md", "t\x00", "c\xff", []string{"\x00"}
		upd.Metadata = map[string]any{"k": "v\x00"}

		// Act
		updated, err := st.UpdateMemoryEntry(ctx, upd, store.MemoryLimits{})

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		const r = "\ufffd"
		if updated.Path != "memories/semantic/notes/b"+r+".md" || updated.Title != "t"+r || updated.Content != "c"+r ||
			!reflect.DeepEqual(updated.Tags, []string{r}) || !reflect.DeepEqual(updated.Metadata, map[string]any{"k": "v" + r}) {
			t.Fatalf("updated = %+v", updated)
		}
	})
}

func memoryDelete(t *testing.T, f Factory) {
	memRun(t, f, "delete removes the entry for good and frees its path", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		const path = "memories/semantic/notes/a.md"
		memCreate(t, st, memEntry("alice", "m1", path, store.MemoryActive))
		memCreate(t, st, memEntry("alice", "m2", "memories/semantic/notes/b.md", store.MemoryActive))

		// Act
		err := st.DeleteMemoryEntry(ctx, "alice", "m1", "")
		_, getErr := st.GetMemoryEntry(ctx, "alice", "m1")
		_, pathErr := st.GetMemoryEntryByPath(ctx, "alice", path)
		again := st.DeleteMemoryEntry(ctx, "alice", "m1", "")
		_, recreate := st.CreateMemoryEntry(ctx, memEntry("alice", "m1", path, store.MemoryActive), store.MemoryLimits{})
		rest, _ := st.ListMemoryEntries(ctx, "alice")

		// Assert
		if err != nil || !memNotFound(getErr) || !memNotFound(pathErr) {
			t.Fatalf("delete: %v, get: %v, by path: %v", err, getErr, pathErr)
		}
		if !memNotFound(again) {
			t.Fatalf("second delete: %v, want ErrNotFound", again)
		}
		if recreate != nil || len(rest) != 2 {
			t.Fatalf("recreate the same id and path: %v; entries %v", recreate, memIDs(rest))
		}
	})

	memRun(t, f, "delete does not reach another user's entry nor a missing one", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))

		// Act
		foreign := st.DeleteMemoryEntry(ctx, "bob", "m1", "")
		foreignGuarded := st.DeleteMemoryEntry(ctx, "bob", "m1", store.MemoryActive)
		missing := st.DeleteMemoryEntry(ctx, "alice", "nope", "")
		missingGuarded := st.DeleteMemoryEntry(ctx, "alice", "nope", store.MemoryActive)

		// Assert
		for what, err := range map[string]error{"another user's": foreign, "another user's, guarded": foreignGuarded, "missing": missing, "missing, guarded": missingGuarded} {
			if !memNotFound(err) {
				t.Fatalf("%s: %v, want ErrNotFound", what, err)
			}
		}
		if _, err := st.GetMemoryEntry(ctx, "alice", "m1"); err != nil {
			t.Fatalf("the entry was deleted: %v", err)
		}
	})

	memRun(t, f, "delete with an expected status only removes an entry that still has it", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "m2", "memories/semantic/notes/b.md", store.MemoryPending))
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "m3", "memories/semantic/notes/c.md", store.MemoryActive)), memAt(5))

		// Act
		wrongPending := st.DeleteMemoryEntry(ctx, "alice", "m1", store.MemoryPending)
		wrongActive := st.DeleteMemoryEntry(ctx, "alice", "m2", store.MemoryActive)
		okPending := st.DeleteMemoryEntry(ctx, "alice", "m2", store.MemoryPending)
		okDeleted := st.DeleteMemoryEntry(ctx, "alice", "m3", store.MemoryDeleted)
		rest, _ := st.ListMemoryEntries(ctx, "alice")

		// Assert
		if !errors.Is(wrongPending, store.ErrMemoryConflict) || !errors.Is(wrongActive, store.ErrMemoryConflict) {
			t.Fatalf("wrong status: %v, %v; want ErrMemoryConflict", wrongPending, wrongActive)
		}
		if okPending != nil || okDeleted != nil {
			t.Fatalf("right status: %v, %v", okPending, okDeleted)
		}
		if got := memIDs(rest); len(got) != 1 || got[0] != "m1" {
			t.Fatalf("entries left = %v, want [m1]", got)
		}
	})
}

func memoryUsersAndPurge(t *testing.T, f Factory) {
	memRun(t, f, "lists the users with active entries, sorted", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		memCreate(t, st, memEntry("carol", "c1", "memories/semantic/notes/c1.md", store.MemoryActive))
		memCreate(t, st, memEntry("carol", "c2", "memories/semantic/notes/c2.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "a1", "memories/semantic/notes/a1.md", store.MemoryActive))
		memCreate(t, st, memEntry("bob", "b1", "memories/semantic/notes/b1.md", store.MemoryPending))
		memSoftDelete(t, st, memCreate(t, st, memEntry("dave", "d1", "memories/semantic/notes/d1.md", store.MemoryActive)), memAt(5))

		// Act
		users, err := st.ListMemoryUsers(ctx)
		memSoftDelete(t, st, mustGetMem(t, st, "carol", "c1"), memAt(6))
		stillCarol, _ := st.ListMemoryUsers(ctx)
		memSoftDelete(t, st, mustGetMem(t, st, "carol", "c2"), memAt(7))
		noCarol, _ := st.ListMemoryUsers(ctx)

		// Assert
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(users, []string{"alice", "carol"}) || !reflect.DeepEqual(stillCarol, []string{"alice", "carol"}) || !reflect.DeepEqual(noCarol, []string{"alice"}) {
			t.Fatalf("users = %v, then %v, then %v", users, stillCarol, noCarol)
		}
	})

	memRun(t, f, "no user has active entries on an empty store", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()

		// Act
		users, err := b.Store.ListMemoryUsers(ctx)

		// Assert
		if err != nil || len(users) != 0 {
			t.Fatalf("users = %v, %v", users, err)
		}
	})

	memRun(t, f, "purge removes the entries soft-deleted before the cutoff and nothing else", func(t *testing.T, b Backend) {
		// Arrange: a deleted entry before the cutoff, one exactly at it, one after, old live ones and another user's old one.
		ctx := context.Background()
		st := b.Store
		cutoff := memAt(100)
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "old", "memories/semantic/notes/old.md", store.MemoryActive)), memAt(50))
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "edge", "memories/semantic/notes/edge.md", store.MemoryActive)), cutoff)
		memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "new", "memories/semantic/notes/new.md", store.MemoryActive)), memAt(150))
		memSoftDelete(t, st, memCreate(t, st, memEntry("bob", "bobold", "memories/semantic/notes/old.md", store.MemoryActive)), memAt(10))
		memCreate(t, st, memEntry("alice", "live", "memories/semantic/notes/live.md", store.MemoryActive))
		memCreate(t, st, memEntry("alice", "pending", "memories/semantic/notes/pending.md", store.MemoryPending))

		// Act
		n, err := st.PurgeDeletedMemoryEntries(ctx, cutoff)
		again, againErr := st.PurgeDeletedMemoryEntries(ctx, cutoff)
		later, laterErr := st.PurgeDeletedMemoryEntries(ctx, memAt(1000))

		// Assert
		if err != nil || againErr != nil || laterErr != nil {
			t.Fatalf("errors: %v %v %v", err, againErr, laterErr)
		}
		if n != 2 || again != 0 || later != 2 {
			t.Fatalf("purged %d then %d then %d, want 2, 0, 2 (edge and new go with the later cutoff)", n, again, later)
		}
		rest, _ := st.ListMemoryEntries(ctx, "alice")
		if got := memIDs(rest); len(got) != 2 {
			t.Fatalf("alice has %v, want live and pending", got)
		}
		if _, err := st.GetMemoryEntry(ctx, "bob", "bobold"); !memNotFound(err) {
			t.Fatalf("another user's purged entry: %v", err)
		}
	})

	memRun(t, f, "purge keeps what is not yet old enough", func(t *testing.T, b Backend) {
		// Arrange
		ctx := context.Background()
		st := b.Store
		deleted := memSoftDelete(t, st, memCreate(t, st, memEntry("alice", "m1", "memories/semantic/notes/a.md", store.MemoryActive)), memAt(50))

		// Act
		n, err := st.PurgeDeletedMemoryEntries(ctx, memAt(40))
		got, getErr := st.GetMemoryEntry(ctx, "alice", "m1")

		// Assert
		if err != nil || n != 0 || getErr != nil {
			t.Fatalf("purged %d, %v; get %v", n, err, getErr)
		}
		memSameEntry(t, "entry", got, deleted)
	})
}

func mustGetMem(t *testing.T, st Store, login, id string) *store.MemoryEntry {
	t.Helper()
	e, err := st.GetMemoryEntry(context.Background(), login, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return e
}
