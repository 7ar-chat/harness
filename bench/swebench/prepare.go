package swebench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options tune how instances are materialised, turned into tasks and scored. The zero value is usable.
type Options struct {
	// BaseURL is where repositories are cloned from: <BaseURL>/<repo>. Default https://github.com.
	// A directory or file:// URL works too (tests clone local fixtures).
	BaseURL string
	// Git runs git for Evaluate, Materialise and Suite (Prepare takes its own). Default ExecGit.
	Git GitRunner
	// Python is the interpreter that runs pytest. Default python3.
	Python string
	// Env is added to the environment of the test processes Evaluate starts (a virtualenv's PATH, say).
	Env []string
	// IncludeHints adds the instance's hints_text (the discussion before the fix) to the prompt.
	IncludeHints bool
	// KeepHistory leaves the clone's other branches, tags and objects in place. By default Prepare
	// drops them so an agent cannot read the fix out of the repository history; that costs a gc,
	// which is slow on a large repository.
	KeepHistory bool
	// MaxRepoBytes bounds the text a repository may contribute to bench.Task.Files (default 2 MiB).
	MaxRepoBytes int64
	// MaxArgBytes bounds the node ids on one pytest command line (default 64 KiB); more are batched.
	MaxArgBytes int
	// TestTimeout bounds each pytest invocation of Evaluate (default 15 minutes).
	TestTimeout time.Duration
	// Verify replaces the default bench verify command (see VerifyCommand).
	Verify func(Instance) string
}

func (o Options) git() GitRunner {
	if o.Git != nil {
		return o.Git
	}
	return ExecGit{}
}

func (o Options) python() string {
	if o.Python != "" {
		return o.Python
	}
	return "python3"
}

func (o Options) baseURL() string {
	if o.BaseURL != "" {
		return strings.TrimRight(o.BaseURL, "/")
	}
	return "https://github.com"
}

func (o Options) maxRepoBytes() int64 {
	if o.MaxRepoBytes > 0 {
		return o.MaxRepoBytes
	}
	return 2 << 20
}

func (o Options) maxArgBytes() int {
	if o.MaxArgBytes > 0 {
		return o.MaxArgBytes
	}
	return 64 << 10
}

func (o Options) testTimeout() time.Duration {
	if o.TestTimeout > 0 {
		return o.TestTimeout
	}
	return 15 * time.Minute
}

// CloneURL is where Prepare clones inst's repository from.
func (o Options) CloneURL(inst Instance) string { return o.baseURL() + "/" + inst.Repo }

// Prepared is what Prepare learned while materialising an instance.
type Prepared struct {
	// TestFiles are the files test_patch adds or changes, sorted. Files it deletes are not listed.
	TestFiles []string
	// Hidden maps TestFiles to their contents after test_patch: what bench.Task.Hidden writes back before verification.
	Hidden map[string]string
	// Solution maps the files the gold patch adds or changes to their contents after it (bench.Task.Solution).
	// Files the gold patch deletes are not representable and stay.
	Solution map[string]string
}

// Prepare clones the instance's repository into dir (which must not exist or be empty) and checks out
// base_commit. This is the state the agent sees: test_patch is NOT applied, it only has to apply
// cleanly, and the post-patch test files are returned so they can be hidden and restored before
// verification (Evaluate does that for a working tree). git nil selects o.Git, then ExecGit.
//
// Unless o.KeepHistory is set, the clone keeps only base_commit and its ancestors: the remote, other
// branches, tags and unreachable objects are removed.
func Prepare(ctx context.Context, inst Instance, dir string, git GitRunner, o Options) (Prepared, error) {
	if git == nil {
		git = o.git()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Prepared{}, fmt.Errorf("swebench: %s: %w", inst.ID, err)
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Prepared{}, fmt.Errorf("swebench: %s: %w", inst.ID, err)
	}
	fail := func(step string, err error) (Prepared, error) {
		return Prepared{}, fmt.Errorf("swebench: %s: %s: %w", inst.ID, step, err)
	}
	if _, err := git.Run(ctx, parent, "clone", "--quiet", "--no-checkout", "--no-tags", "--", o.CloneURL(inst), abs); err != nil {
		return fail("clone", err)
	}
	if _, err := git.Run(ctx, abs, "checkout", "--quiet", "--detach", inst.BaseCommit); err != nil {
		return fail("checkout "+inst.BaseCommit, err)
	}
	if !o.KeepHistory {
		if err := isolate(ctx, git, abs); err != nil {
			return fail("drop history", err)
		}
	}
	hidden, err := contentsAfter(ctx, git, abs, inst.TestPatch)
	if err != nil {
		return fail("test_patch", err)
	}
	p := Prepared{Hidden: hidden}
	for name := range hidden {
		p.TestFiles = append(p.TestFiles, name)
	}
	sort.Strings(p.TestFiles)
	if strings.TrimSpace(inst.Patch) != "" {
		if p.Solution, err = contentsAfter(ctx, git, abs, inst.Patch); err != nil {
			return fail("patch", err)
		}
	}
	return p, nil
}

