package product

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/store"
)

// GitHubApp talks to GitHub as a GitHub App: it finds where the App is
// installed for a repository and mints short-lived installation tokens (read
// and write access to the repositories of that installation only), which the
// project checkout uses to clone and push. Projects never hold a member's
// personal token.
type GitHubApp struct {
	// Slug is the App's URL name, for the install link (https://github.com/apps/<slug>).
	Slug string
	// API is the GitHub API base (default https://api.github.com).
	API string
	// ClientID and ClientSecret are the App's OAuth credentials, used to link a
	// user's GitHub account (connector) so they can import their repositories.
	ClientID, ClientSecret string
	// WebBase is GitHub's website base (default https://github.com), for OAuth.
	WebBase string
	// HTTP is the client used (default http.DefaultClient with a timeout).
	HTTP *http.Client

	appID string
	key   *rsa.PrivateKey

	mu     sync.Mutex
	tokens map[string]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
	grant   InstallationGrant
}

// NewGitHubApp parses the App's private key (PEM, PKCS#1 or PKCS#8).
func NewGitHubApp(appID, slug string, privateKeyPEM []byte) (*GitHubApp, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return nil, errors.New("github app: the private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k8, err8 := x509.ParsePKCS8PrivateKey(block.Bytes); err8 == nil {
		rk, ok := k8.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("github app: the private key is not RSA")
		}
		key = rk
	} else {
		return nil, fmt.Errorf("github app: parsing the private key: %w", err)
	}
	return &GitHubApp{Slug: slug, appID: appID, key: key, tokens: map[string]cachedToken{}}, nil
}

func (g *GitHubApp) api() string {
	if g.API != "" {
		return strings.TrimRight(g.API, "/")
	}
	return "https://api.github.com"
}

func (g *GitHubApp) web() string {
	if g.WebBase != "" {
		return strings.TrimRight(g.WebBase, "/")
	}
	return "https://github.com"
}

func (g *GitHubApp) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwt is the App's own token (RS256, ten minutes), used to ask about installations.
func (g *GitHubApp) jwt() (string, error) {
	now := time.Now()
	hdr, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": g.appID, "iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix()})
	signing := b64(hdr) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, g.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + b64(sig), nil
}

// ErrNotInstalled means the App is not installed on the repository.
var ErrNotInstalled = errors.New("the GitHub App is not installed on this repository")

