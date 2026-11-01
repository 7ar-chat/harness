package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/engine/agent"
)

// DefaultDir is where the repository is checked out in the sandbox.
const DefaultDir = sandbox.Workspace + "/repo"

var safeRef = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

// validRef is safeRef minus what git refuses or reads as an option: a leading
// dash, "..", a leading, trailing or doubled slash, and a ".lock" or "." end.
func validRef(s string) bool {
	return safeRef.MatchString(s) && !strings.HasPrefix(s, "-") && !strings.Contains(s, "..") &&
		!strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/") && !strings.Contains(s, "//") &&
		!strings.HasSuffix(s, ".") && !strings.HasSuffix(s, ".lock")
}

// gitWithToken is git with a one-shot credential: the helper exists only in
// this process's command line (-c is never stored) and reads the token from
// the environment of the command that runs it. The token is in no argv, no
// file and no repository config. Hooks are off so a program the agent left in
// .git/hooks never runs with the token in its environment.
const gitWithToken = `git -c credential.helper= -c 'credential.helper=!f() { echo username=x-access-token; echo "password=$GH_TOKEN"; }; f' -c core.hooksPath=/dev/null`

// Checkout clones the repository of a factory task into the sandbox of its
// thread, on the task's branch, and pushes that branch on the host's behalf.
//
// The agent never holds a push credential. The token is minted per command
// and travels only in the environment of the one Exec that clones, fetches or
// pushes; nothing of it is left in the sandbox afterwards.
type Checkout struct {
	Auth  factory.RepoAuth
	Store factory.Store
	// Dir is the checkout directory (default /workspace/repo).
	Dir string
}

func (c *Checkout) dir() string {
	if c.Dir != "" {
		return c.Dir
	}
	return DefaultDir
}

func (c *Checkout) info(ctx context.Context, threadID string) *factory.ThreadInfo {
	if c.Store == nil || threadID == "" {
		return nil
	}
	ti, err := c.Store.GetThreadInfo(ctx, threadID)
	if err != nil {
		if !errors.Is(err, factory.ErrNotFound) {
			slog.Warn("forge checkout: thread info unavailable", "thread_id", threadID, "error", err)
		}
		return nil
	}
	if ti.Repo == "" {
		return nil
	}
	return ti
}

func (c *Checkout) token(ctx context.Context, repo string) string {
	if c.Auth == nil {
		return ""
	}
	t, err := c.Auth.Token(ctx, repo)
	if err != nil {
		slog.Warn("forge checkout: repository token failed", "repo", repo, "error", Redact(err.Error()))
		return ""
	}
	return t
}

// execOut runs req and returns what it printed (stdout and stderr together).
func execOut(ctx context.Context, sb sandbox.Sandbox, req sandbox.ExecRequest) (string, sandbox.ExecResult, error) {
	var (
		mu  sync.Mutex
		out strings.Builder
	)
	add := func(t string) {
		mu.Lock()
		out.WriteString(t)
		mu.Unlock()
	}
	req.OnStdout, req.OnStderr = add, add
	res, err := sb.Exec(ctx, req)
	mu.Lock()
	defer mu.Unlock()
	return out.String(), res, err
}

