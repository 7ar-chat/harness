package memory_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/features/memory"
)

func TestDedupeContentRemovesDuplicateItemsLikeWork(t *testing.T) {
	// Exact and normalized duplicate list items (case, list marker, trailing punctuation or parenthetical note) are removed; headers, paragraphs and blank lines stay.
	// Arrange
	content := "Intro line\n\n# Facts\n- Uses Go 1.26\n- uses go 1.26.\n* Uses Go 1.26 (duplicate again)\n\n- Deploys on Fridays\n  with a freeze\n- Deploys on Fridays\n  with a freeze\n"

	// Act
	got, removed := memory.DedupeContent(content)
	same, none := memory.DedupeContent("- a\n- b\n")

	// Assert
	want := "Intro line\n\n# Facts\n- Uses Go 1.26\n\n- Deploys on Fridays\n  with a freeze\n"
	if removed != 3 || got != want {
		t.Fatalf("removed %d got %q", removed, got)
	}
	if none != 0 || same != "- a\n- b\n" {
		t.Fatalf("unchanged content rewritten: %q", same)
	}
}

func TestDedupeContentNeverTouchesCodeOrDistinctItems(t *testing.T) {
	// Lines inside fenced code blocks are never items; leading numbers are content, not list markers; paragraphs are never deduplicated.
	// Arrange
	content := "# Decisions\n\n- 2024 budget approved\n- 2025 budget approved\n\n```sh\n- run make\n- run make\n```\n\nSame paragraph\n\nSame paragraph\n~~~\n1. x\n1. x\n~~~\n"

	// Act
	got, removed := memory.DedupeContent(content)

	// Assert
	if removed != 0 || got != content {
		t.Fatalf("removed %d got %q", removed, got)
	}
}

func TestConsolidateDedupesEntriesAndPurgesOldDeletedOnes(t *testing.T) {
	// The job keeps the oldest of identical semantic entries (same content up to whitespace; others soft-deleted with duplicate_of), cleans duplicate lines inside Work's aggregated files only, and purges entries deleted before the retention.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	first := f.create(t, memory.CreateInput{Content: "Prefers French answers", Path: "memories/semantic/preferences/a.md"})
	dup := f.create(t, memory.CreateInput{Content: "Prefers  French\nanswers", Path: "memories/semantic/preferences/b.md"})
	near := f.create(t, memory.CreateInput{Content: "prefers french answers.", Path: "memories/semantic/preferences/n.md"})
	other := f.create(t, memory.CreateInput{Content: "Prefers French answers", Path: "memories/semantic/facts/c.md"})
	facts := f.create(t, memory.CreateInput{Content: "- Standup at 9\n- Standup at 9", Path: "shared/memories/semantic/facts.md"})
	notes := f.create(t, memory.CreateInput{Content: "- Standup at 9\n- Standup at 9", Path: "shared/memories/semantic/notes/team.md"})
	outside := f.create(t, memory.CreateInput{Content: "- x\n- x", Path: "memories/episodic/2026-10.md"})
	old := f.create(t, memory.CreateInput{Content: "Old", Path: "memories/semantic/notes/old.md"})
	f.svc.Delete(ctx, f.w, old.ID, "", map[string]any{"reason": "x"}, "")
	longAgo := now.Add(-40 * 24 * time.Hour)
	f.st.SetMemoryEntryTimes(old.ID, longAgo, &longAgo)
	recent := f.create(t, memory.CreateInput{Content: "Recent", Path: "memories/semantic/notes/recent.md"})
	f.svc.Delete(ctx, f.w, recent.ID, "", map[string]any{"reason": "x"}, "")
	job := memory.Consolidator{Store: f.st, Retention: 30 * 24 * time.Hour, Now: func() time.Time { return now }}

	// Act
	report, err := job.Consolidate(ctx)

	// Assert
	if err != nil || report.Duplicates != 1 || report.ItemsRemoved != 1 || report.Purged != 1 || report.Users != 1 {
		t.Fatalf("report %+v %v", report, err)
	}
	get := func(id string) *store.MemoryEntry {
		e, _ := f.st.GetMemoryEntry(ctx, "alice", id)
		return e
	}
	if d := get(dup.ID); d.Status != store.MemoryDeleted || d.Metadata["consolidation"].(map[string]any)["duplicate_of"] != first.ID {
		t.Fatalf("duplicate %+v", d)
	}
	if get(first.ID).Status != store.MemoryActive || get(other.ID).Status != store.MemoryActive || get(near.ID).Status != store.MemoryActive {
		t.Fatal("kept entries changed (another folder is another file in Work; a near copy is not a copy)")
	}
	if strings.Count(get(facts.ID).Content, "Standup") != 1 || get(notes.ID).Content != "- Standup at 9\n- Standup at 9" ||
		get(outside.ID).Content != "- x\n- x" {
		t.Fatalf("facts %q notes %q outside %q", get(facts.ID).Content, get(notes.ID).Content, get(outside.ID).Content)
	}
	if get(old.ID) != nil || get(recent.ID) == nil {
		t.Fatal("retention purge wrong")
	}
}

func TestConsolidateKeepsDistinctEntriesThatNormalizeAlike(t *testing.T) {
	// Separate entries are only duplicates when their content is identical up to whitespace: leading numbers and trailing notes matter.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	for i, c := range []string{"2024 budget approved", "2025 budget approved", "Use Go (not Python)", "Use Go (not Rust)", "1. Ship", "2. Ship"} {
		f.create(t, memory.CreateInput{Content: c, Path: "memories/semantic/decisions/d" + string(rune('a'+i)) + ".md"})
	}

	// Act
	report, err := memory.Consolidator{Store: f.st, Now: func() time.Time { return now }}.Consolidate(ctx)

	// Assert
	live, _ := f.st.ListMemoryEntries(ctx, "alice", store.MemoryActive)
	if err != nil || report.Duplicates != 0 || len(live) != 6 {
		t.Fatalf("report %+v %v live %d", report, err, len(live))
	}
}

