package gitsafe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// newRepo makes a repo with one commit: a.txt, sub/b.txt; .gitignore ignores *.log.
func newRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "a.txt", "a1\n")
	write(t, dir, "sub/b.txt", "b1\n")
	write(t, dir, ".gitignore", "*.log\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestSnapshotThenRestoreRevertsTheWorkingTree(t *testing.T) {
	// Arrange: a dirty tree (unstaged edit, staged edit, untracked file) at snapshot time.
	ctx := context.Background()
	dir := newRepo(t)
	write(t, dir, "a.txt", "a-dirty\n")
	write(t, dir, "sub/b.txt", "b-staged\n")
	run(t, dir, "add", "sub/b.txt")
	write(t, dir, "new.txt", "untracked\n")
	write(t, dir, "debug.log", "ignored\n")
	headBefore := run(t, dir, "rev-parse", "HEAD")
	statusBefore := run(t, dir, "status", "--porcelain")
	stashBefore := run(t, dir, "stash", "list")
	snap, err := Snapshot(ctx, dir, "thread-1")
	if err != nil {
		t.Fatal(err)
	}

	// Act: the "agent" edits, deletes, adds, commits and writes ignored files.
	write(t, dir, "a.txt", "a-agent\n")
	_ = os.Remove(filepath.Join(dir, "sub/b.txt"))
	write(t, dir, "new.txt", "agent-new\n")
	write(t, dir, "created.go", "package x\n")
	write(t, dir, "deep/dir/c.go", "package y\n")
	write(t, dir, "debug.log", "ignored-changed\n")
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "agent commit")
	write(t, dir, "late.txt", "after the commit\n")
	backup, err := Restore(ctx, dir, snap.Ref)
	if err != nil {
		t.Fatal(err)
	}

	// Assert: files, branch and staged state are as before.
	for name, want := range map[string]string{"a.txt": "a-dirty\n", "sub/b.txt": "b-staged\n", "new.txt": "untracked\n"} {
		if got, ok := read(t, dir, name); !ok || got != want {
			t.Errorf("%s = %q (exists %v), want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{"created.go", "deep/dir/c.go", "late.txt"} {
		if _, ok := read(t, dir, name); ok {
			t.Errorf("%s still exists", name)
		}
	}
	if got, _ := read(t, dir, "debug.log"); got != "ignored-changed\n" {
		t.Errorf("ignored file touched: %q", got)
	}
	if got := run(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD = %s, want %s", got, headBefore)
	}
	if got := run(t, dir, "branch", "--show-current"); got != "main" {
		t.Errorf("branch = %q", got)
	}
	if got := run(t, dir, "status", "--porcelain"); got != statusBefore {
		t.Errorf("status after restore:\n%s\nbefore:\n%s", got, statusBefore)
	}
	if got := run(t, dir, "stash", "list"); got != stashBefore {
		t.Errorf("stash list changed: %q", got)
	}

	// The undo can be undone: the backup holds the agent's tree and its commit.
	if _, err := Restore(ctx, dir, backup.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, dir, "created.go"); got != "package x\n" {
		t.Errorf("redo lost created.go: %q", got)
	}
	if got := run(t, dir, "log", "-1", "--format=%s"); got != "agent commit" {
		t.Errorf("redo HEAD = %q", got)
	}
}

func TestSnapshotLeavesTheUsersRepositoryAlone(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	write(t, dir, "a.txt", "dirty\n")
	write(t, dir, "untracked.txt", "u\n")
	run(t, dir, "add", "a.txt")
	branches := run(t, dir, "for-each-ref", "refs/heads", "refs/tags")
	log := run(t, dir, "log", "--exclude=refs/harness/*", "--all", "--format=%H")
	status := run(t, dir, "status", "--porcelain")
	index, _ := os.ReadFile(filepath.Join(dir, ".git", "index"))

	snap, err := Snapshot(ctx, dir, "t")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(snap.Ref, "refs/harness/snapshots/t/") {
		t.Errorf("ref = %s", snap.Ref)
	}
	if got := run(t, dir, "for-each-ref", "refs/heads", "refs/tags"); got != branches {
		t.Errorf("branches changed:\n%s", got)
	}
	if got := run(t, dir, "log", "--exclude=refs/harness/*", "--all", "--format=%H"); got != log {
		t.Errorf("history changed")
	}
	if got := run(t, dir, "status", "--porcelain"); got != status {
		t.Errorf("status changed:\n%s", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, ".git", "index")); string(got) != string(index) {
		t.Error("the index file was rewritten")
	}
	if got := run(t, dir, "stash", "list"); got != "" {
		t.Errorf("stash list = %q", got)
	}
	if got := run(t, dir, "show", snap.Ref+":untracked.txt"); got != "u" {
		t.Errorf("untracked file not in the snapshot: %q", got)
	}
	if got := run(t, dir, "show", snap.Ref+":a.txt"); got != "dirty" {
		t.Errorf("tracked change not in the snapshot: %q", got)
	}
}

func TestSnapshotNumbersAndDeduplicates(t *testing.T) {
	ctx := context.Background()
	dir := newRepo(t)
	s1, err := Snapshot(ctx, dir, "t")
	if err != nil {
		t.Fatal(err)
	}
	again, err := Snapshot(ctx, dir, "t")
	if err != nil || again.Ref != s1.Ref {
		t.Fatalf("unchanged tree: %v %v (want %s)", again.Ref, err, s1.Ref)
	}
	write(t, dir, "a.txt", "changed\n")
	s2, err := Snapshot(ctx, dir, "t")
	if err != nil || !strings.HasSuffix(s2.Ref, "/2") {
		t.Fatalf("second snapshot: %v %v", s2.Ref, err)
	}
	other, _ := Snapshot(ctx, dir, "other thread/with:odd chars")
	if !strings.HasPrefix(other.Ref, "refs/harness/snapshots/other_thread_with_odd_chars/") {
		t.Errorf("odd thread id ref = %s", other.Ref)
	}
	list, err := List(ctx, dir, "t")
	if err != nil || len(list) != 2 || list[0].Ref != s1.Ref || list[1].Head != s2.Head {
		t.Fatalf("list = %+v %v", list, err)
	}
	// Undo goes back to the latest snapshot (a.txt as it was when s2 was taken).
	write(t, dir, "a.txt", "later\n")
	if _, restored, err := Undo(ctx, dir, "t"); err != nil || restored.Ref != s2.Ref {
		t.Fatalf("undo: %v %v", restored.Ref, err)
	}
	if got, _ := read(t, dir, "a.txt"); got != "changed\n" {
		t.Errorf("a.txt = %q", got)
	}
	if err := Drop(ctx, dir, "t"); err != nil {
		t.Fatal(err)
	}
	if l, _ := List(ctx, dir, "t"); len(l) != 0 {
		t.Errorf("snapshots after drop: %v", l)
	}
}

func TestSnapshotWorksFromASubdirectoryAndWithoutCommits(t *testing.T) {
	ctx := context.Background()
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	run(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "x.txt", "x\n")
	snap, err := Snapshot(ctx, filepath.Join(dir), "t")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Head != "" {
		t.Errorf("head of an empty repository = %q", snap.Head)
	}
	write(t, dir, "x.txt", "changed\n")
	write(t, dir, "y.txt", "new\n")
	if _, err := Restore(ctx, dir, snap.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := read(t, dir, "x.txt"); got != "x\n" {
		t.Errorf("x.txt = %q", got)
	}
	if _, ok := read(t, dir, "y.txt"); ok {
		t.Error("y.txt survived")
	}

	repo := newRepo(t)
	if err := os.MkdirAll(filepath.Join(repo, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if s, err := Snapshot(ctx, filepath.Join(repo, "sub", "deeper"), "t"); err != nil || s.Dir != repo {
		t.Fatalf("subdirectory snapshot: %+v %v", s, err)
	}
}

func TestSnapshotSkipsGracefullyOutsideARepository(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if _, err := Snapshot(ctx, dir, "t"); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("Snapshot err = %v, want ErrNotRepo", err)
	}
	if _, err := Restore(ctx, dir, "refs/harness/snapshots/t/1"); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("Restore err = %v, want ErrNotRepo", err)
	}
	if _, err := List(ctx, dir, "t"); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("List err = %v", err)
	}
}

func TestRestoreRefusesAnUnknownRef(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "keep me\n")
	if _, err := Restore(context.Background(), dir, "refs/harness/snapshots/t/99"); err == nil {
		t.Fatal("restoring a missing ref succeeded")
	}
	if got, _ := read(t, dir, "a.txt"); got != "keep me\n" {
		t.Errorf("a failed restore changed the tree: %q", got)
	}
}
