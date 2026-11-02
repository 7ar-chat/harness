package gitsafe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/store"
)

func TestToplevelReportsNotARepoWhenGitIsNotInstalled(t *testing.T) {
	// Arrange: a real repository, but no git binary on PATH.
	dir := newRepo(t)
	t.Setenv("PATH", t.TempDir())

	// Act
	_, err := Toplevel(context.Background(), dir)

	// Assert
	if !errors.Is(err, ErrNotRepo) {
		t.Fatalf("Toplevel err = %v, want ErrNotRepo", err)
	}
}

func TestEveryEntryPointReportsNotARepoOutsideARepository(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dir := t.TempDir()

	// Act
	_, latestErr := Latest(ctx, dir, "t")
	_, _, undoErr := Undo(ctx, dir, "t")
	dropErr := Drop(ctx, dir, "t")
	recordErr := RecordRun(ctx, dir, "t", "r", Snap{Commit: "abc"})
	_, forRunErr := ForRun(ctx, dir, "t", "r")
	_, changedErr := Changed(ctx, dir, "a", "b")

	// Assert
	for name, err := range map[string]error{"Latest": latestErr, "Undo": undoErr, "Drop": dropErr, "RecordRun": recordErr, "ForRun": forRunErr, "Changed": changedErr} {
		if !errors.Is(err, ErrNotRepo) {
			t.Errorf("%s err = %v, want ErrNotRepo", name, err)
		}
	}
}

func TestLatestAndUndoFailWhenTheThreadHasNoSnapshot(t *testing.T) {
	// Arrange: a snapshot of another thread must not count.
	ctx := context.Background()
	dir := newRepo(t)
	if _, err := Snapshot(ctx, dir, "other"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "a.txt", "keep me\n")

	// Act
	_, latestErr := Latest(ctx, dir, "t")
	_, _, undoErr := Undo(ctx, dir, "t")

	// Assert
	if latestErr == nil || !strings.Contains(latestErr.Error(), "no snapshot") {
		t.Fatalf("Latest err = %v", latestErr)
	}
	if undoErr == nil {
		t.Fatal("Undo without a snapshot succeeded")
	}
	if got, _ := read(t, dir, "a.txt"); got != "keep me\n" {
		t.Errorf("a failed undo changed the tree: %q", got)
	}
}

func TestRestoreRefusesACommitThatIsNotAHarnessSnapshot(t *testing.T) {
	// Arrange: an ordinary commit exists, but it was not made by Snapshot.
	dir := newRepo(t)
	write(t, dir, "a.txt", "keep me\n")

	// Act
	_, err := Restore(context.Background(), dir, "HEAD")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "not a harness snapshot") {
		t.Fatalf("Restore(HEAD) err = %v", err)
	}
	if got, _ := read(t, dir, "a.txt"); got != "keep me\n" {
		t.Errorf("a refused restore changed the tree: %q", got)
	}
	if refs := run(t, dir, "for-each-ref", "refs/harness/"); refs != "" {
		t.Errorf("a refused restore left a backup: %q", refs)
	}
}

func TestRestoreChangesNothingWhenTheBackupCannotBeSaved(t *testing.T) {
	// Arrange: the backup namespace of the thread is blocked by a ref of the same name.
	ctx := context.Background()
	dir := newRepo(t)
	snap, err := Snapshot(ctx, dir, "t")
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, "update-ref", "refs/harness/backups/t", snap.Commit)
	write(t, dir, "a.txt", "agent work\n")

	// Act
	_, err = Restore(ctx, dir, snap.Ref)

	// Assert: the agent's work is not thrown away without a way back.
	if err == nil || !strings.Contains(err.Error(), "saving the current state first") {
		t.Fatalf("Restore err = %v", err)
	}
	if got, _ := read(t, dir, "a.txt"); got != "agent work\n" {
		t.Errorf("a.txt = %q, want the agent's version", got)
	}
}

func TestSnapshotFailsCleanlyWhenTheRefCannotBeCreated(t *testing.T) {
	// Arrange: the thread's snapshot namespace is blocked by a ref of the same name.
	ctx := context.Background()
	dir := newRepo(t)
	run(t, dir, "update-ref", "refs/harness/snapshots/t", run(t, dir, "rev-parse", "HEAD"))
	status := run(t, dir, "status", "--porcelain")

	// Act
	_, err := Snapshot(ctx, dir, "t")

	// Assert
	if err == nil {
		t.Fatal("Snapshot succeeded without a ref to keep the commit alive")
	}
	if got := run(t, dir, "status", "--porcelain"); got != status {
		t.Errorf("status changed:\n%s", got)
	}
}

