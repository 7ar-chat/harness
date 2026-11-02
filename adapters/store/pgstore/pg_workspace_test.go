package pgstore_test

import (
	"context"
	"errors"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/7ar-chat/harness/core/store"
)

func TestPGDeletionOnlyForArtifactsMaterializedInTheSandboxAndUnchanged(t *testing.T) {
	// MarkArtifactDeleted only applies to an artifact present in that sandbox and unchanged since the listing started (ErrConflict otherwise).
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	put := func(path, sandboxID string) *store.Artifact {
		a, _, err := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: path, Name: "f",
			MimeType: "text/plain", Kind: store.ArtifactOutput, SHA256: path, SandboxID: sandboxID})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	here, pending, later := put("/workspace/here", "sb-1"), put("/workspace/pending", ""), put("/workspace/later", "sb-1")
	_ = s.MarkArtifactsRestoreSkipped(ctx, []string{pending.ID})
	since, _ := s.Clock(ctx)
	time.Sleep(5 * time.Millisecond)
	_, _, _ = s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: "/workspace/later", Name: "f",
		MimeType: "text/plain", Kind: store.ArtifactOutput, SHA256: "changed", SandboxID: "sb-1"})

	// Act
	deleted, okErr := s.MarkArtifactDeleted(ctx, here.ID, "", "sb-1", since)
	_, otherErr := s.MarkArtifactDeleted(ctx, here.ID, "", "sb-1", since)
	_, pendingErr := s.MarkArtifactDeleted(ctx, pending.ID, "", "sb-1", since)
	_, laterErr := s.MarkArtifactDeleted(ctx, later.ID, "", "sb-1", since)
	_ = s.MarkArtifactsMaterialized(ctx, "sb-2", []string{pending.ID})
	got, _ := s.GetArtifact(ctx, pending.ID)

	// Assert
	if okErr != nil || !deleted.Deleted || !errors.Is(otherErr, store.ErrConflict) || !errors.Is(pendingErr, store.ErrConflict) || !errors.Is(laterErr, store.ErrConflict) {
		t.Fatalf("ok %v %+v again %v pending %v later %v", okErr, deleted, otherErr, pendingErr, laterErr)
	}
	if got.SandboxID != "sb-2" || got.RestoreSkippedAt != nil {
		t.Fatalf("materialized %+v", got)
	}
}

func TestPGAdvisoryLockIsExclusiveAcrossConnections(t *testing.T) {
	// A second holder of the same advisory lock waits (ErrLockTimeout after its wait) until the first releases it; other names are free.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	unlock, err := s.AdvisoryLock(ctx, "work:artifacts:t1", time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, busy := s.AdvisoryLock(ctx, "work:artifacts:t1", 100*time.Millisecond)
	otherUnlock, otherErr := s.AdvisoryLock(ctx, "work:artifacts:t2", 100*time.Millisecond)
	unlock()
	again, againErr := s.AdvisoryLock(ctx, "work:artifacts:t1", time.Second)

	// Assert
	if !errors.Is(busy, store.ErrLockTimeout) || otherErr != nil || againErr != nil {
		t.Fatalf("busy %v other %v again %v", busy, otherErr, againErr)
	}
	otherUnlock()
	again()
}

func TestPGPurgeDeletesOldDeletedThreadsAndQueuesTheirBlobs(t *testing.T) {
	// Threads deleted before the retention are hard-deleted (cascading); their content (and preview) is deleted once unreferenced for the grace period, unless another thread uses it.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	old, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(old.ID, "x"))
	recent, _ := s.CreateThread(ctx, "alice", "")
	alive, _ := s.CreateThread(ctx, "alice", "")
	for _, x := range []struct {
		th  *store.Thread
		sha string
	}{{old, "only-old"}, {old, "shared"}, {recent, "recent"}, {alive, "shared"}} {
		_ = s.PutBlob(ctx, x.sha, []byte(x.sha))
		if _, _, err := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: x.th.ID, UserLogin: "alice", Path: "/workspace/" + x.sha,
			Name: "f", MimeType: "text/plain", Kind: store.ArtifactOutput, SHA256: x.sha}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = s.DeleteThread(ctx, "alice", old.ID)
	_, _ = s.DeleteThread(ctx, "alice", recent.ID)
	time.Sleep(20 * time.Millisecond)
	_, _ = s.DeleteThread(ctx, "alice", recent.ID) // no-op: already deleted

	_ = s.PutBlob(ctx, "preview/only-old.pdf", []byte("%PDF"))

	// Act
	n, err := s.PurgeDeletedThreads(ctx, 10*time.Millisecond, 10)
	fresh, freshErr := s.BlobCandidates(ctx, 24*time.Hour, 10)
	ageBlobGC(t, pool, 25*time.Hour)
	keys, keysErr := s.BlobCandidates(ctx, 24*time.Hour, 10)
	collected, collectErr := s.CollectBlobs(ctx, keys, 24*time.Hour)
	after, _ := s.BlobCandidates(ctx, 0, 10)

	// Assert
	if err != nil || n != 2 || freshErr != nil || len(fresh) != 0 {
		t.Fatalf("purged %d %v fresh candidates %v %v", n, err, fresh, freshErr)
	}
	if keysErr != nil || collectErr != nil || len(keys) != 2 || len(collected) != 2 || len(after) != 0 {
		t.Fatalf("keys %v %v collected %v %v after %v", keys, keysErr, collected, collectErr, after)
	}
	if _, err := s.GetRun(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("run of the purged thread: %v", err)
	}
	if list, _ := s.ListArtifacts(ctx, alive.ID); len(list) != 1 {
		t.Fatalf("alive artifacts %d", len(list))
	}
	for _, k := range []string{"only-old", "recent", "preview/only-old.pdf"} {
		if ok, _ := s.HasBlob(ctx, k); ok {
			t.Fatalf("blob %s not deleted", k)
		}
	}
	if ok, _ := s.HasBlob(ctx, "shared"); !ok {
		t.Fatal("a referenced blob was deleted")
	}
}

