// Package forge connects the agent factory to GitHub: the factory.Forge over
// the REST API, the tools an agent uses to open and follow its pull request,
// the repository checkout of a task's sandbox, and the loop that drives a
// pull request through CI, review and merge.
package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/7ar-chat/harness/core/factory"
)

// Sentinels for the HTTP statuses callers react to; use errors.Is on an error
// from GitHub (an *APIError carries the status and message).
var (
	ErrNotFound      = errors.New("github: not found")
	ErrForbidden     = errors.New("github: forbidden")
	ErrUnprocessable = errors.New("github: unprocessable")
	// ErrConflict is a 409: for a merge, the head moved since the sha was read.
	ErrConflict = errors.New("github: conflict")
	// ErrRateLimited is a rate limit that outlasted the bounded retry.
	ErrRateLimited = errors.New("github: rate limited")
)

// APIError is a GitHub API response with an error status.
type APIError struct {
	Method  string
	Path    string
	Status  int
	Message string
	// RateLimited marks a 429 or a 403 caused by the primary or secondary rate limit.
	RateLimited bool
}

// Error implements error.
func (e *APIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("github: %s %s: %d: %s", e.Method, e.Path, e.Status, msg)
}

// Is matches the sentinels by status.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == http.StatusNotFound
	case ErrForbidden:
		return e.Status == http.StatusForbidden && !e.RateLimited
	case ErrUnprocessable:
		return e.Status == http.StatusUnprocessableEntity
	case ErrConflict:
		return e.Status == http.StatusConflict
	case ErrRateLimited:
		return e.RateLimited
	}
	return false
}

// Limits.
const (
	defaultAPI = "https://api.github.com"
	perPage    = 100
	maxPages   = 10
	maxDiff    = 2 << 20
	maxBody    = 16 << 20
	// logWindow is how much of the end of a job log is kept while streaming it.
	logWindow = 256 << 10
	// FailureLogChars is how much of a failing job's log FailureLog returns.
	FailureLogChars = 6000
	maxRetries      = 3
	// maxRetryWait bounds one wait for a rate limit: a longer reset (the primary limit's hour) is an error.
	maxRetryWait = 2 * time.Minute
	// secondaryWait is GitHub's advice when a secondary limit names no time.
	secondaryWait = time.Minute
)

// GitHubOptions configures a GitHub forge.
type GitHubOptions struct {
	// API is the REST root (default https://api.github.com; an Enterprise server has its own).
	API string
	// Auth mints the token of every request (installation tokens expire); nil sends no credentials.
	Auth factory.RepoAuth
	// HTTP is the client (default: one with a 30 s timeout).
	HTTP *http.Client
	// UserAgent is sent with every request (default "harness-factory").
	UserAgent string
	// Sleep waits for a rate limit (default: a timer that ends with ctx). For tests.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock for rate-limit reset times (default time.Now).
	Now func() time.Time
}

// GitHub implements factory.Forge over the GitHub REST API.
type GitHub struct {
	api   string
	auth  factory.RepoAuth
	hc    *http.Client
	ua    string
	sleep func(ctx context.Context, d time.Duration) error
	now   func() time.Time
}

var _ factory.Forge = (*GitHub)(nil)

