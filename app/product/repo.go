package product

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/core/blobs"
	"github.com/7ar-chat/harness/core/factory"
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
	// Factory, when set, names the threads an agent factory task owns (factory.Store.GetThreadInfo has one for
	// them). The factory clones its own checkout on its own branch and never gives the agent a credential, so this
	// checkout leaves those threads alone: it would otherwise clone a second time, write the installation token into
	// the sandbox and tell the agent to push.
	Factory factory.Store
	// Brokered tells the agent that GitHub requests are authenticated outside the sandbox (set when the
	// sandbox backend has a credential broker), so it uses plain git and curl without a token.
	Brokered bool
	// Blobs holds the files of projects that have no repository URL (see projectgit.go); default: the harness's blob store.
	Blobs blobs.Blobs
	// Coordinator serialises the saving of those files between processes (default: the harness's, else this process).
	Coordinator sandbox.Coordinator
}

// RepoDir is where the repository is checked out in the sandbox.
const RepoDir = sandbox.Workspace + "/repo"

// ExtraDir holds the project's additional repositories, one directory each (/workspace/repos/<name>).
const ExtraDir = sandbox.Workspace + "/repos"

// repoCheckout is one repository of a project and where it is checked out.
type repoCheckout struct{ URL, Dir string }

var unsafeDirChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// repoDirName is the directory name for a repository URL: its last path segment, made safe.
func repoDirName(raw string) string {
	name := ""
	if _, r, ok := ParseRepo(raw); ok {
		name = r
	} else if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		name = strings.TrimSuffix(path.Base(strings.TrimSuffix(u.Path, "/")), ".git")
	}
	name = strings.Trim(unsafeDirChars.ReplaceAllString(name, "-"), ".-")
	if name == "" {
		name = "repo"
	}
	return name
}

// extraCheckouts lists the project's additional repositories with distinct directories under ExtraDir.
func extraCheckouts(p *store.Project) []repoCheckout {
	used := map[string]bool{}
	var out []repoCheckout
	for _, raw := range p.ExtraRepos {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		name := repoDirName(raw)
		for n, base := 2, name; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		used[name] = true
		out = append(out, repoCheckout{URL: raw, Dir: ExtraDir + "/" + name})
	}
	return out
}

// githubRepos lists the github.com repositories of the project (the main one first) as owner/repo pairs.
func githubRepos(p *store.Project) [][2]string {
	var out [][2]string
	for _, raw := range append([]string{p.RepoURL}, p.ExtraRepos...) {
		if o, r, ok := ParseRepo(raw); ok {
			out = append(out, [2]string{o, r})
		}
	}
	return out
}

const credFile = sandbox.ScratchDir + "/git-credentials"

var safeRef = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)

// Feature installs the checkout into the harness.
func (rc *RepoCheckout) Feature() harness.Feature {
	return harness.FeatureFunc("project-repo", func(b *harness.Builder) error {
		if rc.Blobs == nil {
			rc.Blobs = b.Blobs
		}
		if rc.Coordinator == nil && b.Infra.Coordinator != nil {
			rc.Coordinator = b.Infra.Coordinator
		}
		if b.Sandboxes != nil {
			b.EndOfRun(localPublish{rc: rc, mgr: b.Sandboxes})
		}
		b.OnSandboxCreate(func(ctx context.Context, threadID string, sb sandbox.Sandbox, _ bool) string {
			return rc.setup(ctx, threadID, sb)
		})
		if b.Sandboxes != nil {
			b.RunStart(repoRefresh{rc: rc, mgr: b.Sandboxes})
		}
		b.Section(func(ctx context.Context, in agent.Input) string {
			p := rc.project(ctx, in.Run.ThreadID)
			if p == nil {
				return ""
			}
			if p.RepoURL == "" {
				return localSection(p, in.Run.ThreadID)
			}
			s := fmt.Sprintf("# Project repository\n\nThe project's repository (%s) is checked out in %s on your own branch %s. Work there; commit your changes. "+
				"Other agents of the project work in their own copies, so do not assume they share your files.", p.RepoURL, RepoDir, branchFor(in.Run.ThreadID))
			if extras := extraCheckouts(p); len(extras) > 0 {
				s += "\n\nThe project has more repositories, each checked out on the same branch name in its own directory:"
				for _, e := range extras {
					s += fmt.Sprintf("\n- %s in %s", e.URL, e.Dir)
				}
				s += "\nCommit in each repository you change; they are separate repositories."
			}
			if rc.Factory != nil && p.GitHubInstallation != 0 {
				s += "\n\n" + changeRequestGuidance
			}
			if len(githubRepos(p)) > 0 && rc.Brokered && p.GitHubInstallation != 0 {
				s += "\n\ngit push and the GitHub API (api.github.com, for example `curl -X POST https://api.github.com/repos/OWNER/REPO/pulls` to open a pull request) " +
					"are already authenticated for the project's repositories: send no token and look for none; there is none in the sandbox."
			}
			return s
		})
		return nil
	})
}

