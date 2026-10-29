package pgstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/core/store"
)

func TestPGQueuedBlobIsCollectedOnlyWhenUnreferencedAfterTheGrace(t *testing.T) {
	// A blob queued before it is stored is collected once no version references it for the grace period; a queued blob that a version references is kept (C-03, C-25).
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	for _, k := range []string{"orphan", "used", "preview/tag/used.pdf"} {
		if err := s.QueueBlob(ctx, k); err != nil {
			t.Fatal(err)
		}
		_ = s.PutBlob(ctx, k, []byte(k))
	}
	if _, _, err := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: "/workspace/used",
		Name: "used", MimeType: "text/plain", Kind: store.ArtifactOutput, SHA256: "used"}); err != nil {
		t.Fatal(err)
	}

	// Act
	fresh, _ := s.BlobCandidates(ctx, 24*time.Hour, 10)
	ageBlobGC(t, pool, 25*time.Hour)
	requeueErr := s.QueueBlob(ctx, "preview/tag/used.pdf") // a cache hit refreshes it
	keys, _ := s.BlobCandidates(ctx, 24*time.Hour, 10)
	collected, err := s.CollectBlobs(ctx, keys, 24*time.Hour)

	// Assert
	if len(fresh) != 0 || requeueErr != nil || err != nil || len(collected) != 1 || collected[0] != "orphan" {
		t.Fatalf("fresh %v requeue %v collected %v %v", fresh, requeueErr, collected, err)
	}
	for k, want := range map[string]bool{"orphan": false, "used": true, "preview/tag/used.pdf": true} {
		if ok, _ := s.HasBlob(ctx, k); ok != want {
			t.Fatalf("blob %s present=%v, want %v", k, ok, want)
		}
	}
}

func TestPGQueuedWorkspaceArchiveIsKeptWhileItIsTheCurrentOne(t *testing.T) {
	// A workspace Git archive queued before it is stored is never collected while its thread references it; once replaced it is (C-25).
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	for _, k := range []string{"workspace-git/a.tar.gz", "workspace-git/b.tar.gz"} {
		_ = s.QueueBlob(ctx, k)
		_ = s.PutBlob(ctx, k, []byte(k))
	}
	if err := s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: th.ID, BlobKey: "workspace-git/a.tar.gz", Head: "h1"}); err != nil {
		t.Fatal(err)
	}
	ageBlobGC(t, pool, 25*time.Hour)

	// Act
	keys, _ := s.BlobCandidates(ctx, 24*time.Hour, 10)
	collected, err := s.CollectBlobs(ctx, keys, 24*time.Hour)

	// Assert
	if err != nil || len(collected) != 1 || collected[0] != "workspace-git/b.tar.gz" {
		t.Fatalf("collected %v %v", collected, err)
	}
	if ok, _ := s.HasBlob(ctx, "workspace-git/a.tar.gz"); !ok {
		t.Fatal("the current archive was collected")
	}
}
