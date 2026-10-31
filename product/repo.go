package product

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
)

// RepoCheckout gives the threads of a project its repository: when a thread's
// sandbox is created, the repository is cloned into /workspace/repo on a
// branch of its own (har/<thread>), so parallel agents of the same project
// never share a working tree. With the project connected to a GitHub App, the
// clone and later pushes use a short-lived installation token limited to the
// project's repository (refreshed at the start of every run); public
// repositories clone without one.
//
// Where the sandbox has a credential broker (sandbox.CredentialBroker: the
// OpenSandbox egress sidecar), the token is handed to the broker and added to
// the sandbox's HTTPS requests to github.com and api.github.com on the way
// out: it never exists inside the sandbox. Otherwise it is written to a
// credential file in the sandbox.
type RepoCheckout struct {
	Team   store.TeamStore
	GitHub *GitHubApp
	// Brokered tells the agent that GitHub requests are authenticated outside the sandbox (set when the
	// sandbox backend has a credential broker), so it uses plain git and curl without a token.
	Brokered bool
}

// RepoDir is where the repository is checked out in the sandbox.
const RepoDir = sandbox.Workspace + "/repo"

const credFile = sandbox.ScratchDir + "/git-credentials"

var safeRef = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

// Feature installs the checkout into the harness.
func (rc *RepoCheckout) Feature() harness.Feature {
	return harness.FeatureFunc("project-repo", func(b *harness.Builder) error {
		b.OnSandboxCreate(func(ctx context.Context, threadID string, sb sandbox.Sandbox, _ bool) string {
			return rc.setup(ctx, threadID, sb)
		})
		if b.Sandboxes != nil {
			b.RunStart(repoRefresh{rc: rc, mgr: b.Sandboxes})
		}
		b.Section(func(ctx context.Context, in agent.Input) string {
			p := rc.project(ctx, in.Run.ThreadID)
			if p == nil || p.RepoURL == "" {
				return ""
			}
			s := fmt.Sprintf("# Project repository\n\nThe project's repository (%s) is checked out in %s on your own branch %s. Work there; commit your changes. "+
				"Other agents of the project work in their own copies, so do not assume they share your files.", p.RepoURL, RepoDir, branchFor(in.Run.ThreadID))
			if _, _, ok := ParseRepo(p.RepoURL); ok && rc.Brokered && p.GitHubInstallation != 0 {
				s += "\n\ngit push and the GitHub API (api.github.com, for example `curl -X POST https://api.github.com/repos/OWNER/REPO/pulls` to open a pull request) " +
					"are already authenticated for this repository: send no token and look for none; there is none in the sandbox."
			}
			return s
		})
		return nil
	})
}

func branchFor(threadID string) string {
	id := strings.ReplaceAll(threadID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return "har/" + id
}

func (rc *RepoCheckout) project(ctx context.Context, threadID string) *store.Project {
	meta, err := rc.Team.GetThreadMeta(ctx, threadID)
	if err != nil || meta.ProjectID == "" {
		return nil
	}
	p, err := rc.Team.GetProject(ctx, meta.ProjectID)
	if err != nil {
		return nil
	}
	return p
}

// token is the installation token for the project, "" when it has none.
func (rc *RepoCheckout) token(ctx context.Context, p *store.Project) string {
	if rc.GitHub == nil || p.GitHubInstallation == 0 {
		return ""
	}
	// Limited to the project's repository: the installation may cover many others.
	var repos []string
	if _, repo, ok := ParseRepo(p.RepoURL); ok {
		repos = []string{repo}
	}
	t, err := rc.GitHub.InstallationToken(ctx, p.GitHubInstallation, repos...)
	if err != nil {
		slog.Warn("project repo: GitHub token failed", "project", p.ID, "error", err)
		return ""
	}
	return t
}

// cloneURL is the repository URL to clone: https for GitHub (the token is
// presented through git config, not the URL), the project's URL otherwise.
func cloneURL(p *store.Project) (string, error) {
	if owner, repo, ok := ParseRepo(p.RepoURL); ok {
		return "https://github.com/" + owner + "/" + repo + ".git", nil
	}
	u, err := url.Parse(p.RepoURL)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", fmt.Errorf("only https repository URLs without credentials can be cloned (got %q)", p.RepoURL)
	}
	return u.String(), nil
}

const setupScript = `set -e
mkdir -p /workspace/.work
if [ ! -d "$REPO_DIR/.git" ]; then
  if [ -n "${GH_TOKEN:-}" ]; then
    AUTH="Authorization: Basic $(printf 'x-access-token:%s' "$GH_TOKEN" | base64 | tr -d '\n')"
    git -c "http.extraheader=$AUTH" clone ${REPO_BRANCH:+--branch "$REPO_BRANCH"} -- "$REPO_URL" "$REPO_DIR"
  else
    git clone ${REPO_BRANCH:+--branch "$REPO_BRANCH"} -- "$REPO_URL" "$REPO_DIR"
  fi
fi
cd "$REPO_DIR"
git checkout -B "$THREAD_BRANCH" >/dev/null 2>&1 || git checkout "$THREAD_BRANCH"
git config user.name "${GIT_AUTHOR_NAME:-harness agent}"
git config user.email "${GIT_AUTHOR_EMAIL:-agent@harness.local}"
`

