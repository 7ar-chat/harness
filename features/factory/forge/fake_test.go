package forge_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/features/factory/forge"
)

const (
	repo  = "acme/app"
	token = "ghs_secretInstallationToken1234567890"
	sha1  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sha2  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type staticAuth struct{}

func (staticAuth) Token(context.Context, string) (string, error) { return token, nil }

// clock is a manually advanced time source shared by the store and the loop.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeGitHub is a scriptable GitHub REST API.
type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// state of the single pull request (number 7)
	state     string
	merged    bool
	draft     bool
	mergeable *bool
	head      string
	headSHA   string
	base      string
	runs      map[string][]map[string]any // sha -> check runs
	statuses  map[string][]map[string]any // sha -> commit statuses
	issueCmts []map[string]any
	reviews   []map[string]any
	inline    []map[string]any
	logs      map[string]string // job id -> log
	protected []string
	// requests records "METHOD path"; bodies the JSON bodies of writes by "METHOD path".
	requests []string
	bodies   map[string][]map[string]any
	auths    []string
	// failNext maps "METHOD path" to a queue of statuses (with headers) to answer before the real handler.
	failNext map[string][]failure
	// mergeStatus overrides the merge answer when not 0.
	mergeStatus int
	listPRs     []map[string]any
	createStat  int
}

type failure struct {
	status  int
	header  http.Header
	message string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	g := &fakeGitHub{t: t, state: "open", head: "har/abc", headSHA: sha1, base: "main",
		runs: map[string][]map[string]any{}, statuses: map[string][]map[string]any{}, logs: map[string]string{},
		bodies: map[string][]map[string]any{}, failNext: map[string][]failure{}}
	yes := true
	g.mergeable = &yes
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) client(opts ...func(*forge.GitHubOptions)) *forge.GitHub {
	o := forge.GitHubOptions{API: g.srv.URL, Auth: staticAuth{}, HTTP: g.srv.Client(),
		Sleep: func(context.Context, time.Duration) error { return nil }}
	for _, f := range opts {
		f(&o)
	}
	return forge.NewGitHub(o)
}

func (g *fakeGitHub) set(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f()
}

func (g *fakeGitHub) count(req string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, r := range g.requests {
		if r == req {
			n++
		}
	}
	return n
}

func (g *fakeGitHub) body(req string) []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.bodies[req]...)
}

