package conformance

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

const artGrace = 24 * time.Hour

// artCandidates returns the keys BlobCandidates reports for the grace period.
func artCandidates(t *testing.T, s Store, grace time.Duration, limit int) []string {
	t.Helper()
	keys, err := s.BlobCandidates(context.Background(), grace, limit)
	if err != nil {
		t.Fatalf("BlobCandidates: %v", err)
	}
	return keys
}

// artStoreBlob puts the content of key in the blob store.
func artStoreBlob(t *testing.T, s Store, key string) {
	t.Helper()
	if err := s.PutBlob(context.Background(), key, []byte("content of "+key)); err != nil {
		t.Fatalf("PutBlob(%s): %v", key, err)
	}
}

// artHasBlob reports whether the blob store holds key.
func artHasBlob(t *testing.T, s Store, key string) bool {
	t.Helper()
	ok, err := s.HasBlob(context.Background(), key)
	if err != nil {
		t.Fatalf("HasBlob(%s): %v", key, err)
	}
	return ok
}

func artSuiteBlobs(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("StoresAndReturnsTheContent", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		data := []byte{0, 1, 2, 'x', 0xff}

		// Act
		putErr := s.PutBlob(ctx, "k1", data)
		data[0] = 9 // the store must not alias the caller's slice
		got, getErr := s.GetBlob(ctx, "k1")
		got[1] = 9 // nor hand out its own
		again, againErr := s.GetBlob(ctx, "k1")

		// Assert
		if putErr != nil || getErr != nil || againErr != nil || !bytes.Equal(again, []byte{0, 1, 2, 'x', 0xff}) {
			t.Errorf("put %v get %v %v content %v", putErr, getErr, againErr, again)
		}
		if !artHasBlob(t, s, "k1") {
			t.Error("HasBlob is false for a stored blob")
		}
	})

	t.Run("AnExistingKeyKeepsItsFirstContent", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		artMust(t, "PutBlob", s.PutBlob(ctx, "k1", []byte("first")))

		// Act
		err := s.PutBlob(ctx, "k1", []byte("second"))
		got, getErr := s.GetBlob(ctx, "k1")

		// Assert
		if err != nil || getErr != nil || string(got) != "first" {
			t.Errorf("put %v content %q (%v), want first", err, got, getErr)
		}
	})

	t.Run("StoresAnEmptyBlob", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		err := s.PutBlob(ctx, "empty", nil)
		got, getErr := s.GetBlob(ctx, "empty")

		// Assert
		if err != nil || getErr != nil || len(got) != 0 || !artHasBlob(t, s, "empty") {
			t.Errorf("put %v get %q (%v)", err, got, getErr)
		}
	})

	t.Run("AnUnknownKeyIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		_, err := s.GetBlob(ctx, "nope")

		// Assert
		if !errors.Is(err, store.ErrNotFound) || artHasBlob(t, s, "nope") {
			t.Errorf("GetBlob of an unknown key: %v", err)
		}
	})

	t.Run("DeleteRemovesOnlyThatBlob", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		artStoreBlob(t, s, "gone")
		artStoreBlob(t, s, "kept")

		// Act
		err := s.DeleteBlob(ctx, "gone")
		unknownErr := s.DeleteBlob(ctx, "never-stored")

		// Assert
		if err != nil || unknownErr != nil || artHasBlob(t, s, "gone") || !artHasBlob(t, s, "kept") {
			t.Errorf("delete %v %v, gone present %v, kept present %v", err, unknownErr, artHasBlob(t, s, "gone"), artHasBlob(t, s, "kept"))
		}
		if _, getErr := s.GetBlob(ctx, "gone"); !errors.Is(getErr, store.ErrNotFound) {
			t.Errorf("GetBlob after delete: %v", getErr)
		}
	})
}

