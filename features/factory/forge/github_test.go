package forge_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/features/factory/forge"
)

func TestGetPR(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(g *fakeGitHub)
		state    string
		mergeNil bool
	}{
		{"open", func(*fakeGitHub) {}, "open", false},
		{"merged shows as merged although GitHub says closed", func(g *fakeGitHub) { g.merged, g.state = true, "closed" }, "merged", false},
		{"closed", func(g *fakeGitHub) { g.state = "closed" }, "closed", false},
		{"mergeability not computed", func(g *fakeGitHub) { g.mergeable = nil }, "open", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			g := newFakeGitHub(t)
			tc.setup(g)
			// Act
			pr, err := g.client().GetPR(context.Background(), repo, 7)
			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if pr.State != tc.state || pr.HeadSHA != sha1 || pr.Head != "har/abc" || pr.Base != "main" || pr.Number != 7 {
				t.Fatalf("pr = %+v", pr)
			}
			if (pr.Mergeable == nil) != tc.mergeNil {
				t.Fatalf("mergeable = %v", pr.Mergeable)
			}
		})
	}
}

func TestOpenPRSendsBranchesAndDraft(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	// Act
	pr, err := g.client().OpenPR(context.Background(), factory.OpenPR{Repo: repo, Title: "T", Body: "B", Head: "har/x", Base: "dev", Draft: true})
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	sent := g.body("POST /repos/acme/app/pulls")
	if len(sent) != 1 || sent[0]["head"] != "har/x" || sent[0]["base"] != "dev" || sent[0]["draft"] != true || sent[0]["title"] != "T" {
		t.Fatalf("request body = %v", sent)
	}
	if pr.Head != "har/x" || pr.Base != "dev" || pr.Number != 7 {
		t.Fatalf("pr = %+v", pr)
	}
}

func TestChecksMapping(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	g.set(func() {
		g.runs[sha1] = []map[string]any{
			run("a-success", "completed", "success", ""),
			run("b-neutral", "completed", "neutral", ""),
			run("c-skipped", "completed", "skipped", ""),
			run("d-failure", "completed", "failure", "11"),
			run("e-timed", "completed", "timed_out", ""),
			run("f-cancelled", "completed", "cancelled", ""),
			run("g-action", "completed", "action_required", ""),
			run("h-queued", "queued", "", ""),
			run("i-progress", "in_progress", "", ""),
		}
		g.statuses[sha1] = []map[string]any{
			{"context": "ci/ok", "state": "success"},
			{"context": "ci/bad", "state": "failure", "description": "lint failed"},
			{"context": "ci/err", "state": "error"},
			{"context": "ci/wait", "state": "pending"},
		}
	})
	// Act
	checks, err := g.client().Checks(context.Background(), repo, sha1)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]factory.CheckState{
		"a-success": factory.CheckPassed, "b-neutral": factory.CheckPassed, "c-skipped": factory.CheckPassed,
		"d-failure": factory.CheckFailed, "e-timed": factory.CheckFailed, "f-cancelled": factory.CheckFailed, "g-action": factory.CheckFailed,
		"h-queued": factory.CheckPending, "i-progress": factory.CheckPending,
		"ci/ok": factory.CheckPassed, "ci/bad": factory.CheckFailed, "ci/err": factory.CheckFailed, "ci/wait": factory.CheckPending,
	}
	if len(checks) != len(want) {
		t.Fatalf("got %d checks: %+v", len(checks), checks)
	}
	for _, c := range checks {
		if want[c.Name] != c.State {
			t.Errorf("%s = %s, want %s", c.Name, c.State, want[c.Name])
		}
		if c.Name == "ci/bad" && c.Detail != "lint failed" {
			t.Errorf("status detail = %q", c.Detail)
		}
	}
}

func TestChecksPaginate(t *testing.T) {
	// Arrange: 130 check runs span two pages of 100.
	g := newFakeGitHub(t)
	g.set(func() {
		for i := range 130 {
			g.runs[sha1] = append(g.runs[sha1], run(fmt.Sprintf("job-%03d", i), "completed", "success", ""))
		}
	})
	// Act
	checks, err := g.client().Checks(context.Background(), repo, sha1)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 130 {
		t.Fatalf("got %d checks, want 130", len(checks))
	}
	if n := g.count("GET /repos/acme/app/commits/" + sha1 + "/check-runs"); n != 2 {
		t.Fatalf("check-runs requests = %d, want 2", n)
	}
}