const credScript = `set -e
mkdir -p /workspace/.work
umask 077
printf 'https://x-access-token:%s@github.com\n' "$GH_TOKEN" > ` + credFile + `
cd "$REPO_DIR" 2>/dev/null && git config credential.helper 'store --file=` + credFile + `' || true
`

func (rc *RepoCheckout) setup(ctx context.Context, threadID string, sb sandbox.Sandbox) string {
	p := rc.project(ctx, threadID)
	if p == nil || p.RepoURL == "" {
		return ""
	}
	repoURL, err := cloneURL(p)
	if err != nil {
		return "Project repository not checked out: " + err.Error()
	}
	branch := p.DefaultBranch
	if branch != "" && !safeRef.MatchString(branch) {
		return "Project repository not checked out: the default branch name is not valid."
	}
	token := rc.token(ctx, p)
	_, _, github := ParseRepo(p.RepoURL)
	broker := github && sandbox.BrokerOf(sb) != nil
	env := map[string]string{"REPO_URL": repoURL, "REPO_DIR": RepoDir, "REPO_BRANCH": branch, "THREAD_BRANCH": branchFor(threadID)}
	switch {
	case token != "" && broker:
		// The credentials are in place before the clone, which runs without any token. If the broker refuses them
		// the sandbox still gets none: a private repository then fails to clone with a clear message, rather than
		// the secret silently entering the workload.
		rc.writeCredentials(ctx, sb, p, token)
	case token != "":
		env["GH_TOKEN"] = token
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var out strings.Builder
	res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: setupScript, Env: env, Timeout: 5 * time.Minute,
		OnStderr: func(t string) { out.WriteString(t) }, OnStdout: func(t string) { out.WriteString(t) }})
	if err != nil || res.ExitCode != 0 {
		slog.Warn("project repo: checkout failed", "thread", threadID, "exit", res.ExitCode, "error", err)
		msg := strings.TrimSpace(redact(out.String(), token))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return "Cloning the project repository into " + RepoDir + " failed: " + msg
	}
	if token != "" && !broker {
		rc.writeCredentials(ctx, sb, p, token)
	}
	return fmt.Sprintf("The project repository %s is checked out in %s on branch %s.", p.RepoURL, RepoDir, branchFor(threadID))
}

// writeCredentials gives the sandbox the token. With a credential broker the token goes to the broker: requests to
// github.com for the project's repository, and to the GitHub API for it, are authenticated on their way out and the
// secret never enters the sandbox. Without one, it is written to a credential file the sandbox's git reads.
// It reports whether the credentials are in place.
func (rc *RepoCheckout) writeCredentials(ctx context.Context, sb sandbox.Sandbox, p *store.Project, token string) bool {
	if broker := sandbox.BrokerOf(sb); broker != nil {
		owner, repo, ok := ParseRepo(p.RepoURL)
		if !ok {
			return false
		}
		if err := broker.SetCredentials(ctx, brokeredGitHub(owner, repo, token)); err != nil {
			slog.Warn("project repo: handing the token to the credential broker failed", "project", p.ID, "error", redact(err.Error(), token))
			return false
		}
		return true
	}
	res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: credScript, Timeout: time.Minute,
		Env: map[string]string{"GH_TOKEN": token, "REPO_DIR": RepoDir}})
	return err == nil && res.ExitCode == 0
}

// brokeredGitHub is what the broker needs to authenticate git (smart HTTP on github.com) and the REST API for one
// repository, and nothing else: the token itself is limited to that repository too.
func brokeredGitHub(owner, repo, token string) sandbox.CredentialSet {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return sandbox.CredentialSet{
		Credentials: []sandbox.Credential{
			{Name: "github-git", Value: basic},
			{Name: "github-api", Value: token},
		},
		Bindings: []sandbox.CredentialBinding{
			{Name: "github-git", Host: "github.com", Paths: []string{"/" + owner + "/" + repo + ".git*", "/" + owner + "/" + repo + "/*"},
				Auth: sandbox.AuthBasic, Credential: "github-git"},
			{Name: "github-api", Host: "api.github.com", Paths: []string{"/repos/" + owner + "/" + repo, "/repos/" + owner + "/" + repo + "/*"},
				Auth: sandbox.AuthBearer, Credential: "github-api"},
		},
	}
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// repoRefresh renews the sandbox's GitHub credentials at the start of a run.
type repoRefresh struct {
	rc  *RepoCheckout
	mgr sandbox.Manager
}

func (r repoRefresh) BeforeRun(ctx context.Context, run *store.Run) {
	p := r.rc.project(ctx, run.ThreadID)
	if p == nil || p.GitHubInstallation == 0 {
		return
	}
	sb, err := r.mgr.Get(ctx, run.ThreadID)
	if err != nil {
		return // no sandbox yet: its creation sets the credentials up
	}
	if token := r.rc.token(ctx, p); token != "" {
		r.rc.writeCredentials(ctx, sb, p, token)
	}
}
