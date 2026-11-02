package memory_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/memory"
)

func TestConsolidateReportsStoreFailuresPerUserAndKeepsGoing(t *testing.T) {
	// A user whose entries cannot be read is reported in the error; the other users are still consolidated; purge failures are reported too.
	// Arrange
	st, _, _ := newFaulty(memory.Options{})
	ctx := context.Background()
	seed(st, "alice", "a1", "memories/semantic/notes/1.md", "same")
	seed(st, "bob", "b1", "memories/semantic/notes/1.md", "same")
	seed(st, "bob", "b2", "memories/semantic/notes/2.md", "same")
	st.errList = map[string]error{"alice": errBoom}
	job := memory.Consolidator{Store: st, Retention: time.Hour, Now: func() time.Time { return now }}

	// Act
	report, err := job.Consolidate(ctx)
	st.errPurge = errors.New("purge failed")
	_, purgeErr := job.Consolidate(ctx)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "user alice") || !errors.Is(err, errBoom) {
		t.Fatalf("error = %v", err)
	}
	if report.Users != 2 || report.Duplicates != 1 {
		t.Fatalf("bob was not consolidated after alice failed: %+v", report)
	}
	if purgeErr == nil || !strings.Contains(purgeErr.Error(), "purge failed") {
		t.Fatalf("purge error = %v", purgeErr)
	}
}

func TestConsolidateStopsWhenTheUserListIsUnavailableOrTheContextEnds(t *testing.T) {
	// Arrange
	st, _, _ := newFaulty(memory.Options{})
	seed(st, "alice", "a1", "memories/semantic/notes/1.md", "same")
	seed(st, "alice", "a2", "memories/semantic/notes/2.md", "same")
	job := memory.Consolidator{Store: st}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	st.errUsers = errBoom
	_, listErr := job.Consolidate(context.Background())
	st.errUsers = nil
	_, ctxErr := job.Consolidate(cancelled)

	// Assert
	if !errors.Is(listErr, errBoom) || !errors.Is(ctxErr, context.Canceled) {
		t.Fatalf("list %v ctx %v", listErr, ctxErr)
	}
	if es, _ := st.ListMemoryEntries(context.Background(), "alice", store.MemoryActive); len(es) != 2 {
		t.Fatalf("a cancelled job changed entries: %d active", len(es))
	}
}

func TestConsolidatorRunReturnsTheJobError(t *testing.T) {
	// Run is what the sweeper calls: it succeeds on a healthy store and returns the failure otherwise.
	// Arrange
	st, _, _ := newFaulty(memory.Options{})
	seed(st, "alice", "a1", "memories/semantic/notes/1.md", "same")
	seed(st, "alice", "a2", "memories/semantic/notes/2.md", "same")
	job := memory.Consolidator{Store: st, Now: func() time.Time { return now }}

	// Act
	ok := job.Run(context.Background())
	live, _ := st.ListMemoryEntries(context.Background(), "alice", store.MemoryActive)
	st.errUsers = errBoom
	failed := job.Run(context.Background())

	// Assert
	if ok != nil || len(live) != 1 || !errors.Is(failed, errBoom) {
		t.Fatalf("ok %v live %d failed %v", ok, len(live), failed)
	}
}

func TestConsolidateStopsOnAStorageErrorWhileRewriting(t *testing.T) {
	// A storage error (not a concurrent change) while deduplicating aborts that user's pass and is reported, never swallowed.
	// Arrange
	st, _, _ := newFaulty(memory.Options{})
	seed(st, "alice", "a1", "shared/memories/semantic/facts.md", "- a\n- a")
	st.errUpdate = errBoom
	job := memory.Consolidator{Store: st, Now: func() time.Time { return now }}

	// Act
	report, err := job.Consolidate(context.Background())

	// Assert
	if !errors.Is(err, errBoom) || report.ItemsRemoved != 0 {
		t.Fatalf("report %+v err %v", report, err)
	}
}

func TestRevisionHistoryKeepsTheTenLatestNewestFirst(t *testing.T) {
	// Every content change records the previous text; the history is bounded and the oldest revisions fall off.
	// Arrange
	f := newFixture(memory.Options{})
	id := newEntry(t, f, f.w, memory.CreateInput{Content: "version 0"})

	// Act
	for i := 1; i <= 12; i++ {
		c := "version " + string(rune('0'+i/10)) + string(rune('0'+i%10))
		if r := f.svc.Update(context.Background(), f.w, memory.UpdateInput{ID: id, Content: &c}); r.IsError {
			t.Fatal(r.Data)
		}
	}

	// Assert
	e, _ := f.st.GetMemoryEntry(context.Background(), "alice", id)
	h := e.Metadata["history"].([]any)
	if len(h) != 10 || h[0].(map[string]any)["content"] != "version 11" || h[9].(map[string]any)["content"] != "version 02" {
		t.Fatalf("history of %d: first %v last %v", len(h), h[0], h[len(h)-1])
	}
}
