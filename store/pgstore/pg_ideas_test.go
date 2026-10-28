package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGRecordIdeaUsagePersistsAndListsNewestFirst(t *testing.T) {
	// work_idea_usage keeps the user, idea, action, surface, metadata and the user's own thread only.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	mine, _ := s.CreateThread(ctx, "alice", "")
	theirs, _ := s.CreateThread(ctx, "bob", "")

	// Act
	first, err := s.RecordIdeaUsage(ctx, store.IdeaUsage{UserLogin: "alice", ThreadID: mine.ID, IdeaID: "analyze-excel",
		Action: store.IdeaSelected, Surface: "ideas", Metadata: map[string]any{"entrypoint": "sidebar"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RecordIdeaUsage(ctx, store.IdeaUsage{UserLogin: "alice", ThreadID: theirs.ID, IdeaID: "generate-faq", Action: store.IdeaSubmitted})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.RecordIdeaUsage(ctx, store.IdeaUsage{UserLogin: "alice", ThreadID: "not-a-uuid", IdeaID: "generate-faq", Action: store.IdeaSubmitted})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListIdeaUsage(ctx, "alice")

	// Assert
	if err != nil || len(list) != 3 || list[0].ID != third.ID || list[2].ID != first.ID {
		t.Fatalf("list %v %v", list, err)
	}
	if first.ThreadID != mine.ID || first.Metadata["entrypoint"] != "sidebar" || first.Surface != "ideas" || first.CreatedAt.IsZero() {
		t.Fatalf("first = %+v", first)
	}
	if second.ThreadID != "" || third.ThreadID != "" || len(second.Metadata) != 0 {
		t.Fatalf("second %+v third %+v", second, third)
	}
	if other, _ := s.ListIdeaUsage(ctx, "bob"); len(other) != 0 {
		t.Fatalf("bob sees %v", other)
	}
}

func TestPGRecordIdeaUsageRejectsBadActionsAndKeepsRowsOfPurgedThreads(t *testing.T) {
	// The action CHECK rejects unknown actions; purging the thread keeps the usage with no thread.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	if _, err := s.RecordIdeaUsage(ctx, store.IdeaUsage{UserLogin: "alice", ThreadID: th.ID, IdeaID: "x", Action: store.IdeaSelected}); err != nil {
		t.Fatal(err)
	}

	// Act
	_, bad := s.RecordIdeaUsage(ctx, store.IdeaUsage{UserLogin: "alice", IdeaID: "x", Action: "viewed"})
	if _, err := s.DeleteThread(ctx, "alice", th.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeDeletedThreads(ctx, -time.Hour, 10); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListIdeaUsage(ctx, "alice")

	// Assert
	if bad == nil || !store.IsDataError(bad) || errors.Is(bad, store.ErrNotFound) {
		t.Fatalf("bad action: %v", bad)
	}
	if len(list) != 1 || list[0].ThreadID != "" {
		t.Fatalf("list = %+v", list)
	}
}
