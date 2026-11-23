package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/githost"
	"github.com/7ar-chat/harness/core/requirements"
)

// Multi is the factory.Forge of a deployment with several code hosts: a repository id of the short form
// ("owner/name") goes to the GitHub forge, "host/path" to the provider of the registered host of that name. The
// providers are built from the host registry at the time of the call, so a host an administrator adds or changes
// is used at once, and a host that is not registered is unreachable (the egress guard of the API side).
type Multi struct {
	// GitHub serves "owner/name" (nil: GitHub.com is not configured).
	GitHub factory.Forge
	Hosts  githost.Resolver
	// HTTP is the client of the providers (default: one with a 30 s timeout).
	HTTP *http.Client

	mu    sync.Mutex
	cache map[string]factory.Forge
}

var _ factory.Forge = (*Multi)(nil)

type findOpener interface {
	FindOpenPR(ctx context.Context, repo, head, base string) (*factory.PRInfo, error)
}

// forgeFor returns the provider of repo.
func (m *Multi) forgeFor(ctx context.Context, repo string) (factory.Forge, error) {
	host, _, ok := githost.SplitRepo(repo)
	if !ok {
		return nil, fmt.Errorf("invalid repository %q", repo)
	}
	if host == "" {
		if m.GitHub == nil {
			return nil, errors.New("GitHub is not configured on this server")
		}
		return m.GitHub, nil
	}
	if m.Hosts == nil {
		return nil, githost.ErrUnknownHost
	}
	h, err := m.Hosts.Host(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", host, err)
	}
	key := fmt.Sprintf("%s|%s|%s", h.Name, h.Kind, h.BaseURL)
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.cache[key]; ok {
		return f, nil
	}
	var f factory.Forge
	switch h.Kind {
	case githost.KindGitLab:
		f = NewGitLab(h, m.Hosts, m.HTTP)
	case githost.KindGitea:
		f = NewGitea(h, m.Hosts, m.HTTP)
	default:
		f = noAPIForge{noAPI{h.Name}}
	}
	if m.cache == nil {
		m.cache = map[string]factory.Forge{}
	}
	m.cache[key] = f
	return f, nil
}

// OpenPR implements factory.Forge.
func (m *Multi) OpenPR(ctx context.Context, in factory.OpenPR) (*factory.PRInfo, error) {
	f, err := m.forgeFor(ctx, in.Repo)
	if err != nil {
		return nil, err
	}
	return f.OpenPR(ctx, in)
}

// FindOpenPR finds the open pull request of a branch where the provider can.
func (m *Multi) FindOpenPR(ctx context.Context, repo, head, base string) (*factory.PRInfo, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	if fo, ok := f.(findOpener); ok {
		return fo.FindOpenPR(ctx, repo, head, base)
	}
	return nil, ErrNotFound
}

// GetPR implements factory.Forge.
func (m *Multi) GetPR(ctx context.Context, repo string, number int) (*factory.PRInfo, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return f.GetPR(ctx, repo, number)
}

// Diff implements factory.Forge.
func (m *Multi) Diff(ctx context.Context, repo string, number int) (string, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return "", err
	}
	return f.Diff(ctx, repo, number)
}

// Checks implements factory.Forge.
func (m *Multi) Checks(ctx context.Context, repo, sha string) ([]factory.Check, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return f.Checks(ctx, repo, sha)
}

// FailureLog implements factory.Forge.
func (m *Multi) FailureLog(ctx context.Context, repo string, c factory.Check) (string, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return "", err
	}
	return f.FailureLog(ctx, repo, c)
}

// Comments implements factory.Forge.
func (m *Multi) Comments(ctx context.Context, repo string, number int) ([]factory.Comment, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return f.Comments(ctx, repo, number)
}

// Comment implements factory.Forge.
func (m *Multi) Comment(ctx context.Context, repo string, number int, body string) error {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return err
	}
	return f.Comment(ctx, repo, number, body)
}

// Review implements factory.Forge.
func (m *Multi) Review(ctx context.Context, repo string, number int, event, body string) error {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return err
	}
	return f.Review(ctx, repo, number, event, body)
}

// Merge implements factory.Forge.
func (m *Multi) Merge(ctx context.Context, repo string, number int, sha string, mm factory.MergeMethod) error {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return err
	}
	return f.Merge(ctx, repo, number, sha, mm)
}

// DefaultBranch implements factory.Forge.
func (m *Multi) DefaultBranch(ctx context.Context, repo string) (string, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return "", err
	}
	return f.DefaultBranch(ctx, repo)
}

// ProtectedBranches implements factory.Forge.
func (m *Multi) ProtectedBranches(ctx context.Context, repo string) ([]string, error) {
	f, err := m.forgeFor(ctx, repo)
	if err != nil {
		return nil, err
	}
	return f.ProtectedBranches(ctx, repo)
}

// noAPIForge is the provider of a plain git host: every call says there is no API.
type noAPIForge struct{ noAPI }

func (n noAPIForge) OpenPR(context.Context, factory.OpenPR) (*factory.PRInfo, error) {
	return nil, n.err()
}
func (n noAPIForge) GetPR(context.Context, string, int) (*factory.PRInfo, error) { return nil, n.err() }
func (n noAPIForge) Diff(context.Context, string, int) (string, error)           { return "", n.err() }
func (n noAPIForge) Checks(context.Context, string, string) ([]factory.Check, error) {
	return nil, n.err()
}
func (n noAPIForge) FailureLog(context.Context, string, factory.Check) (string, error) {
	return "", n.err()
}
func (n noAPIForge) Comments(context.Context, string, int) ([]factory.Comment, error) {
	return nil, n.err()
}
func (n noAPIForge) Comment(context.Context, string, int, string) error        { return n.err() }
func (n noAPIForge) Review(context.Context, string, int, string, string) error { return n.err() }
func (n noAPIForge) Merge(context.Context, string, int, string, factory.MergeMethod) error {
	return n.err()
}
func (n noAPIForge) DefaultBranch(context.Context, string) (string, error)       { return "", n.err() }
func (n noAPIForge) ProtectedBranches(context.Context, string) ([]string, error) { return nil, nil }

// ---- issues ----

type issueRow struct {
	Number int
	Title  string
	Body   string
	URL    string
	Author string
	Labels []string
}

// issueSource adapts a provider's issue listing to requirements.IssueSource.
type issueSource struct {
	kind string
	list func(ctx context.Context, repo, label string, max int) ([]issueRow, error)
}

func (s issueSource) Source() string { return s.kind }

func (s issueSource) OpenIssues(ctx context.Context, repo, label string, max int) ([]requirements.Issue, error) {
	rows, err := s.list(ctx, repo, label, max)
	if err != nil {
		return nil, err
	}
	out := make([]requirements.Issue, 0, len(rows))
	for _, r := range rows {
		out = append(out, requirements.Issue{Number: r.Number, Title: r.Title, Body: r.Body, URL: r.URL, Author: r.Author, Labels: r.Labels})
	}
	return out, nil
}

// IssueSourceFor returns the issue provider of a registered host: GitLab and Gitea have one, a plain git host
// does not (nil, false).
func IssueSourceFor(h githost.Host, res githost.Resolver, hc *http.Client) (requirements.IssueSource, bool) {
	switch h.Kind {
	case githost.KindGitLab:
		return issueSource{requirements.SourceGitLabIssue, NewGitLab(h, res, hc).openIssues}, true
	case githost.KindGitea:
		return issueSource{requirements.SourceGiteaIssue, NewGitea(h, res, hc).openIssues}, true
	}
	return nil, false
}
