package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/core/store"
)

func artSuitePut(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("CreatesTheArtifactAsVersionOne", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		in := artIn(th.ID, "a.txt", "aaa")
		in.RunID, in.Mtime, in.SandboxID = run.ID, 1.5, "sb-1"

		// Act
		a, change, err := s.PutArtifactVersion(ctx, in)

		// Assert
		if err != nil || change != store.ChangeCreated {
			t.Fatalf("PutArtifactVersion: %q %v", change, err)
		}
		if a.ID == "" || a.ThreadID != th.ID || a.UserLogin != "alice" || a.Path != "/workspace/a.txt" || a.Name != "a.txt" ||
			a.MimeType != "text/plain" || a.Kind != store.ArtifactOutput || a.SizeBytes != 3 || a.SHA256 != "aaa" ||
			a.Version != 1 || a.RunID != run.ID || a.SourceMtime != 1.5 || a.SandboxID != "sb-1" || a.Deleted ||
			a.RestoreSkippedAt != nil || a.CreatedAt.IsZero() || a.UpdatedAt.IsZero() {
			t.Errorf("artifact %+v", a)
		}
		got := artGet(t, s, a.ID)
		if got.ID != a.ID || got.SHA256 != "aaa" || got.Version != 1 || got.RunID != run.ID || got.SandboxID != "sb-1" {
			t.Errorf("stored artifact %+v", got)
		}
		versions, err := s.ListArtifactVersions(ctx, a.ID)
		if err != nil || len(versions) != 1 || versions[0].Version != 1 || versions[0].SHA256 != "aaa" || versions[0].SizeBytes != 3 ||
			versions[0].RunID != run.ID || versions[0].CreatedAt.IsZero() {
			t.Errorf("versions %+v (%v)", versions, err)
		}
	})

	t.Run("SameContentOnlyRefreshesTheObservedMtime", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		first := artIn(th.ID, "a.txt", "aaa")
		first.RunID, first.Mtime = run.ID, 1
		created, _ := artPut(t, s, first)
		again := artIn(th.ID, "a.txt", "aaa")
		again.Mtime = 2

		// Act
		a, change := artPut(t, s, again)

		// Assert
		if change != "" || a.ID != created.ID || a.Version != 1 || a.SourceMtime != 2 || a.RunID != run.ID || !a.UpdatedAt.Equal(created.UpdatedAt) {
			t.Errorf("change %q artifact %+v (created %+v)", change, a, created)
		}
		versions, err := s.ListArtifactVersions(ctx, a.ID)
		if err != nil || len(versions) != 1 {
			t.Errorf("versions %+v (%v)", versions, err)
		}
		if got := artGet(t, s, a.ID); got.SourceMtime != 2 {
			t.Errorf("stored mtime %v", got.SourceMtime)
		}
	})

	t.Run("SameContentKeepsTheSandboxUnlessSeenInAnother", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		in := artIn(th.ID, "a.txt", "aaa")
		in.SandboxID = "sb-1"
		a, _ := artPut(t, s, in)
		artMust(t, "MarkArtifactsRestoreSkipped", s.MarkArtifactsRestoreSkipped(ctx, []string{a.ID}))

		// Act
		in.SandboxID = ""
		_, c1 := artPut(t, s, in)
		kept := artGet(t, s, a.ID)
		in.SandboxID = "sb-2"
		_, c2 := artPut(t, s, in)
		moved := artGet(t, s, a.ID)

		// Assert
		if c1 != "" || kept.SandboxID != "sb-1" || kept.RestoreSkippedAt == nil {
			t.Errorf("without a sandbox: change %q %+v", c1, kept)
		}
		if c2 != "" || moved.SandboxID != "sb-2" || moved.RestoreSkippedAt != nil || moved.Version != 1 {
			t.Errorf("seen in sb-2: change %q %+v", c2, moved)
		}
	})

	t.Run("NewContentAddsAVersionAndKeepsTheKind", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		first := artIn(th.ID, "a.txt", "aaa")
		first.SandboxID = "sb-1"
		created, _ := artPut(t, s, first)
		next := artIn(th.ID, "a.txt", "bbbb")
		next.Kind, next.MimeType, next.RunID, next.Mtime = store.ArtifactUpload, "text/markdown", run.ID, 7

		// Act
		a, change := artPut(t, s, next)

		// Assert
		if change != store.ChangeUpdated || a.ID != created.ID || a.Version != 2 || a.SizeBytes != 4 || a.SHA256 != "bbbb" ||
			a.Kind != store.ArtifactOutput || a.MimeType != "text/markdown" || a.RunID != run.ID || a.SourceMtime != 7 ||
			a.SandboxID != "" || a.Deleted || a.UpdatedAt.Before(created.UpdatedAt) {
			t.Errorf("change %q artifact %+v", change, a)
		}
		versions, err := s.ListArtifactVersions(ctx, a.ID)
		if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[0].SHA256 != "bbbb" || versions[0].SizeBytes != 4 ||
			versions[0].RunID != run.ID || versions[1].Version != 1 || versions[1].SHA256 != "aaa" || versions[1].RunID != "" {
			t.Errorf("versions (newest first) %+v (%v)", versions, err)
		}
		v1, err := s.GetArtifactVersion(ctx, a.ID, 1)
		if err != nil || v1.SHA256 != "aaa" || v1.SizeBytes != 3 {
			t.Errorf("version 1 %+v (%v)", v1, err)
		}
		v2, err := s.GetArtifactVersion(ctx, a.ID, 2)
		if err != nil || v2.SHA256 != "bbbb" || v2.RunID != run.ID {
			t.Errorf("version 2 %+v (%v)", v2, err)
		}
	})

	t.Run("ForceRecordsTheSameContentAgain", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		in := artIn(th.ID, "a.txt", "aaa")
		in.SandboxID = "sb-1"
		created, _ := artPut(t, s, in)
		in.Force, in.SandboxID = true, "sb-2"

		// Act
		a, change := artPut(t, s, in)

		// Assert
		if change != store.ChangeUpdated || a.ID != created.ID || a.Version != 2 || a.SHA256 != "aaa" || a.SandboxID != "sb-2" {
			t.Errorf("change %q artifact %+v", change, a)
		}
		versions, err := s.ListArtifactVersions(ctx, a.ID)
		if err != nil || len(versions) != 2 || versions[0].SHA256 != "aaa" || versions[1].SHA256 != "aaa" {
			t.Errorf("versions %+v (%v)", versions, err)
		}
	})

	t.Run("ADeletedArtifactComesBackAsANewVersion", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		in := artIn(th.ID, "a.txt", "aaa")
		in.SandboxID = "sb-1"
		created, _ := artPut(t, s, in)
		since, _ := s.Clock(ctx)
		if _, err := s.MarkArtifactDeleted(ctx, created.ID, "", "sb-1", since.Add(time.Hour)); err != nil {
			t.Fatalf("MarkArtifactDeleted: %v", err)
		}

		// Act
		a, change := artPut(t, s, in) // the very same content

		// Assert
		if change != store.ChangeUpdated || a.ID != created.ID || a.Deleted || a.Version != 2 || a.SHA256 != "aaa" {
			t.Errorf("change %q artifact %+v", change, a)
		}
		if got := artGet(t, s, a.ID); got.Deleted || got.Version != 2 {
			t.Errorf("stored artifact %+v", got)
		}
	})

	t.Run("APathIsOneArtifactPerThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th1, th2 := artThread(t, s), artThread(t, s)

		// Act
		a1, _ := artPut(t, s, artIn(th1.ID, "a.txt", "aaa"))
		a2, c2 := artPut(t, s, artIn(th2.ID, "a.txt", "aaa"))
		a1again, c3 := artPut(t, s, artIn(th1.ID, "a.txt", "aaa"))

		// Assert
		if a1.ID == a2.ID || c2 != store.ChangeCreated || a2.Version != 1 || a2.ThreadID != th2.ID {
			t.Errorf("the same path in another thread must be another artifact: %+v %+v (%q)", a1, a2, c2)
		}
		if a1again.ID != a1.ID || c3 != "" {
			t.Errorf("the same path in the same thread must be the same artifact: %+v (%q)", a1again, c3)
		}
	})

	t.Run("RefusesAnUnknownKindWhenCreating", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		in := artIn(th.ID, "a.txt", "aaa")
		in.Kind = "weird"

		// Act
		_, _, err := s.PutArtifactVersion(ctx, in)

		// Assert
		if err == nil {
			t.Error("an artifact of an unknown kind was created")
		}
		if list, lerr := s.ListArtifacts(ctx, th.ID); lerr != nil || len(list) != 0 {
			t.Errorf("artifacts %+v (%v)", list, lerr)
		}
	})

	t.Run("RefusesAnUnknownThread", func(t *testing.T) {
		// Arrange
		s := f(t).Store

		// Act
		_, _, err := s.PutArtifactVersion(ctx, artIn(uuid.NewString(), "a.txt", "aaa"))

		// Assert
		if err == nil {
			t.Error("an artifact of an unknown thread was created")
		}
	})

	t.Run("ADeletedThreadStillAcceptsVersionsButHidesTheArtifact", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		a, _ := artPut(t, s, artIn(th.ID, "a.txt", "aaa"))
		artDeleteThread(t, s, th.ID)

		// Act
		next, change, err := s.PutArtifactVersion(ctx, artIn(th.ID, "a.txt", "bbb"))
		_, getErr := s.GetArtifact(ctx, a.ID)

		// Assert
		if err != nil || change != store.ChangeUpdated || next.Version != 2 {
			t.Errorf("version on a deleted thread: %q %+v (%v)", change, next, err)
		}
		if !errors.Is(getErr, store.ErrNotFound) {
			t.Errorf("GetArtifact of a deleted thread: %v", getErr)
		}
	})
}

