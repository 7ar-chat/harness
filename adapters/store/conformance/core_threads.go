package conformance

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func coreSuiteThreads(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("CreateThreadStartsEmptyAndOwnedByItsLogin", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		titled := coreNewThread(t, s, "alice", "Plans")
		untitled := coreNewThread(t, s, "alice", "")

		// Assert
		if titled.ID == "" || titled.ID == untitled.ID {
			t.Fatalf("ids %q %q", titled.ID, untitled.ID)
		}
		if titled.UserLogin != "alice" || titled.Title != "Plans" || titled.TitleSource != store.TitleUser {
			t.Fatalf("titled thread %+v", titled)
		}
		if untitled.Title != "" || untitled.TitleSource != store.TitleAuto {
			t.Fatalf("untitled thread %+v", untitled)
		}
		if titled.CreatedAt.IsZero() || titled.UpdatedAt.IsZero() || titled.PinnedAt != nil || titled.ActiveRun != nil ||
			titled.LastRun != nil || titled.Unread || titled.SandboxID != "" {
			t.Fatalf("a new thread has no pin, run, sandbox or unread mark: %+v", titled)
		}
		got := coreGetThread(t, s, "alice", titled.ID)
		if got.ID != titled.ID || got.Title != "Plans" {
			t.Fatalf("GetThread %+v", got)
		}
	})

	t.Run("ThreadsAreInvisibleToOtherLogins", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "mine")
		title := "stolen"
		pin := true

		// Act
		_, getErr := s.GetThread(ctx, "bob", th.ID)
		_, updErr := s.UpdateThread(ctx, "bob", th.ID, store.ThreadPatch{Title: &title, Pinned: &pin})
		_, delErr := s.DeleteThread(ctx, "bob", th.ID)
		bobs, listErr := s.ListThreads(ctx, "bob", 10, nil)

		// Assert
		for name, err := range map[string]error{"get": getErr, "update": updErr, "delete": delErr} {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s by another login: %v, want ErrNotFound", name, err)
			}
		}
		if listErr != nil || len(bobs) != 0 {
			t.Errorf("bob lists %d threads (%v)", len(bobs), listErr)
		}
		untouched := coreGetThread(t, s, "alice", th.ID)
		if untouched.Title != "mine" || untouched.PinnedAt != nil {
			t.Errorf("another login's update leaked: %+v", untouched)
		}
	})

	t.Run("UnknownThreadIdsAreNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		title := "x"

		for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
			// Act
			_, getErr := s.GetThread(ctx, "alice", id)
			_, updErr := s.UpdateThread(ctx, "alice", id, store.ThreadPatch{Title: &title})
			_, delErr := s.DeleteThread(ctx, "alice", id)
			seenErr := s.MarkThreadSeen(ctx, "alice", id)

			// Assert
			for name, err := range map[string]error{"get": getErr, "update": updErr, "delete": delErr, "seen": seenErr} {
				if !errors.Is(err, store.ErrNotFound) {
					t.Errorf("%s of %q: %v, want ErrNotFound", name, id, err)
				}
			}
		}
	})

	t.Run("UpdateThreadPatchesTitleAndPin", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "")
		title := "Renamed"
		yes, no := true, false

		// Act
		renamed, err1 := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Title: &title})
		pinned, err2 := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Pinned: &yes})
		repinned, err3 := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Pinned: &yes})
		unpinned, err4 := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{Pinned: &no})
		noop, err5 := s.UpdateThread(ctx, "alice", th.ID, store.ThreadPatch{})

		// Assert
		for i, err := range []error{err1, err2, err3, err4, err5} {
			if err != nil {
				t.Fatalf("UpdateThread #%d: %v", i+1, err)
			}
		}
		if renamed.Title != title || renamed.TitleSource != store.TitleUser || renamed.PinnedAt != nil {
			t.Errorf("rename: %+v", renamed)
		}
		if pinned.PinnedAt == nil || pinned.Title != title {
			t.Errorf("pin: %+v", pinned)
		}
		if repinned.PinnedAt == nil || !repinned.PinnedAt.Equal(*pinned.PinnedAt) {
			t.Errorf("pinning a pinned thread must keep its first pin time: %v then %v", pinned.PinnedAt, repinned.PinnedAt)
		}
		if unpinned.PinnedAt != nil {
			t.Errorf("unpin: %+v", unpinned)
		}
		if noop.Title != title || noop.PinnedAt != nil {
			t.Errorf("an empty patch changes nothing: %+v", noop)
		}
		if got := coreGetThread(t, s, "alice", th.ID); got.Title != title || got.PinnedAt != nil {
			t.Errorf("stored thread %+v", got)
		}
	})

	t.Run("ListThreadsOrdersPinnedFirstThenNewestFirst", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		a := coreNewThread(t, s, "alice", "a")
		b := coreNewThread(t, s, "alice", "b")
		c := coreNewThread(t, s, "alice", "c")
		coreNewThread(t, s, "bob", "bobs")
		yes := true

		// Act
		plain, err1 := s.ListThreads(ctx, "alice", 10, nil)
		_, err2 := s.UpdateThread(ctx, "alice", a.ID, store.ThreadPatch{Pinned: &yes})
		pinnedFirst, err3 := s.ListThreads(ctx, "alice", 10, nil)

		// Assert
		for _, err := range []error{err1, err2, err3} {
			if err != nil {
				t.Fatal(err)
			}
		}
		if got, want := coreThreadIDs(plain), []string{c.ID, b.ID, a.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("newest first: got %v want %v", got, want)
		}
		if got, want := coreThreadIDs(pinnedFirst), []string{a.ID, c.ID, b.ID}; !reflect.DeepEqual(got, want) {
			t.Errorf("pinned first: got %v want %v", got, want)
		}
		if pinnedFirst[0].PinnedAt == nil {
			t.Errorf("a listed pinned thread carries its pin time")
		}
	})

	t.Run("ListThreadsPagesWithACursorWithoutGapsOrRepeats", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		var created []*store.Thread
		for range 5 {
			created = append(created, coreNewThread(t, s, "alice", ""))
		}
		yes := true
		if _, err := s.UpdateThread(ctx, "alice", created[1].ID, store.ThreadPatch{Pinned: &yes}); err != nil {
			t.Fatal(err)
		}
		all, err := s.ListThreads(ctx, "alice", 100, nil)
		if err != nil || len(all) != 5 {
			t.Fatalf("full list: %d %v", len(all), err)
		}

		// Act
		var paged []*store.Thread
		var cursor *store.ThreadCursor
		for range 10 {
			page, err := s.ListThreads(ctx, "alice", 2, cursor)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) > 2 {
				t.Fatalf("page of %d, limit 2", len(page))
			}
			if len(page) == 0 {
				break
			}
			paged = append(paged, page...)
			cursor = coreCursor(page[len(page)-1])
		}

		// Assert
		if got, want := coreThreadIDs(paged), coreThreadIDs(all); !reflect.DeepEqual(got, want) {
			t.Errorf("paged %v, full list %v", got, want)
		}
		if all[0].ID != created[1].ID {
			t.Errorf("the pinned thread leads the list")
		}
	})

	t.Run("ListThreadsHonoursTheLimit", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		for range 3 {
			coreNewThread(t, s, "alice", "")
		}

		// Act
		two, err := s.ListThreads(ctx, "alice", 2, nil)

		// Assert
		if err != nil || len(two) != 2 {
			t.Fatalf("limit 2: %d threads, %v", len(two), err)
		}
	})

	t.Run("DeleteThreadHidesTheThreadAndReturnsItAsItWas", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := coreNewThread(t, s, "alice", "doomed")
		run := coreStart(t, s, th, "rt")
		if err := s.SetThreadSandbox(ctx, th.ID, "", "sbx-1"); err != nil {
			t.Fatal(err)
		}
		keep := coreNewThread(t, s, "alice", "keep")

		// Act
		deleted, err := s.DeleteThread(ctx, "alice", th.ID)
		_, getErr := s.GetThread(ctx, "alice", th.ID)
		_, again := s.DeleteThread(ctx, "alice", th.ID)
		listed, listErr := s.ListThreads(ctx, "alice", 10, nil)
		_, _, createErr := s.CreateRun(ctx, coreInput(th.ID, "alice", "late"))

		// Assert
		if err != nil {
			t.Fatalf("DeleteThread: %v", err)
		}
		if deleted.ID != th.ID || deleted.SandboxID != "sbx-1" || deleted.ActiveRun == nil || deleted.ActiveRun.ID != run.ID {
			t.Errorf("the deleted thread is returned with its sandbox and active run so the caller can stop them: %+v", deleted)
		}
		if !errors.Is(getErr, store.ErrNotFound) || !errors.Is(again, store.ErrNotFound) {
			t.Errorf("get %v, second delete %v, want ErrNotFound", getErr, again)
		}
		if listErr != nil || !reflect.DeepEqual(coreThreadIDs(listed), []string{keep.ID}) {
			t.Errorf("list after delete: %v %v", coreThreadIDs(listed), listErr)
		}
		if !errors.Is(createErr, store.ErrNotFound) {
			t.Errorf("a run on a deleted thread: %v, want ErrNotFound", createErr)
		}
	})
}