// NewGitHub returns a forge for the GitHub API.
func NewGitHub(o GitHubOptions) *GitHub {
	g := &GitHub{api: strings.TrimRight(o.API, "/"), auth: o.Auth, hc: o.HTTP, ua: o.UserAgent, sleep: o.Sleep, now: o.Now}
	if g.api == "" {
		g.api = defaultAPI
	}
	if g.hc == nil {
		g.hc = &http.Client{Timeout: 30 * time.Second}
	}
	if g.ua == "" {
		g.ua = "harness-factory"
	}
	if g.sleep == nil {
		g.sleep = sleepCtx
	}
	if g.now == nil {
		g.now = time.Now
	}
	return g
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var (
	reRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	reSHA  = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	reJob  = regexp.MustCompile(`/actions/runs/\d+/jobs?/(\d+)`)
)

func checkRepo(repo string) error {
	if !reRepo.MatchString(repo) || strings.Contains(repo, "..") {
		return fmt.Errorf("github: invalid repository %q (want owner/name)", repo)
	}
	return nil
}

type request struct {
	method string
	path   string
	query  url.Values
	body   any
	accept string
	// limit caps the body read (0: maxBody); tail keeps the end of a longer body instead of failing.
	limit int64
	tail  bool
}

type response struct {
	status int
	body   []byte
	// truncated: the body was longer than request.limit and was cut.
	truncated bool
}

// do sends r, retrying a rate-limited call up to maxRetries times, and returns
// the body of a 2xx answer; any other status is an *APIError.
func (g *GitHub) do(ctx context.Context, repo string, r request) (response, error) {
	var payload []byte
	if r.body != nil {
		b, err := json.Marshal(r.body)
		if err != nil {
			return response{}, fmt.Errorf("github: encode %s %s: %w", r.method, r.path, err)
		}
		payload = b
	}
	u := g.api + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if payload != nil {
			body = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, r.method, u, body)
		if err != nil {
			return response{}, fmt.Errorf("github: %s %s: %w", r.method, r.path, err)
		}
		req.Header.Set("User-Agent", g.ua)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		accept := r.accept
		if accept == "" {
			accept = "application/vnd.github+json"
		}
		req.Header.Set("Accept", accept)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if g.auth != nil {
			tok, err := g.auth.Token(ctx, repo)
			if err != nil {
				return response{}, fmt.Errorf("github: token for %s: %w", repo, err)
			}
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		}
		resp, err := g.hc.Do(req)
		if err != nil {
			return response{}, fmt.Errorf("github: %s %s: %w", r.method, r.path, err)
		}
		data, cut, readErr := readBody(resp.Body, r)
		resp.Body.Close()
		if readErr != nil {
			return response{}, fmt.Errorf("github: read %s %s: %w", r.method, r.path, readErr)
		}
		if resp.StatusCode < 400 {
			return response{status: resp.StatusCode, body: data, truncated: cut}, nil
		}
		apiErr := &APIError{Method: r.method, Path: r.path, Status: resp.StatusCode, Message: errorMessage(data)}
		wait, limited := g.rateLimitWait(resp, apiErr.Message)
		apiErr.RateLimited = limited
		if !limited || attempt >= maxRetries || wait > maxRetryWait {
			return response{}, apiErr
		}
		if err := g.sleep(ctx, wait); err != nil {
			return response{}, err
		}
	}
}

func readBody(rc io.Reader, r request) (data []byte, truncated bool, err error) {
	if r.tail {
		w := &tailBuffer{max: int(r.limit)}
		n, err := io.Copy(w, rc)
		return w.bytes(), n > r.limit, err
	}
	limit := r.limit
	if limit <= 0 {
		limit = maxBody
	}
	data, err = io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		if r.limit > 0 {
			return data[:limit], true, nil
		}
		return nil, false, fmt.Errorf("response over %d bytes", limit)
	}
	return data, false, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

func (t *tailBuffer) bytes() []byte { return t.buf }

func errorMessage(data []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
			Field   string `json:"field"`
		} `json:"errors"`
	}
	if json.Unmarshal(data, &e) != nil || e.Message == "" {
		return clipRunes(strings.TrimSpace(string(data)), 300)
	}
	msg := e.Message
	for _, x := range e.Errors {
		switch {
		case x.Message != "":
			msg += "; " + x.Message
		case x.Code != "":
			msg += "; " + x.Field + " " + x.Code
		}
	}
	return Redact(msg)
}

// rateLimitWait says whether resp is a rate-limit answer and how long to wait.
func (g *GitHub) rateLimitWait(resp *http.Response, msg string) (time.Duration, bool) {
	h := resp.Header
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
	case http.StatusForbidden:
		if h.Get("Retry-After") == "" && h.Get("X-RateLimit-Remaining") != "0" && !strings.Contains(strings.ToLower(msg), "rate limit") {
			return 0, false
		}
	default:
		return 0, false
	}
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return time.Duration(max(secs, 0)) * time.Second, true
		}
		if t, err := http.ParseTime(ra); err == nil {
			return max(t.Sub(g.now()), 0), true
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			// A second of slack: the reset instant itself may still be refused.
			return max(time.Unix(reset, 0).Sub(g.now()), 0) + time.Second, true
		}
	}
	return secondaryWait, true
}

func (g *GitHub) get(ctx context.Context, repo, path string, q url.Values, out any) error {
	r, err := g.do(ctx, repo, request{method: http.MethodGet, path: path, query: q})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(r.body, out); err != nil {
		return fmt.Errorf("github: decode GET %s: %w", path, err)
	}
	return nil
}