func artSuiteDelete(t *testing.T, f Factory) {
	ctx := context.Background()
	// after is a "listing started" instant later than every write made so far.
	after := func(t *testing.T, s Store) time.Time {
		t.Helper()
		now, err := s.Clock(ctx)
		if err != nil {
			t.Fatalf("Clock: %v", err)
		}
		return now.Add(time.Hour)
	}

	t.Run("FlagsAnArtifactMaterializedInTheSandboxAndUnchanged", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		in := artIn(th.ID, "a.txt", "aaa")
		in.SandboxID = "sb-1"
		created, _ := artPut(t, s, in)

		// Act
		a, err := s.MarkArtifactDeleted(ctx, created.ID, run.ID, "sb-1", after(t, s))

		// Assert
		if err != nil {
			t.Fatalf("MarkArtifactDeleted: %v", err)
		}
		if !a.Deleted || a.RunID != run.ID || a.Version != 1 || a.ID != created.ID || a.UpdatedAt.Before(created.UpdatedAt) {
			t.Errorf("deleted artifact %+v", a)
		}
		if got := artGet(t, s, created.ID); !got.Deleted || got.RunID != run.ID {
			t.Errorf("stored artifact %+v", got)
		}
		if versions, _ := s.ListArtifactVersions(ctx, created.ID); len(versions) != 1 {
			t.Errorf("a deletion is not a version: %+v", versions)
		}
	})

	t.Run("WithoutARunKeepsTheLastOne", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		run := artRun(t, s, th.ID)
		in := artIn(th.ID, "a.txt", "aaa")
		in.RunID, in.SandboxID = run.ID, "sb-1"
		created, _ := artPut(t, s, in)

		// Act
		a, err := s.MarkArtifactDeleted(ctx, created.ID, "", "sb-1", after(t, s))

		// Assert
		if err != nil || !a.Deleted || a.RunID != run.ID {
			t.Errorf("deleted artifact %+v (%v)", a, err)
		}
	})

	t.Run("ConflictsWhenItCannotBeSureTheFileIsGone", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		mk := func(name, sandbox string) *store.Artifact {
			in := artIn(th.ID, name, name)
			in.SandboxID = sandbox
			a, _ := artPut(t, s, in)
			return a
		}
		other, pending, deleted, changed := mk("other", "sb-2"), mk("pending", ""), mk("deleted", "sb-1"), mk("changed", "sb-1")
		since := after(t, s)
		if _, err := s.MarkArtifactDeleted(ctx, deleted.ID, "", "sb-1", since); err != nil {
			t.Fatalf("MarkArtifactDeleted: %v", err)
		}
		listing, _ := s.Clock(ctx) // the listing starts here; the file is rewritten after it
		changedIn := artIn(th.ID, "changed", "changed-v2")
		changedIn.SandboxID = "sb-1"
		artPut(t, s, changedIn)

		// Act
		_, otherErr := s.MarkArtifactDeleted(ctx, other.ID, "", "sb-1", since)
		_, pendingErr := s.MarkArtifactDeleted(ctx, pending.ID, "", "sb-1", since)
		_, noSandboxErr := s.MarkArtifactDeleted(ctx, pending.ID, "", "", since)
		_, deletedErr := s.MarkArtifactDeleted(ctx, deleted.ID, "", "sb-1", since)
		_, changedErr := s.MarkArtifactDeleted(ctx, changed.ID, "", "sb-1", listing)
		_, unknownErr := s.MarkArtifactDeleted(ctx, uuid.NewString(), "", "sb-1", since)
		_, malformedErr := s.MarkArtifactDeleted(ctx, "not-an-id", "", "sb-1", since)

		// Assert
		if malformedErr == nil {
			t.Error("a malformed artifact id was flagged deleted")
		}
		for name, err := range map[string]error{"another sandbox": otherErr, "never materialized": pendingErr, "empty sandbox": noSandboxErr,
			"already deleted": deletedErr, "changed since the listing": changedErr, "unknown artifact": unknownErr} {
			if !errors.Is(err, store.ErrConflict) {
				t.Errorf("%s: %v, want ErrConflict", name, err)
			}
		}
		for _, a := range []*store.Artifact{other, pending, changed} {
			if got := artGet(t, s, a.ID); got.Deleted {
				t.Errorf("%s was flagged deleted", a.Name)
			}
		}
	})

	t.Run("ListsDeletedArtifactsLast", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		in := artIn(th.ID, "old", "o")
		in.SandboxID = "sb-1"
		old, _ := artPut(t, s, in)
		b.Advance(t, time.Hour)
		artPut(t, s, artIn(th.ID, "new", "n"))
		if _, err := s.MarkArtifactDeleted(ctx, old.ID, "", "sb-1", after(t, s)); err != nil {
			t.Fatalf("MarkArtifactDeleted: %v", err)
		}

		// Act
		list, err := s.ListArtifacts(ctx, th.ID)

		// Assert
		if err != nil || len(list) != 2 || list[0].Name != "new" || list[0].Deleted || list[1].Name != "old" || !list[1].Deleted {
			t.Errorf("artifacts (live first) %+v (%v)", list, err)
		}
	})
}

