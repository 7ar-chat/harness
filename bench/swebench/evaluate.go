package swebench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Result is the outcome of Evaluate.
type Result struct {
	// Resolved: every FAIL_TO_PASS test passes and no PASS_TO_PASS test breaks.
	Resolved                          bool
	FailToPassPassed, FailToPassTotal int
	FailToPassFailed                  []string // F2P ids that did not pass
	PassToPassTotal                   int
	PassToPassBroken                  []string // P2P ids that did not pass
	// Output is the tail of what pytest printed.
	Output string
}

const maxOutput = 16 << 10

// Evaluate scores the git working tree workdir (a repository checked out by Prepare and edited by the
// agent) the way SWE-bench does: the files test_patch touches are restored from base_commit, so
// whatever the agent did to the tests is gone, test_patch is applied, and the F2P and P2P tests run.
// test_patch stays applied afterwards. Tests that pytest does not report as passed (failed, errored,
// skipped, not collected) count as not passing.
//
// An error means the evaluation could not be carried out (base commit missing, test_patch not applying,
// python missing); a repository that merely fails the tests is a Result with Resolved false.
func Evaluate(ctx context.Context, inst Instance, workdir string, o Options) (Result, error) {
	git := o.git()
	fail := func(step string, err error) (Result, error) {
		return Result{}, fmt.Errorf("swebench: %s: %s: %w", inst.ID, step, err)
	}
	patchFile, cleanup, err := writePatch(inst.TestPatch)
	if err != nil {
		return fail("test_patch", err)
	}
	defer cleanup()
	paths, err := patchPaths(ctx, git, workdir, patchFile)
	if err != nil {
		return fail("test_patch", err)
	}
	if err := restoreFromBase(ctx, git, workdir, inst.BaseCommit, paths); err != nil {
		return fail("restore test files", err)
	}
	if _, err := git.Run(ctx, workdir, "apply", "--whitespace=nowarn", patchFile); err != nil {
		return fail("apply test_patch", err)
	}

	f2p, p2p := dedupe(inst.FailToPass), dedupe(inst.PassToPass)
	statuses, output, err := runPytest(ctx, workdir, append(append([]string(nil), f2p...), p2p...), o)
	if err != nil {
		return fail("pytest", err)
	}
	res := Result{FailToPassTotal: len(f2p), PassToPassTotal: len(p2p), Output: tail(output, maxOutput)}
	for _, id := range f2p {
		if statuses[id] == passed {
			res.FailToPassPassed++
		} else {
			res.FailToPassFailed = append(res.FailToPassFailed, id)
		}
	}
	for _, id := range p2p {
		if statuses[id] != passed {
			res.PassToPassBroken = append(res.PassToPassBroken, id)
		}
	}
	res.Resolved = res.FailToPassTotal > 0 && res.FailToPassPassed == res.FailToPassTotal && len(res.PassToPassBroken) == 0
	return res, nil
}

