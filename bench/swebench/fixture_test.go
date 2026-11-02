package swebench_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/bench/swebench"
)

// repoSpec describes the history of one fixture repository: the base files, the gold fix and the
// tests the instance's test_patch brings. The patches are generated with git, never typed by hand.
type repoSpec struct {
	base  map[string]string // files at base_commit
	fix   map[string]string // gold patch: files changed or added
	tests map[string]string // test_patch: files changed or added
}

var specs = map[string]repoSpec{
	"fx__calc-1": {
		base: map[string]string{
			"mathx.py":            "def total(xs):\n    return sum(xs)\n\n\ndef mean(xs):\n    return sum(xs) / (len(xs) + 1)\n",
			"tests/test_mathx.py": "from mathx import total\n\n\ndef test_total():\n    assert total([1, 2, 3]) == 6\n",
		},
		fix: map[string]string{
			"mathx.py": "def total(xs):\n    return sum(xs)\n\n\ndef mean(xs):\n    return sum(xs) / len(xs)\n",
		},
		tests: map[string]string{
			"tests/test_mathx.py": `import pytest

from mathx import mean, total


def test_total():
    assert total([1, 2, 3]) == 6


@pytest.mark.parametrize(
    "xs,want",
    [([1, 2, 3], 2.0), ([5], 5.0), ([2, 4], 3.0), ([4, 4], 4.0)],
    ids=["three items", "it's one", "a - b", 'say "hi" $HOME [x]'],
)
def test_mean(xs, want):
    assert mean(xs) == want
`,
		},
	},
	"fx__textkit-2": {
		base: map[string]string{
			"textkit/__init__.py": "from .slug import slugify\n",
			"textkit/util.py":     "def collapse_spaces(s):\n    return s.replace(\" \", \"-\")\n",
			"textkit/slug.py":     "from .util import collapse_spaces\n\n\ndef slugify(s):\n    return collapse_spaces(s.strip())\n",
			"tests/test_slug.py":  "from textkit import slugify\n\n\ndef test_plain():\n    assert slugify(\"abc\") == \"abc\"\n",
		},
		fix: map[string]string{
			"textkit/util.py": "def collapse_spaces(s):\n    return \"-\".join(s.split())\n",
			"textkit/slug.py": "from .util import collapse_spaces\n\n\ndef slugify(s):\n    return collapse_spaces(s.strip().lower())\n",
		},
		tests: map[string]string{
			"tests/test_extra.py": "from textkit import slugify\n\n\ndef test_lowercases():\n    assert slugify(\"Hello World\") == \"hello-world\"\n\n\ndef test_collapses():\n    assert slugify(\"a  b\") == \"a-b\"\n",
		},
	},
	"fx__pricing-3": {
		base: map[string]string{
			"pricing.py":            "def parse_qty(s):\n    s = s.strip()\n    if s == \"\":\n        return 0\n    return int(s)\n",
			"tests/test_pricing.py": "from pricing import parse_qty\n\n\ndef test_plain():\n    assert parse_qty(\"42\") == 42\n",
		},
		fix: map[string]string{
			"pricing.py": "def parse_qty(s):\n    s = s.strip()\n    if s == \"\":\n        return 0\n    return int(s.replace(\",\", \"\"))\n",
		},
		tests: map[string]string{
			"tests/test_pricing.py": "from pricing import parse_qty\n\n\ndef test_plain():\n    assert parse_qty(\"42\") == 42\n\n\ndef test_blank_is_zero():\n    assert parse_qty(\"  \") == 0\n\n\ndef test_thousands():\n    assert parse_qty(\"1,000\") == 1000\n",
		},
	},
}

// naivePricing fixes the thousands separator but drops the blank-input branch, which breaks a P2P test.
const naivePricing = "def parse_qty(s):\n    return int(s.strip().replace(\",\", \"\"))\n"

// fixture is a set of real git repositories under root (clone URL root/<repo>) plus the instances that describe them.
type fixture struct {
	root  string
	insts []swebench.Instance
	opts  swebench.Options
}

func (f fixture) byID(t *testing.T, id string) swebench.Instance {
	t.Helper()
	for _, i := range f.insts {
		if i.ID == id {
			return i
		}
	}
	t.Fatalf("no instance %s", id)
	return swebench.Instance{}
}

func needTools(t *testing.T) {
	t.Helper()
	for _, p := range []string{"git", "python3"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("%s not on PATH", p)
		}
	}
	if err := exec.Command("python3", "-m", "pytest", "--version").Run(); err != nil {
		t.Skip("pytest is not installed for python3")
	}
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String()
}

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stagedPatch stages files, captures the patch they make against HEAD and resets the repository.
func stagedPatch(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	write(t, dir, files)
	run(t, dir, "add", "-A")
	patch := run(t, dir, "diff", "--cached", "--no-color")
	run(t, dir, "reset", "--hard", "--quiet")
	if patch == "" {
		t.Fatal("empty patch")
	}
	return patch
}

// newFixture builds the repositories and loads testdata/fixture.jsonl, filling in what only git knows:
// base_commit, patch and test_patch. The gold fix also lives on a branch and a tag of each repository,
// so the tests can check that Prepare keeps it away from the agent.
func newFixture(t *testing.T) fixture {
	t.Helper()
	needTools(t)
	t.Setenv("GIT_AUTHOR_NAME", "fixture")
	t.Setenv("GIT_AUTHOR_EMAIL", "fixture@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "fixture")
	t.Setenv("GIT_COMMITTER_EMAIL", "fixture@example.com")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	f, err := os.Open("testdata/fixture.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	insts, err := swebench.Load(f)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for i := range insts {
		spec, ok := specs[insts[i].ID]
		if !ok {
			t.Fatalf("no spec for %s", insts[i].ID)
		}
		dir := filepath.Join(root, filepath.FromSlash(insts[i].Repo))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		run(t, dir, "init", "--quiet", "--initial-branch=main")
		write(t, dir, spec.base)
		run(t, dir, "add", "-A")
		run(t, dir, "commit", "--quiet", "-m", "base")
		base := strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))
		insts[i].BaseCommit = base
		insts[i].EnvironmentSetupCommit = base
		insts[i].Patch = stagedPatch(t, dir, spec.fix)
		insts[i].TestPatch = stagedPatch(t, dir, spec.tests)

		run(t, dir, "checkout", "--quiet", "-b", "fix")
		write(t, dir, spec.fix)
		run(t, dir, "add", "-A")
		run(t, dir, "commit", "--quiet", "-m", "the fix, with the upstream tests")
		run(t, dir, "tag", "fixed")
		run(t, dir, "checkout", "--quiet", "main")
	}
	return fixture{root: root, insts: insts, opts: swebench.Options{BaseURL: root}}
}

// prepared clones inst into a fresh directory.
func (f fixture) prepared(t *testing.T, inst swebench.Instance) (string, swebench.Prepared) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	p, err := swebench.Prepare(t.Context(), inst, dir, nil, f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return dir, p
}

// gitApply applies a patch the way an agent's edit would land.
func gitApply(t *testing.T, dir, patch string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gold.patch")
	if err := os.WriteFile(p, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "apply", p)
}