// setupScript clones and fetches through gitx, which authenticates only when
// a token was given. It never writes a credential, leaves no helper in the
// repository's config, and the token is in the environment of the gitx
// commands alone: every other command of the script runs without it.
const setupScript = `set -e
export GIT_TERMINAL_PROMPT=0
TOK="${GH_TOKEN:-}"
unset GH_TOKEN
gitx() {
  if [ -n "$TOK" ]; then
    GH_TOKEN="$TOK" ` + gitWithToken + ` "$@"
  else
    git "$@"
  fi
}
if [ -e "$REPO_DIR/.git" ] && [ ! -d "$REPO_DIR/.git" ]; then
  echo "$REPO_DIR/.git is not a directory: the checkout is a linked worktree of another repository"
  exit 3
fi
if [ ! -d "$REPO_DIR/.git" ]; then
  gitx clone --depth 50 --single-branch ${BASE_BRANCH:+--branch "$BASE_BRANCH"} -- "$REPO_URL" "$REPO_DIR"
fi
cd "$REPO_DIR"
top=$(git rev-parse --show-toplevel)
here=$(pwd -P)
if [ "$top" != "$here" ]; then
  echo "$REPO_DIR is inside another repository ($top): refusing to configure it"
  exit 3
fi
` + guardScript + `git config user.name 'Harness Agent'
git config user.email 'agent@harness.local'
if git show-ref --verify --quiet "refs/heads/$THREAD_BRANCH"; then
  git checkout "$THREAD_BRANCH"
elif gitx fetch --depth 50 origin "+refs/heads/$THREAD_BRANCH:refs/remotes/origin/$THREAD_BRANCH" >/dev/null 2>&1; then
  git checkout -B "$THREAD_BRANCH" "origin/$THREAD_BRANCH"
else
  git checkout -B "$THREAD_BRANCH"
fi
`