// restoreFromBase puts the working-tree files at paths back to their base_commit content and removes
// those base_commit does not have (test files the test_patch adds, or an agent made up).
func restoreFromBase(ctx context.Context, git GitRunner, dir, base string, paths []string) error {
	inBase := map[string]bool{}
	for len(paths) > 0 {
		n := min(len(paths), 200)
		batch := paths[:n]
		paths = paths[n:]
		out, err := git.Run(ctx, dir, append([]string{"--literal-pathspecs", "ls-tree", "-z", "--name-only", base, "--"}, batch...)...)
		if err != nil {
			return fmt.Errorf("base commit %s: %w", base, err)
		}
		var present []string
		for _, name := range strings.Split(out, "\x00") {
			if name != "" {
				inBase[name] = true
				present = append(present, name)
			}
		}
		if len(present) > 0 {
			args := append([]string{"--literal-pathspecs", "restore", "--source=" + base, "--worktree", "--"}, present...)
			if _, err := git.Run(ctx, dir, args...); err != nil {
				return err
			}
		}
		for _, name := range batch {
			if inBase[name] {
				continue
			}
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(name))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

type status int

const (
	unseen status = iota
	passed
	failed
)

// runPytest runs the test files of ids and returns what pytest reported per id plus the combined
// output. Whole files are run, as SWE-bench does, because pytest aborts a run on a single node id it
// cannot find, which would turn one renamed test into a failure of every other; ids are matched
// against the report afterwards, and an id that never shows up counts as not passing.
func runPytest(ctx context.Context, dir string, ids []string, o Options) (map[string]status, string, error) {
	want := make(map[string]struct{}, len(ids))
	seen := map[string]bool{}
	var paths []string
	for _, id := range ids {
		want[id] = struct{}{}
		path, _, _ := strings.Cut(id, "::")
		if seen[path] {
			continue
		}
		seen[path] = true
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err == nil {
			paths = append(paths, path) // a missing file would abort the whole run
		}
	}
	got := map[string]status{}
	var all strings.Builder
	for _, batch := range batches(paths, o.maxArgBytes()) {
		out, err := pytestOnce(ctx, dir, batch, o)
		all.WriteString(out)
		if err != nil {
			return nil, all.String(), err
		}
		parseStatuses(out, want, got)
	}
	return got, all.String(), nil
}

func pytestOnce(ctx context.Context, dir string, ids []string, o Options) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, o.testTimeout())
	defer cancel()
	args := append([]string{"-m", "pytest", "-rA", "-q", "--color=no", "--tb=short", "-p", "no:cacheprovider", "--"}, ids...)
	cmd := exec.CommandContext(tctx, o.python(), args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "PY_COLORS=0"), o.Env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil || errors.As(err, &exit): // failing tests exit non-zero; their lines are parsed
		return out.String(), nil
	case ctx.Err() != nil:
		return out.String(), ctx.Err()
	case tctx.Err() != nil:
		return out.String(), fmt.Errorf("timed out after %s", o.testTimeout())
	default:
		return out.String(), err
	}
}

var statusWords = []struct {
	prefix string
	st     status
}{
	{"PASSED ", passed}, {"XFAIL ", passed}, {"XPASS ", passed},
	{"FAILED ", failed}, {"ERROR ", failed},
}

// parseStatuses reads the `-rA` summary lines ("PASSED id", "FAILED id - message"). A later FAILED or
// ERROR (a teardown error after a pass) outweighs a PASSED for the same id.
func parseStatuses(out string, want map[string]struct{}, got map[string]status) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		for _, w := range statusWords {
			rest, ok := strings.CutPrefix(line, w.prefix)
			if !ok {
				continue
			}
			id, ok := matchID(rest, want)
			if !ok {
				break
			}
			if w.st == failed || got[id] != failed {
				got[id] = w.st
			}
			break
		}
	}
}

// matchID finds which wanted id a summary remainder starts with: the id itself or, for failures, the id
// followed by " - message". Splitting on the first " - " would cut ids that contain one.
func matchID(rest string, want map[string]struct{}) (string, bool) {
	if _, ok := want[rest]; ok {
		return rest, true
	}
	for i := 0; i < len(rest); {
		j := strings.Index(rest[i:], " - ")
		if j < 0 {
			break
		}
		i += j
		if _, ok := want[rest[:i]]; ok {
			return rest[:i], true
		}
		i++
	}
	return "", false
}

func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// batches splits ids so that each group's quoted size stays within limit (one id per group at minimum).
func batches(ids []string, limit int) [][]string {
	var out [][]string
	var cur []string
	size := 0
	for _, id := range ids {
		n := len(shellQuote(id)) + 1
		if len(cur) > 0 && size+n > limit {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, id)
		size += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// shellQuote quotes s for POSIX sh: single quotes, with embedded ones as '\”.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-./:=+,@%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