// racingStore changes an entry right after the job listed the entries.
type racingStore struct {
	store.MemoryStore
	race func()
}

func (r racingStore) ListMemoryEntries(ctx context.Context, login string, statuses ...string) ([]*store.MemoryEntry, error) {
	out, err := r.MemoryStore.ListMemoryEntries(ctx, login, statuses...)
	if r.race != nil {
		r.race()
	}
	return out, err
}

func TestConsolidateSkipsEntriesChangedMeanwhile(t *testing.T) {
	// An entry deleted, or edited, after the job listed it is skipped: the job neither resurrects it nor overwrites the newer content.
	// Arrange
	f := newFixture(memory.Options{})
	ctx := context.Background()
	gone := f.create(t, memory.CreateInput{Content: "- a\n- a", Path: "memories/semantic/facts.md"})
	edited := f.create(t, memory.CreateInput{Content: "- b\n- b", Path: "memories/semantic/decisions.md"})
	keep := f.create(t, memory.CreateInput{Content: "Same", Path: "memories/semantic/notes/1.md"})
	copyOf := f.create(t, memory.CreateInput{Content: "Same", Path: "memories/semantic/notes/2.md"})
	newer := "- b\n- b\n- c"
	race := func() {
		f.svc.Delete(ctx, f.w, gone.ID, "", map[string]any{"reason": "user"}, "")
		f.svc.Update(ctx, f.w, memory.UpdateInput{ID: edited.ID, Content: &newer})
		f.svc.Delete(ctx, f.w, copyOf.ID, "", map[string]any{"reason": "user"}, "hard")
	}
	job := memory.Consolidator{Store: racingStore{MemoryStore: f.st, race: race}, Now: func() time.Time { return now }}

	// Act
	report, err := job.Consolidate(ctx)

	// Assert
	get := func(id string) *store.MemoryEntry {
		e, _ := f.st.GetMemoryEntry(ctx, "alice", id)
		return e
	}
	if err != nil || report.Skipped != 3 || report.ItemsRemoved != 0 || report.Duplicates != 0 {
		t.Fatalf("report %+v %v", report, err)
	}
	if g := get(gone.ID); g.Status != store.MemoryDeleted || g.DeletedAt == nil || g.Content != "- a\n- a" {
		t.Fatalf("deleted entry rewritten: %+v", g)
	}
	if e := get(edited.ID); e.Content != newer {
		t.Fatalf("edit lost: %q", e.Content)
	}
	if k := get(keep.ID); k.Status != store.MemoryActive {
		t.Fatalf("kept %+v", k)
	}
}

func TestRecallItemsFollowWorksOrder(t *testing.T) {
	// The priority files come first (their lines, headings skipped), then the typed semantic entries most recently updated first; at most max items; pending entries are skipped.
	// Arrange
	pending := entry("p", "memories/semantic/facts/p.md", "Pending fact", now)
	pending.Status = store.MemoryPending
	entries := []*store.MemoryEntry{
		entry("old", "memories/semantic/preferences/old.md", "Old preference", now.Add(-48*time.Hour)),
		entry("new", "memories/semantic/notes/new.md", "New note", now),
		entry("facts", "memories/semantic/facts.md", "# Facts\n- Uses Go\n\n- Ships Fridays", now.Add(-72*time.Hour)),
		entry("deep", "memories/semantic/notes/sub/deep.md", "Not typed (nested)", now),
		pending,
	}

	// Act
	items := memory.RecallItems(entries, 4)
	two := memory.RecallItems(entries, 2)
	block := memory.RecallContext(items[:1])

	// Assert
	want := []string{"memories/semantic/facts.md: Uses Go", "memories/semantic/facts.md: Ships Fridays",
		"memories/semantic/notes/new.md: New note", "memories/semantic/preferences/old.md: Old preference"}
	if !reflect.DeepEqual(items, want) || len(two) != 2 {
		t.Fatalf("items %v", items)
	}
	if block != "<user_memories>\n- memories/semantic/facts.md: Uses Go\n</user_memories>" || memory.RecallContext(nil) != "" {
		t.Fatalf("block %q", block)
	}
}

func TestRecallLinesAndBlockAreBounded(t *testing.T) {
	// Each recalled line is cut to the snippet length (Work's 240-char snippet) and the whole block stays under MaxRecallChars.
	// Arrange
	long := strings.Repeat("word ", 400)
	entries := []*store.MemoryEntry{
		entry("a", "memories/semantic/facts.md", long+"\n"+long+"\n"+long+"\n"+long, now),
	}

	// Act
	items := memory.RecallItems(entries, 4)
	block := memory.RecallContext(items)

	// Assert
	if len(items) != 4 {
		t.Fatalf("items %d", len(items))
	}
	for _, it := range items {
		line := strings.TrimPrefix(it, "memories/semantic/facts.md: ")
		if line != memory.Snippet(long) || len([]rune(line)) > memory.SnippetChars+2 {
			t.Fatalf("line of %d chars: %q", len([]rune(line)), line)
		}
	}
	if n := len([]rune(block)); n > memory.MaxRecallChars {
		t.Fatalf("block of %d chars", n)
	}
	huge := make([]string, 50)
	for i := range huge {
		huge[i] = strings.Repeat("p", 500) + ": " + strings.Repeat("x", 240)
	}
	if n := len([]rune(memory.RecallContext(huge))); n > memory.MaxRecallChars {
		t.Fatalf("unbounded block of %d chars", n)
	}
}
