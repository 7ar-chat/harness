package forge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/githost"
)

// GitLab implements factory.Forge for one GitLab host over the REST API v4: a pull request is a merge request,
// CI is the pipeline of the head commit. Repositories are "host/group/subgroup/project".
type GitLab struct{ c *hostClient }

var _ factory.Forge = (*GitLab)(nil)

// NewGitLab returns the forge of a registered GitLab host. res supplies the token of the user in the context.
func NewGitLab(h githost.Host, res githost.Resolver, hc *http.Client) *GitLab {
	return &GitLab{newHostClient("gitlab", h, res, hc, func(tok string) (string, string) { return "PRIVATE-TOKEN", tok })}
}

var reGLJob = regexp.MustCompile(`/jobs/(\d+)`)

// glProject is the escaped project id of the API ("group%2Fproject").
func (g *GitLab) glProject(repo string) (string, error) {
	p, err := g.c.projectPath(repo)
	if err != nil {
		return "", err
	}
	return "/projects/" + strings.ReplaceAll(url.PathEscape(p), "/", "%2F"), nil
}

type glMR struct {
	IID          int    `json:"iid"`
	WebURL       string `json:"web_url"`
	Title        string `json:"title"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	TargetBranch string `json:"target_branch"`
	SourceBranch string `json:"source_branch"`
	SHA          string `json:"sha"`
	HasConflicts bool   `json:"has_conflicts"`
	Detailed     string `json:"detailed_merge_status"`
}

func (m glMR) info() *factory.PRInfo {
	state := m.State
	switch state {
	case "opened", "locked":
		state = "open"
	}
	pr := &factory.PRInfo{Number: m.IID, URL: m.WebURL, Title: m.Title, State: state, Draft: m.Draft || isDraftTitle(m.Title),
		Base: m.TargetBranch, Head: m.SourceBranch, HeadSHA: m.SHA}
	switch {
	case m.HasConflicts || m.Detailed == "conflict":
		f := false
		pr.Mergeable = &f
	case m.Detailed == "mergeable":
		t := true
		pr.Mergeable = &t
	}
	return pr
}

func mrPath(proj string, n int, rest string) string {
	return proj + "/merge_requests/" + strconv.Itoa(n) + rest
}

// OpenPR implements factory.Forge.
func (g *GitLab) OpenPR(ctx context.Context, in factory.OpenPR) (*factory.PRInfo, error) {
	proj, err := g.glProject(in.Repo)
	if err != nil {
		return nil, err
	}
	var m glMR
	body := map[string]any{"source_branch": in.Head, "target_branch": in.Base, "title": titleFor(in.Title, in.Draft), "description": in.Body}
	data, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: proj + "/merge_requests", body: body})
	if err != nil {
		return nil, fmt.Errorf("open merge request: %w", duplicatePR(err))
	}
	if err := decode(data, &m); err != nil {
		return nil, err
	}
	return m.info(), nil
}

// FindOpenPR returns the open merge request from head into base.
func (g *GitLab) FindOpenPR(ctx context.Context, repo, head, base string) (*factory.PRInfo, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return nil, err
	}
	var ms []glMR
	q := url.Values{"state": {"opened"}, "source_branch": {head}, "per_page": {"20"}}
	if base != "" {
		q.Set("target_branch", base)
	}
	if err := g.c.getJSON(ctx, proj+"/merge_requests", q, &ms); err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, ErrNotFound
	}
	return ms[0].info(), nil
}

// GetPR implements factory.Forge.
func (g *GitLab) GetPR(ctx context.Context, repo string, number int) (*factory.PRInfo, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return nil, err
	}
	var m glMR
	if err := g.c.getJSON(ctx, mrPath(proj, number, ""), nil, &m); err != nil {
		return nil, fmt.Errorf("get merge request !%d: %w", number, err)
	}
	return m.info(), nil
}

// Diff implements factory.Forge: the merge request's changes as a unified diff, cut at 2 MiB.
func (g *GitLab) Diff(ctx context.Context, repo string, number int) (string, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return "", err
	}
	var out struct {
		Changes []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
			New     bool   `json:"new_file"`
			Deleted bool   `json:"deleted_file"`
			Diff    string `json:"diff"`
		} `json:"changes"`
	}
	if err := g.c.getJSON(ctx, mrPath(proj, number, "/changes"), nil, &out); err != nil {
		return "", fmt.Errorf("get diff of !%d: %w", number, err)
	}
	var b strings.Builder
	for _, ch := range out.Changes {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n", ch.OldPath, ch.NewPath)
		switch {
		case ch.New:
			b.WriteString("new file mode 100644\n--- /dev/null\n")
			fmt.Fprintf(&b, "+++ b/%s\n", ch.NewPath)
		case ch.Deleted:
			fmt.Fprintf(&b, "deleted file mode 100644\n--- a/%s\n+++ /dev/null\n", ch.OldPath)
		default:
			fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", ch.OldPath, ch.NewPath)
		}
		b.WriteString(ch.Diff)
		if !strings.HasSuffix(ch.Diff, "\n") {
			b.WriteString("\n")
		}
		if b.Len() > maxDiff {
			s := b.String()[:maxDiff]
			for len(s) > 0 && !utf8.ValidString(s) {
				s = s[:len(s)-1]
			}
			return s + "\n… diff truncated at 2 MiB\n", nil
		}
	}
	return b.String(), nil
}

type glJob struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	WebURL       string `json:"web_url"`
	AllowFailure bool   `json:"allow_failure"`
	FailureReas  string `json:"failure_reason"`
}

func (j glJob) check() (factory.Check, bool) {
	c := factory.Check{Name: j.Name, URL: j.WebURL}
	switch j.Status {
	case "success":
		c.State = factory.CheckPassed
	case "failed":
		c.State = factory.CheckFailed
		if j.AllowFailure {
			c.State = factory.CheckPassed
		}
		c.Detail = firstNonEmptyStr(j.FailureReas, j.Stage)
	case "canceled", "canceling":
		c.State = factory.CheckFailed
		c.Detail = "canceled"
	case "skipped", "manual":
		return c, false // not run, nothing to wait for
	default: // created, pending, running, preparing, waiting_for_resource, scheduled
		c.State = factory.CheckPending
	}
	return c, true
}

// Checks implements factory.Forge: the jobs of the newest pipeline of the commit.
func (g *GitLab) Checks(ctx context.Context, repo, sha string) ([]factory.Check, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return nil, err
	}
	var ps []struct {
		ID int `json:"id"`
	}
	if err := g.c.getJSON(ctx, proj+"/pipelines", url.Values{"sha": {sha}, "per_page": {"1"}, "order_by": {"id"}, "sort": {"desc"}}, &ps); err != nil {
		return nil, fmt.Errorf("list pipelines of %s: %w", shortSHA(sha), err)
	}
	if len(ps) == 0 {
		return nil, nil
	}
	var out []factory.Check
	err = g.c.pages(ctx, proj+"/pipelines/"+strconv.Itoa(ps[0].ID)+"/jobs", nil, "per_page", perPage, func(page []byte) (int, error) {
		var jobs []glJob
		if err := decode(page, &jobs); err != nil {
			return 0, err
		}
		for _, j := range jobs {
			if c, ok := j.check(); ok {
				out = append(out, c)
			}
		}
		return len(jobs), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list jobs of pipeline %d: %w", ps[0].ID, err)
	}
	return out, nil
}

// FailureLog implements factory.Forge: the end of the failed job's trace, secrets masked.
func (g *GitLab) FailureLog(ctx context.Context, repo string, c factory.Check) (string, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return "", err
	}
	m := reGLJob.FindStringSubmatch(c.URL)
	if m == nil || c.State != factory.CheckFailed {
		return "", nil
	}
	data, err := g.c.do(ctx, hostRequest{method: http.MethodGet, path: proj + "/jobs/" + m[1] + "/trace", accept: "text/plain", limit: logWindow, tail: true})
	if err != nil {
		if ae, ok := err.(*APIError); ok && (ae.Status == http.StatusNotFound || ae.Status == http.StatusForbidden) {
			return "", nil
		}
		return "", fmt.Errorf("get log of %q: %w", c.Name, err)
	}
	tok, _ := g.c.token(ctx)
	log := Redact(cleanLog(strings.ToValidUTF8(string(data), "")), tok)
	return strings.TrimSpace(tailRunes(strings.TrimSpace(log), FailureLogChars)), nil
}

// Comments implements factory.Forge: the discussion notes (system notes left out) and the approvals.
func (g *GitLab) Comments(ctx context.Context, repo string, number int) ([]factory.Comment, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return nil, err
	}
	var out []factory.Comment
	err = g.c.pages(ctx, mrPath(proj, number, "/discussions"), nil, "per_page", perPage, func(page []byte) (int, error) {
		var ds []struct {
			Notes []struct {
				Body   string `json:"body"`
				System bool   `json:"system"`
				Author struct {
					Username string `json:"username"`
				} `json:"author"`
				Position *struct {
					NewPath string `json:"new_path"`
					OldPath string `json:"old_path"`
					NewLine int    `json:"new_line"`
					OldLine int    `json:"old_line"`
				} `json:"position"`
			} `json:"notes"`
		}
		if err := decode(page, &ds); err != nil {
			return 0, err
		}
		for _, d := range ds {
			for _, n := range d.Notes {
				if n.System || strings.TrimSpace(n.Body) == "" {
					continue
				}
				c := factory.Comment{Author: n.Author.Username, Body: n.Body}
				if p := n.Position; p != nil {
					c.Path, c.Line = firstNonEmptyStr(p.NewPath, p.OldPath), p.NewLine
					if c.Line == 0 {
						c.Line = p.OldLine
					}
				}
				out = append(out, c)
			}
		}
		return len(ds), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list comments of !%d: %w", number, err)
	}
	var ap struct {
		ApprovedBy []struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"approved_by"`
	}
	if g.c.getJSON(ctx, mrPath(proj, number, "/approvals"), nil, &ap) == nil { // absent on GitLab CE
		for _, a := range ap.ApprovedBy {
			out = append(out, factory.Comment{Author: a.User.Username, Review: "approved"})
		}
	}
	return out, nil
}