func TestSnapshotFailsWhenTheTempDirectoryIsUnusable(t *testing.T) {
	// Arrange: the private index copies cannot be created.
	dir := newRepo(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	index, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))

	// Act
	_, err := Snapshot(context.Background(), dir, "t")

	// Assert
	if err == nil {
		t.Fatal("Snapshot succeeded without a temporary directory")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".git", "index")); string(got) != string(index) {
		t.Error("the user's index was rewritten")
	}
}

func TestForRunReportsASnapshotThatIsGone(t *testing.T) {
	// Arrange: the run ref survives, the numbered snapshot ref was deleted.
	ctx := context.Background()
	dir := newRepo(t)
	s, _ := Snapshot(ctx, dir, "th")
	if err := RecordRun(ctx, dir, "th", "run-1", s); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "update-ref", "-d", s.Ref)

	// Act
	_, err := ForRun(ctx, dir, "th", "run-1")

	// Assert
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("ForRun err = %v", err)
	}
}

func TestChangedRejectsAnUnknownRef(t *testing.T) {
	// Arrange
	dir := newRepo(t)
	s, _ := Snapshot(context.Background(), dir, "th")

	// Act
	files, err := Changed(context.Background(), dir, s.Ref, "refs/harness/snapshots/th/99")

	// Assert
	if err == nil || files != nil {
		t.Fatalf("Changed = %v, %v", files, err)
	}
}

func TestChangedIsEmptyForIdenticalSnapshots(t *testing.T) {
	// Arrange
	dir := newRepo(t)
	s, _ := Snapshot(context.Background(), dir, "th")

	// Act
	files, err := Changed(context.Background(), dir, s.Ref, s.Ref)

	// Assert
	if err != nil || len(files) != 0 {
		t.Fatalf("Changed = %v, %v", files, err)
	}
}

func TestHookSnapshotsTheRepositoryBeforeAChatRunAndRecordsTheRun(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dir := newRepo(t)
	write(t, dir, "a.txt", "before the run\n")
	h := Hook{Dir: dir}

	// Act
	h.BeforeRun(ctx, &store.Run{ID: "run-1", ThreadID: "th", Kind: store.KindChat})
	write(t, dir, "a.txt", "agent edit\n")
	got, err := ForRun(ctx, dir, "th", "run-1")

	// Assert: the run can be rolled back to the tree it started on.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dir, got.Ref); err != nil {
		t.Fatal(err)
	}
	if body, _ := read(t, dir, "a.txt"); body != "before the run\n" {
		t.Errorf("a.txt = %q", body)
	}
}

func TestHookSkipsCompactionRuns(t *testing.T) {
	// Arrange
	ctx := context.Background()
	dir := newRepo(t)

	// Act
	Hook{Dir: dir}.BeforeRun(ctx, &store.Run{ID: "run-1", ThreadID: "th", Kind: store.KindCompact})

	// Assert
	if refs := run(t, dir, "for-each-ref", "refs/harness/"); refs != "" {
		t.Fatalf("a compaction run left refs: %q", refs)
	}
}

func TestHookIgnoresADirectoryThatIsNotARepository(t *testing.T) {
	// Arrange
	dir := t.TempDir()

	// Act
	Hook{Dir: dir}.BeforeRun(context.Background(), &store.Run{ID: "run-1", ThreadID: "th", Kind: store.KindChat})

	// Assert: nothing is created in the directory.
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("hook wrote into a non-repository: %v", entries)
	}
}

func TestHookSurvivesASnapshotFailure(t *testing.T) {
	// Arrange: the snapshot cannot be taken (no temporary directory), the run must still start.
	ctx := context.Background()
	dir := newRepo(t)
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))

	// Act
	Hook{Dir: dir}.BeforeRun(ctx, &store.Run{ID: "run-1", ThreadID: "th", Kind: store.KindChat})

	// Assert
	if _, err := ForRun(ctx, dir, "th", "run-1"); err == nil {
		t.Fatal("a run was recorded without a snapshot")
	}
}

func TestHookLogsAFailureToRecordTheRun(t *testing.T) {
	// Arrange: the run ref is blocked by a ref of the same name, so the snapshot succeeds but recording does not.
	ctx := context.Background()
	dir := newRepo(t)
	run(t, dir, "update-ref", "refs/harness/runs/th", run(t, dir, "rev-parse", "HEAD"))

	// Act
	Hook{Dir: dir}.BeforeRun(ctx, &store.Run{ID: "run-1", ThreadID: "th", Kind: store.KindChat})

	// Assert: the snapshot exists, the run is unknown.
	if l, err := List(ctx, dir, "th"); err != nil || len(l) != 1 {
		t.Fatalf("snapshots = %v, %v", l, err)
	}
	if _, err := ForRun(ctx, dir, "th", "run-1"); err == nil {
		t.Fatal("run recorded despite the blocked ref")
	}
}
