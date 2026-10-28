// Package gitsafe takes restorable snapshots of a git working tree without
// touching what belongs to the user: no commit on a branch, no change to the
// index or the stash list. A snapshot is a commit object (tracked and
// untracked, non-ignored files as they were) kept alive by a ref under
// refs/harness/snapshots/<thread>/<n>; Restore puts the tree, HEAD and the
// staged state back as they were. The harness takes one at the start of every
// run of the local backend so an agent's changes can be undone.
package gitsafe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// ErrNotRepo is returned when the directory is not inside a git work tree
// (callers skip snapshots then).
var ErrNotRepo = errors.New("gitsafe: not a git repository")

// Refs live under these namespaces, outside heads/ and tags/, so they do not
// show in `git branch` and are never pushed by default.
const (
	snapshotNS = "refs/harness/snapshots/"
	backupNS   = "refs/harness/backups/"
)

// Snap is one snapshot.
type Snap struct {
	// Ref is refs/harness/snapshots/<thread>/<n>.
	Ref string
	// Commit is the snapshot commit (the working tree, with untracked files).
	Commit string
	// Head is the commit HEAD pointed at ("" in a repository without commits).
	Head string
	// Branch is the checked-out branch ("" when detached).
	Branch string
	// IndexTree is the tree of the staged state ("" if it could not be recorded).
	IndexTree string
	// Dir is the top level of the repository.
	Dir string
}

// git runs a git command in dir and returns its trimmed stdout.
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Toplevel is the root of the work tree containing dir, or ErrNotRepo.
func Toplevel(ctx context.Context, dir string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", ErrNotRepo
	}
	top, err := git(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", ErrNotRepo
	}
	return top, nil
}

func safeThread(thread string) string {
	var b strings.Builder
	for _, r := range thread {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "default"
	}
	return b.String()
}

// Snapshot records the working tree of the repository containing dir under
// refs/harness/snapshots/<thread>/<n> and returns it. Ignored files are not
// part of it. When nothing changed since the thread's latest snapshot, that
// snapshot is returned. It returns ErrNotRepo outside a repository.
func Snapshot(ctx context.Context, dir, thread string) (Snap, error) {
	return takeSnapshot(ctx, dir, safeThread(thread), snapshotNS, true)
}

func takeSnapshot(ctx context.Context, dir, thread, ns string, dedupe bool) (Snap, error) {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return Snap{}, err
	}
	s := Snap{Dir: top}
	s.Head, _ = git(ctx, top, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	s.Branch, _ = git(ctx, top, nil, "symbolic-ref", "--short", "-q", "HEAD")
	// Neither the user's index nor its cache is written: git works on private copies.
	indexPath, err := git(ctx, top, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return Snap{}, err
	}
	stagedIndex, cleanupStaged, err := copyFile(indexPath)
	if err != nil {
		return Snap{}, err
	}
	s.IndexTree, _ = git(ctx, top, []string{"GIT_INDEX_FILE=" + stagedIndex}, "write-tree") // fails with unmerged paths: not recorded
	cleanupStaged()
	tmpName, cleanup, err := copyFile(indexPath)
	if err != nil {
		return Snap{}, err
	}
	defer cleanup()
	env := []string{"GIT_INDEX_FILE=" + tmpName}
	if _, err := git(ctx, top, env, "add", "-A", "--", "."); err != nil {
		return Snap{}, err
	}
	tree, err := git(ctx, top, env, "write-tree")
	if err != nil {
		return Snap{}, err
	}

	if dedupe {
		if last, ok := latest(ctx, top, ns, thread); ok {
			lastTree, _ := git(ctx, top, nil, "rev-parse", last.Commit+"^{tree}")
			if lastTree == tree && last.Head == s.Head && last.IndexTree == s.IndexTree {
				return last, nil
			}
		}
	}

	msg := "harness snapshot\n\nhead: " + s.Head + "\nbranch: " + s.Branch + "\nindex-tree: " + s.IndexTree + "\n"
	ident := []string{
		"GIT_AUTHOR_NAME=harness", "GIT_AUTHOR_EMAIL=harness@localhost", "GIT_COMMITTER_NAME=harness", "GIT_COMMITTER_EMAIL=harness@localhost",
	}
	args := []string{"commit-tree", tree, "-m", msg}
	if s.Head != "" {
		args = append(args, "-p", s.Head)
	}
	if s.Commit, err = git(ctx, top, ident, args...); err != nil {
		return Snap{}, err
	}
	n := nextNumber(ctx, top, ns, thread)
	s.Ref = fmt.Sprintf("%s%s/%d", ns, thread, n)
	if _, err := git(ctx, top, nil, "update-ref", "-m", "harness snapshot", s.Ref, s.Commit); err != nil {
		return Snap{}, err
	}
	return s, nil
}

