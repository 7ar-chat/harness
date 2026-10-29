package pgstore_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
)

func TestPGThreadIsUnreadWhenARunEndedAfterItWasSeen(t *testing.T) {
	// A run finishing after the thread was last seen makes it unread (counted, filtered, lastRun set); MarkThreadSeen clears it.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	quiet, _ := s.CreateThread(ctx, "alice", "quiet")
	th, _ := s.CreateThread(ctx, "alice", "job")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	_, _ = s.ClaimRun(ctx, run.ID, "rt")
	_ = s.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", "")
	unread, _ := s.GetThread(ctx, "alice", th.ID)
	count, _ := s.CountUnreadThreads(ctx, "alice")
	listed, _ := s.SearchUnreadThreads(ctx, "alice", "", 10, nil)

	// Act
	err := s.MarkThreadSeen(ctx, "alice", th.ID)

	// Assert
	seen, _ := s.GetThread(ctx, "alice", th.ID)
	idle, _ := s.GetThread(ctx, "alice", quiet.ID)
	after, _ := s.CountUnreadThreads(ctx, "alice")
	if !unread.Unread || unread.LastRun == nil || unread.LastRun.ID != run.ID || unread.LastRun.FinishedAt == nil || count != 1 || len(listed) != 1 || listed[0].ID != th.ID {
		t.Fatalf("before seen: %+v count %d listed %d", unread, count, len(listed))
	}
	if err != nil || seen.Unread || after != 0 || idle.Unread || idle.LastRun != nil {
		t.Fatalf("after seen: err %v %+v idle %+v count %d", err, seen, idle, after)
	}
	if err := s.MarkThreadSeen(ctx, "bob", th.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bob marked alice's thread: %v", err)
	}
}