// Comment implements factory.Forge.
func (g *GitLab) Comment(ctx context.Context, repo string, number int, body string) error {
	proj, err := g.glProject(repo)
	if err != nil {
		return err
	}
	if _, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: mrPath(proj, number, "/notes"), body: map[string]string{"body": body}}); err != nil {
		return fmt.Errorf("comment on !%d: %w", number, err)
	}
	return nil
}

// Review implements factory.Forge. GitLab has no "request changes" verdict: it becomes a comment that says so.
func (g *GitLab) Review(ctx context.Context, repo string, number int, event, body string) error {
	proj, err := g.glProject(repo)
	if err != nil {
		return err
	}
	switch event {
	case "APPROVE":
		if _, err := g.c.do(ctx, hostRequest{method: http.MethodPost, path: mrPath(proj, number, "/approve"), body: map[string]string{}}); err == nil {
			if strings.TrimSpace(body) != "" {
				return g.Comment(ctx, repo, number, body)
			}
			return nil
		}
		// An author cannot approve their own merge request, and CE has no approvals: say it in a comment.
		return g.Comment(ctx, repo, number, firstNonEmptyStr(body, "Approved."))
	case "REQUEST_CHANGES":
		return g.Comment(ctx, repo, number, "Changes requested.\n\n"+body)
	}
	return g.Comment(ctx, repo, number, body)
}

