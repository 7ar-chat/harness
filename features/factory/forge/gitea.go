package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/githost"
)

// Gitea implements factory.Forge for one Gitea or Forgejo host over its REST API v1. Repositories are
// "host/owner/name".
type Gitea struct{ c *hostClient }

var _ factory.Forge = (*Gitea)(nil)

// NewGitea returns the forge of a registered Gitea or Forgejo host.
func NewGitea(h githost.Host, res githost.Resolver, hc *http.Client) *Gitea {
	return &Gitea{newHostClient("gitea", h, res, hc, func(tok string) (string, string) { return "Authorization", "token " + tok })}
}

const giteaPage = 50

func (g *Gitea) repoPath(repo string) (string, error) {
	p, err := g.c.projectPath(repo)
	if err != nil {
		return "", err
	}
	parts := strings.Split(p, "/")
	if len(parts) != 2 {
		return "", fmt.Errorf("gitea: %q is not owner/name", p)
	}
	return "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]), nil
}

type gtPR struct {
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Mergeable *bool  `json:"mergeable"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p gtPR) info() *factory.PRInfo {
	state := p.State
	if p.Merged {
		state = "merged"
	}
	pr := &factory.PRInfo{Number: p.Number, URL: p.HTMLURL, Title: p.Title, State: state, Draft: isDraftTitle(p.Title),
		Base: p.Base.Ref, Head: p.Head.Ref, HeadSHA: p.Head.SHA}
	if state == "open" {
		pr.Mergeable = p.Mergeable
	}
	return pr
}

func gtPull(repo string, n int, rest string) string {
	return repo + "/pulls/" + strconv.Itoa(n) + rest
}

// OpenPR implements factory.Forge.
func (g *Gitea) OpenPR(ctx context.Context, in factory.OpenPR) (*factory.PRInfo, error) {
	repo, err := g.repoPath(in.Repo)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"head": in.Head, "base": in.Base, "title": titleFor(in.Title, in.Draft), "body": in.Body}
	data, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: repo + "/pulls", body: body})
	if err != nil {
		return nil, fmt.Errorf("open pull request: %w", duplicatePR(err))
	}
	var p gtPR
	if err := decode(data, &p); err != nil {
		return nil, err
	}
	return p.info(), nil
}

// FindOpenPR returns the open pull request from head into base.
func (g *Gitea) FindOpenPR(ctx context.Context, repo, head, base string) (*factory.PRInfo, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return nil, err
	}
	var found *factory.PRInfo
	err = g.c.pages(ctx, rp+"/pulls", url.Values{"state": {"open"}}, "limit", giteaPage, func(page []byte) (int, error) {
		var ps []gtPR
		if err := decode(page, &ps); err != nil {
			return 0, err
		}
		for _, p := range ps {
			if p.Head.Ref == head && (base == "" || p.Base.Ref == base) && found == nil {
				found = p.info()
			}
		}
		if found != nil {
			return 0, nil
		}
		return len(ps), nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}

// GetPR implements factory.Forge.
func (g *Gitea) GetPR(ctx context.Context, repo string, number int) (*factory.PRInfo, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return nil, err
	}
	var p gtPR
	if err := g.c.getJSON(ctx, gtPull(rp, number, ""), nil, &p); err != nil {
		return nil, fmt.Errorf("get pull request #%d: %w", number, err)
	}
	return p.info(), nil
}

// Diff implements factory.Forge, cut at 2 MiB.
func (g *Gitea) Diff(ctx context.Context, repo string, number int) (string, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return "", err
	}
	data, err := g.c.do(ctx, hostRequest{method: http.MethodGet, path: gtPull(rp, number, ".diff"), accept: "text/plain", limit: maxDiff})
	if err != nil {
		return "", fmt.Errorf("get diff of #%d: %w", number, err)
	}
	if len(data) < maxDiff {
		return string(data), nil
	}
	cut := data
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut) + "\n… diff truncated at 2 MiB\n", nil
}

// Checks implements factory.Forge: the commit statuses of the head (Gitea Actions and external CI report there).
func (g *Gitea) Checks(ctx context.Context, repo, sha string) ([]factory.Check, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out []factory.Check
	seen := map[string]int{}
	err = g.c.pages(ctx, rp+"/commits/"+url.PathEscape(sha)+"/statuses", url.Values{"sort": {"recentupdate"}}, "limit", giteaPage, func(page []byte) (int, error) {
		var ss []struct {
			Context string `json:"context"`
			Status  string `json:"status"`
			Target  string `json:"target_url"`
			Desc    string `json:"description"`
		}
		if err := decode(page, &ss); err != nil {
			return 0, err
		}
		for _, s := range ss {
			if _, dup := seen[s.Context]; dup { // newest first: an older status of the same check is superseded
				continue
			}
			c := factory.Check{Name: s.Context, URL: s.Target, Detail: s.Desc}
			switch s.Status {
			case "success", "warning":
				c.State = factory.CheckPassed
			case "error", "failure":
				c.State = factory.CheckFailed
			default:
				c.State = factory.CheckPending
			}
			seen[s.Context] = len(out)
			out = append(out, c)
		}
		return len(ss), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list statuses of %s: %w", shortSHA(sha), err)
	}
	return out, nil
}

// FailureLog implements factory.Forge. Gitea's API serves no job logs: the check's URL is the way to read one.
func (g *Gitea) FailureLog(ctx context.Context, repo string, c factory.Check) (string, error) {
	if _, err := g.repoPath(repo); err != nil {
		return "", err
	}
	return "", nil
}

// Comments implements factory.Forge: the conversation, the reviews with a verdict or a body and their inline comments.
func (g *Gitea) Comments(ctx context.Context, repo string, number int) ([]factory.Comment, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out []factory.Comment
	err = g.c.pages(ctx, rp+"/issues/"+strconv.Itoa(number)+"/comments", nil, "limit", giteaPage, func(page []byte) (int, error) {
		var cs []struct {
			Body string `json:"body"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := decode(page, &cs); err != nil {
			return 0, err
		}
		for _, c := range cs {
			if strings.TrimSpace(c.Body) != "" {
				out = append(out, factory.Comment{Author: c.User.Login, Body: c.Body})
			}
		}
		return len(cs), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list comments of #%d: %w", number, err)
	}
	var reviews []struct {
		ID    int    `json:"id"`
		Body  string `json:"body"`
		State string `json:"state"`
		Count int    `json:"comments_count"`
		User  struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := g.c.getJSON(ctx, gtPull(rp, number, "/reviews"), url.Values{"limit": {"30"}}, &reviews); err != nil {
		return out, nil // the conversation alone is still useful
	}
	for _, r := range reviews {
		verdict := ""
		switch r.State {
		case "APPROVED":
			verdict = "approved"
		case "REQUEST_CHANGES":
			verdict = "changes_requested"
		}
		if verdict != "" || strings.TrimSpace(r.Body) != "" {
			out = append(out, factory.Comment{Author: r.User.Login, Body: r.Body, Review: verdict})
		}
		if r.Count == 0 {
			continue
		}
		var inline []struct {
			Body string `json:"body"`
			Path string `json:"path"`
			Line int    `json:"line"`
			Pos  int    `json:"position"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if g.c.getJSON(ctx, gtPull(rp, number, "/reviews/"+strconv.Itoa(r.ID)+"/comments"), nil, &inline) != nil {
			continue
		}
		for _, c := range inline {
			out = append(out, factory.Comment{Author: c.User.Login, Body: c.Body, Path: c.Path, Line: firstNonZero(c.Line, c.Pos)})
		}
	}
	return out, nil
}

func firstNonZero(a, b int) int {
	if a != 0 {
		return a
	}
	return b
}

// Comment implements factory.Forge.
func (g *Gitea) Comment(ctx context.Context, repo string, number int, body string) error {
	rp, err := g.repoPath(repo)
	if err != nil {
		return err
	}
	if _, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: rp + "/issues/" + strconv.Itoa(number) + "/comments", body: map[string]string{"body": body}}); err != nil {
		return fmt.Errorf("comment on #%d: %w", number, err)
	}
	return nil
}

// Review implements factory.Forge.
func (g *Gitea) Review(ctx context.Context, repo string, number int, event, body string) error {
	rp, err := g.repoPath(repo)
	if err != nil {
		return err
	}
	ev := map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "REQUEST_CHANGES"}[event]
	if ev == "" {
		ev = "COMMENT"
	}
	if _, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: gtPull(rp, number, "/reviews"), body: map[string]string{"event": ev, "body": body}}); err != nil {
		if event == "APPROVE" { // an author cannot approve their own pull request
			return g.Comment(ctx, repo, number, firstNonEmptyStr(body, "Approved."))
		}
		return fmt.Errorf("review #%d: %w", number, err)
	}
	return nil
}

// Merge implements factory.Forge.
func (g *Gitea) Merge(ctx context.Context, repo string, number int, sha string, m factory.MergeMethod) error {
	rp, err := g.repoPath(repo)
	if err != nil {
		return err
	}
	do := string(m)
	if do == "" {
		do = "merge"
	}
	body := map[string]any{"Do": do}
	if sha != "" {
		body["head_commit_id"] = sha
	}
	if _, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: gtPull(rp, number, "/merge"), body: body}); err != nil {
		return fmt.Errorf("merge #%d: %w", number, err)
	}
	return nil
}

// DefaultBranch implements factory.Forge.
func (g *Gitea) DefaultBranch(ctx context.Context, repo string) (string, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return "", err
	}
	var r struct {
		Default string `json:"default_branch"`
	}
	if err := g.c.getJSON(ctx, rp, nil, &r); err != nil {
		return "", fmt.Errorf("get repository: %w", err)
	}
	return r.Default, nil
}

// ProtectedBranches implements factory.Forge.
func (g *Gitea) ProtectedBranches(ctx context.Context, repo string) ([]string, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return nil, err
	}
	var bs []struct {
		Branch string `json:"branch_name"`
		Rule   string `json:"rule_name"`
	}
	if err := g.c.getJSON(ctx, rp+"/branch_protections", nil, &bs); err != nil {
		return nil, fmt.Errorf("list protected branches: %w", err)
	}
	var out []string
	for _, b := range bs {
		if n := firstNonEmptyStr(b.Rule, b.Branch); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

func (g *Gitea) openIssues(ctx context.Context, repo, label string, max int) ([]issueRow, error) {
	rp, err := g.repoPath(repo)
	if err != nil {
		return nil, err
	}
	q := url.Values{"state": {"open"}, "type": {"issues"}}
	if label != "" {
		q.Set("labels", label)
	}
	var out []issueRow
	err = g.c.pages(ctx, rp+"/issues", q, "limit", giteaPage, func(page []byte) (int, error) {
		var items []struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
			Body   string `json:"body"`
			URL    string `json:"html_url"`
			User   struct {
				Login string `json:"login"`
			} `json:"user"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		}
		if err := decode(page, &items); err != nil {
			return 0, err
		}
		for _, it := range items {
			row := issueRow{it.Number, it.Title, it.Body, it.URL, it.User.Login, nil}
			for _, l := range it.Labels {
				row.Labels = append(row.Labels, l.Name)
			}
			out = append(out, row)
		}
		if len(out) >= max {
			return 0, nil
		}
		return len(items), nil
	})
	if len(out) > max {
		out = out[:max]
	}
	return out, err
}

func decode(data []byte, v any) error {
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
