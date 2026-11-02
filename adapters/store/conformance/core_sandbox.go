package conformance

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
)

func coreSuiteSandbox(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("ThreadSandboxIsEmptyUntilRecorded", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")

		// Act
		id, err := s.ThreadSandbox(ctx, th.ID)

		// Assert
		if err != nil || id != "" {
			t.Fatalf("got %q, %v", id, err)
		}
	})

	t.Run("SetThreadSandboxIsACompareAndSet", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")

		// Act
		err1 := s.SetThreadSandbox(ctx, th.ID, "", "sbx-1")
		err2 := s.SetThreadSandbox(ctx, th.ID, "", "sbx-2")      // stale expectation: ignored
		err3 := s.SetThreadSandbox(ctx, th.ID, "wrong", "sbx-3") // wrong expectation: ignored
		afterStale, _ := s.ThreadSandbox(ctx, th.ID)
		err4 := s.SetThreadSandbox(ctx, th.ID, "sbx-1", "sbx-4") // right expectation: replaced
		afterSwap, _ := s.ThreadSandbox(ctx, th.ID)
		err5 := s.SetThreadSandbox(ctx, th.ID, "sbx-4", "") // cleared
		afterClear, _ := s.ThreadSandbox(ctx, th.ID)

		// Assert
		for _, err := range []error{err1, err2, err3, err4, err5} {
			if err != nil {
				t.Fatalf("a lost compare-and-set is not an error: %v", err)
			}
		}
		if afterStale != "sbx-1" || afterSwap != "sbx-4" || afterClear != "" {
			t.Errorf("sandbox ids %q %q %q", afterStale, afterSwap, afterClear)
		}
		if got := coreGetThread(t, s, "alice", th.ID); got.SandboxID != "" {
			t.Errorf("thread sandbox %q", got.SandboxID)
		}
	})

	t.Run("SwapThreadSandboxReportsWhetherItChangedTheRecord", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")

		// Act
		first, err1 := s.SwapThreadSandbox(ctx, th.ID, "", "sbx-1")
		lostRace, err2 := s.SwapThreadSandbox(ctx, th.ID, "", "sbx-2")
		replaced, err3 := s.SwapThreadSandbox(ctx, th.ID, "sbx-1", "sbx-3")
		cleared, err4 := s.SwapThreadSandbox(ctx, th.ID, "sbx-3", "")
		unknown, err5 := s.SwapThreadSandbox(ctx, uuid.NewString(), "", "sbx-x")

		// Assert
		for _, err := range []error{err1, err2, err3, err4, err5} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if !first || lostRace || !replaced || !cleared || unknown {
			t.Errorf("swaps: %v %v %v %v %v", first, lostRace, replaced, cleared, unknown)
		}
		if id, _ := s.ThreadSandbox(ctx, th.ID); id != "" {
			t.Errorf("sandbox after clearing: %q", id)
		}
	})

	t.Run("DeletedAndUnknownThreadsHaveNoSandbox", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		if err := s.SetThreadSandbox(ctx, th.ID, "", "sbx-1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.DeleteThread(ctx, "alice", th.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		_, deletedErr := s.ThreadSandbox(ctx, th.ID)
		_, unknownErr := s.ThreadSandbox(ctx, uuid.NewString())
		swapped, swapErr := s.SwapThreadSandbox(ctx, th.ID, "", "sbx-2")
		setErr := s.SetThreadSandbox(ctx, th.ID, "", "sbx-3")
		count, countErr := s.CountUserSandboxes(ctx, "alice")
		list, listErr := s.UserSandboxes(ctx, "alice")

		// Assert
		if !errors.Is(deletedErr, sandbox.ErrThreadNotFound) || !errors.Is(unknownErr, sandbox.ErrThreadNotFound) {
			t.Errorf("deleted %v, unknown %v, want ErrThreadNotFound", deletedErr, unknownErr)
		}
		if swapErr != nil || swapped || setErr != nil {
			t.Errorf("a deleted thread keeps no sandbox: swapped %v (%v) set %v", swapped, swapErr, setErr)
		}
		if countErr != nil || count != 0 || listErr != nil || len(list) != 0 {
			t.Errorf("deleting the thread releases its sandbox from the quota: %d %v / %v %v", count, countErr, list, listErr)
		}
	})

	t.Run("UserSandboxesListsLiveThreadsOfOneUser", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreNewThread(t, s, "alice", "")
		b := coreNewThread(t, s, "alice", "")
		none := coreNewThread(t, s, "alice", "")
		gone := coreNewThread(t, s, "alice", "")
		bob := coreNewThread(t, s, "bob", "")
		for th, id := range map[*store.Thread]string{a: "sbx-a", b: "sbx-b", gone: "sbx-gone", bob: "sbx-bob"} {
			if err := s.SetThreadSandbox(ctx, th.ID, "", id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.DeleteThread(ctx, "alice", gone.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		count, countErr := s.CountUserSandboxes(ctx, "alice")
		list, listErr := s.UserSandboxes(ctx, "alice")
		bobCount, _ := s.CountUserSandboxes(ctx, "bob")
		nobody, nobodyErr := s.UserSandboxes(ctx, "carol")
		nobodyCount, _ := s.CountUserSandboxes(ctx, "carol")

		// Assert
		if countErr != nil || listErr != nil || nobodyErr != nil {
			t.Fatalf("errors %v %v %v", countErr, listErr, nobodyErr)
		}
		got := map[string]string{}
		for _, u := range list {
			got[u.ThreadID] = u.SandboxID
		}
		want := map[string]string{a.ID: "sbx-a", b.ID: "sbx-b"}
		if !reflect.DeepEqual(got, want) || len(list) != 2 {
			t.Errorf("alice's sandboxes %v, want %v (not %s without sandbox)", list, want, none.ID)
		}
		if count != 2 || bobCount != 1 || nobodyCount != 0 || len(nobody) != 0 {
			t.Errorf("counts alice %d bob %d carol %d", count, bobCount, nobodyCount)
		}
	})
}

func coreSuiteNotifications(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("AFinishedRunMakesTheThreadUnreadUntilSeen", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		quiet := coreNewThread(t, s, "alice", "quiet")
		th := coreNewThread(t, s, "alice", "job")
		before := coreGetThread(t, s, "alice", th.ID)
		run := coreFinish(t, s, th, store.StatusCompleted)

		// Act
		unread := coreGetThread(t, s, "alice", th.ID)
		count, countErr := s.CountUnreadThreads(ctx, "alice")
		listed, listErr := s.SearchUnreadThreads(ctx, "alice", "", 10, nil)
		seenErr := s.MarkThreadSeen(ctx, "alice", th.ID)
		seen := coreGetThread(t, s, "alice", th.ID)
		after, afterErr := s.CountUnreadThreads(ctx, "alice")
		idle := coreGetThread(t, s, "alice", quiet.ID)

		// Assert
		if countErr != nil || listErr != nil || seenErr != nil || afterErr != nil {
			t.Fatalf("errors %v %v %v %v", countErr, listErr, seenErr, afterErr)
		}
		if before.Unread || before.LastRun != nil {
			t.Errorf("a fresh thread is read and has no last run: %+v", before)
		}
		if !unread.Unread || unread.LastRun == nil || unread.LastRun.ID != run.ID ||
			unread.LastRun.Status != store.StatusCompleted || unread.LastRun.FinishedAt == nil {
			t.Errorf("after the run ended: %+v lastRun %+v", unread, unread.LastRun)
		}
		if count != 1 || len(listed) != 1 || listed[0].ID != th.ID || !listed[0].Unread {
			t.Errorf("count %d listed %v", count, coreThreadIDs(listed))
		}
		if seen.Unread || after != 0 || seen.LastSeenAt == nil || seen.LastRun == nil || seen.LastRun.ID != run.ID {
			t.Errorf("after MarkThreadSeen: %+v count %d", seen, after)
		}
		if idle.Unread || idle.LastRun != nil {
			t.Errorf("a thread without runs stays read: %+v", idle)
		}
	})

	t.Run("ARunStillInFlightDoesNotMakeTheThreadUnread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		queued := coreNewThread(t, s, "alice", "")
		running := coreNewThread(t, s, "alice", "")
		coreQueue(t, s, queued, "x")
		coreStart(t, s, running, "rt")

		// Act
		count, err := s.CountUnreadThreads(ctx, "alice")

		// Assert
		if err != nil || count != 0 {
			t.Fatalf("count %d, %v", count, err)
		}
		got := coreGetThread(t, s, "alice", running.ID)
		if got.Unread || got.LastRun == nil || got.LastRun.Status != store.StatusRunning || got.LastRun.FinishedAt != nil {
			t.Errorf("running thread %+v lastRun %+v", got, got.LastRun)
		}
	})

	t.Run("EveryTerminalStatusMakesTheThreadUnread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		for _, status := range []string{store.StatusCompleted, store.StatusFailed, store.StatusCancelled, store.StatusInterrupted} {
			th := coreNewThread(t, s, "alice", status)
			coreFinish(t, s, th, status)

			// Act
			got := coreGetThread(t, s, "alice", th.ID)

			// Assert
			if !got.Unread || got.LastRun == nil || got.LastRun.Status != status {
				t.Errorf("%s: %+v lastRun %+v", status, got, got.LastRun)
			}
		}
		if n, err := s.CountUnreadThreads(ctx, "alice"); err != nil || n != 4 {
			t.Errorf("count %d, %v", n, err)
		}
	})

	t.Run("ACancelledQueuedRunIsUnread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		run := coreQueue(t, s, th, "x")

		// Act
		ok, err := s.CancelQueuedRun(ctx, run.ID)

		// Assert
		got := coreGetThread(t, s, "alice", th.ID)
		if err != nil || !ok || !got.Unread || got.LastRun == nil || got.LastRun.Status != store.StatusCancelled {
			t.Fatalf("cancel %v %v: %+v lastRun %+v", ok, err, got, got.LastRun)
		}
	})

	t.Run("LastRunIsTheLatestCreatedRun", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		coreFinish(t, s, th, store.StatusFailed)
		second := coreFinish(t, s, th, store.StatusCompleted)
		third := coreQueue(t, s, th, "pending")

		// Act
		got := coreGetThread(t, s, "alice", th.ID)

		// Assert
		if got.LastRun == nil || got.LastRun.ID != third.ID || got.LastRun.Status != store.StatusQueued {
			t.Fatalf("lastRun %+v, want the queued run %s (after %s)", got.LastRun, third.ID, second.ID)
		}
		if !got.Unread {
			t.Errorf("earlier finished runs keep the thread unread")
		}
	})

	t.Run("UnreadCountsAndSearchAreScopedToTheUser", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		mine := coreNewThread(t, s, "alice", "mine")
		deleted := coreNewThread(t, s, "alice", "deleted")
		theirs := coreNewThread(t, s, "bob", "theirs")
		for _, th := range []*store.Thread{mine, deleted, theirs} {
			coreFinish(t, s, th, store.StatusCompleted)
		}
		if _, err := s.DeleteThread(ctx, "alice", deleted.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		aliceCount, err1 := s.CountUnreadThreads(ctx, "alice")
		bobCount, err2 := s.CountUnreadThreads(ctx, "bob")
		nobody, err3 := s.CountUnreadThreads(ctx, "carol")
		alice, err4 := s.SearchUnreadThreads(ctx, "alice", "", 10, nil)
		seenErr := s.MarkThreadSeen(ctx, "bob", mine.ID)

		// Assert
		for _, err := range []error{err1, err2, err3, err4} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if aliceCount != 1 || bobCount != 1 || nobody != 0 {
			t.Errorf("counts alice %d bob %d carol %d", aliceCount, bobCount, nobody)
		}
		if !reflect.DeepEqual(coreThreadIDs(alice), []string{mine.ID}) {
			t.Errorf("alice's unread %v", coreThreadIDs(alice))
		}
		if !errors.Is(seenErr, store.ErrNotFound) {
			t.Errorf("marking another user's thread seen: %v, want ErrNotFound", seenErr)
		}
		if got := coreGetThread(t, s, "alice", mine.ID); !got.Unread {
			t.Errorf("another user's MarkThreadSeen cleared my unread mark")
		}
	})

	t.Run("MarkThreadSeenOfADeletedThreadIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		if _, err := s.DeleteThread(ctx, "alice", th.ID); err != nil {
			t.Fatal(err)
		}

		// Act
		err := s.MarkThreadSeen(ctx, "alice", th.ID)

		// Assert
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("SearchUnreadThreadsFiltersPagesAndLimits", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		var unread []*store.Thread
		for _, title := range []string{"Alpha report", "Beta report", "Gamma notes", "Delta report"} {
			th := coreNewThread(t, s, "alice", title)
			coreFinish(t, s, th, store.StatusCompleted)
			unread = append(unread, th)
		}
		readOne := coreNewThread(t, s, "alice", "Epsilon report") // read: never finished a run
		if got := unread[1]; got != nil {
			if err := s.MarkThreadSeen(ctx, "alice", got.ID); err != nil {
				t.Fatal(err)
			}
		}

		// Act
		all, err1 := s.SearchUnreadThreads(ctx, "alice", "", 10, nil)
		reports, err2 := s.SearchUnreadThreads(ctx, "alice", "REPORT", 10, nil)
		limited, err3 := s.SearchUnreadThreads(ctx, "alice", "", 2, nil)
		next, err4 := s.SearchUnreadThreads(ctx, "alice", "", 2, coreCursor(limited[len(limited)-1]))
		none, err5 := s.SearchUnreadThreads(ctx, "alice", "zzz-no-match", 10, nil)

		// Assert
		for _, err := range []error{err1, err2, err3, err4, err5} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if want := []string{unread[3].ID, unread[2].ID, unread[0].ID}; !reflect.DeepEqual(coreThreadIDs(all), want) {
			t.Errorf("unread newest first (not the seen %s nor the read %s): %v, want %v", unread[1].ID, readOne.ID, coreThreadIDs(all), want)
		}
		if want := []string{unread[3].ID, unread[0].ID}; !reflect.DeepEqual(coreThreadIDs(reports), want) {
			t.Errorf("search by title, case-insensitive: %v, want %v", coreThreadIDs(reports), want)
		}
		if want := []string{unread[3].ID, unread[2].ID}; !reflect.DeepEqual(coreThreadIDs(limited), want) {
			t.Errorf("limit 2: %v, want %v", coreThreadIDs(limited), want)
		}
		if want := []string{unread[0].ID}; !reflect.DeepEqual(coreThreadIDs(next), want) {
			t.Errorf("next page: %v, want %v", coreThreadIDs(next), want)
		}
		if len(none) != 0 {
			t.Errorf("no match: %v", coreThreadIDs(none))
		}
	})
}