func (g *fakeGitHub) pr() map[string]any {
	return map[string]any{
		"number": 7, "html_url": "https://github.com/" + repo + "/pull/7", "title": "Fix it",
		"state": g.state, "merged": g.merged, "draft": g.draft, "mergeable": g.mergeable,
		"base": map[string]any{"ref": g.base}, "head": map[string]any{"ref": g.head, "sha": g.headSHA},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func page(r *http.Request, items []map[string]any) []map[string]any {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pp, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if p < 1 {
		p = 1
	}
	if pp < 1 {
		pp = len(items)
	}
	lo := (p - 1) * pp
	if lo > len(items) {
		lo = len(items)
	}
	hi := min(lo+pp, len(items))
	return items[lo:hi]
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := r.Method + " " + r.URL.Path
	g.requests = append(g.requests, key)
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	if q := g.failNext[key]; len(q) > 0 {
		f := q[0]
		g.failNext[key] = q[1:]
		for k, v := range f.header {
			w.Header()[k] = v
		}
		writeJSON(w, f.status, map[string]any{"message": f.message})
		return
	}
	var body map[string]any
	if r.Body != nil && (r.Method == http.MethodPost || r.Method == http.MethodPut) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		g.bodies[key] = append(g.bodies[key], body)
	}
	p := strings.TrimPrefix(r.URL.Path, "/repos/"+repo)
	switch {
	case r.Method == http.MethodGet && p == "":
		writeJSON(w, 200, map[string]any{"default_branch": "main"})
	case r.Method == http.MethodGet && p == "/branches":
		var out []map[string]any
		for _, b := range g.protected {
			out = append(out, map[string]any{"name": b, "protected": true})
		}
		writeJSON(w, 200, out)
	case r.Method == http.MethodGet && p == "/pulls":
		writeJSON(w, 200, g.listPRs)
	case r.Method == http.MethodPost && p == "/pulls":
		if g.createStat != 0 {
			writeJSON(w, g.createStat, map[string]any{"message": "Validation Failed",
				"errors": []map[string]any{{"message": "A pull request already exists for acme:" + g.head + "."}}})
			return
		}
		out := g.pr()
		out["title"], out["state"] = body["title"], "open"
		out["head"], out["base"] = map[string]any{"ref": body["head"], "sha": g.headSHA}, map[string]any{"ref": body["base"]}
		writeJSON(w, 201, out)
	case r.Method == http.MethodGet && p == "/pulls/7":
		if strings.Contains(r.Header.Get("Accept"), "diff") {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, strings.Repeat("d", 2<<20)+"TAIL")
			return
		}
		writeJSON(w, 200, g.pr())
	case r.Method == http.MethodPut && p == "/pulls/7/merge":
		if g.mergeStatus != 0 {
			writeJSON(w, g.mergeStatus, map[string]any{"message": "Head branch was modified. Review and try the merge again."})
			return
		}
		if body["sha"] != g.headSHA {
			writeJSON(w, 409, map[string]any{"message": "Head branch was modified. Review and try the merge again."})
			return
		}
		g.merged, g.state = true, "closed"
		writeJSON(w, 200, map[string]any{"merged": true, "message": "Pull Request successfully merged"})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/check-runs"):
		s := strings.Split(p, "/")[2]
		writeJSON(w, 200, map[string]any{"total_count": len(g.runs[s]), "check_runs": page(r, g.runs[s])})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/status"):
		s := strings.Split(p, "/")[2]
		writeJSON(w, 200, map[string]any{"state": "x", "statuses": page(r, g.statuses[s])})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/actions/jobs/") && strings.HasSuffix(p, "/logs"):
		id := strings.Split(p, "/")[3]
		l, ok := g.logs[id]
		if !ok {
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		_, _ = io.WriteString(w, l)
	case r.Method == http.MethodGet && p == "/issues/7/comments":
		writeJSON(w, 200, page(r, g.issueCmts))
	case r.Method == http.MethodGet && p == "/pulls/7/reviews":
		writeJSON(w, 200, page(r, g.reviews))
	case r.Method == http.MethodGet && p == "/pulls/7/comments":
		writeJSON(w, 200, page(r, g.inline))
	case r.Method == http.MethodPost && p == "/issues/7/comments":
		writeJSON(w, 201, map[string]any{"id": 1})
	case r.Method == http.MethodPost && p == "/pulls/7/reviews":
		writeJSON(w, 200, map[string]any{"id": 1})
	default:
		g.t.Errorf("fake github: unexpected request %s", key)
		writeJSON(w, 500, map[string]any{"message": "unexpected " + key})
	}
}

func run(name, status, conclusion, jobID string) map[string]any {
	m := map[string]any{"name": name, "status": status, "html_url": "https://github.com/" + repo + "/runs/1"}
	if conclusion != "" {
		m["conclusion"] = conclusion
	}
	if jobID != "" {
		m["details_url"] = "https://github.com/" + repo + "/actions/runs/99/job/" + jobID
		m["html_url"] = m["details_url"]
	}
	return m
}

func user(login string) map[string]any { return map[string]any{"login": login} }

// ctrl is a factory.Controller over a store: it does what the tasks feature does to the task.
type ctrl struct {
	store factory.Store
	mu    sync.Mutex
	calls []string
	// feedback of the resumes, by order
	feedback []string
	blocks   []string
	fails    []string
	results  []string
	prs      []factory.PullRequest
}

func (c *ctrl) record(s string) {
	c.mu.Lock()
	c.calls = append(c.calls, s)
	c.mu.Unlock()
}

func (c *ctrl) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}

