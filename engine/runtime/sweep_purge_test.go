package runtime_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/features/artifacts"
	"github.com/GhaziBenDahmane/harness/store"
)

func shaOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestSweepPurgesThreadsDeletedLongAgoAndTheirUnreferencedBlobs(t *testing.T) {
	// The sweeper hard-deletes threads soft-deleted more than 30 days ago with their runs and artifacts, then deletes blobs (and previews) nothing referenced for the GC grace period.
	// Arrange
	r := newV2Rig(t, false)
	r.opts.Purger = artifacts.PurgeJob{Svc: r.arts, After: 30 * 24 * time.Hour}
	r.opts.PurgeEvery = 10 * time.Millisecond
	var offset atomic.Int64 // fake clock of the store
	r.st.Now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	ctx := context.Background()
	put := func(th *store.Thread, path, content string) {
		if err := r.st.PutBlob(ctx, shaOf(content), []byte(content)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.st.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: path, Name: "f",
			MimeType: "text/plain", Kind: store.ArtifactOutput, SizeBytes: int64(len(content)), SHA256: shaOf(content)}); err != nil {
			t.Fatal(err)
		}
	}
	old, oldRun := r.queued("old")
	put(old, "/workspace/only-old.txt", "only in the old thread")
	put(old, "/workspace/shared.txt", "shared content")
	_ = r.st.PutBlob(ctx, "preview/"+shaOf("only in the old thread")+".pdf", []byte("%PDF"))
	recent, _ := r.queued("recent")
	put(recent, "/workspace/recent.txt", "recently deleted")
	alive, _ := r.queued("alive")
	put(alive, "/workspace/shared.txt", "shared content")
	for _, th := range []*store.Thread{old, recent} {
		_, _ = r.st.DeleteThread(ctx, "alice", th.ID)
	}
	r.st.SetThreadDeletedAt(old.ID, time.Now().Add(-31*24*time.Hour))

	// Act
	r.runtime().Sweep(ctx)
	keptInGrace := r.st.Blob(shaOf("only in the old thread"))
	offset.Store(int64(25 * time.Hour))
	time.Sleep(20 * time.Millisecond)
	r.mr.FastForward(time.Hour) // sweeper and purge locks
	r.runtime().Sweep(ctx)

	// Assert
	if !keptInGrace {
		t.Fatal("unreferenced blob deleted within the grace period")
	}
	if _, err := r.st.GetRun(ctx, oldRun.ID); !errors.Is(err, store.ErrNotFound) || len(r.messages(old.ID)) != 0 {
		t.Fatalf("old thread not purged: run %v messages %d", err, len(r.messages(old.ID)))
	}
	if list, _ := r.st.ListArtifacts(ctx, old.ID); len(list) != 0 {
		t.Fatalf("old artifacts kept: %d", len(list))
	}
	if r.st.Blob(shaOf("only in the old thread")) || r.st.Blob("preview/"+shaOf("only in the old thread")+".pdf") {
		t.Fatal("unreferenced blob or its preview kept")
	}
	if !r.st.Blob(shaOf("shared content")) || !r.st.Blob(shaOf("recently deleted")) {
		t.Fatal("a referenced blob was deleted")
	}
	if list, _ := r.st.ListArtifacts(ctx, recent.ID); len(list) != 1 {
		t.Fatalf("recently deleted thread purged early: %d artifacts", len(list))
	}
}