// copyFile copies src to a temporary file (a missing src gives a path that does
// not exist yet, which git treats as an empty index).
func copyFile(src string) (name string, cleanup func(), err error) {
	tmp, err := os.CreateTemp("", "harness-index-*")
	if err != nil {
		return "", nil, err
	}
	name = tmp.Name()
	_ = tmp.Close()
	cleanup = func() { _ = os.Remove(name) }
	data, readErr := os.ReadFile(src)
	if readErr != nil {
		cleanup()
		return name, cleanup, nil
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return name, cleanup, nil
}

func nextNumber(ctx context.Context, top, ns, thread string) int {
	refs, _ := git(ctx, top, nil, "for-each-ref", "--format=%(refname)", ns+thread+"/")
	max := 0
	for _, r := range strings.Fields(refs) {
		if n, err := strconv.Atoi(r[strings.LastIndex(r, "/")+1:]); err == nil && n > max {
			max = n
		}
	}
	return max + 1
}

// List returns the thread's snapshots, oldest first.
func List(ctx context.Context, dir, thread string) ([]Snap, error) {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	return list(ctx, top, snapshotNS, safeThread(thread)), nil
}

func list(ctx context.Context, top, ns, thread string) []Snap {
	out, _ := git(ctx, top, nil, "for-each-ref", "--format=%(refname) %(objectname)", ns+thread+"/")
	var snaps []Snap
	for _, line := range strings.Split(out, "\n") {
		ref, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		snaps = append(snaps, readSnap(ctx, top, ref, sha))
	}
	sort.Slice(snaps, func(i, j int) bool { return number(snaps[i].Ref) < number(snaps[j].Ref) })
	return snaps
}

func number(ref string) int {
	n, _ := strconv.Atoi(ref[strings.LastIndex(ref, "/")+1:])
	return n
}

func latest(ctx context.Context, top, ns, thread string) (Snap, bool) {
	l := list(ctx, top, ns, thread)
	if len(l) == 0 {
		return Snap{}, false
	}
	return l[len(l)-1], true
}

// Latest is the thread's most recent snapshot.
func Latest(ctx context.Context, dir, thread string) (Snap, error) {
	snaps, err := List(ctx, dir, thread)
	if err != nil {
		return Snap{}, err
	}
	if len(snaps) == 0 {
		return Snap{}, fmt.Errorf("gitsafe: no snapshot for thread %q", thread)
	}
	return snaps[len(snaps)-1], nil
}

// readSnap rebuilds a Snap from its commit message.
func readSnap(ctx context.Context, top, ref, sha string) Snap {
	s := Snap{Ref: ref, Commit: sha, Dir: top}
	body, _ := git(ctx, top, nil, "show", "-s", "--format=%B", sha)
	for _, line := range strings.Split(body, "\n") {
		switch k, v, _ := strings.Cut(line, ": "); k {
		case "head":
			s.Head = v
		case "branch":
			s.Branch = v
		case "index-tree":
			s.IndexTree = v
		}
	}
	return s
}

// Restore puts the repository containing dir back as it was at the snapshot
// ref (a ref of Snapshot, or any commit-ish): files changed, deleted or added
// since are reverted (ignored files are left alone), the branch is moved back to
// the recorded HEAD when commits were made since, and the staged state is
// restored. Before changing anything it saves the current state under
// refs/harness/backups/<thread>/<n>, returned so the undo can be undone.
func Restore(ctx context.Context, dir, ref string) (backup Snap, err error) {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return Snap{}, err
	}
	sha, err := git(ctx, top, nil, "rev-parse", "--verify", "-q", ref+"^{commit}")
	if err != nil || sha == "" {
		return Snap{}, fmt.Errorf("gitsafe: %q is not a snapshot", ref)
	}
	target := readSnap(ctx, top, ref, sha)
	if target.Head == "" && target.IndexTree == "" && !strings.HasPrefix(ref, "refs/harness/") {
		return Snap{}, fmt.Errorf("gitsafe: %q is not a harness snapshot", ref)
	}

	thread := "default"
	if rest, ok := strings.CutPrefix(ref, snapshotNS); ok {
		thread, _, _ = strings.Cut(rest, "/")
	}
	if backup, err = takeSnapshot(ctx, top, thread, backupNS, false); err != nil {
		return Snap{}, fmt.Errorf("gitsafe: saving the current state first: %w", err)
	}

	// 1. The branch back to where it was (commits made since stay reachable from the backup).
	if target.Head != "" {
		if cur, _ := git(ctx, top, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}"); cur != target.Head {
			if _, err := git(ctx, top, nil, "update-ref", "-m", "harness restore", "HEAD", target.Head); err != nil {
				return backup, err
			}
		}
	}
	// 2. Index and working tree to the snapshot tree (tracked files, and the files that were untracked then).
	if _, err := git(ctx, top, nil, "read-tree", "--reset", "-u", sha); err != nil {
		return backup, err
	}
	// 3. What appeared since and is not ignored goes away.
	if _, err := git(ctx, top, nil, "clean", "-fdq"); err != nil {
		return backup, err
	}
	// 4. The staged state as the user had it (the index only: the files stay as restored).
	switch {
	case target.IndexTree != "":
		_, err = git(ctx, top, nil, "read-tree", target.IndexTree)
	case target.Head != "":
		_, err = git(ctx, top, nil, "read-tree", target.Head)
	}
	if err != nil {
		return backup, err
	}
	return backup, nil
}

// Undo restores the thread's latest snapshot.
func Undo(ctx context.Context, dir, thread string) (backup Snap, restored Snap, err error) {
	restored, err = Latest(ctx, dir, thread)
	if err != nil {
		return Snap{}, Snap{}, err
	}
	backup, err = Restore(ctx, dir, restored.Ref)
	return backup, restored, err
}

// Drop deletes the thread's snapshots and backups.
func Drop(ctx context.Context, dir, thread string) error {
	top, err := Toplevel(ctx, dir)
	if err != nil {
		return err
	}
	for _, ns := range []string{snapshotNS, backupNS} {
		for _, s := range list(ctx, top, ns, safeThread(thread)) {
			if _, err := git(ctx, top, nil, "update-ref", "-d", s.Ref); err != nil {
				return err
			}
		}
	}
	return nil
}