func (c *ctrl) Resume(ctx context.Context, id, feedback string) error {
	c.record("resume")
	st := factory.StateReady
	c.mu.Lock()
	c.feedback = append(c.feedback, feedback)
	c.mu.Unlock()
	_, err := c.store.UpdateTask(ctx, id, factory.Patch{State: &st, Feedback: &feedback,
		IfState: []factory.State{factory.StateRunning, factory.StateReview, factory.StateBlocked}})
	return err
}

func (c *ctrl) Block(ctx context.Context, id, reason string) error {
	c.record("block")
	st := factory.StateBlocked
	c.mu.Lock()
	c.blocks = append(c.blocks, reason)
	c.mu.Unlock()
	_, err := c.store.UpdateTask(ctx, id, factory.Patch{State: &st, BlockedReason: &reason})
	return err
}

func (c *ctrl) Complete(ctx context.Context, id, result string) error {
	c.record("complete")
	st := factory.StateDone
	c.mu.Lock()
	c.results = append(c.results, result)
	c.mu.Unlock()
	_, err := c.store.UpdateTask(ctx, id, factory.Patch{State: &st, Result: &result})
	return err
}

func (c *ctrl) Fail(ctx context.Context, id, reason string) error {
	c.record("fail")
	st := factory.StateFailed
	c.mu.Lock()
	c.fails = append(c.fails, reason)
	c.mu.Unlock()
	_, err := c.store.UpdateTask(ctx, id, factory.Patch{State: &st, Error: &reason})
	return err
}

func (c *ctrl) Review(ctx context.Context, id string, pr factory.PullRequest) error {
	c.record("review")
	st := factory.StateReview
	c.mu.Lock()
	c.prs = append(c.prs, pr)
	c.mu.Unlock()
	_, err := c.store.UpdateTask(ctx, id, factory.Patch{State: &st, PR: &pr})
	return err
}

func (c *ctrl) Create(context.Context, factory.Task) (*factory.Task, bool, error) {
	return nil, false, fmt.Errorf("not used")
}

// gate is a scripted factory.Gate.
type gate struct {
	mu      sync.Mutex
	verdict factory.Verdict
	err     error
	calls   int
}

func (g *gate) Judge(context.Context, factory.Task, factory.PRInfo, []factory.Check) (factory.Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.verdict, g.err
}

func (g *gate) n() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

// recSandbox records every command, env and written file, and answers the
// scripted exit code. A script, when set, answers per command instead.
type recSandbox struct {
	mu     sync.Mutex
	cmds   []sandbox.ExecRequest
	files  map[string]string
	exit   int
	out    string
	script func(req sandbox.ExecRequest) (out string, exit int)
}

func (s *recSandbox) ID() string { return "sb" }
func (s *recSandbox) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, req)
	exit, out := s.exit, s.out
	script := s.script
	s.mu.Unlock()
	if script != nil {
		out, exit = script(req)
	}
	if out != "" && req.OnStdout != nil {
		req.OnStdout(out)
	}
	return sandbox.ExecResult{ExitCode: exit}, nil
}
func (s *recSandbox) ReadFile(context.Context, string, int64) ([]byte, int64, error) {
	return nil, 0, sandbox.ErrNotFound
}
func (s *recSandbox) WriteFile(_ context.Context, path string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string]string{}
	}
	s.files[path] = string(data)
	return nil
}
func (s *recSandbox) MkdirAll(context.Context, string) error { return nil }
func (s *recSandbox) List(context.Context, string, int) ([]sandbox.FileInfo, error) {
	return nil, nil
}
func (s *recSandbox) Stat(context.Context, string) (sandbox.FileInfo, error) {
	return sandbox.FileInfo{}, sandbox.ErrNotFound
}

func newStore(clk *clock) *factorystore.Mem { return factorystore.NewMem(clk.Now) }