func artSuiteBlobGC(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("AQueuedBlobIsACandidateOnlyAfterTheGrace", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "orphan"))

		// Act
		fresh := artCandidates(t, s, artGrace, 10)
		b.Advance(t, artGrace+time.Hour)
		aged := artCandidates(t, s, artGrace, 10)
		young := artCandidates(t, s, 48*time.Hour, 10)

		// Assert
		if len(fresh) != 0 || len(young) != 0 || !slices.Equal(aged, []string{"orphan"}) {
			t.Errorf("candidates: fresh %v, aged %v, with a longer grace %v", fresh, aged, young)
		}
	})

	t.Run("CandidatesAreOldestFirstAndLimited", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "z-oldest"))
		b.Advance(t, 2*time.Hour)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "m-middle"))
		b.Advance(t, 2*time.Hour)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "a-newest"))
		b.Advance(t, artGrace+time.Minute)

		// Act
		all := artCandidates(t, s, artGrace, 10)
		limited := artCandidates(t, s, artGrace, 2)

		// Assert
		if !slices.Equal(all, []string{"z-oldest", "m-middle", "a-newest"}) || !slices.Equal(limited, []string{"z-oldest", "m-middle"}) {
			t.Errorf("all %v, limited to 2 %v", all, limited)
		}
	})

	t.Run("ANonPositiveLimitDoesNotLimit", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "a"))
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "b"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		keys := artCandidates(t, s, artGrace, 0)

		// Assert
		if len(keys) != 2 {
			t.Errorf("candidates %v, want both queued blobs", keys)
		}
	})

	t.Run("QueueingAgainRefreshesTheLastReference", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "again"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "again"))
		candidates := artCandidates(t, s, artGrace, 10)

		// Assert
		if len(candidates) != 0 {
			t.Errorf("a re-queued blob is a candidate: %v", candidates)
		}
	})

	t.Run("TouchRefreshesAQueuedBlob", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "reused"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		err := s.TouchBlob(ctx, "reused")
		candidates := artCandidates(t, s, artGrace, 10)

		// Assert
		if err != nil || len(candidates) != 0 {
			t.Errorf("touch %v, candidates %v", err, candidates)
		}
	})

	t.Run("TouchingAnUnqueuedBlobQueuesNothing", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store

		// Act
		err := s.TouchBlob(ctx, "never-queued")
		b.Advance(t, artGrace+time.Hour)
		candidates := artCandidates(t, s, artGrace, 10)

		// Assert
		if err != nil || len(candidates) != 0 {
			t.Errorf("touch %v, candidates %v", err, candidates)
		}
	})

	t.Run("ABlobReferencedByAVersionLeavesTheQueue", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "used"))
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "orphan"))
		artStoreBlob(t, s, "used")
		artStoreBlob(t, s, "orphan")
		artPut(t, s, artIn(th.ID, "used", "used"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		candidates := artCandidates(t, s, artGrace, 10)
		usedCollected, usedErr := s.CollectBlob(ctx, "used", artGrace, func(context.Context) error { return nil })

		// Assert
		if !slices.Equal(candidates, []string{"orphan"}) || usedErr != nil || usedCollected {
			t.Errorf("candidates %v, collecting the referenced blob: %v %v", candidates, usedCollected, usedErr)
		}
	})

	t.Run("ANewVersionReferencingAQueuedBlobRefreshesIt", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "reused"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		artPut(t, s, artIn(th.ID, "again", "reused"))
		candidates := artCandidates(t, s, artGrace, 10)

		// Assert
		if len(candidates) != 0 {
			t.Errorf("a referenced blob is a candidate: %v", candidates)
		}
	})

	t.Run("CollectBlobDeletesTheContentThroughTheCallback", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artStoreBlob(t, s, "orphan")
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "orphan"))
		b.Advance(t, artGrace+time.Hour)
		calls := 0

		// Act
		ok, err := s.CollectBlob(ctx, "orphan", artGrace, func(ctx context.Context) error {
			calls++
			return s.DeleteBlob(ctx, "orphan")
		})
		again, againErr := s.CollectBlob(ctx, "orphan", artGrace, func(context.Context) error { calls++; return nil })

		// Assert
		if !ok || err != nil || calls != 1 {
			t.Errorf("CollectBlob: %v %v after %d calls", ok, err, calls)
		}
		if again || againErr != nil || calls != 1 {
			t.Errorf("a collected blob is not queued any more: %v %v after %d calls", again, againErr, calls)
		}
		if left := artCandidates(t, s, artGrace, 10); len(left) != 0 || artHasBlob(t, s, "orphan") {
			t.Errorf("candidates %v, blob present %v", left, artHasBlob(t, s, "orphan"))
		}
	})

	t.Run("CollectBlobSparesWhatIsNotCollectable", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "old"))
		b.Advance(t, artGrace+time.Hour)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "young"))
		called := false
		del := func(context.Context) error { called = true; return nil }

		// Act
		young, youngErr := s.CollectBlob(ctx, "young", artGrace, del)
		unknown, unknownErr := s.CollectBlob(ctx, "never-queued", artGrace, del)
		early, earlyErr := s.CollectBlob(ctx, "old", 48*time.Hour, del)

		// Assert
		if young || unknown || early || youngErr != nil || unknownErr != nil || earlyErr != nil || called {
			t.Errorf("young %v %v, unknown %v %v, longer grace %v %v, callback called %v", young, youngErr, unknown, unknownErr, early, earlyErr, called)
		}
		if got := artCandidates(t, s, artGrace, 10); !slices.Equal(got, []string{"old"}) {
			t.Errorf("candidates %v, want [old]", got)
		}
	})

	t.Run("AFailedExternalDeleteKeepsTheQueueRow", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "failing"))
		b.Advance(t, artGrace+time.Hour)
		boom := errors.New("s3 down")

		// Act
		ok, err := s.CollectBlob(ctx, "failing", artGrace, func(context.Context) error { return boom })
		left := artCandidates(t, s, artGrace, 10)
		retry, retryErr := s.CollectBlob(ctx, "failing", artGrace, func(context.Context) error { return nil })

		// Assert
		if ok || !errors.Is(err, boom) {
			t.Errorf("failed delete: %v %v", ok, err)
		}
		if !slices.Equal(left, []string{"failing"}) {
			t.Errorf("candidates after the failure %v, want [failing]", left)
		}
		if !retry || retryErr != nil {
			t.Errorf("retry: %v %v", retry, retryErr)
		}
	})

	t.Run("CollectBlobsDeletesContentAndPreviewInOneCall", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		for _, k := range []string{"b-orphan", "a-orphan", "preview/a-orphan.pdf", "used", "young", "unqueued"} {
			artStoreBlob(t, s, k)
		}
		for _, k := range []string{"b-orphan", "a-orphan", "used"} {
			artMust(t, "QueueBlob", s.QueueBlob(ctx, k))
		}
		artPut(t, s, artIn(th.ID, "used", "used"))
		b.Advance(t, artGrace+time.Hour)
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "young"))

		// Act
		collected, err := s.CollectBlobs(ctx, []string{"b-orphan", "a-orphan", "used", "young", "unqueued", "never-stored"}, artGrace)
		none, noneErr := s.CollectBlobs(ctx, nil, artGrace)

		// Assert
		if err != nil || !slices.Equal(collected, []string{"a-orphan", "b-orphan"}) {
			t.Errorf("collected %v (%v), want [a-orphan b-orphan]", collected, err)
		}
		if noneErr != nil || len(none) != 0 {
			t.Errorf("collecting no keys: %v (%v)", none, noneErr)
		}
		for key, want := range map[string]bool{"a-orphan": false, "b-orphan": false, "preview/a-orphan.pdf": false, "used": true, "young": true, "unqueued": true} {
			if got := artHasBlob(t, s, key); got != want {
				t.Errorf("blob %s present = %v, want %v", key, got, want)
			}
		}
		if left := artCandidates(t, s, artGrace, 10); len(left) != 0 {
			t.Errorf("collected keys are still queued: %v", left)
		}
	})

	t.Run("ACurrentWorkspaceArchiveIsNeverCollected", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		for _, k := range []string{"workspace-git/a", "workspace-git/b"} {
			artMust(t, "QueueBlob", s.QueueBlob(ctx, k))
			artStoreBlob(t, s, k)
		}
		artMust(t, "PutWorkspaceGit", s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: th.ID, BlobKey: "workspace-git/a", Head: "h1"}))
		// The put took its key out of the queue; queue it again, as a racing writer may have.
		artMust(t, "QueueBlob", s.QueueBlob(ctx, "workspace-git/a"))
		b.Advance(t, artGrace+time.Hour)

		// Act
		candidates := artCandidates(t, s, artGrace, 10)
		collected, err := s.CollectBlobs(ctx, []string{"workspace-git/a", "workspace-git/b"}, artGrace)

		// Assert
		if !slices.Equal(candidates, []string{"workspace-git/b"}) || err != nil || !slices.Equal(collected, []string{"workspace-git/b"}) {
			t.Errorf("candidates %v, collected %v (%v)", candidates, collected, err)
		}
		if !artHasBlob(t, s, "workspace-git/a") {
			t.Error("the current archive was collected")
		}
	})
}