func TestChecksRejectsBadInput(t *testing.T) {
	g := newFakeGitHub(t)
	if _, err := g.client().Checks(context.Background(), repo, "../../etc"); err == nil {
		t.Fatal("a sha with path characters must be refused")
	}
	if _, err := g.client().Checks(context.Background(), "not a repo", sha1); err == nil {
		t.Fatal("an invalid repository must be refused")
	}
	if n := len(g.requests); n != 0 {
		t.Fatalf("no request expected, got %d", n)
	}
}

func TestFailureLog(t *testing.T) {
	failing := forge.FailureLogChars
	tests := []struct {
		name   string
		check  factory.Check
		log    string
		check_ func(t *testing.T, got string)
	}{
		{
			name:  "tail, ansi stripped, secrets masked",
			check: factory.Check{Name: "build", State: factory.CheckFailed, URL: "https://github.com/acme/app/actions/runs/99/job/11"},
			log: strings.Repeat("noise line\n", 2000) + "2026-10-31T10:00:00.0000000Z \x1b[31mFAIL\x1b[0m TestX\n" +
				"using " + token + " and ghp_abcdefghijklmnopqrstuvwxyz0123456789 and password=hunter22\nFinal error\n",
			check_: func(t *testing.T, got string) {
				if len([]rune(got)) > failing+10 {
					t.Errorf("log has %d runes, want about %d", len([]rune(got)), failing)
				}
				if !strings.HasSuffix(got, "Final error") || !strings.Contains(got, "FAIL TestX") {
					t.Errorf("tail missing: %q", got[max(0, len(got)-200):])
				}
				for _, bad := range []string{"\x1b", token, "ghp_abcdef", "hunter22", "2026-10-31T10"} {
					if strings.Contains(got, bad) {
						t.Errorf("log still contains %q", bad)
					}
				}
			},
		},
		{
			name:  "a job that has no log is empty, not an error",
			check: factory.Check{Name: "build", State: factory.CheckFailed, URL: "https://github.com/acme/app/actions/runs/99/job/404"},
			check_: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("got %q", got)
				}
			},
		},
		{
			name:  "a check that is not an Actions job is empty",
			check: factory.Check{Name: "ci/bad", State: factory.CheckFailed, URL: "https://ci.example.com/build/1"},
			check_: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("got %q", got)
				}
			},
		},
		{
			name:  "a passing check has no failure log",
			check: factory.Check{Name: "build", State: factory.CheckPassed, URL: "https://github.com/acme/app/actions/runs/99/job/11"},
			log:   "fine",
			check_: func(t *testing.T, got string) {
				if got != "" {
					t.Errorf("got %q", got)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			g := newFakeGitHub(t)
			if tc.log != "" {
				g.set(func() { g.logs["11"] = tc.log })
			}
			// Act
			got, err := g.client().FailureLog(context.Background(), repo, tc.check)
			// Assert
			if err != nil {
				t.Fatal(err)
			}
			tc.check_(t, got)
		})
	}
}

func TestCheckRunUsesTheJobURLOfActions(t *testing.T) {
	// Arrange: an Actions check run: html_url is the run page, details_url names the job.
	g := newFakeGitHub(t)
	g.set(func() {
		r := run("build", "completed", "failure", "")
		r["html_url"] = "https://github.com/acme/app/runs/5"
		r["details_url"] = "https://github.com/acme/app/actions/runs/99/job/11"
		g.runs[sha1] = []map[string]any{r}
		g.logs["11"] = "boom\n"
	})
	cl := g.client()
	// Act
	checks, err := cl.Checks(context.Background(), repo, sha1)
	if err != nil {
		t.Fatal(err)
	}
	log, err := cl.FailureLog(context.Background(), repo, checks[0])
	// Assert
	if err != nil || log != "boom" {
		t.Fatalf("log = %q, err = %v", log, err)
	}
}