// ageBlobGC moves every queued blob's last reference d into the past (fake clock).
func ageBlobGC(t *testing.T, pool *pgxpool.Pool, d time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE work_blob_gc SET last_ref_at = last_ref_at - make_interval(secs => $1)`,
		d.Seconds()); err != nil {
		t.Fatal(err)
	}
}

// queuedBlob stores key as the content of an artifact of a thread that is then purged.
func queuedBlob(t *testing.T, s *pgstore.PG, key string) {
	t.Helper()
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	_ = s.PutBlob(ctx, key, []byte(key))
	if _, _, err := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: th.ID, UserLogin: "alice", Path: "/workspace/" + key,
		Name: "f", MimeType: "text/plain", Kind: store.ArtifactOutput, SHA256: key}); err != nil {
		t.Fatal(err)
	}
	_, _ = s.DeleteThread(ctx, "alice", th.ID)
	time.Sleep(5 * time.Millisecond)
	if n, err := s.PurgeDeletedThreads(ctx, time.Millisecond, 10); err != nil || n != 1 {
		t.Fatalf("purge %d %v", n, err)
	}
}

func TestPGBlobGCSparesBlobsReferencedOrReusedWithinTheGracePeriod(t *testing.T) {
	// A queued blob whose reuse was announced (touch) or that got a new reference is not collected for the grace period.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	queuedBlob(t, s, "reused")
	queuedBlob(t, s, "referenced")
	ageBlobGC(t, pool, 25*time.Hour)
	alive, _ := s.CreateThread(ctx, "alice", "")

	// Act
	touchErr := s.TouchBlob(ctx, "reused")
	_, _, putErr := s.PutArtifactVersion(ctx, store.PutArtifact{ThreadID: alive.ID, UserLogin: "alice", Path: "/workspace/again",
		Name: "f", MimeType: "text/plain", Kind: store.ArtifactOutput, SHA256: "referenced"})
	candidates, candErr := s.BlobCandidates(ctx, 24*time.Hour, 10)
	collected, collectErr := s.CollectBlobs(ctx, []string{"reused", "referenced"}, 24*time.Hour)
	var recent int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM work_blob_gc WHERE last_ref_at > now() - interval '1 minute'`).Scan(&recent)

	// Assert
	if touchErr != nil || putErr != nil || candErr != nil || collectErr != nil {
		t.Fatalf("touch %v put %v candidates %v collect %v", touchErr, putErr, candErr, collectErr)
	}
	if len(candidates) != 0 || len(collected) != 0 || recent != 2 {
		t.Fatalf("candidates %v collected %v refreshed %d", candidates, collected, recent)
	}
	for _, k := range []string{"reused", "referenced"} {
		if ok, _ := s.HasBlob(ctx, k); !ok {
			t.Fatalf("blob %s deleted", k)
		}
	}
}

func TestPGCollectBlobHoldsItsClaimUntilTheExternalDeleteIsDone(t *testing.T) {
	// A reuse announced while the external delete of a collected blob is in flight waits for it; a failed delete keeps the blob queued.
	// Arrange
	s, pool := newPGPool(t)
	ctx := context.Background()
	queuedBlob(t, s, "collected")
	queuedBlob(t, s, "failing")
	ageBlobGC(t, pool, 25*time.Hour)
	started := make(chan struct{})
	touched := make(chan error, 1)
	var touchedEarly bool

	// Act
	go func() {
		<-started
		touched <- s.TouchBlob(ctx, "collected")
	}()
	ok, err := s.CollectBlob(ctx, "collected", 24*time.Hour, func(context.Context) error {
		close(started)
		select {
		case err := <-touched:
			touchedEarly = true
			touched <- err
		case <-time.After(200 * time.Millisecond):
		}
		return nil
	})
	touchErr := <-touched
	failed, failErr := s.CollectBlob(ctx, "failing", 24*time.Hour, func(context.Context) error { return errors.New("s3 down") })
	left, _ := s.BlobCandidates(ctx, 24*time.Hour, 10)

	// Assert
	if !ok || err != nil || touchedEarly || touchErr != nil {
		t.Fatalf("collected %v %v touched during the delete %v touch %v", ok, err, touchedEarly, touchErr)
	}
	if failed || failErr == nil || len(left) != 1 || left[0] != "failing" {
		t.Fatalf("failed delete: %v %v left %v", failed, failErr, left)
	}
}

func TestPGAppendRunArtifactsWithoutAnswerCreatesAnEmptyAssistantMessage(t *testing.T) {
	// A run without an assistant message gets an empty assistant message carrying its end-of-run artifact changes.
	// Arrange
	s := newPG(t)
	ctx := context.Background()
	th, _ := s.CreateThread(ctx, "alice", "")
	run, _, _ := s.CreateRun(ctx, newRun(th.ID, "go"))

	// Act
	err := s.AppendRunArtifacts(ctx, run.ID, []any{map[string]any{"path": "/workspace/a.csv", "change": "created"}})

	// Assert
	msgs, _ := s.ListMessages(ctx, th.ID)
	last := msgs[len(msgs)-1]
	if err != nil || len(msgs) != 2 || last.Role != store.RoleAssistant || last.Content != "" || last.RunID != run.ID ||
		len(store.ArtifactPaths(last.Metadata)) != 1 {
		t.Fatalf("err %v messages %+v", err, last)
	}
}