func artSuitePurge(t *testing.T, f Factory) {
	ctx := context.Background()
	const retention = 24 * time.Hour

	t.Run("PurgesOnlyThreadsDeletedBeforeTheRetention", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		old, recent, alive := artThread(t, s), artThread(t, s), artThread(t, s)
		for _, th := range []*store.Thread{old, recent, alive} {
			artPut(t, s, artIn(th.ID, "a", "sha-"+th.ID))
		}
		artDeleteThread(t, s, old.ID)
		b.Advance(t, retention+time.Hour)
		artDeleteThread(t, s, recent.ID)

		// Act
		n, err := s.PurgeDeletedThreads(ctx, retention, 10)
		again, againErr := s.PurgeDeletedThreads(ctx, retention, 10)

		// Assert
		if err != nil || n != 1 || againErr != nil || again != 0 {
			t.Errorf("purged %d (%v), then %d (%v)", n, err, again, againErr)
		}
		for th, want := range map[*store.Thread]int{old: 0, recent: 1, alive: 1} {
			if list, _ := s.ListArtifacts(ctx, th.ID); len(list) != want {
				t.Errorf("thread %s keeps %d artifacts, want %d", th.ID, len(list), want)
			}
		}
	})

	t.Run("PurgesOldestFirstUpToTheLimit", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		ths := []*store.Thread{artThread(t, s), artThread(t, s), artThread(t, s)}
		for _, th := range ths {
			artPut(t, s, artIn(th.ID, "a", "sha-"+th.ID))
		}
		artDeleteThread(t, s, ths[1].ID) // the oldest deletion
		b.Advance(t, time.Hour)
		artDeleteThread(t, s, ths[2].ID)
		b.Advance(t, time.Hour)
		artDeleteThread(t, s, ths[0].ID)
		b.Advance(t, retention+time.Hour)

		// Act
		n1, err1 := s.PurgeDeletedThreads(ctx, retention, 2)
		left := func(th *store.Thread) int { l, _ := s.ListArtifacts(ctx, th.ID); return len(l) }
		afterFirst := [3]int{left(ths[0]), left(ths[1]), left(ths[2])}
		n2, err2 := s.PurgeDeletedThreads(ctx, retention, 2)

		// Assert
		if err1 != nil || n1 != 2 || afterFirst != [3]int{1, 0, 0} {
			t.Errorf("first purge %d (%v), artifacts left per thread %v, want [1 0 0]", n1, err1, afterFirst)
		}
		if err2 != nil || n2 != 1 || left(ths[0]) != 0 {
			t.Errorf("second purge %d (%v), thread 0 keeps %d", n2, err2, left(ths[0]))
		}
	})

	t.Run("ANonPositiveLimitStillPurges", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th1, th2 := artThread(t, s), artThread(t, s)
		artDeleteThread(t, s, th1.ID)
		artDeleteThread(t, s, th2.ID)
		b.Advance(t, retention+time.Hour)

		// Act
		n, err := s.PurgeDeletedThreads(ctx, retention, 0)

		// Assert
		if err != nil || n != 2 {
			t.Errorf("purged %d (%v), want 2", n, err)
		}
	})

	t.Run("CascadesToEverythingOfTheThread", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th, keep := artThread(t, s), artThread(t, s)
		run := artRun(t, s, th.ID)
		keepRun := artRun(t, s, keep.ID)
		artMust(t, "ClaimRun", func() error { _, err := s.ClaimRun(ctx, run.ID, "rt"); return err }())
		artMust(t, "AppendMessages", s.AppendMessages(ctx, &store.Message{ThreadID: th.ID, RunID: run.ID, Role: store.RoleAssistant, Content: "answer"},
			&store.Message{ThreadID: keep.ID, RunID: keepRun.ID, Role: store.RoleAssistant, Content: "kept answer"}))
		msgs, _ := s.ListMessages(ctx, th.ID)
		if len(msgs) != 2 {
			t.Fatalf("messages %+v", msgs)
		}
		artMust(t, "AppendEvents", s.AppendEvents(ctx, run.ID, []store.Event{{Seq: 1, Type: "RUN_STARTED", Data: []byte(`{"type":"RUN_STARTED"}`)}}))
		artMust(t, "AppendEvents", s.AppendEvents(ctx, keepRun.ID, []store.Event{{Seq: 1, Type: "RUN_STARTED", Data: []byte(`{"type":"RUN_STARTED"}`)}}))
		if _, err := s.PutGoal(ctx, "alice", th.ID, "ship it", 3); err != nil {
			t.Fatalf("PutGoal: %v", err)
		}
		artMust(t, "SetFeedback", s.SetFeedback(ctx, "alice", msgs[1].ID, store.Feedback{Sentiment: "like"}))
		artMust(t, "SetThreadSummary", s.SetThreadSummary(ctx, th.ID, store.ThreadSummary{Text: "sum", ThroughPosition: 1}))
		approval := &store.Approval{ThreadID: th.ID, RunID: run.ID, ToolCallID: "c1", ToolName: "write_file"}
		artMust(t, "CreateApproval", s.CreateApproval(ctx, approval))
		artMust(t, "RecordCompaction", s.RecordCompaction(ctx, store.Compaction{ThreadID: th.ID, RunID: run.ID, Trigger: "auto"}))
		artMust(t, "RecordCompaction", s.RecordCompaction(ctx, store.Compaction{ThreadID: keep.ID, RunID: keepRun.ID, Trigger: "auto"}))
		in := artIn(th.ID, "a", "sha-1")
		in.RunID = run.ID
		a, _ := artPut(t, s, in)
		keptArtifact, _ := artPut(t, s, artIn(keep.ID, "a", "sha-keep"))
		artDeleteThread(t, s, th.ID)
		b.Advance(t, retention+time.Hour)

		// Act
		n, err := s.PurgeDeletedThreads(ctx, retention, 10)

		// Assert
		if err != nil || n != 1 {
			t.Fatalf("purged %d (%v)", n, err)
		}
		if _, err := s.GetRun(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("run: %v", err)
		}
		if m, _ := s.ListMessages(ctx, th.ID); len(m) != 0 {
			t.Errorf("messages %+v", m)
		}
		if ev, _ := s.ListEvents(ctx, run.ID, 0, 10); len(ev) != 0 {
			t.Errorf("events %+v", ev)
		}
		if list, _ := s.ListArtifacts(ctx, th.ID); len(list) != 0 {
			t.Errorf("artifacts %+v", list)
		}
		if v, _ := s.ListArtifactVersions(ctx, a.ID); len(v) != 0 {
			t.Errorf("versions %+v", v)
		}
		if g, _ := s.ThreadGoal(ctx, th.ID); g != nil {
			t.Errorf("goal %+v", g)
		}
		if fb, _ := s.ListFeedback(ctx, th.ID); len(fb) != 0 {
			t.Errorf("feedback %+v", fb)
		}
		if _, err := s.GetApproval(ctx, approval.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("approval: %v", err)
		}
		if _, err := s.GetThreadSummary(ctx, th.ID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("summary: %v", err)
		}
		if c, _ := s.ListCompactions(ctx, th.ID); len(c) != 0 {
			t.Errorf("compactions %+v", c)
		}
		if c, _ := s.ListCompactions(ctx, keep.ID); len(c) != 1 {
			t.Errorf("the other thread's compactions %+v", c)
		}
		if _, err := s.GetRun(ctx, keepRun.ID); err != nil {
			t.Errorf("the other thread's run: %v", err)
		}
		if m, _ := s.ListMessages(ctx, keep.ID); len(m) != 2 {
			t.Errorf("the other thread's messages %+v", m)
		}
		if ev, _ := s.ListEvents(ctx, keepRun.ID, 0, 10); len(ev) != 1 {
			t.Errorf("the other thread's events %+v", ev)
		}
		if got := artGet(t, s, keptArtifact.ID); got.SHA256 != "sha-keep" {
			t.Errorf("the other thread's artifact %+v", got)
		}
	})

	t.Run("QueuesEveryVersionsContentUnlessAnotherThreadUsesIt", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		doomed, alive := artThread(t, s), artThread(t, s)
		for _, k := range []string{"only-v1", "only-v2", "shared", "preview/only-v2.pdf"} {
			artStoreBlob(t, s, k)
		}
		artPut(t, s, artIn(doomed.ID, "a", "only-v1"))
		artPut(t, s, artIn(doomed.ID, "a", "only-v2"))
		artPut(t, s, artIn(doomed.ID, "b", "shared"))
		artPut(t, s, artIn(alive.ID, "c", "shared"))
		artDeleteThread(t, s, doomed.ID)
		b.Advance(t, retention+time.Hour)

		// Act
		n, err := s.PurgeDeletedThreads(ctx, retention, 10)
		fresh := artCandidates(t, s, artGrace, 10)
		b.Advance(t, artGrace+time.Hour)
		candidates := artCandidates(t, s, artGrace, 10)
		collected, collectErr := s.CollectBlobs(ctx, candidates, artGrace)

		// Assert
		if err != nil || n != 1 || len(fresh) != 0 {
			t.Errorf("purged %d (%v), candidates before the grace %v", n, err, fresh)
		}
		if !slices.Equal(candidates, []string{"only-v1", "only-v2"}) || collectErr != nil || !slices.Equal(collected, candidates) {
			t.Errorf("candidates %v, collected %v (%v)", candidates, collected, collectErr)
		}
		for key, want := range map[string]bool{"only-v1": false, "only-v2": false, "preview/only-v2.pdf": false, "shared": true} {
			if got := artHasBlob(t, s, key); got != want {
				t.Errorf("blob %s present = %v, want %v", key, got, want)
			}
		}
	})

	t.Run("QueuesTheWorkspaceArchiveAndDropsItsIssue", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		artStoreBlob(t, s, "workspace-git/a")
		artMust(t, "PutWorkspaceGit", s.PutWorkspaceGit(ctx, store.WorkspaceGitArchive{ThreadID: th.ID, BlobKey: "workspace-git/a", Head: "h1"}))
		artMust(t, "SetWorkspaceGitIssue", s.SetWorkspaceGitIssue(ctx, store.WorkspaceGitIssue{ThreadID: th.ID, Reason: store.GitIssueFailed}))
		artDeleteThread(t, s, th.ID)
		b.Advance(t, retention+time.Hour)

		// Act
		n, err := s.PurgeDeletedThreads(ctx, retention, 10)
		b.Advance(t, artGrace+time.Hour)
		candidates := artCandidates(t, s, artGrace, 10)
		_, archiveErr := s.WorkspaceGit(ctx, th.ID)
		_, issueErr := s.WorkspaceGitIssue(ctx, th.ID)

		// Assert
		if err != nil || n != 1 || !slices.Equal(candidates, []string{"workspace-git/a"}) {
			t.Errorf("purged %d (%v), candidates %v", n, err, candidates)
		}
		if !errors.Is(archiveErr, store.ErrNotFound) || !errors.Is(issueErr, store.ErrNotFound) {
			t.Errorf("archive %v, issue %v", archiveErr, issueErr)
		}
	})
}