func TestComments(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	g.set(func() {
		g.issueCmts = []map[string]any{{"user": user("alice"), "body": "looks odd"}}
		g.reviews = []map[string]any{
			{"user": user("bob"), "body": "please add tests", "state": "CHANGES_REQUESTED"},
			{"user": user("carol"), "body": "", "state": "APPROVED"},
			{"user": user("dave"), "body": "", "state": "COMMENTED"},
			{"user": user("erin"), "body": "note", "state": "COMMENTED"},
			{"user": user("frank"), "body": "old", "state": "DISMISSED"},
		}
		g.inline = []map[string]any{{"user": user("bob"), "body": "rename", "path": "a.go", "line": 12}}
	})
	// Act
	cs, err := g.client().Comments(context.Background(), repo, 7)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		got = append(got, fmt.Sprintf("%s|%s|%s|%s|%d", c.Author, c.Review, c.Body, c.Path, c.Line))
	}
	want := []string{
		"alice||looks odd||0",
		"bob|changes_requested|please add tests||0",
		"carol|approved|||0",
		"erin||note||0",
		"bob||rename|a.go|12",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("comments:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestMergeSendsShaGuardAndMethod(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	cl := g.client()
	// Act
	err := cl.Merge(context.Background(), repo, 7, sha1, factory.MergeRebase)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	sent := g.body("PUT /repos/acme/app/pulls/7/merge")
	if len(sent) != 1 || sent[0]["sha"] != sha1 || sent[0]["merge_method"] != "rebase" {
		t.Fatalf("merge body = %v", sent)
	}
	// A stale sha is refused by GitHub and surfaces as a conflict.
	g.set(func() { g.merged, g.state, g.headSHA = false, "open", sha2 })
	err = cl.Merge(context.Background(), repo, 7, sha1, factory.MergeSquash)
	if !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("stale sha merge err = %v, want ErrConflict", err)
	}
}

func TestDefaultAndProtectedBranches(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	g.set(func() { g.protected = []string{"main", "release"} })
	cl := g.client()
	// Act
	def, err1 := cl.DefaultBranch(context.Background(), repo)
	prot, err2 := cl.ProtectedBranches(context.Background(), repo)
	// Assert
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if def != "main" || strings.Join(prot, ",") != "main,release" {
		t.Fatalf("default=%q protected=%v", def, prot)
	}
}

func TestDiffIsCappedAtTwoMiB(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	// Act
	d, err := g.client().Diff(context.Background(), repo, 7)
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if len(d) > 2<<20+100 || !strings.Contains(d, "truncated") || strings.Contains(d, "TAIL") {
		t.Fatalf("diff len=%d, tail=%q", len(d), d[len(d)-40:])
	}
}

func TestErrorsAreDistinguishable(t *testing.T) {
	tests := []struct {
		status   int
		sentinel error
		others   []error
	}{
		{404, forge.ErrNotFound, []error{forge.ErrForbidden, forge.ErrUnprocessable}},
		{403, forge.ErrForbidden, []error{forge.ErrNotFound, forge.ErrUnprocessable}},
		{422, forge.ErrUnprocessable, []error{forge.ErrNotFound, forge.ErrForbidden}},
	}
	for _, tc := range tests {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			// Arrange
			g := newFakeGitHub(t)
			g.set(func() {
				g.failNext["GET /repos/acme/app/pulls/7"] = []failure{{status: tc.status, message: "Resource not accessible"}}
			})
			// Act
			_, err := g.client().GetPR(context.Background(), repo, 7)
			// Assert
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("err = %v, want %v", err, tc.sentinel)
			}
			for _, o := range tc.others {
				if errors.Is(err, o) {
					t.Errorf("err also matches %v", o)
				}
			}
			var ae *forge.APIError
			if !errors.As(err, &ae) || ae.Status != tc.status || !strings.Contains(err.Error(), "Resource not accessible") {
				t.Fatalf("err = %v (%+v)", err, ae)
			}
		})
	}
}

func TestOpenPRValidationMessageIsKept(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	g.set(func() { g.createStat = 422 })
	// Act
	_, err := g.client().OpenPR(context.Background(), factory.OpenPR{Repo: repo, Title: "T", Head: "h", Base: "main"})
	// Assert
	if !errors.Is(err, forge.ErrUnprocessable) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v", err)
	}
}

