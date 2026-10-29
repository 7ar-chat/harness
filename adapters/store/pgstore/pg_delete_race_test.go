package pgstore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GhaziBenDahmane/harness/core/store"
)

func TestPGDeletedThreadsNeverGetARunExecuted(t *testing.T) {
	// 300 concurrent delete/create pairs (the API cancelling the active run the delete reports) leave no claimable run on a deleted thread.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	const pairs = 300
	runs := make([]*store.Run, pairs)
	var wg sync.WaitGroup

	// Act
	for i := 0; i < pairs; i++ {
		th, err := s.CreateThread(ctx, "alice", "")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(2)
		go func() {
			defer wg.Done()
			runs[i], _, _ = s.CreateRun(ctx, newRun(th.ID, "q"))
		}()
		go func() {
			defer wg.Done()
			if d, err := s.DeleteThread(ctx, "alice", th.ID); err == nil && d.ActiveRun != nil {
				_, _ = s.CancelQueuedRun(ctx, d.ActiveRun.ID)
			}
		}()
	}
	wg.Wait()

	// Assert
	executed := 0
	for _, run := range runs {
		if run == nil {
			continue
		}
		if _, err := s.ClaimRun(ctx, run.ID, "rt"); !errors.Is(err, store.ErrNotClaimable) {
			executed++
		}
	}
	if executed != 0 {
		t.Fatalf("%d of %d runs on deleted threads were claimable", executed, pairs)
	}
}

func TestPGGetUserRunHidesRunsOfDeletedThreadsAndOtherUsers(t *testing.T) {
	// GetUserRun finds the caller's run, and reports ErrNotFound for another user or once the thread is deleted.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "q"))
	_, mineErr := s.GetUserRun(ctx, "alice", run.ID)
	_, otherErr := s.GetUserRun(ctx, "bob", run.ID)
	_, _ = s.DeleteThread(ctx, "alice", th.ID)

	// Act
	_, deletedErr := s.GetUserRun(ctx, "alice", run.ID)

	// Assert
	if mineErr != nil || !errors.Is(otherErr, store.ErrNotFound) || !errors.Is(deletedErr, store.ErrNotFound) {
		t.Fatalf("mine %v other %v deleted %v", mineErr, otherErr, deletedErr)
	}
}

func TestPGThreadPagingKeepsAThreadUpdatedBetweenPages(t *testing.T) {
	// Pages are keyed on created_at: renaming a thread of page 2 after fetching page 1 does not skip it.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	oldest, _ := s.CreateThread(ctx, "alice", "oldest")
	_, _ = s.CreateThread(ctx, "alice", "middle")
	_, _ = s.CreateThread(ctx, "alice", "newest")
	p1, _ := s.ListThreads(ctx, "alice", 2, nil)
	last := p1[len(p1)-1]
	title := "renamed"
	_, _ = s.UpdateThread(ctx, "alice", oldest.ID, store.ThreadPatch{Title: &title})

	// Act
	p2, err := s.ListThreads(ctx, "alice", 2, &store.ThreadCursor{Pinned: last.PinnedAt != nil, CreatedAt: last.CreatedAt, ID: last.ID})

	// Assert
	if err != nil || len(p2) != 1 || p2[0].ID != oldest.ID {
		t.Fatalf("page 2 %v %v", titles(p2), err)
	}
}