// Setup checks the thread's repository out into sb and returns a notice for
// the model. It does nothing (and returns "") for a thread that is not for a
// repository. On the branch of a retried task the earlier attempts' commits
// are fetched, so the new attempt continues them.
func (c *Checkout) Setup(ctx context.Context, threadID string, sb sandbox.Sandbox) string {
	ti := c.info(ctx, threadID)
	if ti == nil {
		return ""
	}
	dir := c.dir()
	if err := checkRepo(ti.Repo); err != nil {
		return "Repository not checked out: " + err.Error()
	}
	if ti.Branch == "" || !validRef(ti.Branch) {
		return "Repository not checked out: the task's branch name is missing or not valid."
	}
	if ti.BaseBranch != "" && !validRef(ti.BaseBranch) {
		return "Repository not checked out: the base branch name is not valid."
	}
	if ti.Branch == ti.BaseBranch {
		return "Repository not checked out: the task's branch is its base branch, which an agent must not push to."
	}
	env := withIsolatedConfig(map[string]string{
		"REPO_URL":      repoURL(ti.Repo),
		"EXPECT_URL":    repoURL(ti.Repo),
		"REPO_DIR":      dir,
		"BASE_BRANCH":   ti.BaseBranch,
		"THREAD_BRANCH": ti.Branch,
	})
	token := c.token(ctx, ti.Repo)
	if token != "" {
		env["GH_TOKEN"] = token
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, res, err := execOut(cctx, sb, sandbox.ExecRequest{Command: setupScript, Env: env, Timeout: 5 * time.Minute})
	if err != nil || res.ExitCode != 0 {
		slog.Warn("forge checkout: clone failed", "thread_id", threadID, "exit", res.ExitCode, "error", Redact(fmt.Sprint(err), token))
		msg := strings.TrimSpace(Redact(out, token))
		if err != nil && msg == "" {
			msg = Redact(err.Error(), token)
		}
		return "Cloning " + ti.Repo + " into " + dir + " failed: " + tailRunes(msg, 400)
	}
	base := ti.BaseBranch
	if base == "" {
		base = "the default branch"
	}
	return fmt.Sprintf("The repository %s is checked out in %s on branch %s (based on %s).", ti.Repo, dir, ti.Branch, base)
}

func repoURL(repo string) string { return "https://github.com/" + repo + ".git" }

// PushResult is what Checkout.Push published.
type PushResult struct {
	// Branch is the branch that was pushed.
	Branch string
	// Ahead is the number of commits the branch has beyond its base.
	Ahead int
	// Dirty is the number of uncommitted changes, which are not pushed.
	Dirty int
}

// guardScript refuses to continue when origin is not the task's repository or
// the repository config could redirect git's network access. The agent can
// edit .git/config, and a command that carries the token sends it wherever git
// is pointed (url.*.insteadOf, a proxy, a remote helper), so the checkout is
// verified, in the same command, before every one that does.
const guardScript = `url=$(git remote get-url --push origin)
if [ "$url" != "$EXPECT_URL" ]; then
  echo "origin points at $url, not at $EXPECT_URL"
  exit 3
fi
if git config --local --get-regexp '^(include|includeif|http|url|credential|core\.(sshcommand|gitproxy|fsmonitor)|remote\.origin\.(pushurl|proxy|receivepack|uploadpack|vcs))' >/dev/null; then
  echo "the repository config redirects or intercepts git's network access; remove those settings"
  exit 3
fi
`

// inspectScript reads the checkout without any credential: it counts the
// uncommitted changes and how far HEAD is ahead of the base.
const inspectScript = `set -e
cd "$REPO_DIR"
st=$(git status --porcelain)
if [ -n "$st" ]; then dirty=$(printf '%s\n' "$st" | wc -l | tr -d ' '); else dirty=0; fi
ahead=$(git rev-list --count "$BASE_REF..HEAD")
echo "dirty=$dirty"
echo "ahead=$ahead"
`

// pushError tails git's message after redacting it.
func pushError(what string, out string, token string) string {
	msg := strings.TrimSpace(Redact(out, token))
	if msg == "" {
		return what
	}
	return what + ": " + tailRunes(msg, 600)
}

// Push publishes HEAD of the thread's checkout as the task's own branch on the
// remote. It is how a branch leaves the sandbox: the agent has no push
// credential, the host pushes, only this branch, only when open_pr asks.
//
// protected are branch names or path.Match patterns that are never pushed.
// Uncommitted changes are not pushed; Dirty reports them.
func (c *Checkout) Push(ctx context.Context, threadID string, sb sandbox.Sandbox, protected ...string) (PushResult, error) {
	ti := c.info(ctx, threadID)
	if ti == nil {
		return PushResult{}, errors.New("this thread has no repository to push")
	}
	if err := checkRepo(ti.Repo); err != nil {
		return PushResult{}, err
	}
	branch, base := ti.Branch, ti.BaseBranch
	switch {
	case branch == "":
		return PushResult{}, errors.New("this thread has no working branch")
	case !validRef(branch):
		return PushResult{}, fmt.Errorf("refusing to push: %q is not a valid branch name", branch)
	case base != "" && !validRef(base):
		return PushResult{}, fmt.Errorf("refusing to push: the base branch name %q is not valid", base)
	case branch == base:
		return PushResult{}, fmt.Errorf("refusing to push the base branch %s: commit on your own branch", base)
	case isProtected(branch, protected):
		return PushResult{}, fmt.Errorf("refusing to push the protected branch %s: switch to your own branch", branch)
	}
	token := c.token(ctx, ti.Repo)
	if token == "" {
		return PushResult{}, errors.New("no repository token is available to push with")
	}
	dir := c.dir()
	baseRef := "origin/HEAD"
	if base != "" {
		baseRef = "origin/" + base
	}

	ictx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, res, err := execOut(ictx, sb, sandbox.ExecRequest{Command: inspectScript, Timeout: time.Minute,
		Env: withIsolatedConfig(map[string]string{"REPO_DIR": dir, "BASE_REF": baseRef})})
	if err != nil {
		return PushResult{}, fmt.Errorf("inspecting the checkout: %s", Redact(err.Error(), token))
	}
	if res.ExitCode != 0 {
		return PushResult{}, errors.New(pushError("inspecting the checkout failed", out, token))
	}
	dirty, ahead, ok := parseInspect(out)
	if !ok {
		return PushResult{}, errors.New(pushError("could not read the state of the checkout", out, token))
	}
	if ahead == 0 {
		return PushResult{}, errors.New("nothing to push: commit your changes first")
	}

	// --force-with-lease is acceptable only because the branch is the
	// factory's own task branch: after a conflict the agent rebases, and the
	// rebased history must be pushable. The lease still refuses to overwrite a
	// commit this checkout has not seen.
	cmd := "set -e\ncd \"$REPO_DIR\"\n" + guardScript + gitWithToken + " -C " + shellQuote(dir) +
		" push --force-with-lease origin HEAD:refs/heads/" + branch
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Minute)
	defer pcancel()
	out, res, err = execOut(pctx, sb, sandbox.ExecRequest{Command: cmd, Timeout: 5 * time.Minute,
		Env: withIsolatedConfig(map[string]string{"GH_TOKEN": token, "GIT_TERMINAL_PROMPT": "0",
			"REPO_DIR": dir, "EXPECT_URL": repoURL(ti.Repo)})})
	if err != nil {
		return PushResult{}, fmt.Errorf("git push: %s", Redact(err.Error(), token))
	}
	if res.ExitCode != 0 {
		return PushResult{}, errors.New(pushError("git push failed (exit "+strconv.Itoa(res.ExitCode)+")", out, token))
	}
	return PushResult{Branch: branch, Ahead: ahead, Dirty: dirty}, nil
}