// pages walks a paginated list until a short page; each calls fn with the raw page and returns its item count.
func (g *GitHub) pages(ctx context.Context, repo, path string, q url.Values, fn func(page []byte) (int, error)) error {
	for p := 1; p <= maxPages; p++ {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		qq.Set("per_page", strconv.Itoa(perPage))
		qq.Set("page", strconv.Itoa(p))
		r, err := g.do(ctx, repo, request{method: http.MethodGet, path: path, query: qq})
		if err != nil {
			return err
		}
		n, err := fn(r.body)
		if err != nil {
			return fmt.Errorf("github: decode GET %s: %w", path, err)
		}
		if n < perPage {
			return nil
		}
	}
	return nil
}

type prJSON struct {
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Merged    bool   `json:"merged"`
	Draft     bool   `json:"draft"`
	Mergeable *bool  `json:"mergeable"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (p prJSON) info() *factory.PRInfo {
	state := p.State
	if p.Merged {
		state = "merged"
	}
	return &factory.PRInfo{Number: p.Number, URL: p.HTMLURL, Title: p.Title, State: state, Draft: p.Draft,
		Base: p.Base.Ref, Head: p.Head.Ref, HeadSHA: p.Head.SHA, Mergeable: p.Mergeable}
}

func prPath(repo string, n int, rest string) string {
	return "/repos/" + repo + "/pulls/" + strconv.Itoa(n) + rest
}

// OpenPR implements factory.Forge.
func (g *GitHub) OpenPR(ctx context.Context, in factory.OpenPR) (*factory.PRInfo, error) {
	if err := checkRepo(in.Repo); err != nil {
		return nil, err
	}
	if in.Title == "" || in.Head == "" || in.Base == "" {
		return nil, fmt.Errorf("github: open pr: title, head and base are required")
	}
	r, err := g.do(ctx, in.Repo, request{method: http.MethodPost, path: "/repos/" + in.Repo + "/pulls",
		body: map[string]any{"title": in.Title, "body": in.Body, "head": in.Head, "base": in.Base, "draft": in.Draft}})
	if err != nil {
		return nil, fmt.Errorf("open pull request: %w", err)
	}
	var p prJSON
	if err := json.Unmarshal(r.body, &p); err != nil {
		return nil, fmt.Errorf("github: decode pull request: %w", err)
	}
	return p.info(), nil
}

// FindOpenPR returns the open pull request from head (a branch of repo itself)
// into base, or nil: how a caller recovers when OpenPR says one already exists.
func (g *GitHub) FindOpenPR(ctx context.Context, repo, head, base string) (*factory.PRInfo, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	owner, _, _ := strings.Cut(repo, "/")
	q := url.Values{"state": {"open"}, "head": {owner + ":" + head}}
	if base != "" {
		q.Set("base", base)
	}
	var list []prJSON
	if err := g.get(ctx, repo, "/repos/"+repo+"/pulls", q, &list); err != nil {
		return nil, fmt.Errorf("find pull request: %w", err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	return g.GetPR(ctx, repo, list[0].Number)
}

// GetPR implements factory.Forge.
func (g *GitHub) GetPR(ctx context.Context, repo string, number int) (*factory.PRInfo, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	var p prJSON
	if err := g.get(ctx, repo, prPath(repo, number, ""), nil, &p); err != nil {
		return nil, fmt.Errorf("get pull request #%d: %w", number, err)
	}
	return p.info(), nil
}

// Diff implements factory.Forge: the unified diff, cut at 2 MiB.
func (g *GitHub) Diff(ctx context.Context, repo string, number int) (string, error) {
	if err := checkRepo(repo); err != nil {
		return "", err
	}
	r, err := g.do(ctx, repo, request{method: http.MethodGet, path: prPath(repo, number, ""),
		accept: "application/vnd.github.diff", limit: maxDiff})
	if err != nil {
		return "", fmt.Errorf("get diff of #%d: %w", number, err)
	}
	if !r.truncated {
		return string(r.body), nil
	}
	cut := r.body
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(cut) + "\n… diff truncated at 2 MiB\n", nil
}

type checkRunJSON struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

func (c checkRunJSON) check() factory.Check {
	// An Actions run's html_url is the bare check-run page; its details_url names the job FailureLog needs.
	out := factory.Check{Name: c.Name, State: factory.CheckPending, URL: c.HTMLURL}
	if out.URL == "" || reJob.MatchString(c.DetailsURL) {
		out.URL = c.DetailsURL
	}
	if c.Status != "completed" {
		return out
	}
	switch c.Conclusion {
	case "success", "neutral", "skipped":
		out.State = factory.CheckPassed
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		out.State = factory.CheckFailed
		detail := strings.TrimSpace(c.Output.Title)
		if s := firstLine(c.Output.Summary); s != "" && s != detail {
			if detail != "" {
				detail += ": "
			}
			detail += s
		}
		out.Detail = clipRunes(detail, 300)
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// Checks implements factory.Forge: the check runs and the commit statuses of sha.
func (g *GitHub) Checks(ctx context.Context, repo, sha string) ([]factory.Check, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	if !reSHA.MatchString(sha) {
		return nil, fmt.Errorf("github: invalid commit sha %q", sha)
	}
	var out []factory.Check
	err := g.pages(ctx, repo, "/repos/"+repo+"/commits/"+sha+"/check-runs", nil, func(page []byte) (int, error) {
		var v struct {
			CheckRuns []checkRunJSON `json:"check_runs"`
		}
		if err := json.Unmarshal(page, &v); err != nil {
			return 0, err
		}
		for _, c := range v.CheckRuns {
			out = append(out, c.check())
		}
		return len(v.CheckRuns), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list check runs of %s: %w", shortSHA(sha), err)
	}
	err = g.pages(ctx, repo, "/repos/"+repo+"/commits/"+sha+"/status", nil, func(page []byte) (int, error) {
		var v struct {
			Statuses []struct {
				Context     string `json:"context"`
				State       string `json:"state"`
				TargetURL   string `json:"target_url"`
				Description string `json:"description"`
			} `json:"statuses"`
		}
		if err := json.Unmarshal(page, &v); err != nil {
			return 0, err
		}
		for _, s := range v.Statuses {
			c := factory.Check{Name: s.Context, State: factory.CheckPending, URL: s.TargetURL}
			switch s.State {
			case "success":
				c.State = factory.CheckPassed
			case "failure", "error":
				c.State = factory.CheckFailed
				c.Detail = clipRunes(strings.TrimSpace(s.Description), 300)
			}
			out = append(out, c)
		}
		return len(v.Statuses), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list commit statuses of %s: %w", shortSHA(sha), err)
	}
	return out, nil
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// FailureLog implements factory.Forge: the end of the log of the failed
// Actions job behind c, ANSI codes stripped and secrets masked. "" when the
// check is not a failed Actions job or its log is gone or not readable.
func (g *GitHub) FailureLog(ctx context.Context, repo string, c factory.Check) (string, error) {
	if err := checkRepo(repo); err != nil {
		return "", err
	}
	m := reJob.FindStringSubmatch(c.URL)
	if m == nil || c.State != factory.CheckFailed {
		return "", nil
	}
	r, err := g.do(ctx, repo, request{method: http.MethodGet, path: "/repos/" + repo + "/actions/jobs/" + m[1] + "/logs",
		accept: "*/*", limit: logWindow, tail: true})
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Status == http.StatusGone || ae.Status == http.StatusForbidden) {
			return "", nil
		}
		return "", fmt.Errorf("get log of %q: %w", c.Name, err)
	}
	var token string
	if g.auth != nil {
		token, _ = g.auth.Token(ctx, repo)
	}
	log := Redact(cleanLog(strings.ToValidUTF8(string(r.body), "")), token)
	return strings.TrimSpace(tailRunes(strings.TrimSpace(log), FailureLogChars)), nil
}

// Comments implements factory.Forge: the issue comments, the reviews that have
// a verdict or a body and the inline review comments of the pull request.
func (g *GitHub) Comments(ctx context.Context, repo string, number int) ([]factory.Comment, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	type user struct {
		Login string `json:"login"`
	}
	var out []factory.Comment
	err := g.pages(ctx, repo, "/repos/"+repo+"/issues/"+strconv.Itoa(number)+"/comments", nil, func(page []byte) (int, error) {
		var v []struct {
			User user   `json:"user"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal(page, &v); err != nil {
			return 0, err
		}
		for _, c := range v {
			out = append(out, factory.Comment{Author: c.User.Login, Body: c.Body})
		}
		return len(v), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list comments of #%d: %w", number, err)
	}
	err = g.pages(ctx, repo, prPath(repo, number, "/reviews"), nil, func(page []byte) (int, error) {
		var v []struct {
			User  user   `json:"user"`
			Body  string `json:"body"`
			State string `json:"state"`
		}
		if err := json.Unmarshal(page, &v); err != nil {
			return 0, err
		}
		for _, r := range v {
			c := factory.Comment{Author: r.User.Login, Body: r.Body}
			switch strings.ToUpper(r.State) {
			case "APPROVED":
				c.Review = "approved"
			case "CHANGES_REQUESTED":
				c.Review = "changes_requested"
			case "COMMENTED":
				if strings.TrimSpace(r.Body) == "" {
					continue // the wrapper of inline comments, listed below
				}
			default: // DISMISSED, PENDING
				continue
			}
			out = append(out, c)
		}
		return len(v), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list reviews of #%d: %w", number, err)
	}
	err = g.pages(ctx, repo, prPath(repo, number, "/comments"), nil, func(page []byte) (int, error) {
		var v []struct {
			User         user   `json:"user"`
			Body         string `json:"body"`
			Path         string `json:"path"`
			Line         int    `json:"line"`
			OriginalLine int    `json:"original_line"`
		}
		if err := json.Unmarshal(page, &v); err != nil {
			return 0, err
		}
		for _, c := range v {
			line := c.Line
			if line == 0 {
				line = c.OriginalLine
			}
			out = append(out, factory.Comment{Author: c.User.Login, Body: c.Body, Path: c.Path, Line: line})
		}
		return len(v), nil
	})
	if err != nil {
		return nil, fmt.Errorf("list review comments of #%d: %w", number, err)
	}
	return out, nil
}

