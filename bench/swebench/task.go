package swebench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/7ar-chat/harness/bench"
)

// SetupScript is the file a large-repository task carries instead of the repository (see Suite).
const SetupScript = "swebench_setup.sh"

// Prompt is the task text the agent gets: an instruction header, the problem statement and, with
// o.IncludeHints, the hints.
func (inst Instance) Prompt(o Options) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in a checkout of the %s repository (a git working tree, in the current directory).\n", inst.Repo)
	b.WriteString("Resolve the issue below by changing the library's source code so that the behaviour it describes is fixed.\n")
	b.WriteString("Do not modify or add tests: the repository's own tests are replaced by the maintainers' versions when your work is checked. ")
	b.WriteString("Keep the change minimal and do not break existing behaviour. When you are done, say briefly what you changed.\n\n")
	b.WriteString("<issue>\n")
	b.WriteString(strings.TrimSpace(inst.ProblemStatement))
	b.WriteString("\n</issue>\n")
	if o.IncludeHints && strings.TrimSpace(inst.HintsText) != "" {
		b.WriteString("\n<hints>\n")
		b.WriteString(strings.TrimSpace(inst.HintsText))
		b.WriteString("\n</hints>\n")
	}
	return b.String()
}

// VerifyCommand is the bash command bench runs after the agent finished (test files restored): pytest
// over the FAIL_TO_PASS then the PASS_TO_PASS node ids, each quoted for the shell. A very long id
// list is split over several pytest invocations joined with &&, each below o.MaxArgBytes. o.Verify,
// when set, replaces it.
func (o Options) VerifyCommand(inst Instance) string {
	if o.Verify != nil {
		return o.Verify(inst)
	}
	ids := dedupe(append(append([]string(nil), inst.FailToPass...), inst.PassToPass...))
	py := shellQuote(o.python())
	var cmds []string
	for _, batch := range batches(ids, o.maxArgBytes()) {
		q := make([]string, len(batch))
		for i, id := range batch {
			q[i] = shellQuote(id)
		}
		cmds = append(cmds, py+" -m pytest -x -q -p no:cacheprovider -- "+strings.Join(q, " "))
	}
	return strings.Join(cmds, " && ")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Task is the bench task for inst without a repository: Prompt, Needs and Verify are set; Files, Hidden
// and Solution come from Materialise (Suite does both).
func (inst Instance) Task(o Options) bench.Task {
	return bench.Task{
		Name:   unsafeName.ReplaceAllString(inst.ID, "_"),
		Tier:   bench.TierHard,
		Prompt: inst.Prompt(o),
		Verify: o.VerifyCommand(inst),
		Needs:  []string{"git", o.python()},
	}
}

// Materialise returns inst's bench task with the repository at base_commit embedded in Files, the
// post-test_patch test files as Hidden and the gold patch's result as Solution. It clones into a
// temporary directory (removed again) using Prepare.
//
// Files holds the text files git tracks, read from the checkout. Binary files, symlinks and files
// that are not valid UTF-8 are left out and permissions are not kept (bench.Materialize writes 0644).
// bench.Task has no per-task setup hook, so when the text exceeds o.MaxRepoBytes (default 2 MiB) the
// repository is not embedded; Files then holds only SetupScript, which the prompt tells the agent
// to run first: it fetches base_commit (depth 1, so the fix is not in the history) from the
// instance's clone URL into the working directory. Verify and Hidden work either way.
func Materialise(ctx context.Context, inst Instance, o Options) (bench.Task, error) {
	tmp, err := os.MkdirTemp("", "swebench-")
	if err != nil {
		return bench.Task{}, fmt.Errorf("swebench: %s: %w", inst.ID, err)
	}
	defer os.RemoveAll(tmp)
	dir := filepath.Join(tmp, "repo")
	prep, err := Prepare(ctx, inst, dir, nil, o)
	if err != nil {
		return bench.Task{}, err
	}
	t := inst.Task(o)
	t.Hidden, t.Solution = prep.Hidden, prep.Solution
	files, err := readRepo(ctx, o.git(), dir, o.maxRepoBytes())
	switch {
	case errors.Is(err, errTooLarge):
		t.Files = map[string]string{SetupScript: setupScript(inst, o)}
		t.Prompt = "First run `sh " + SetupScript + "` in the current directory: it checks out the repository.\n\n" + t.Prompt
	case err != nil:
		return bench.Task{}, fmt.Errorf("swebench: %s: read repository: %w", inst.ID, err)
	default:
		t.Files = files
	}
	return t, nil
}

// Suite materialises every instance (see Materialise). Instances that cannot be prepared are left out;
// their errors are joined into the returned error, so a partial suite and a non-nil error can come together.
func Suite(ctx context.Context, insts []Instance, o Options) ([]bench.Task, error) {
	var tasks []bench.Task
	var errs []error
	for _, inst := range insts {
		t, err := Materialise(ctx, inst, o)
		if err != nil {
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, errors.Join(errs...)
}

var errTooLarge = errors.New("repository exceeds the size limit")

func readRepo(ctx context.Context, git GitRunner, dir string, limit int64) (map[string]string, error) {
	out, err := git.Run(ctx, dir, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	files := map[string]string{}
	var total int64
	for _, name := range strings.Split(out, "\x00") {
		if name == "" {
			continue
		}
		p := filepath.Join(dir, filepath.FromSlash(name))
		info, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if total += info.Size(); total > limit {
			return nil, errTooLarge
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if !utf8.Valid(b) || strings.IndexByte(string(b), 0) >= 0 {
			continue
		}
		files[name] = string(b)
	}
	return files, nil
}

func setupScript(inst Instance, o Options) string {
	q := shellQuote
	return "#!/bin/sh\n" +
		"# Checks out " + inst.Repo + " at the base commit of " + inst.ID + " into the current directory.\n" +
		"set -e\n" +
		"git init --quiet .\n" +
		"git remote add origin " + q(o.CloneURL(inst)) + "\n" +
		"git fetch --quiet --depth 1 origin " + q(inst.BaseCommit) + "\n" +
		"git checkout --quiet --detach FETCH_HEAD\n" +
		"git remote remove origin\n"
}
