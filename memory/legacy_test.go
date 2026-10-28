package memory_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/memory"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

func legacyEntry(id, content, title string) *store.MemoryEntry {
	return &store.MemoryEntry{ID: id, UserLogin: "alice", Path: "memories/semantic/facts/2026-01-15-" + id + ".md", Title: title,
		Content: content, Tags: []string{"fact"}, Scope: "personal", Repository: "default", Source: store.MemorySourceLegacy,
		Status: store.MemoryActive, ContentHash: "raw", CreatedAt: now, UpdatedAt: now, ContentUpdatedAt: now,
		Metadata: map[string]any{"trigger": "explicit_remember", "migrated_from": "work_memories", "legacy_backfill": true}}
}

func TestBackfillLegacyRedactsAndDerivesTheTitleOfMigratedFacts(t *testing.T) {
	// Facts migrated by 0010 are redacted like a write today; their 120-character markdown title becomes DeriveTitle's, so later edits re-derive it; the flag goes and a second run does nothing.
	// Arrange
	st := memstore.New()
	ctx := context.Background()
	long := "## Deploy rules for the risk platform " + strings.Repeat("and more ", 30) + "token=abc123"
	migrated := legacyEntry("mem_a", long+"\nsecond line", "")
	migrated.Title = strings.TrimSpace(string([]rune(long)[:120]))
	custom := legacyEntry("mem_b", "Prefers French", "My password=hunter2 note")
	for _, e := range []*store.MemoryEntry{migrated, custom} {
		if _, err := st.CreateMemoryEntry(ctx, e, store.MemoryLimits{}); err != nil {
			t.Fatal(err)
		}
	}
	svc := memory.New(st, memory.Options{})

	// Act
	n, err := memory.BackfillLegacy(ctx, st)
	again, _ := memory.BackfillLegacy(ctx, st)
	a, _ := st.GetMemoryEntry(ctx, "alice", "mem_a")
	b, _ := st.GetMemoryEntry(ctx, "alice", "mem_b")
	edited := "Deploy on Mondays"
	upd := svc.Update(ctx, memory.Writer{Login: "alice"}, memory.UpdateInput{ID: "mem_a", Content: &edited})

	// Assert
	if err != nil || n != 2 || again != 0 {
		t.Fatalf("backfilled %d %v, again %d", n, err, again)
	}
	if strings.Contains(a.Content, "abc123") || !strings.Contains(a.Content, "token= [REDACTED]") || a.ContentHash == "raw" {
		t.Fatalf("content not redacted: %q %s", a.Content, a.ContentHash)
	}
	if a.Title != memory.DeriveTitle(a.Content) || strings.HasPrefix(a.Title, "#") || a.Metadata["legacy_backfill"] != nil ||
		a.Metadata["migrated_from"] != "work_memories" || !a.UpdatedAt.Equal(now) {
		t.Fatalf("migrated entry %+v", a)
	}
	if b.Title != "My password= [REDACTED] note" || b.Content != "Prefers French" {
		t.Fatalf("custom title %+v", b)
	}
	if upd.IsError || upd.Changes[0].Entry.Title != "Deploy on Mondays" {
		t.Fatalf("edit kept the migrated title: %v", upd.Data)
	}
}