// changeRequestGuidance tells the agent when a pull request is wanted: the user's intent decides, not the tools.
const changeRequestGuidance = `# Pull requests

Decide from what the user asked:
- A question, an explanation, a review, an investigation or a plan: answer it. Do not edit files, commit or open a pull request.
- A request to change something (a fix, a feature, a refactor, a rename, "make X do Y"): make the change on your branch, run the checks the repository has, commit, then call open_pr with a one-line title and a short description of what changed and how you verified it. Do this without being asked to open the pull request.
- Unsure whether the user wants a change or an answer: ask one short question before touching anything.
After open_pr, CI and a reviewer judge it; their findings come back to you in this conversation. Fix them on the same branch and call open_pr again.`

func branchFor(threadID string) string {
	id := strings.ReplaceAll(threadID, "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return "har/" + id
}

func (rc *RepoCheckout) project(ctx context.Context, threadID string) *store.Project {
	if rc.factoryOwns(ctx, threadID) {
		return nil
	}
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

// factoryOwns reports whether an agent factory task works on the thread. A lookup that fails for another reason than
// "no such thread" counts as owned: leaving the checkout out is safe, handing a task thread a token is not.
func (rc *RepoCheckout) factoryOwns(ctx context.Context, threadID string) bool {
	if rc.Factory == nil {
		return false
	}
	ti, err := rc.Factory.GetThreadInfo(ctx, threadID)
	switch {
	case errors.Is(err, factory.ErrNotFound):
		return false
	case err != nil:
		slog.Warn("project repo: reading the factory thread info failed; leaving the checkout out", "thread", threadID, "error", err)
		return true
	}
	return ti.TaskID != "" || ti.Repo != ""
}

// token is the installation token for the project, "" when it has none.
func (rc *RepoCheckout) token(ctx context.Context, p *store.Project) string {
	if rc.GitHub == nil || p.GitHubInstallation == 0 {
		return ""
	}
	// Only a github.com repository gets a token, and one limited to that repository: the installation may cover
	// many others, and a project whose URL was changed to another host must never receive GitHub credentials.
	var names []string
	for _, or := range githubRepos(p) {
		names = append(names, or[1])
	}
	if len(names) == 0 {
		return ""
	}
	t, err := rc.GitHub.InstallationToken(ctx, p.GitHubInstallation, names...)
	if err != nil {
		slog.Warn("project repo: GitHub token failed", "project", p.ID, "error", err)
		return ""
	}
	return t
}

// cloneURL is the repository URL to clone: https for GitHub (the token is
// presented through git config, not the URL), the project's URL otherwise.
func cloneURL(p *store.Project) (string, error) { return cloneURLOf(p.RepoURL) }

func cloneURLOf(raw string) (string, error) {
	if owner, repo, ok := ParseRepo(raw); ok {
		return "https://github.com/" + owner + "/" + repo + ".git", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return "", fmt.Errorf("only https repository URLs without credentials can be cloned (got %q)", raw)
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
	if p == nil {
		return ""
	}
	if p.RepoURL == "" {
		return rc.setupLocal(ctx, threadID, sb, p)
	}
	branch := p.DefaultBranch
	if branch != "" && !safeRef.MatchString(branch) {
		return "Project repository not checked out: the default branch name is not valid."
	}
	token := rc.token(ctx, p)
	broker := len(githubRepos(p)) > 0 && sandbox.BrokerOf(sb) != nil
	if token != "" && broker {
		// The credentials are in place before the clones, which run without any token. If the broker refuses them
		// the sandbox still gets none: a private repository then fails to clone with a clear message, rather than
		// the secret silently entering the workload.
		rc.writeCredentials(ctx, sb, p, token)
	}
	all := append([]repoCheckout{{URL: p.RepoURL, Dir: RepoDir}}, extraCheckouts(p)...)
	var notes []string
	anyOK := false
	for i, rp := range all {
		repoURL, err := cloneURLOf(rp.URL)
		if err != nil {
			if i == 0 {
				notes = append(notes, "Project repository not checked out: "+err.Error())
			} else {
				notes = append(notes, "Not checked out: "+rp.URL+": "+err.Error())
			}
			continue
		}
		b := ""
		if i == 0 {
			b = branch // the other repositories use their own default branch
		}
		env := map[string]string{"REPO_URL": repoURL, "REPO_DIR": rp.Dir, "REPO_BRANCH": b, "THREAD_BRANCH": branchFor(threadID)}
		_, _, github := ParseRepo(rp.URL)
		if token != "" && !broker && github {
			env["GH_TOKEN"] = token
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		var out strings.Builder
		res, err := sb.Exec(cctx, sandbox.ExecRequest{Command: setupScript, Env: env, Timeout: 5 * time.Minute,
			OnStderr: func(t string) { out.WriteString(t) }, OnStdout: func(t string) { out.WriteString(t) }})
		cancel()
		if err != nil || res.ExitCode != 0 {
			slog.Warn("project repo: checkout failed", "thread", threadID, "repo", rp.URL, "exit", res.ExitCode, "error", err)
			msg := strings.TrimSpace(redact(out.String(), token))
			if len(msg) > 400 {
				msg = msg[len(msg)-400:]
			}
			if i == 0 {
				notes = append(notes, "Cloning the project repository into "+RepoDir+" failed: "+msg)
			} else {
				notes = append(notes, "Cloning "+rp.URL+" into "+rp.Dir+" failed: "+msg)
			}
			continue
		}
		anyOK = true
		if i == 0 {
			notes = append(notes, fmt.Sprintf("The project repository %s is checked out in %s on branch %s.", p.RepoURL, RepoDir, branchFor(threadID)))
		} else {
			notes = append(notes, fmt.Sprintf("%s is checked out in %s.", rp.URL, rp.Dir))
		}
	}
	if token != "" && !broker && anyOK {
		rc.writeCredentials(ctx, sb, p, token)
	}
	return strings.Join(notes, " ")
}

// writeCredentials gives the sandbox the token. With a credential broker the token goes to the broker: requests to
// github.com for the project's repository, and to the GitHub API for it, are authenticated on their way out and the
// secret never enters the sandbox. Without one, it is written to a credential file the sandbox's git reads.
// It reports whether the credentials are in place.
func (rc *RepoCheckout) writeCredentials(ctx context.Context, sb sandbox.Sandbox, p *store.Project, token string) bool {
	if broker := sandbox.BrokerOf(sb); broker != nil {
		repos := githubRepos(p)
		if len(repos) == 0 {
			return false
		}
		if err := broker.SetCredentials(ctx, brokeredGitHubRepos(repos, token)); err != nil {
			slog.Warn("project repo: handing the token to the credential broker failed", "project", p.ID, "error", redact(err.Error(), token))
			return false
		}
		return true
	}
	ok := true
	for _, d := range append([]string{RepoDir}, dirsOf(extraCheckouts(p))...) {
		res, err := sb.Exec(ctx, sandbox.ExecRequest{Command: credScript, Timeout: time.Minute,
			Env: map[string]string{"GH_TOKEN": token, "REPO_DIR": d}})
		ok = ok && err == nil && res.ExitCode == 0
	}
	return ok
}

// brokeredGitHub is what the broker needs to authenticate git (smart HTTP on github.com) and the REST API for one
// repository, and nothing else: the token itself is limited to that repository too.
func brokeredGitHub(owner, repo, token string) sandbox.CredentialSet {
	return brokeredGitHubRepos([][2]string{{owner, repo}}, token)
}

func dirsOf(cs []repoCheckout) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Dir
	}
	return out
}

// brokeredGitHubRepos is brokeredGitHub for several repositories (owner, repo) that share one token.
func brokeredGitHubRepos(repos [][2]string, token string) sandbox.CredentialSet {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	var gitPaths, apiPaths []string
	for _, or := range repos {
		owner, repo := or[0], or[1]
		gitPaths = append(gitPaths, "/"+owner+"/"+repo+".git*", "/"+owner+"/"+repo+"/*")
		apiPaths = append(apiPaths, "/repos/"+owner+"/"+repo, "/repos/"+owner+"/"+repo+"/*")
	}
	return sandbox.CredentialSet{
		Credentials: []sandbox.Credential{
			{Name: "github-git", Value: basic},
			{Name: "github-api", Value: token},
		},
		Bindings: []sandbox.CredentialBinding{
			{Name: "github-git", Host: "github.com", Paths: gitPaths, Auth: sandbox.AuthBasic, Credential: "github-git"},
			{Name: "github-api", Host: "api.github.com", Paths: apiPaths, Auth: sandbox.AuthBearer, Credential: "github-api"},
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