func artSuiteMaterialized(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("RecordsTheSandboxAndClearsTheRestoreSkip", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		a, _ := artPut(t, s, artIn(th.ID, "a", "a"))
		b, _ := artPut(t, s, artIn(th.ID, "b", "b"))
		artMust(t, "MarkArtifactsRestoreSkipped", s.MarkArtifactsRestoreSkipped(ctx, []string{a.ID}))

		// Act
		err := s.MarkArtifactsMaterialized(ctx, "sb-1", []string{a.ID, uuid.NewString(), "not-an-id"})

		// Assert
		if err != nil {
			t.Fatalf("MarkArtifactsMaterialized: %v", err)
		}
		if got := artGet(t, s, a.ID); got.SandboxID != "sb-1" || got.RestoreSkippedAt != nil {
			t.Errorf("materialized artifact %+v", got)
		}
		if got := artGet(t, s, b.ID); got.SandboxID != "" {
			t.Errorf("an artifact not listed was marked: %+v", got)
		}
	})

	t.Run("WithoutASandboxChangesNothing", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		in := artIn(th.ID, "a", "a")
		in.SandboxID = "sb-1"
		a, _ := artPut(t, s, in)
		artMust(t, "MarkArtifactsRestoreSkipped", s.MarkArtifactsRestoreSkipped(ctx, []string{a.ID}))

		// Act
		err := s.MarkArtifactsMaterialized(ctx, "", []string{a.ID})

		// Assert
		if got := artGet(t, s, a.ID); err != nil || got.SandboxID != "sb-1" || got.RestoreSkippedAt == nil {
			t.Errorf("artifact %+v (%v)", got, err)
		}
	})

	t.Run("RestoreSkipKeepsTheFirstTimeUntilMaterialized", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th := artThread(t, s)
		a, _ := artPut(t, s, artIn(th.ID, "a", "a"))

		// Act
		err1 := s.MarkArtifactsRestoreSkipped(ctx, []string{a.ID, uuid.NewString(), "not-an-id"})
		first := artGet(t, s, a.ID).RestoreSkippedAt
		b.Advance(t, time.Hour)
		err2 := s.MarkArtifactsRestoreSkipped(ctx, []string{a.ID})
		second := artGet(t, s, a.ID).RestoreSkippedAt
		err3 := s.MarkArtifactsRestoreSkipped(ctx, nil)
		now, clockErr := s.Clock(ctx)

		// Assert
		if err1 != nil || err2 != nil || err3 != nil || clockErr != nil {
			t.Fatalf("errors %v %v %v %v", err1, err2, err3, clockErr)
		}
		if first == nil || second == nil || now.Sub(*second) < 59*time.Minute {
			t.Errorf("skip times %v then %v at %v: the first time must be kept, so it is an hour old now", first, second, now)
		}
	})

	t.Run("AGetReturnsACopy", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		a, _ := artPut(t, s, artIn(th.ID, "a", "a"))
		artMust(t, "MarkArtifactsRestoreSkipped", s.MarkArtifactsRestoreSkipped(ctx, []string{a.ID}))
		got := artGet(t, s, a.ID)
		skipped := *got.RestoreSkippedAt

		// Act
		got.Name, got.Deleted = "changed", true
		*got.RestoreSkippedAt = time.Time{}

		// Assert
		again := artGet(t, s, a.ID)
		if again.Name != "a" || again.Deleted || again.RestoreSkippedAt == nil || !again.RestoreSkippedAt.Equal(skipped) {
			t.Errorf("the store followed the caller's changes: %+v", again)
		}
		list, _ := s.ListArtifacts(ctx, th.ID)
		if len(list) != 1 || list[0].RestoreSkippedAt == nil {
			t.Fatalf("list %+v", list)
		}
		*list[0].RestoreSkippedAt = time.Time{}
		if again := artGet(t, s, a.ID); !again.RestoreSkippedAt.Equal(skipped) {
			t.Errorf("a listed artifact shares memory with the store: %+v", again)
		}
	})
}

