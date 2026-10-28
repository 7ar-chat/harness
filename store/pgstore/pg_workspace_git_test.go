package pgstore_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

func TestPGWorkspaceGitArchivesAreGarbageCollectedWhenReplacedOrPurged(t *testing.T) {
	// The 0007 trigger queues a replaced archive blob and, through the thread purge cascade, the current one; a reused key leaves the queue; unknown threads are ErrNotFound.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	for _, k := range []string{"git/a", "git/b"} {
		_ = s.PutBlob(ctx, k, []byte(k))
	}

	// Act
	_, missingErr := s.WorkspaceGit(ctx, th.ID)
	firstErr := s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: th.ID, BlobKey: "git/a", Head: "h1", SizeBytes: 3})
	secondErr := s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: th.ID, BlobKey: "git/b", Head: "h2", SizeBytes: 4})
	got, getErr := s.WorkspaceGit(ctx, th.ID)
	ageBlobGC(t, pool, 25*time.Hour)
	replaced, _ := s.BlobCandidates(ctx, 24*time.Hour, 10)
	_, _ = s.DeleteThread(ctx, "alice", th.ID)
	time.Sleep(20 * time.Millisecond)
	n, purgeErr := s.PurgeDeletedThreads(ctx, 10*time.Millisecond, 10)
	ageBlobGC(t, pool, 25*time.Hour)
	all, _ := s.BlobCandidates(ctx, 24*time.Hour, 10)
	collected, _ := s.CollectBlobs(ctx, all, 24*time.Hour)
	_, goneErr := s.WorkspaceGit(ctx, th.ID)
	unknownErr := s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: "00000000-0000-0000-0000-000000000000", BlobKey: "git/c"})

	// Assert
	if !errors.Is(missingErr, store.ErrNotFound) || firstErr != nil || secondErr != nil || getErr != nil || got.BlobKey != "git/b" || got.Head != "h2" || got.SizeBytes != 4 {
		t.Fatalf("missing %v first %v second %v get %+v %v", missingErr, firstErr, secondErr, got, getErr)
	}
	sort.Strings(all)
	if len(replaced) != 1 || replaced[0] != "git/a" || purgeErr != nil || n != 1 || len(all) != 2 || len(collected) != 2 {
		t.Fatalf("replaced %v purged %d %v all %v collected %v", replaced, n, purgeErr, all, collected)
	}
	if !errors.Is(goneErr, store.ErrNotFound) || !errors.Is(unknownErr, store.ErrNotFound) {
		t.Fatalf("gone %v unknown %v", goneErr, unknownErr)
	}
	for _, k := range []string{"git/a", "git/b"} {
		if ok, _ := s.HasBlob(ctx, k); ok {
			t.Fatalf("blob %s kept", k)
		}
	}
}

func TestPGWorkspaceGitIssueIsRecordedClearedAndPurgedWithTheThread(t *testing.T) {
	// An archive issue is upserted per thread, cleared by the next archive, ignored for an unknown thread and removed with the thread.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "t")

	// Act
	setErr := s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueFailed})
	_ = s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueTooLarge, Head: "abc", SizeBytes: 99})
	got, getErr := s.WorkspaceGitIssue(ctx, th.ID)
	unknownErr := s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: uuid.NewString(), Reason: store.GitIssueFailed})
	clearErr := s.ClearWorkspaceGitIssue(ctx, th.ID)
	_, cleared := s.WorkspaceGitIssue(ctx, th.ID)
	_ = s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueFailed})
	_, _ = pool.Exec(ctx, `DELETE FROM work_threads WHERE id = $1`, th.ID)
	_, purged := s.WorkspaceGitIssue(ctx, th.ID)

	// Assert
	if setErr != nil || getErr != nil || got.Reason != store.GitIssueTooLarge || got.Head != "abc" || got.SizeBytes != 99 || got.UpdatedAt.IsZero() {
		t.Fatalf("issue %+v %v %v", got, setErr, getErr)
	}
	if unknownErr != nil || clearErr != nil || !errors.Is(cleared, store.ErrNotFound) || !errors.Is(purged, store.ErrNotFound) {
		t.Fatalf("unknown %v clear %v cleared %v purged %v", unknownErr, clearErr, cleared, purged)
	}
}