// isolate leaves HEAD (base_commit) as the only root of the repository's history.
func isolate(ctx context.Context, git GitRunner, dir string) error {
	if _, err := git.Run(ctx, dir, "remote", "remove", "origin"); err != nil {
		return err
	}
	for _, kind := range []struct{ list, del []string }{
		{[]string{"for-each-ref", "--format=%(refname:short)", "refs/heads"}, []string{"branch", "-D", "--quiet"}},
		{[]string{"tag", "--list"}, []string{"tag", "-d"}},
	} {
		out, err := git.Run(ctx, dir, kind.list...)
		if err != nil {
			return err
		}
		names := strings.Fields(out)
		for len(names) > 0 {
			n := min(len(names), 200)
			if _, err := git.Run(ctx, dir, append(append([]string(nil), kind.del...), names[:n]...)...); err != nil {
				return err
			}
			names = names[n:]
		}
	}
	for _, args := range [][]string{{"reflog", "expire", "--expire=now", "--all"}, {"gc", "--quiet", "--prune=now"}} {
		if _, err := git.Run(ctx, dir, args...); err != nil {
			return err
		}
	}
	return nil
}

// contentsAfter returns the contents patch gives the files it adds or changes in dir's HEAD, without
// touching the working tree: the patch goes into the index, the blobs are read from there, and the
// index is reset again. A patch that does not apply is an error.
func contentsAfter(ctx context.Context, git GitRunner, dir, patch string) (map[string]string, error) {
	file, cleanup, err := writePatch(patch)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if _, err := git.Run(ctx, dir, "apply", "--cached", "--whitespace=nowarn", file); err != nil {
		return nil, fmt.Errorf("does not apply to the base commit: %w", err)
	}
	// The index must go back even when ctx is already cancelled.
	defer git.Run(context.WithoutCancel(ctx), dir, "reset", "--quiet") //nolint:errcheck // best effort, the clone is disposable
	out, err := git.Run(ctx, dir, "diff", "--cached", "--no-renames", "--no-color", "--name-only", "--diff-filter=AMT", "-z")
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	for _, name := range strings.Split(out, "\x00") {
		if name == "" {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) {
			return nil, fmt.Errorf("patch touches %q outside the repository", name)
		}
		content, err := git.Run(ctx, dir, "show", ":"+name)
		if err != nil {
			return nil, err
		}
		files[name] = content
	}
	return files, nil
}

// patchPaths lists every path a patch touches (renames: both names), without applying it.
func patchPaths(ctx context.Context, git GitRunner, dir, file string) ([]string, error) {
	out, err := git.Run(ctx, dir, "apply", "--numstat", "-z", file)
	if err != nil {
		return nil, err
	}
	// -z: "added\tdeleted\tpath\0", or "added\tdeleted\t\0old\0new\0" for a rename.
	tok := strings.Split(out, "\x00")
	var paths []string
	for i := 0; i < len(tok); i++ {
		t := tok[i]
		if t == "" {
			continue
		}
		parts := strings.SplitN(t, "\t", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected numstat entry %q", t)
		}
		if parts[2] != "" {
			paths = append(paths, parts[2])
			continue
		}
		if i+2 >= len(tok) {
			return nil, errors.New("truncated numstat rename entry")
		}
		paths = append(paths, tok[i+1], tok[i+2])
		i += 2
	}
	for _, p := range paths {
		if !filepath.IsLocal(filepath.FromSlash(p)) {
			return nil, fmt.Errorf("patch touches %q outside the repository", p)
		}
	}
	return paths, nil
}

// writePatch stores patch in a temp file (GitRunner has no stdin) and returns it with its remover.
func writePatch(patch string) (string, func(), error) {
	f, err := os.CreateTemp("", "swebench-*.patch")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n" // git apply rejects a truncated last line
	}
	if _, err := f.WriteString(patch); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}