func (g *GitHubApp) do(ctx context.Context, method, path, bearer string, body any, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api()+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("github: %s %s: %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

// RepoInstallation returns the id of the App's installation on owner/repo.
func (g *GitHubApp) RepoInstallation(ctx context.Context, owner, repo string) (int64, error) {
	tok, err := g.jwt()
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	status, err := g.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/installation", tok, nil, &out)
	if status == http.StatusNotFound {
		return 0, ErrNotInstalled
	}
	return out.ID, err
}

// InstallationToken returns a token for the installation (cached until shortly before it expires). With repos
// (repository names, without the owner) the token is limited to those repositories of the installation; without,
// it covers every repository the App is installed on.
func (g *GitHubApp) InstallationToken(ctx context.Context, installation int64, repos ...string) (string, error) {
	tok, _, err := g.installationToken(ctx, installation, repos...)
	return tok, err
}

// InstallationGrant is what an installation token may do: the permissions GitHub put in the token, such as
// {"contents": "write", "pull_requests": "write"}. It is the grant of the token, not of any person, which is
// why a repository's own "permissions" field (a user's collaborator rights) is no answer to "can this push".
type InstallationGrant map[string]string

// Can reports whether the grant gives at least level ("read", "write" or "admin") on a permission.
func (g InstallationGrant) Can(permission, level string) bool {
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	return rank[g[permission]] >= rank[level] && rank[level] > 0
}

// InstallationGrant mints (or reuses) a token like InstallationToken and returns its grant too.
func (g *GitHubApp) InstallationGrant(ctx context.Context, installation int64, repos ...string) (string, InstallationGrant, error) {
	return g.installationToken(ctx, installation, repos...)
}

func (g *GitHubApp) installationToken(ctx context.Context, installation int64, repos ...string) (string, InstallationGrant, error) {
	key := strconv.FormatInt(installation, 10) + "/" + strings.Join(repos, ",")
	g.mu.Lock()
	if c, ok := g.tokens[key]; ok && time.Until(c.expires) > 2*time.Minute {
		g.mu.Unlock()
		return c.token, c.grant, nil
	}
	g.mu.Unlock()
	jwt, err := g.jwt()
	if err != nil {
		return "", nil, err
	}
	var out struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	body := map[string]any{}
	if len(repos) > 0 {
		body["repositories"] = repos
	}
	if _, err := g.do(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(installation, 10)+"/access_tokens", jwt, body, &out); err != nil {
		return "", nil, err
	}
	grant := InstallationGrant(out.Permissions)
	g.mu.Lock()
	g.tokens[key] = cachedToken{out.Token, out.ExpiresAt, grant}
	g.mu.Unlock()
	return out.Token, grant, nil
}

// ParseRepo extracts owner and repository from a GitHub URL
// (https://github.com/o/r[.git], ssh://git@github.com/o/r, git@github.com:o/r.git).
func ParseRepo(raw string) (owner, repo string, ok bool) {
	raw = strings.TrimSpace(raw)
	var path string
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		path = strings.TrimPrefix(raw, "git@github.com:")
	default:
		u, err := url.Parse(raw)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return "", "", false
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ---- routes ----

type githubStatus struct {
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	InstallURL string `json:"installUrl,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Message    string `json:"message,omitempty"`
}

func (s *server) routeGitHub() {
	s.Route("GET /projects/{pid}/github", s.githubStatus)
	s.Route("POST /projects/{pid}/github/connect", s.githubConnect)
}

func (s *server) githubState(p *store.Project) githubStatus {
	st := githubStatus{Configured: s.opt.GitHub != nil, Connected: p.GitHubInstallation != 0}
	if owner, repo, ok := ParseRepo(p.RepoURL); ok {
		st.Repo = owner + "/" + repo
	}
	switch {
	case !st.Configured:
		st.Message = "This server has no GitHub App configured; public repositories can still be cloned."
	case p.RepoURL == "":
		st.Message = "Set the repository URL first."
	case st.Repo == "":
		st.Message = "Only github.com repositories can be connected."
	case !st.Connected:
		st.InstallURL = "https://github.com/apps/" + s.opt.GitHub.Slug + "/installations/new"
		st.Message = "Install the GitHub App on this repository, then check again."
	}
	return st
}

func (s *server) githubStatus(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleViewer)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.githubState(p))
}

func (s *server) githubConnect(w http.ResponseWriter, r *http.Request, c Caller) {
	p, _, ok := s.project(w, r, c, store.RoleOwner)
	if !ok {
		return
	}
	if s.opt.GitHub == nil {
		writeJSON(w, http.StatusOK, s.githubState(p))
		return
	}
	owner, repo, parsed := ParseRepo(p.RepoURL)
	if !parsed {
		writeJSON(w, http.StatusOK, s.githubState(p))
		return
	}
	id, err := s.opt.GitHub.RepoInstallation(r.Context(), owner, repo)
	switch {
	case errors.Is(err, ErrNotInstalled):
		id = 0
	case err != nil:
		writeError(w, http.StatusBadGateway, "github_error", "GitHub could not be reached; try again.")
		return
	}
	up, err := s.team.UpdateProject(r.Context(), p.ID, store.ProjectPatch{GitHubInstallation: &id})
	if err != nil {
		fail(w, err)
		return
	}
	s.note(Caller{Actor: c.Actor, ProjectID: p.ID}, "", "github.connect", map[string]any{"connected": id != 0}, FeedEvent{Type: "project.changed"})
	writeJSON(w, http.StatusOK, s.githubState(up))
}

// FileContent reads a file of the repository at ref (the default branch when empty) with the
// installation's token; errNoRepoFile when it does not exist.
func (g *GitHubApp) FileContent(ctx context.Context, installation int64, owner, repo, path, ref string) ([]byte, error) {
	tok, err := g.InstallationToken(ctx, installation)
	if err != nil {
		return nil, err
	}
	u := g.api() + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/contents/" + path
	if ref != "" {
		u += "?ref=" + url.QueryEscape(ref)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github.raw+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNoRepoFile
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github: reading %s: %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// AppInfo is what GitHub says about the App itself: the permissions it was granted and the webhook events it
// is subscribed to.
type AppInfo struct {
	Slug, Name string
	// Permissions maps a permission ("contents", "pull_requests"…) to its level ("read", "write", "admin").
	Permissions map[string]string
	Events      []string
}

// Info asks GitHub about the App (GET /app, authenticated as the App).
func (g *GitHubApp) Info(ctx context.Context) (*AppInfo, error) {
	tok, err := g.jwt()
	if err != nil {
		return nil, err
	}
	var out struct {
		Slug        string            `json:"slug"`
		Name        string            `json:"name"`
		Permissions map[string]string `json:"permissions"`
		Events      []string          `json:"events"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/app", tok, nil, &out); err != nil {
		return nil, err
	}
	return &AppInfo{Slug: out.Slug, Name: out.Name, Permissions: out.Permissions, Events: out.Events}, nil
}

// RepoDetails is a repository as the installation sees it.
type RepoDetails struct {
	FullName, DefaultBranch string
	Private                 bool
	// CanPush and CanAdmin are what the installation token may do to the repository's contents (its grant:
	// contents write/admin), not the "permissions" field of the repository, which describes a user.
	CanPush, CanAdmin bool
}

// RepoDetails reads owner/repo with an installation token limited to that repository.
func (g *GitHubApp) RepoDetails(ctx context.Context, installation int64, owner, repo string) (*RepoDetails, error) {
	tok, grant, err := g.InstallationGrant(ctx, installation, repo)
	if err != nil {
		return nil, err
	}
	var out struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Private       bool   `json:"private"`
	}
	if _, err := g.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo), tok, nil, &out); err != nil {
		return nil, err
	}
	return &RepoDetails{FullName: out.FullName, DefaultBranch: out.DefaultBranch, Private: out.Private,
		CanPush: grant.Can("contents", "write"), CanAdmin: grant.Can("administration", "write")}, nil
}

// BranchInfo is a branch of a repository.
type BranchInfo struct {
	Name      string
	Protected bool
	SHA       string
}

// ErrBranchNotFound means the repository has no such branch.
var ErrBranchNotFound = errors.New("the repository has no such branch")

// Branch reads a branch of owner/repo with an installation token limited to that repository.
func (g *GitHubApp) Branch(ctx context.Context, installation int64, owner, repo, branch string) (*BranchInfo, error) {
	tok, err := g.InstallationToken(ctx, installation, repo)
	if err != nil {
		return nil, err
	}
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	var out struct {
		Name      string `json:"name"`
		Protected bool   `json:"protected"`
		Commit    struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	status, err := g.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repo)+"/branches/"+strings.Join(segs, "/"), tok, nil, &out)
	if status == http.StatusNotFound {
		return nil, ErrBranchNotFound
	}
	if err != nil {
		return nil, err
	}
	return &BranchInfo{Name: out.Name, Protected: out.Protected, SHA: out.Commit.SHA}, nil
}