func artSuiteListing(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("ListsAThreadsArtifactsNewestFirst", func(t *testing.T) {
		// Arrange
		b := f(t)
		s := b.Store
		th, other := artThread(t, s), artThread(t, s)
		artPut(t, s, artIn(th.ID, "a", "a"))
		b.Advance(t, 2*time.Hour)
		artPut(t, s, artIn(th.ID, "b", "b"))
		b.Advance(t, time.Hour)
		artPut(t, s, artIn(th.ID, "c", "c"))
		artPut(t, s, artIn(other.ID, "z", "z"))

		// Act
		list, err := s.ListArtifacts(ctx, th.ID)

		// Assert
		var names []string
		for _, a := range list {
			names = append(names, a.Name)
		}
		if err != nil || !artSameStrings(names, []string{"c", "b", "a"}) {
			t.Errorf("artifacts %s (%v), want [c b a]", artStrings(names), err)
		}
	})

	t.Run("ListsNothingForAThreadWithoutArtifacts", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)

		// Act
		empty, err1 := s.ListArtifacts(ctx, th.ID)
		unknown, err2 := s.ListArtifacts(ctx, uuid.NewString())

		// Assert
		if err1 != nil || err2 != nil || len(empty) != 0 || len(unknown) != 0 {
			t.Errorf("listings %+v %+v (%v %v)", empty, unknown, err1, err2)
		}
	})

	t.Run("AnUnknownArtifactIsNotFound", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		a, _ := artPut(t, s, artIn(th.ID, "a", "a"))

		// Act
		_, getErr := s.GetArtifact(ctx, uuid.NewString())
		_, notID := s.GetArtifact(ctx, "not-an-id")
		_, versionErr := s.GetArtifactVersion(ctx, a.ID, 9)
		_, noArtifact := s.GetArtifactVersion(ctx, uuid.NewString(), 1)
		versions, listErr := s.ListArtifactVersions(ctx, uuid.NewString())
		malformedVersions, _ := s.ListArtifactVersions(ctx, "not-an-id")
		malformedList, _ := s.ListArtifacts(ctx, "not-an-id")

		// Assert
		for name, err := range map[string]error{"unknown id": getErr, "malformed id": notID, "unknown version": versionErr, "version of an unknown artifact": noArtifact} {
			if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("%s: %v, want ErrNotFound", name, err)
			}
		}
		if len(malformedVersions) != 0 || len(malformedList) != 0 {
			t.Errorf("malformed ids list %v %v", malformedVersions, malformedList)
		}
		if listErr != nil || len(versions) != 0 {
			t.Errorf("versions of an unknown artifact: %+v (%v)", versions, listErr)
		}
	})
}

func artSuiteClock(t *testing.T, f Factory) {
	ctx := context.Background()

	t.Run("IsTheReferenceOfUpdatedAt", func(t *testing.T) {
		// Arrange
		s := f(t).Store
		th := artThread(t, s)
		first, _ := artPut(t, s, artIn(th.ID, "a", "a"))

		// Act
		c1, err1 := s.Clock(ctx)
		second, _ := artPut(t, s, artIn(th.ID, "a", "b"))
		c2, err2 := s.Clock(ctx)

		// Assert
		if err1 != nil || err2 != nil {
			t.Fatalf("Clock: %v %v", err1, err2)
		}
		if first.UpdatedAt.After(c1) || second.UpdatedAt.Before(c1) || c2.Before(second.UpdatedAt) {
			t.Errorf("updated_at %v then %v, clock %v then %v", first.UpdatedAt, second.UpdatedAt, c1, c2)
		}
	})
}
