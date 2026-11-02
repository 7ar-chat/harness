package filestore

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

func memEntry(login, path, content string) *store.MemoryEntry {
	return &store.MemoryEntry{ID: "mem_" + path, UserLogin: login, Path: path, Title: path, Content: content, Tags: []string{"t"},
		Metadata: map[string]any{"pinned": true}, Scope: "personal", Repository: "default", Source: store.MemorySourceUser,
		Status: store.MemoryActive, ContentHash: "h" + content, CreatedAt: time.Now(), UpdatedAt: time.Now(), ContentUpdatedAt: time.Now()}
}

func TestMemoryEntriesSurviveARestartAndACompaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	lim := store.MemoryLimits{Active: 100, Pending: 100}
	a := must(s.CreateMemoryEntry(ctx, memEntry("project:p1", "a.md", "first"), lim))
	b := must(s.CreateMemoryEntry(ctx, memEntry("project:p1", "b.md", "second"), lim))
	c := must(s.CreateMemoryEntry(ctx, memEntry("alice", "c.md", "third"), lim))
	a.Content, a.UpdatedAt = "first, revised", time.Now()
	must(s.UpdateMemoryEntry(ctx, a, lim))
	ok(t, s.DeleteMemoryEntry(ctx, "project:p1", b.ID, ""))
	ok(t, s.SetMemoryEnabled(ctx, "alice", false))
	_ = c
	ok(t, s.Close())

	check := func(s *Store, what string) {
		t.Helper()
		es, _ := s.ListMemoryEntries(ctx, "project:p1")
		if len(es) != 1 || es[0].Content != "first, revised" || es[0].Metadata["pinned"] != true {
			t.Fatalf("%s: project entries = %+v", what, es)
		}
		if es, _ := s.ListMemoryEntries(ctx, "alice"); len(es) != 1 || es[0].Content != "third" {
			t.Fatalf("%s: alice entries = %+v", what, es)
		}
		if on, _ := s.MemoryEnabled(ctx, "alice"); on {
			t.Fatalf("%s: the disabled flag was lost", what)
		}
		if on, _ := s.MemoryEnabled(ctx, "bob"); !on {
			t.Fatalf("%s: memory should be on by default", what)
		}
	}
	s = open(t, dir)
	check(s, "after a restart")
	ok(t, s.compact())
	ok(t, s.Close())
	s = open(t, dir)
	check(s, "after a compaction")
	// Hard deletions made by the retention job leave the disk too.
	past := time.Now().Add(-48 * time.Hour)
	d := must(s.CreateMemoryEntry(ctx, memEntry("alice", "d.md", "old"), lim))
	d.Status, d.DeletedAt, d.UpdatedAt = store.MemoryDeleted, &past, time.Now()
	must(s.UpdateMemoryEntry(ctx, d, lim))
	if n, err := s.PurgeDeletedMemoryEntries(ctx, time.Now()); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v", n, err)
	}
	ok(t, s.Close())
	s = open(t, dir)
	if es, _ := s.ListMemoryEntries(ctx, "alice"); len(es) != 1 {
		t.Fatalf("a purged entry came back: %+v", es)
	}
	ok(t, s.Close())
}
