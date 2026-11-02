package forge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runSetup executes the real setup script with bash against a local bare repository standing in for GitHub.
func runSetup(t *testing.T, repoDir, remote string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", setupScript)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0", "REPO_URL=" + remote, "EXPECT_URL=" + remote, "REPO_DIR=" + repoDir,
		"BASE_BRANCH=main", "THREAD_BRANCH=factory/t1"}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func bareRemote(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	work, bare := filepath.Join(root, "work"), filepath.Join(root, "remote.git")
	git(t, root, "init", "-q", "-b", "main", work)
	if err := os.WriteFile(filepath.Join(work, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", ".")
	git(t, work, "commit", "-q", "-m", "init")
	git(t, root, "clone", "-q", "--bare", work, bare)
	return bare
}

func TestSetupScriptClonesIntoACleanDirectoryOnTheTaskBranch(t *testing.T) {
	// Arrange
	remote := bareRemote(t)
	dir := filepath.Join(t.TempDir(), "repo")

	// Act
	out, code := runSetup(t, dir, remote)

	// Assert
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if b, _ := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); strings.TrimSpace(string(b)) != "factory/t1" {
		t.Errorf("branch = %q, want factory/t1", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("the base branch was not checked out: %v", err)
	}
}

func TestSetupScriptRefusesALinkedWorktreeOfAnotherRepository(t *testing.T) {
	// Arrange: REPO_DIR is a worktree of some other repository (its .git is a file), as a clone that an agent
	// replaced by `git worktree add` would be. Configuring it would write into that repository.
	other := bareRemote(t)
	home := filepath.Join(t.TempDir(), "home")
	git(t, filepath.Dir(other), "clone", "-q", other, home)
	dir := filepath.Join(t.TempDir(), "repo")
	git(t, home, "worktree", "add", "-q", "-b", "factory/t1", dir, "main")

	// Act
	out, code := runSetup(t, dir, other)

	// Assert: refused, and the other repository's config was not touched.
	if code != 3 || !strings.Contains(out, "linked worktree") {
		t.Errorf("exit %d, output %q; want exit 3 naming the worktree", code, out)
	}
	if b, _ := exec.Command("git", "-C", home, "config", "--local", "--get", "user.name").Output(); strings.TrimSpace(string(b)) == "Harness Agent" {
		t.Error("the setup configured the other repository")
	}
}

func TestSetupScriptRefusesADirectoryInsideAnotherRepository(t *testing.T) {
	// Arrange: a plain directory with a .git directory missing, nested in another repository's working tree.
	remote := bareRemote(t)
	outer := filepath.Join(t.TempDir(), "outer")
	git(t, filepath.Dir(remote), "clone", "-q", remote, outer)
	dir := filepath.Join(outer, "repo")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil { // looks like a clone, is not a repository
		t.Fatal(err)
	}

	// Act
	out, code := runSetup(t, dir, remote)

	// Assert
	if code == 0 {
		t.Fatalf("setup accepted a directory that is not its own repository:\n%s", out)
	}
	if b, _ := exec.Command("git", "-C", outer, "config", "--local", "--get", "user.name").Output(); strings.TrimSpace(string(b)) == "Harness Agent" {
		t.Error("the setup configured the enclosing repository")
	}
}
