package pgstore_test

import (
	"context"
	"testing"
)

func TestPGSwapThreadSandboxReportsWhetherItRecorded(t *testing.T) {
	// The thread's sandbox compare-and-set reports a lost race and refuses deleted threads (C-10).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	gone, _ := s.CreateThread(ctx, "alice", "")
	_, _ = s.DeleteThread(ctx, "alice", gone.ID)

	// Act
	first, err1 := s.SwapThreadSandbox(ctx, th.ID, "", "sbx-1")
	lost, err2 := s.SwapThreadSandbox(ctx, th.ID, "", "sbx-2")
	deleted, err3 := s.SwapThreadSandbox(ctx, gone.ID, "", "sbx-3")

	// Assert
	cur, _ := s.ThreadSandbox(ctx, th.ID)
	if err1 != nil || err2 != nil || err3 != nil || !first || lost || deleted || cur != "sbx-1" {
		t.Fatalf("first %v lost %v deleted %v cur %q errs %v %v %v", first, lost, deleted, cur, err1, err2, err3)
	}
}

func TestPGUserSandboxesListsWhatTheQuotaCounts(t *testing.T) {
	// UserSandboxes lists the user's live threads with a recorded sandbox, exactly those CountUserSandboxes counts (C-09).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	a, _ := s.CreateThread(ctx, "alice", "")
	b, _ := s.CreateThread(ctx, "alice", "")
	none, _ := s.CreateThread(ctx, "alice", "")
	bobs, _ := s.CreateThread(ctx, "bob", "")
	for th, id := range map[string]string{a.ID: "sbx-a", b.ID: "sbx-b", bobs.ID: "sbx-bob"} {
		_ = s.SetThreadSandbox(ctx, th, "", id)
	}
	_, _ = s.DeleteThread(ctx, "alice", b.ID)
	_ = none

	// Act
	refs, err := s.UserSandboxes(ctx, "alice")
	n, _ := s.CountUserSandboxes(ctx, "alice")

	// Assert
	if err != nil || len(refs) != 1 || refs[0].ThreadID != a.ID || refs[0].SandboxID != "sbx-a" || n != 1 {
		t.Fatalf("refs %+v err %v count %d", refs, err, n)
	}
}