func TestRetryOnRateLimit(t *testing.T) {
	retryAfter := func(s string) http.Header { return http.Header{"Retry-After": {s}} }
	tests := []struct {
		name     string
		fails    []failure
		wantReqs int
		wantErr  error
		wantWait []time.Duration
	}{
		{"429 with Retry-After", []failure{{status: 429, header: retryAfter("2"), message: "slow down"}}, 2, nil, []time.Duration{2 * time.Second}},
		{"secondary limit 403 without a time waits a minute", []failure{{status: 403, message: "You have exceeded a secondary rate limit"}}, 2, nil, []time.Duration{time.Minute}},
		{"exhausted primary limit waits for the reset", []failure{{status: 403, message: "API rate limit exceeded", header: http.Header{
			"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {fmt.Sprint(time.Date(2026, 10, 31, 12, 0, 30, 0, time.UTC).Unix())}}}}, 2, nil, []time.Duration{31 * time.Second}},
		{"three retries then give up", []failure{
			{status: 429, header: retryAfter("1"), message: "x"}, {status: 429, header: retryAfter("1"), message: "x"},
			{status: 429, header: retryAfter("1"), message: "x"}, {status: 429, header: retryAfter("1"), message: "x"},
		}, 4, forge.ErrRateLimited, []time.Duration{time.Second, time.Second, time.Second}},
		{"a wait longer than the bound is an error at once", []failure{{status: 429, header: retryAfter("3600"), message: "x"}}, 1, forge.ErrRateLimited, nil},
		{"a plain 403 is not retried", []failure{{status: 403, message: "Resource not accessible by integration"}}, 1, forge.ErrForbidden, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			g := newFakeGitHub(t)
			g.set(func() { g.failNext["GET /repos/acme/app/pulls/7"] = tc.fails })
			var waits []time.Duration
			cl := g.client(func(o *forge.GitHubOptions) {
				o.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
				o.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			})
			// Act
			_, err := cl.GetPR(context.Background(), repo, 7)
			// Assert
			if !errors.Is(err, tc.wantErr) && tc.wantErr != nil || tc.wantErr == nil && err != nil {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if n := g.count("GET /repos/acme/app/pulls/7"); n != tc.wantReqs {
				t.Fatalf("requests = %d, want %d", n, tc.wantReqs)
			}
			if fmt.Sprint(waits) != fmt.Sprint(tc.wantWait) {
				t.Fatalf("waits = %v, want %v", waits, tc.wantWait)
			}
		})
	}
}

func TestRetryStopsWhenContextEnds(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	g.set(func() {
		g.failNext["GET /repos/acme/app/pulls/7"] = []failure{{status: 429, header: http.Header{"Retry-After": {"5"}}, message: "x"}}
	})
	ctx, cancel := context.WithCancel(context.Background())
	cl := g.client(func(o *forge.GitHubOptions) {
		o.Sleep = func(ctx context.Context, _ time.Duration) error { cancel(); return ctx.Err() }
	})
	// Act
	_, err := cl.GetPR(ctx, repo, 7)
	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestEveryRequestCarriesAFreshToken(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	n := 0
	cl := g.client(func(o *forge.GitHubOptions) {
		o.Auth = authFunc(func(context.Context, string) (string, error) { n++; return fmt.Sprintf("tok%d", n), nil })
		o.UserAgent = "factory-test"
	})
	// Act
	_, _ = cl.GetPR(context.Background(), repo, 7)
	_, _ = cl.GetPR(context.Background(), repo, 7)
	// Assert
	if g.auths[0] != "Bearer tok1" || g.auths[1] != "Bearer tok2" {
		t.Fatalf("authorization headers = %v", g.auths)
	}
}

func TestAuthFailureIsReported(t *testing.T) {
	// Arrange
	g := newFakeGitHub(t)
	cl := g.client(func(o *forge.GitHubOptions) {
		o.Auth = authFunc(func(context.Context, string) (string, error) { return "", errors.New("app not installed") })
	})
	// Act
	_, err := cl.GetPR(context.Background(), repo, 7)
	// Assert
	if err == nil || !strings.Contains(err.Error(), "app not installed") {
		t.Fatalf("err = %v", err)
	}
	if len(g.requests) != 0 {
		t.Fatal("no request may go out without a token")
	}
}

type authFunc func(ctx context.Context, repo string) (string, error)

func (f authFunc) Token(ctx context.Context, repo string) (string, error) { return f(ctx, repo) }
