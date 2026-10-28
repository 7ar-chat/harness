package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// DirMarketplaceSource is a local checkout (AstraForge's
// ASTRAFORGE_SKILLS_MARKETPLACE_SOURCE_PATH): CommitSHA, else "local".
type DirMarketplaceSource struct {
	Path      string
	CommitSHA string
}

// Checkout implements MarketplaceSource.
func (d DirMarketplaceSource) Checkout(context.Context) (fs.FS, string, error) {
	info, err := os.Stat(d.Path)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("marketplace source path %s is not a directory", d.Path)
	}
	commit := strings.TrimSpace(d.CommitSHA)
	if commit == "" {
		commit = "local"
	}
	return os.DirFS(d.Path), commit, nil
}

// GitLabMarketplaceSource downloads the skills/ directory of a GitLab
// repository branch through the repository API (no git binary): the branch's
// commit, then archive.zip at that commit. Authentication: Token (personal,
// project or group access token) or Username / Password exchanged for an
// OAuth token (AstraForge's _checkout_gitlab_oauth_archive).
type GitLabMarketplaceSource struct {
	RepositoryURL string // https://<gitlab>/<group>/<project>(.git)
	Branch        string
	Token         string
	Username      string
	Password      string
	Timeout       time.Duration // per request (default 5 min)
	MaxBytes      int64         // archive size cap (default 256 MiB)
	Client        *http.Client  // default http.DefaultClient
}

// Checkout implements MarketplaceSource.
func (g GitLabMarketplaceSource) Checkout(ctx context.Context) (fs.FS, string, error) {
	fsys, commit, err := g.checkout(ctx)
	if err != nil {
		return nil, "", errors.New(g.redact(err.Error()))
	}
	return fsys, commit, nil
}

func (g GitLabMarketplaceSource) redact(s string) string {
	for _, secret := range []string{g.Token, g.Password} {
		if len(strings.TrimSpace(secret)) >= 4 { // a shorter "secret" would garble the message
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
	return strings.ReplaceAll(s, g.RepositoryURL, RedactURLCredentials(g.RepositoryURL))
}

func (g GitLabMarketplaceSource) checkout(ctx context.Context) (fs.FS, string, error) {
	u, err := url.Parse(RedactURLCredentials(strings.TrimSpace(g.RepositoryURL)))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, "", errors.New("the marketplace repository URL must be an https URL")
	}
	project := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if project == "" {
		return nil, "", errors.New("the marketplace repository URL must include a project path")
	}
	base := u.Scheme + "://" + u.Host
	api := base + "/api/v4/projects/" + url.PathEscape(project) + "/repository"
	branch := strings.TrimSpace(g.Branch)
	if branch == "" {
		branch = "main"
	}
	token, err := g.token(ctx, base)
	if err != nil {
		return nil, "", err
	}
	body, err := g.get(ctx, api+"/branches/"+url.PathEscape(branch), token, 1<<20)
	if err != nil {
		return nil, "", fmt.Errorf("GitLab branch %q: %w", branch, err)
	}
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(body, &b); err != nil || strings.TrimSpace(b.Commit.ID) == "" {
		return nil, "", errors.New("the GitLab branch response did not include a commit id")
	}
	commit := strings.TrimSpace(b.Commit.ID)
	maxBytes := g.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	q := url.Values{"sha": {commit}, "path": {marketplaceSkillsDir}}
	archive, err := g.get(ctx, api+"/archive.zip?"+q.Encode(), token, maxBytes)
	if err != nil {
		return nil, "", fmt.Errorf("GitLab repository archive: %w", err)
	}
	fsys, err := archiveRoot(archive)
	if err != nil {
		return nil, "", err
	}
	return fsys, commit, nil
}

func (g GitLabMarketplaceSource) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return http.DefaultClient
}

func (g GitLabMarketplaceSource) timeout() time.Duration {
	if g.Timeout > 0 {
		return g.Timeout
	}
	return 5 * time.Minute
}

func (g GitLabMarketplaceSource) token(ctx context.Context, base string) (string, error) {
	if t := strings.TrimSpace(g.Token); t != "" {
		return t, nil
	}
	if strings.TrimSpace(g.Username) == "" || g.Password == "" {
		return "", errors.New("GitLab credentials are missing (token, or username and password)")
	}
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	form := url.Values{"grant_type": {"password"}, "username": {g.Username}, "password": {g.Password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := g.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("GitLab OAuth token: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GitLab OAuth token: HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var t struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil || strings.TrimSpace(t.AccessToken) == "" {
		return "", errors.New("the GitLab OAuth token response did not include an access token")
	}
	return t.AccessToken, nil
}

func (g GitLabMarketplaceSource) get(ctx context.Context, target, token string, maxBytes int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, g.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("response larger than %d bytes", maxBytes)
	}
	return body, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// archiveRoot opens a repository archive and returns its single top-level
// directory (GitLab's <project>-<sha>[-<path>]/) as the repository root.
func archiveRoot(data []byte) (fs.FS, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("the GitLab repository archive is not a valid zip file")
	}
	roots := map[string]bool{}
	for _, f := range zr.File {
		name := strings.TrimSuffix(f.Name, "/")
		if name == "" {
			continue
		}
		if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || path.Clean(name) != name ||
			name == ".." || strings.HasPrefix(name, "../") {
			return nil, errors.New("the GitLab repository archive contains an unsafe path")
		}
		roots[strings.SplitN(name, "/", 2)[0]] = true
	}
	if len(roots) == 1 {
		for root := range roots {
			if info, err := fs.Stat(zr, root); err == nil && info.IsDir() {
				return fs.Sub(zr, root)
			}
		}
	}
	return zr, nil
}

// FSMarketplaceSource is an in-memory checkout (tests).
type FSMarketplaceSource struct {
	FS        fs.FS
	CommitSHA string
}

// Checkout implements MarketplaceSource.
func (f FSMarketplaceSource) Checkout(context.Context) (fs.FS, string, error) {
	return f.FS, f.CommitSHA, nil
}
