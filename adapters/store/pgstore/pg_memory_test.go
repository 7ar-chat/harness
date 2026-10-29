package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
)

func memEntry(id, login, path, status string) *store.MemoryEntry {
	imp := 0.5
	return &store.MemoryEntry{ID: id, UserLogin: login, Path: path, Title: "T", Content: "Prefers French", Tags: []string{"preference"},
		Importance: &imp, Metadata: map[string]any{"trigger": "durable_preference", "nested": map[string]any{"a": "b"}}, Scope: "personal",
		Repository: "default", Source: store.MemorySourceProxy, Status: status, ContentHash: "h"}
}

func TestPGMemoryEntriesRoundTripAndEnforceTheirConstraints(t *testing.T) {
	// Entries round-trip (tags, importance, metadata), are per user, one live entry per path, limited per user; hard delete and purge remove rows.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")
	a := memEntry("mem_a", "alice", "memories/semantic/preferences/a.md", store.MemoryActive)
	a.SourceThreadID = th.ID

	// Act
	lim := store.MemoryLimits{Active: 2, Pending: 1}
	created, err := s.CreateMemoryEntry(ctx, a, lim)
	_, clash := s.CreateMemoryEntry(ctx, memEntry("mem_b", "alice", a.Path, store.MemoryActive), lim)
	_, bobErr := s.CreateMemoryEntry(ctx, memEntry("mem_c", "bob", a.Path, store.MemoryActive), lim)
	_, _ = s.CreateMemoryEntry(ctx, memEntry("mem_d", "alice", "memories/semantic/notes/d.md", store.MemoryPending), lim)
	_, pendingLimit := s.CreateMemoryEntry(ctx, memEntry("mem_g", "alice", "memories/semantic/notes/g.md", store.MemoryPending), lim)
	_, secondActive := s.CreateMemoryEntry(ctx, memEntry("mem_h", "alice", "memories/semantic/notes/h.md", store.MemoryActive), lim)
	_, limit := s.CreateMemoryEntry(ctx, memEntry("mem_e", "alice", "memories/semantic/notes/e.md", store.MemoryActive), lim)

	// Assert
	if err != nil || created.SourceThreadID != th.ID || created.Importance == nil || *created.Importance != 0.5 || len(created.Tags) != 1 ||
		created.Metadata["nested"].(map[string]any)["a"] != "b" || created.CreatedAt.IsZero() {
		t.Fatalf("created %+v %v", created, err)
	}
	if !errors.Is(clash, store.ErrMemoryPathExists) || bobErr != nil || !errors.Is(limit, store.ErrMemoryLimit) ||
		!errors.Is(pendingLimit, store.ErrMemoryLimit) || secondActive != nil {
		t.Fatalf("clash %v bob %v limit %v pending %v second active %v", clash, bobErr, limit, pendingLimit, secondActive)
	}
	if _, err := s.GetMemoryEntry(ctx, "bob", "mem_a"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob read alice's entry: %v", err)
	}
	pending, _ := s.ListMemoryEntries(ctx, "alice", store.MemoryPending)
	all, _ := s.ListMemoryEntries(ctx, "alice")
	if len(pending) != 1 || pending[0].ID != "mem_d" || len(all) != 3 || all[0].ID != "mem_a" {
		t.Fatalf("pending %+v all %d", pending, len(all))
	}

	// Soft delete frees the path; by-path lookups prefer the live entry; purge removes old deleted rows.
	now := time.Now()
	gone := created.Clone()
	gone.Status, gone.DeletedAt, gone.UpdatedAt = store.MemoryDeleted, &now, now
	if _, err := s.UpdateMemoryEntry(ctx, gone, store.MemoryLimits{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMemoryEntry(ctx, memEntry("mem_f", "alice", a.Path, store.MemoryActive), store.MemoryLimits{}); err != nil {
		t.Fatalf("path not freed by the soft delete: %v", err)
	}
	if byPath, _ := s.GetMemoryEntryByPath(ctx, "alice", a.Path); byPath.ID != "mem_f" {
		t.Fatalf("by path %+v", byPath)
	}
	if n, err := s.PurgeDeletedMemoryEntries(ctx, now.Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
	if err := s.DeleteMemoryEntry(ctx, "alice", "mem_f", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMemoryEntry(ctx, "alice", "mem_f", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete %v", err)
	}
	_ = s.DeleteMemoryEntry(ctx, "alice", "mem_h", "")
	users, _ := s.ListMemoryUsers(ctx)
	if len(users) != 1 || users[0] != "bob" {
		t.Fatalf("users with active entries %v", users)
	}
}

func TestPGMemorySettingDefaultsToEnabled(t *testing.T) {
	// Memory is on until the user turns it off, per user.
	// Arrange
	s := newPG(t)
	ctx := context.Background()

	// Act
	before, _ := s.MemoryEnabled(ctx, "alice")
	_ = s.SetMemoryEnabled(ctx, "alice", false)
	after, _ := s.MemoryEnabled(ctx, "alice")
	bob, _ := s.MemoryEnabled(ctx, "bob")

	// Assert
	if !before || after || !bob {
		t.Fatalf("before %v after %v bob %v", before, after, bob)
	}
}

func TestPGMemoryEntryRejectsAnOutOfRangeImportance(t *testing.T) {
	// The CHECK constraint keeps importance in [0, 1].
	// Arrange
	s := newPG(t)
	e := memEntry("mem_x", "alice", "memories/semantic/notes/x.md", store.MemoryActive)
	bad := 2.0
	e.Importance = &bad

	// Act
	_, err := s.CreateMemoryEntry(context.Background(), e, store.MemoryLimits{})

	// Assert
	if err == nil {
		t.Fatal("importance 2 accepted")
	}
}

func TestPGMemoryUpdateIsGuardedByTheVersionRead(t *testing.T) {
	// An update applies only to the version it read and bumps it: a stale write (e.g. the consolidation job's) fails with ErrMemoryConflict instead of overwriting or resurrecting the entry.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	created, err := s.CreateMemoryEntry(ctx, memEntry("mem_v", "alice", "memories/semantic/facts.md", store.MemoryActive), store.MemoryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	stale := created.Clone()
	deleted := created.Clone()
	now := time.Now()
	deleted.Status, deleted.DeletedAt, deleted.UpdatedAt = store.MemoryDeleted, &now, now

	// Act
	first, firstErr := s.UpdateMemoryEntry(ctx, deleted, store.MemoryLimits{})
	stale.Content = "rewritten"
	_, staleErr := s.UpdateMemoryEntry(ctx, stale, store.MemoryLimits{})
	missing := created.Clone()
	missing.ID = "mem_none"
	_, missingErr := s.UpdateMemoryEntry(ctx, missing, store.MemoryLimits{})

	// Assert
	if firstErr != nil || created.Version != 0 || first.Version != 1 {
		t.Fatalf("first %+v %v", first, firstErr)
	}
	if !errors.Is(staleErr, store.ErrMemoryConflict) || !errors.Is(missingErr, store.ErrNotFound) {
		t.Fatalf("stale %v missing %v", staleErr, missingErr)
	}
	if got, _ := s.GetMemoryEntry(ctx, "alice", "mem_v"); got.Status != store.MemoryDeleted || got.Content != "Prefers French" {
		t.Fatalf("stale write applied: %+v", got)
	}
}

func TestPGMemoryProposalRoundTripsAndGuardedDeleteChecksTheStatus(t *testing.T) {
	// The proposal of an entry round-trips (content, tags, importance, metadata, hash, thread, date) and clears; a hard delete expecting pending fails with ErrMemoryConflict on an active entry.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	e, err := s.CreateMemoryEntry(ctx, memEntry("mem_p", "alice", "memories/semantic/notes/p.md", store.MemoryActive), store.MemoryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	imp := 0.9
	at := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	e.Proposal = &store.MemoryProposal{Path: e.Path, Title: "New", Content: "New content", Tags: []string{"fact"}, Importance: &imp,
		Metadata: map[string]any{"trigger": "stale_correction"}, ContentHash: "hp", ThreadID: "t1", ProposedAt: at}

	// Act
	withProposal, err := s.UpdateMemoryEntry(ctx, e, store.MemoryLimits{})
	read, _ := s.GetMemoryEntry(ctx, "alice", "mem_p")
	read.Proposal = nil
	cleared, clearErr := s.UpdateMemoryEntry(ctx, read, store.MemoryLimits{})
	guarded := s.DeleteMemoryEntry(ctx, "alice", "mem_p", store.MemoryPending)
	missing := s.DeleteMemoryEntry(ctx, "alice", "mem_none", store.MemoryPending)

	// Assert
	p := withProposal.Proposal
	if err != nil || p == nil || p.Content != "New content" || p.Title != "New" || len(p.Tags) != 1 || p.Importance == nil || *p.Importance != 0.9 ||
		p.Metadata["trigger"] != "stale_correction" || p.ContentHash != "hp" || p.ThreadID != "t1" || !p.ProposedAt.Equal(at) ||
		withProposal.Content != "Prefers French" {
		t.Fatalf("proposal %+v %v", p, err)
	}
	if clearErr != nil || cleared.Proposal != nil {
		t.Fatalf("cleared %+v %v", cleared, clearErr)
	}
	if !errors.Is(guarded, store.ErrMemoryConflict) || !errors.Is(missing, store.ErrNotFound) {
		t.Fatalf("guarded %v missing %v", guarded, missing)
	}
	if _, err := s.GetMemoryEntry(ctx, "alice", "mem_p"); err != nil {
		t.Fatalf("active entry deleted: %v", err)
	}
}

func TestPGMemoryStatusChangesCheckTheLimitUnderTheLock(t *testing.T) {
	// Restoring or confirming an entry checks the limit of its new status in the same transaction as the write: concurrent restores cannot overshoot it.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	lim := store.MemoryLimits{Active: 1, Pending: 10}
	now := time.Now()
	var deleted []*store.MemoryEntry
	for _, id := range []string{"mem_1", "mem_2", "mem_3", "mem_4"} {
		e, err := s.CreateMemoryEntry(ctx, memEntry(id, "alice", "memories/semantic/notes/"+id+".md", store.MemoryPending), lim)
		if err != nil {
			t.Fatal(err)
		}
		e.Status, e.DeletedAt = store.MemoryDeleted, &now
		if e, err = s.UpdateMemoryEntry(ctx, e, lim); err != nil {
			t.Fatal(err)
		}
		deleted = append(deleted, e)
	}

	// Act
	errs := make(chan error, len(deleted))
	for _, e := range deleted {
		go func(e *store.MemoryEntry) {
			e = e.Clone()
			e.Status, e.DeletedAt = store.MemoryActive, nil
			_, err := s.UpdateMemoryEntry(ctx, e, lim)
			errs <- err
		}(e)
	}
	restored, limited := 0, 0
	for range deleted {
		switch err := <-errs; {
		case err == nil:
			restored++
		case errors.Is(err, store.ErrMemoryLimit):
			limited++
		default:
			t.Fatal(err)
		}
	}

	// Assert
	active, _ := s.ListMemoryEntries(ctx, "alice", store.MemoryActive)
	if restored != 1 || limited != 3 || len(active) != 1 {
		t.Fatalf("restored %d limited %d active %d", restored, limited, len(active))
	}
}