// withIsolatedConfig keeps the user-level and system git config out of the
// publishing commands: the agent may edit ~/.gitconfig (url.*.insteadOf would
// send the token elsewhere), and the repository's own config is checked by
// inspectScript.
func withIsolatedConfig(env map[string]string) map[string]string {
	env["GIT_CONFIG_GLOBAL"] = "/dev/null"
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	return env
}

// parseInspect reads the dirty= and ahead= lines of inspectScript.
func parseInspect(out string) (dirty, ahead int, ok bool) {
	var gotDirty, gotAhead bool
	for _, line := range strings.Split(out, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			continue
		}
		switch k {
		case "dirty":
			dirty, gotDirty = n, true
		case "ahead":
			ahead, gotAhead = n, true
		}
	}
	return dirty, ahead, gotDirty && gotAhead
}

// isProtected matches a branch against exact names and path.Match patterns.
func isProtected(branch string, protected []string) bool {
	for _, p := range protected {
		if p == branch {
			return true
		}
		if m, err := path.Match(p, branch); err == nil && m {
			return true
		}
	}
	return false
}

var plainPath = regexp.MustCompile(`^[A-Za-z0-9._/@:+=-]+$`)

// shellQuote leaves a plain path alone and single-quotes anything else.
func shellQuote(s string) string {
	if plainPath.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Section tells the agent of a repository thread how to deliver its work. The
// repository is assumed to be at DefaultDir; use Checkout.Section for another Dir.
func Section(st factory.Store) agent.PromptSection { return section(st, DefaultDir) }

// Section is Section for the checkout's own Dir.
func (c *Checkout) Section() agent.PromptSection { return section(c.Store, c.dir()) }

func section(st factory.Store, dir string) agent.PromptSection {
	return func(ctx context.Context, in agent.Input) string {
		if st == nil || in.Run == nil || in.Run.ThreadID == "" {
			return ""
		}
		ti, err := st.GetThreadInfo(ctx, in.Run.ThreadID)
		if err != nil {
			if !errors.Is(err, factory.ErrNotFound) {
				slog.Warn("forge: thread info unavailable", "thread_id", in.Run.ThreadID, "error", err)
			}
			return ""
		}
		if ti.Repo == "" {
			return ""
		}
		base := ti.BaseBranch
		if base == "" {
			base = "the repository's default branch"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "# Repository\n\nYour work is in the repository %s, checked out in %s on your branch %s (based on %s). Work there and commit often, in small steps with clear messages.\n", ti.Repo, dir, ti.Branch, base)
		b.WriteString("\nDelivering:\n")
		b.WriteString("- You cannot push: you have no push credentials. Commit your work on your branch; uncommitted changes are never published.\n")
		if ti.TaskID != "" {
			b.WriteString("- Call open_pr (title, body) to publish: it pushes your branch and opens the pull request. Calling it again pushes your newer commits.\n")
			b.WriteString("- After CI runs, pr_status shows the checks; read a failure with ci_log and pr_comments shows the review.\n")
		}
		fmt.Fprintf(&b, "- Stay on your own branch: never commit on %s or on any protected branch.\n", base)
		b.WriteString("- Keep the credentials out of files, commits and output.\n")
		return b.String()
	}
}