// Merge implements factory.Forge. sha guards against a head that moved since it was read. GitLab has no per-call
// rebase merge: rebase merges like merge (a project that requires fast-forward enforces it itself).
func (g *GitLab) Merge(ctx context.Context, repo string, number int, sha string, m factory.MergeMethod) error {
	proj, err := g.glProject(repo)
	if err != nil {
		return err
	}
	body := map[string]any{"squash": m == factory.MergeSquash}
	if sha != "" {
		body["sha"] = sha
	}
	if _, err := g.c.do(ctx, hostRequest{method: http.MethodPut, path: mrPath(proj, number, "/merge"), body: body}); err != nil {
		return fmt.Errorf("merge !%d: %w", number, err)
	}
	return nil
}

// DefaultBranch implements factory.Forge.
func (g *GitLab) DefaultBranch(ctx context.Context, repo string) (string, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return "", err
	}
	var p struct {
		Default string `json:"default_branch"`
	}
	if err := g.c.getJSON(ctx, proj, nil, &p); err != nil {
		return "", fmt.Errorf("get repository: %w", err)
	}
	return p.Default, nil
}

// ProtectedBranches implements factory.Forge.
func (g *GitLab) ProtectedBranches(ctx context.Context, repo string) ([]string, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return nil, err
	}
	var out []string
	err = g.c.pages(ctx, proj+"/protected_branches", nil, "per_page", perPage, func(page []byte) (int, error) {
		var bs []struct {
			Name string `json:"name"`
		}
		if err := decode(page, &bs); err != nil {
			return 0, err
		}
		for _, b := range bs {
			out = append(out, b.Name)
		}
		return len(bs), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list protected branches: %w", err)
	}
	return out, nil
}

// OpenIssues lists the open issues of the repository, for the requirements import.
func (g *GitLab) openIssues(ctx context.Context, repo, label string, max int) ([]issueRow, error) {
	proj, err := g.glProject(repo)
	if err != nil {
		return nil, err
	}
	q := url.Values{"state": {"opened"}, "order_by": {"created_at"}, "sort": {"desc"}}
	if label != "" {
		q.Set("labels", label)
	}
	var out []issueRow
	err = g.c.pages(ctx, proj+"/issues", q, "per_page", perPage, func(page []byte) (int, error) {
		var items []struct {
			IID    int      `json:"iid"`
			Title  string   `json:"title"`
			Desc   string   `json:"description"`
			WebURL string   `json:"web_url"`
			Labels []string `json:"labels"`
			Author struct {
				Username string `json:"username"`
			} `json:"author"`
		}
		if err := decode(page, &items); err != nil {
			return 0, err
		}
		for _, it := range items {
			out = append(out, issueRow{it.IID, it.Title, it.Desc, it.WebURL, it.Author.Username, it.Labels})
		}
		if len(out) >= max {
			return 0, nil // stop: a short count ends the walk
		}
		return len(items), nil
	})
	if len(out) > max {
		out = out[:max]
	}
	return out, err
}