// Comment implements factory.Forge.
func (g *GitHub) Comment(ctx context.Context, repo string, number int, body string) error {
	if err := checkRepo(repo); err != nil {
		return err
	}
	_, err := g.do(ctx, repo, request{method: http.MethodPost, path: "/repos/" + repo + "/issues/" + strconv.Itoa(number) + "/comments",
		body: map[string]string{"body": body}})
	if err != nil {
		return fmt.Errorf("comment on #%d: %w", number, err)
	}
	return nil
}

// Review implements factory.Forge.
func (g *GitHub) Review(ctx context.Context, repo string, number int, event, body string) error {
	if err := checkRepo(repo); err != nil {
		return err
	}
	switch event {
	case "APPROVE", "REQUEST_CHANGES", "COMMENT":
	default:
		return fmt.Errorf("github: review event %q is not APPROVE, REQUEST_CHANGES or COMMENT", event)
	}
	payload := map[string]string{"event": event}
	if body != "" {
		payload["body"] = body
	}
	if _, err := g.do(ctx, repo, request{method: http.MethodPost, path: prPath(repo, number, "/reviews"), body: payload}); err != nil {
		return fmt.Errorf("review #%d: %w", number, err)
	}
	return nil
}

// Merge implements factory.Forge. A non-empty sha makes GitHub refuse (409) when the head moved.
func (g *GitHub) Merge(ctx context.Context, repo string, number int, sha string, m factory.MergeMethod) error {
	if err := checkRepo(repo); err != nil {
		return err
	}
	if m == "" {
		m = factory.MergeSquash
	}
	body := map[string]string{"merge_method": string(m)}
	if sha != "" {
		body["sha"] = sha
	}
	r, err := g.do(ctx, repo, request{method: http.MethodPut, path: prPath(repo, number, "/merge"), body: body})
	if err != nil {
		return fmt.Errorf("merge #%d: %w", number, err)
	}
	var v struct {
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if json.Unmarshal(r.body, &v) == nil && !v.Merged {
		return fmt.Errorf("github: merge #%d: not merged: %s", number, v.Message)
	}
	return nil
}

// DefaultBranch implements factory.Forge.
func (g *GitHub) DefaultBranch(ctx context.Context, repo string) (string, error) {
	if err := checkRepo(repo); err != nil {
		return "", err
	}
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.get(ctx, repo, "/repos/"+repo, nil, &v); err != nil {
		return "", fmt.Errorf("get repository %s: %w", repo, err)
	}
	if v.DefaultBranch == "" {
		return "", fmt.Errorf("github: repository %s reports no default branch", repo)
	}
	return v.DefaultBranch, nil
}

// ProtectedBranches implements factory.Forge: the first 100 protected branches.
func (g *GitHub) ProtectedBranches(ctx context.Context, repo string) ([]string, error) {
	if err := checkRepo(repo); err != nil {
		return nil, err
	}
	var list []struct {
		Name      string `json:"name"`
		Protected bool   `json:"protected"`
	}
	q := url.Values{"protected": {"true"}, "per_page": {strconv.Itoa(perPage)}}
	if err := g.get(ctx, repo, "/repos/"+repo+"/branches", q, &list); err != nil {
		return nil, fmt.Errorf("list protected branches of %s: %w", repo, err)
	}
	var out []string
	for _, b := range list {
		if b.Protected {
			out = append(out, b.Name)
		}
	}
	return out, nil
}