func artSuiteLock(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("IsExclusivePerNameUntilReleased", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		unlock, err := s.AdvisoryLock(ctx, "work:artifacts:t1", time.Second)
		if err != nil {
			t.Fatalf("AdvisoryLock: %v", err)
		}

		// Act
		_, busy := s.AdvisoryLock(ctx, "work:artifacts:t1", 30*time.Millisecond)
		otherUnlock, otherErr := s.AdvisoryLock(ctx, "work:artifacts:t2", time.Second)
		unlock()
		again, againErr := s.AdvisoryLock(ctx, "work:artifacts:t1", time.Second)

		// Assert
		if !errors.Is(busy, store.ErrLockTimeout) {
			t.Errorf("a held lock: %v, want ErrLockTimeout", busy)
		}
		if otherErr != nil || againErr != nil {
			t.Fatalf("another name: %v, after release: %v", otherErr, againErr)
		}
		otherUnlock()
		again()
	})

	t.Run("AWaiterGetsTheLockWhenItIsReleased", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		unlock, err := s.AdvisoryLock(ctx, "name", time.Second)
		if err != nil {
			t.Fatalf("AdvisoryLock: %v", err)
		}
		type result struct {
			unlock func()
			err    error
		}
		got := make(chan result, 1)
		go func() {
			u, err := s.AdvisoryLock(ctx, "name", time.Minute)
			got <- result{u, err}
		}()

		// Act
		unlock()
		r := <-got

		// Assert
		if r.err != nil {
			t.Fatalf("the waiter: %v", r.err)
		}
		r.unlock()
	})

	t.Run("CancellingAWaitingAcquisitionReturnsTheContextError", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		unlock, err := s.AdvisoryLock(ctx, "name", time.Second)
		if err != nil {
			t.Fatalf("AdvisoryLock: %v", err)
		}
		waiting, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := s.AdvisoryLock(waiting, "name", time.Minute)
			done <- err
		}()

		// Act
		cancel()
		waitErr := <-done
		unlock()

		// Assert
		if !errors.Is(waitErr, context.Canceled) {
			t.Errorf("cancelled wait: %v, want context.Canceled", waitErr)
		}
	})

	t.Run("ACancelledWaitReturnsTheContextError", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		unlock, err := s.AdvisoryLock(ctx, "name", time.Second)
		if err != nil {
			t.Fatalf("AdvisoryLock: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()

		// Act
		_, waitErr := s.AdvisoryLock(cancelled, "name", time.Minute)
		unlock()
		again, againErr := s.AdvisoryLock(ctx, "name", time.Second)

		// Assert
		if !errors.Is(waitErr, context.Canceled) {
			t.Errorf("cancelled wait: %v, want context.Canceled", waitErr)
		}
		if againErr != nil {
			t.Fatalf("the cancelled wait left the lock held: %v", againErr)
		}
		again()
	})
}
