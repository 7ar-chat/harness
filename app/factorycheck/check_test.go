package factorycheck_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/app/factorycheck"
	"github.com/7ar-chat/harness/app/product"
)

const (
	repo   = "acme/api"
	secret = "whsec-0123456789abcdef"
)

// fakeGH is a GitHub that serves what the preflight reads and records every request.
type fakeGH struct {
	mu   sync.Mutex
	hits []string

	perms         map[string]string
	events        []string
	installed     bool
	push          bool
	protected     bool
	branchMissing bool
	defaultBranch string
	status        map[string]int // path -> forced status
}

func newFakeGH() *fakeGH {
	return &fakeGH{
		perms:     map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "checks": "read", "metadata": "read"},
		events:    []string{"issues", "issue_comment", "pull_request", "pull_request_review", "check_suite"},
		installed: true, push: true, protected: true, defaultBranch: "main", status: map[string]int{},
	}
}

func (g *fakeGH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := r.URL.EscapedPath()
	g.hits = append(g.hits, r.Method+" "+p)
	if st := g.status[p]; st != 0 {
		http.Error(w, "boom", st)
		return
	}
	switch {
	case p == "/app":
		_ = json.NewEncoder(w).Encode(map[string]any{"slug": "my-app", "name": "My App", "permissions": g.perms, "events": g.events})
	case p == "/repos/acme/api/installation":
		if !g.installed {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"id":77}`))
	case p == "/app/installations/77/access_tokens":
		// As on GitHub: what the token may do is in the token response, as the permissions of the installation.
		grant := map[string]string{"metadata": "read", "pull_requests": "write"}
		if g.push {
			grant["contents"] = "write"
		} else {
			grant["contents"] = "read"
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_secret", "expires_at": "2099-01-01T00:00:00Z", "permissions": grant})
	case p == "/repos/acme/api":
		// As on GitHub for an installation token: "permissions" describes a user, so it is all false here
		// whatever the App may do. A check that read push from it would fail every real installation.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"full_name": "acme/api", "default_branch": g.defaultBranch, "private": true,
			"permissions": map[string]bool{"push": false, "admin": false, "pull": false},
		})
	case p == "/repos/acme/api/branches/"+g.defaultBranch:
		if g.branchMissing {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": g.defaultBranch, "protected": g.protected, "commit": map[string]string{"sha": "abc"}})
	default:
		http.NotFound(w, r)
	}
}

func newApp(t *testing.T, api string) *product.GitHubApp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	app, err := product.NewGitHubApp("12345", "my-app", pemKey)
	if err != nil {
		t.Fatal(err)
	}
	app.API = api
	return app
}

// goodEnv is a configuration with nothing to complain about.
func goodEnv() map[string]string {
	return map[string]string{
		"FACTORY": "1", "GITHUB_APP_ID": "12345", "GITHUB_APP_PRIVATE_KEY": "pem",
		"FACTORY_WEBHOOK_SECRET": secret,
		"FACTORY_REVIEW_MODEL":   "reviewer-model", "LLM_DEFAULT_MODEL": "author-model", "LLM_MODELS": "author-model,reviewer-model",
		"SANDBOX_BACKEND": "opensandbox", "OPENSANDBOX_URL": "http://sandbox", "OPENSANDBOX_CREDENTIAL_PROXY": "true",
		"HARNESS_TEAM": "1", "MAX_SANDBOXES_PER_USER": "10", "FACTORY_MAX_CONCURRENT": "4",
		"FACTORY_TASK_BUDGET_USD": "5", "FACTORY_DAILY_BUDGET_USD": "50",
	}
}

type rig struct {
	t   *testing.T
	gh  *fakeGH
	env map[string]string
	app *product.GitHubApp
	srv *httptest.Server
	rp  string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	gh := newFakeGH()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	return &rig{t: t, gh: gh, env: goodEnv(), app: newApp(t, srv.URL), srv: srv, rp: repo}
}

func (r *rig) run() factorycheck.Report {
	r.t.Helper()
	return factorycheck.Run(context.Background(), factorycheck.Options{
		Getenv: func(k string) string { return r.env[k] }, App: r.app, Repo: r.rp,
		Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
	})
}

func statuses(rep factorycheck.Report) map[string]factorycheck.Status {
	out := map[string]factorycheck.Status{}
	for _, c := range rep.Checks {
		out[c.Name] = c.Status
	}
	return out
}

func find(t *testing.T, rep factorycheck.Report, name string) factorycheck.Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %v", name, statuses(rep))
	return factorycheck.Check{}
}

func TestRunReportsEveryCheckInOrderAndPassesWhenEverythingIsRight(t *testing.T) {
	// Arrange
	r := newRig(t)

	// Act
	rep := r.run()

	// Assert
	want := []string{
		factorycheck.NameFactory, factorycheck.NameApp, factorycheck.NameWebhook, factorycheck.NamePermissions,
		factorycheck.NameEvents, factorycheck.NameInstalled, factorycheck.NameToken, factorycheck.NameRepoAccess,
		factorycheck.NameBranch, factorycheck.NameReviewer, factorycheck.NameSandbox, factorycheck.NameSandboxQuota,
		factorycheck.NameBudgets,
	}
	if len(rep.Checks) != len(want) {
		t.Fatalf("checks = %v", statuses(rep))
	}
	for i, c := range rep.Checks {
		if c.Name != want[i] {
			t.Errorf("check %d = %q, want %q", i, c.Name, want[i])
		}
		if c.Status != factorycheck.OK {
			t.Errorf("%s = %v (%s; fix: %s)", c.Name, c.Status, c.Detail, c.Fix)
		}
	}
	if rep.Failed() {
		t.Fatal("a clean setup failed")
	}
	if !rep.At.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("At = %v", rep.At)
	}
}

func TestRunNeverWritesToTheRepository(t *testing.T) {
	// Arrange
	r := newRig(t)

	// Act
	r.run()

	// Assert: only reads, plus the one POST that mints the installation token.
	for _, h := range r.gh.hits {
		if !strings.HasPrefix(h, "GET ") && h != "POST /app/installations/77/access_tokens" {
			t.Errorf("non-read request %q", h)
		}
	}
	if len(r.gh.hits) != 5 {
		t.Fatalf("requests = %v", r.gh.hits)
	}
}

func TestRunScenarios(t *testing.T) {
	type want = map[string]factorycheck.Status
	cases := []struct {
		name  string
		setup func(r *rig)
		want  want
		fixes map[string]string // check -> substring its Fix must contain
	}{
		{
			name:  "missing contents write fails naming the permission",
			setup: func(r *rig) { r.gh.perms["contents"] = "read" },
			want:  want{factorycheck.NamePermissions: factorycheck.Fail},
			fixes: map[string]string{factorycheck.NamePermissions: "Contents"},
		},
		{
			name:  "missing pull request permission fails",
			setup: func(r *rig) { delete(r.gh.perms, "pull_requests") },
			want:  want{factorycheck.NamePermissions: factorycheck.Fail},
			fixes: map[string]string{factorycheck.NamePermissions: "Pull requests"},
		},
		{
			name:  "checks permission lacking fails",
			setup: func(r *rig) { delete(r.gh.perms, "checks") },
			want:  want{factorycheck.NamePermissions: factorycheck.Fail},
		},
		{
			name:  "admin counts as write",
			setup: func(r *rig) { r.gh.perms["contents"] = "admin" },
			want:  want{factorycheck.NamePermissions: factorycheck.OK},
		},
		{
			name:  "issues permission lacking fails",
			setup: func(r *rig) { delete(r.gh.perms, "issues") },
			want:  want{factorycheck.NamePermissions: factorycheck.Fail},
		},
		{
			name:  "metadata lacking fails",
			setup: func(r *rig) { delete(r.gh.perms, "metadata") },
			want:  want{factorycheck.NamePermissions: factorycheck.Fail},
		},
		{
			name:  "missing events warn",
			setup: func(r *rig) { r.gh.events = []string{"issues"} },
			want:  want{factorycheck.NameEvents: factorycheck.Warn},
		},
		{
			name: "missing issues event warns that label triggers cannot fire",
			setup: func(r *rig) {
				r.gh.events = []string{"pull_request", "issue_comment", "pull_request_review", "check_suite"}
			},
			want:  want{factorycheck.NameEvents: factorycheck.Warn},
			fixes: map[string]string{factorycheck.NameEvents: "issues"},
		},
		{
			name:  "not installed fails with the install url",
			setup: func(r *rig) { r.gh.installed = false },
			want: want{factorycheck.NameInstalled: factorycheck.Fail, factorycheck.NameToken: factorycheck.Skip,
				factorycheck.NameRepoAccess: factorycheck.Skip, factorycheck.NameBranch: factorycheck.Skip},
			fixes: map[string]string{factorycheck.NameInstalled: "https://github.com/apps/my-app/installations/new"},
		},
		{
			name:  "push not permitted fails",
			setup: func(r *rig) { r.gh.push = false },
			want:  want{factorycheck.NameToken: factorycheck.OK, factorycheck.NameRepoAccess: factorycheck.Fail},
		},
		{
			name:  "unprotected default branch warns",
			setup: func(r *rig) { r.gh.protected = false },
			want:  want{factorycheck.NameBranch: factorycheck.Warn},
			fixes: map[string]string{factorycheck.NameBranch: "main"},
		},
		{
			name:  "missing default branch fails",
			setup: func(r *rig) { r.gh.branchMissing = true },
			want:  want{factorycheck.NameBranch: factorycheck.Fail},
		},
		{
			name:  "default branch with a slash is read",
			setup: func(r *rig) { r.gh.defaultBranch = "release/1" },
			want:  want{factorycheck.NameBranch: factorycheck.OK},
		},
		{
			name:  "factory off fails",
			setup: func(r *rig) { delete(r.env, "FACTORY") },
			want:  want{factorycheck.NameFactory: factorycheck.Fail},
			fixes: map[string]string{factorycheck.NameFactory: "FACTORY=1"},
		},
		{
			name:  "missing webhook secret fails",
			setup: func(r *rig) { delete(r.env, "FACTORY_WEBHOOK_SECRET") },
			want:  want{factorycheck.NameWebhook: factorycheck.Fail},
		},
		{
			name:  "short webhook secret warns",
			setup: func(r *rig) { r.env["FACTORY_WEBHOOK_SECRET"] = "short" },
			want:  want{factorycheck.NameWebhook: factorycheck.Warn},
		},
		{
			name:  "reviewer equal to the default model fails",
			setup: func(r *rig) { r.env["FACTORY_REVIEW_MODEL"] = "author-model" },
			want:  want{factorycheck.NameReviewer: factorycheck.Fail},
		},
		{
			name:  "reviewer outside LLM_MODELS fails",
			setup: func(r *rig) { r.env["LLM_MODELS"] = "author-model" },
			want:  want{factorycheck.NameReviewer: factorycheck.Fail},
			fixes: map[string]string{factorycheck.NameReviewer: "LLM_MODELS"},
		},
		{
			name:  "no reviewer and no opt out fails",
			setup: func(r *rig) { delete(r.env, "FACTORY_REVIEW_MODEL") },
			want:  want{factorycheck.NameReviewer: factorycheck.Fail},
		},
		{
			name: "no review opt out warns that merges rest on CI",
			setup: func(r *rig) {
				delete(r.env, "FACTORY_REVIEW_MODEL")
				r.env["FACTORY_NO_REVIEW"] = "1"
			},
			want: want{factorycheck.NameReviewer: factorycheck.Warn},
		},
		{
			name:  "local sandbox in team mode fails",
			setup: func(r *rig) { r.env["SANDBOX_BACKEND"] = "local" },
			want:  want{factorycheck.NameSandbox: factorycheck.Fail},
		},
		{
			name: "local sandbox with OIDC counts as team mode",
			setup: func(r *rig) {
				delete(r.env, "HARNESS_TEAM")
				r.env["OIDC_ISSUER"] = "https://idp"
				r.env["SANDBOX_BACKEND"] = "local"
			},
			want: want{factorycheck.NameSandbox: factorycheck.Fail},
		},
		{
			name: "local sandbox accepted explicitly warns",
			setup: func(r *rig) {
				r.env["SANDBOX_BACKEND"] = "local"
				r.env["HARNESS_ALLOW_LOCAL_SANDBOX"] = "1"
			},
			want: want{factorycheck.NameSandbox: factorycheck.Warn},
		},
		{
			name: "local sandbox outside team mode is fine",
			setup: func(r *rig) {
				delete(r.env, "HARNESS_TEAM")
				r.env["SANDBOX_BACKEND"] = "local"
			},
			want: want{factorycheck.NameSandbox: factorycheck.OK},
		},
		{
			name:  "no sandbox backend fails",
			setup: func(r *rig) { delete(r.env, "SANDBOX_BACKEND"); delete(r.env, "OPENSANDBOX_URL") },
			want:  want{factorycheck.NameSandbox: factorycheck.Fail},
		},
		{
			// The host pushes on open_pr and the agent holds no credential, so the proxy is not a requirement.
			name:  "credential proxy off is fine: the agent holds no push credential",
			setup: func(r *rig) { delete(r.env, "OPENSANDBOX_CREDENTIAL_PROXY") },
			want:  want{factorycheck.NameSandbox: factorycheck.OK},
		},
		{
			name:  "quota below concurrency fails",
			setup: func(r *rig) { r.env["MAX_SANDBOXES_PER_USER"] = "3" },
			want:  want{factorycheck.NameSandboxQuota: factorycheck.Fail},
		},
		{
			name:  "quota below twice the concurrency warns",
			setup: func(r *rig) { r.env["MAX_SANDBOXES_PER_USER"] = "7" },
			want:  want{factorycheck.NameSandboxQuota: factorycheck.Warn},
		},
		{
			name: "quota and concurrency defaults are 5 and 4",
			setup: func(r *rig) {
				delete(r.env, "MAX_SANDBOXES_PER_USER")
				delete(r.env, "FACTORY_MAX_CONCURRENT")
			},
			want: want{factorycheck.NameSandboxQuota: factorycheck.Warn},
		},
		{
			name:  "equal quota and concurrency passes the floor but warns",
			setup: func(r *rig) { r.env["MAX_SANDBOXES_PER_USER"] = "4" },
			want:  want{factorycheck.NameSandboxQuota: factorycheck.Warn},
		},
		{
			name:  "invalid concurrency fails",
			setup: func(r *rig) { r.env["FACTORY_MAX_CONCURRENT"] = "many" },
			want:  want{factorycheck.NameSandboxQuota: factorycheck.Fail},
		},
		{
			name: "no budgets warn",
			setup: func(r *rig) {
				delete(r.env, "FACTORY_TASK_BUDGET_USD")
				delete(r.env, "FACTORY_DAILY_BUDGET_USD")
			},
			want: want{factorycheck.NameBudgets: factorycheck.Warn},
		},
		{
			name: "a token budget alone is a cap",
			setup: func(r *rig) {
				delete(r.env, "FACTORY_TASK_BUDGET_USD")
				delete(r.env, "FACTORY_DAILY_BUDGET_USD")
				r.env["FACTORY_DAILY_BUDGET_TOKENS"] = "1000000"
			},
			want: want{factorycheck.NameBudgets: factorycheck.OK},
		},
		{
			name: "a zero budget is not a cap",
			setup: func(r *rig) {
				r.env["FACTORY_TASK_BUDGET_USD"] = "0"
				r.env["FACTORY_DAILY_BUDGET_USD"] = "abc"
			},
			want: want{factorycheck.NameBudgets: factorycheck.Warn},
		},
		{
			name:  "invalid harness setting fails the checks that depend on it",
			setup: func(r *rig) { r.env["MAX_SANDBOXES_PER_USER"] = "lots" },
			want: want{factorycheck.NameReviewer: factorycheck.Fail, factorycheck.NameSandbox: factorycheck.Fail,
				factorycheck.NameSandboxQuota: factorycheck.Fail},
		},
		{
			name:  "no repository skips the repository checks",
			setup: func(r *rig) { r.rp = "" },
			want:  want{factorycheck.NameApp: factorycheck.OK, factorycheck.NamePermissions: factorycheck.OK, factorycheck.NameInstalled: factorycheck.Skip, factorycheck.NameToken: factorycheck.Skip},
		},
		{
			name:  "malformed repository fails",
			setup: func(r *rig) { r.rp = "acme" },
			want:  want{factorycheck.NameInstalled: factorycheck.Fail, factorycheck.NameToken: factorycheck.Skip},
		},
		{
			name:  "repository with extra segments fails",
			setup: func(r *rig) { r.rp = "acme/api/x" },
			want:  want{factorycheck.NameInstalled: factorycheck.Fail},
		},
		{
			name:  "GitHub 500 on the app endpoint fails only that check and its dependents skip",
			setup: func(r *rig) { r.gh.status["/app"] = 500 },
			want: want{factorycheck.NamePermissions: factorycheck.Fail, factorycheck.NameEvents: factorycheck.Skip,
				factorycheck.NameInstalled: factorycheck.OK, factorycheck.NameToken: factorycheck.OK,
				factorycheck.NameRepoAccess: factorycheck.OK, factorycheck.NameBranch: factorycheck.OK},
		},
		{
			name:  "GitHub 500 on the installation lookup fails only that check",
			setup: func(r *rig) { r.gh.status["/repos/acme/api/installation"] = 500 },
			want: want{factorycheck.NamePermissions: factorycheck.OK, factorycheck.NameInstalled: factorycheck.Fail,
				factorycheck.NameToken: factorycheck.Skip, factorycheck.NameSandbox: factorycheck.OK},
		},
		{
			name:  "GitHub 500 on token minting fails the token check",
			setup: func(r *rig) { r.gh.status["/app/installations/77/access_tokens"] = 500 },
			want: want{factorycheck.NameInstalled: factorycheck.OK, factorycheck.NameToken: factorycheck.Fail,
				factorycheck.NameRepoAccess: factorycheck.Skip, factorycheck.NameBranch: factorycheck.Skip},
		},
		{
			name:  "GitHub 500 on the repository fails the access check and skips the branch",
			setup: func(r *rig) { r.gh.status["/repos/acme/api"] = 500 },
			want: want{factorycheck.NameToken: factorycheck.OK, factorycheck.NameRepoAccess: factorycheck.Fail,
				factorycheck.NameBranch: factorycheck.Skip, factorycheck.NameReviewer: factorycheck.OK},
		},
		{
			name:  "GitHub 500 on the branch fails only the branch check",
			setup: func(r *rig) { r.gh.status["/repos/acme/api/branches/main"] = 500 },
			want: want{factorycheck.NameRepoAccess: factorycheck.OK, factorycheck.NameBranch: factorycheck.Fail,
				factorycheck.NameBudgets: factorycheck.OK},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t)
			tc.setup(r)

			// Act
			rep := r.run()

			// Assert: the named checks have the status; every other check is untouched by the scenario.
			got := statuses(rep)
			for name, st := range tc.want {
				if got[name] != st {
					c := find(t, rep, name)
					t.Errorf("%s = %v, want %v (%s)", name, got[name], st, c.Detail)
				}
			}
			for name, sub := range tc.fixes {
				if c := find(t, rep, name); !strings.Contains(c.Fix, sub) {
					t.Errorf("%s fix = %q, want it to contain %q", name, c.Fix, sub)
				}
			}
			wantFail := false
			for _, st := range tc.want {
				wantFail = wantFail || st == factorycheck.Fail
			}
			if rep.Failed() != wantFail {
				t.Errorf("Failed() = %v, want %v (%v)", rep.Failed(), wantFail, got)
			}
		})
	}
}

func TestRunWithoutAGitHubAppFailsTheAppAndSkipsTheGitHubChecks(t *testing.T) {
	// Arrange
	envs := map[string]map[string]string{
		"no id":         {"GITHUB_APP_ID": ""},
		"no key":        {"GITHUB_APP_PRIVATE_KEY": ""},
		"key unparsed":  {},
		"key from file": {"GITHUB_APP_PRIVATE_KEY": "", "GITHUB_APP_PRIVATE_KEY_FILE": "/nope.pem"},
	}
	fixes := map[string]string{"no id": "GITHUB_APP_ID", "no key": "GITHUB_APP_PRIVATE_KEY", "key unparsed": "PEM", "key from file": "PEM"}
	for name, over := range envs {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.app = nil
			for k, v := range over {
				r.env[k] = v
			}

			// Act
			rep := r.run()

			// Assert
			got := statuses(rep)
			if got[factorycheck.NameApp] != factorycheck.Fail {
				t.Fatalf("app = %v", got[factorycheck.NameApp])
			}
			if f := find(t, rep, factorycheck.NameApp).Fix; !strings.Contains(f, fixes[name]) {
				t.Errorf("fix = %q, want %q in it", f, fixes[name])
			}
			for _, n := range []string{factorycheck.NamePermissions, factorycheck.NameEvents, factorycheck.NameInstalled,
				factorycheck.NameToken, factorycheck.NameRepoAccess, factorycheck.NameBranch} {
				if got[n] != factorycheck.Skip {
					t.Errorf("%s = %v, want Skip", n, got[n])
				}
			}
			if got[factorycheck.NameReviewer] != factorycheck.OK || got[factorycheck.NameWebhook] != factorycheck.OK {
				t.Errorf("the checks that need no GitHub did not run: %v", got)
			}
			if !rep.Failed() {
				t.Error("a missing App did not fail the report")
			}
			if len(r.gh.hits) != 0 {
				t.Errorf("GitHub was called without an App: %v", r.gh.hits)
			}
		})
	}
}

func TestRunRedactsSecretsInGitHubErrors(t *testing.T) {
	// Arrange: an error path that echoes the repository name, which here contains the webhook secret.
	r := newRig(t)
	r.rp = "acme/" + secret
	r.gh.status["/repos/acme/"+secret+"/installation"] = 500

	// Act
	rep := r.run()

	// Assert
	c := find(t, rep, factorycheck.NameInstalled)
	if c.Status != factorycheck.Fail || strings.Contains(c.Detail, secret) || !strings.Contains(c.Detail, "500") {
		t.Fatalf("installed = %v %q", c.Status, c.Detail)
	}
}

func TestRunUsesTheGivenHTTPClientForAnAppWithoutOne(t *testing.T) {
	// Arrange
	r := newRig(t)
	var used bool
	client := &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		used = true
		return http.DefaultTransport.RoundTrip(req)
	})}

	// Act
	rep := factorycheck.Run(context.Background(), factorycheck.Options{
		Getenv: func(k string) string { return r.env[k] }, App: r.app, Repo: repo, HTTP: client,
	})

	// Assert
	if !used || rep.Failed() {
		t.Fatalf("client used = %v, failed = %v", used, rep.Failed())
	}
	if rep.At.IsZero() {
		t.Fatal("the default clock gave no time")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRunReadsTheProcessEnvironmentByDefault(t *testing.T) {
	// Arrange
	t.Setenv("FACTORY", "1")
	t.Setenv("GITHUB_APP_ID", "")

	// Act
	rep := factorycheck.Run(context.Background(), factorycheck.Options{})

	// Assert
	got := statuses(rep)
	if got[factorycheck.NameFactory] != factorycheck.OK || got[factorycheck.NameApp] != factorycheck.Fail {
		t.Fatalf("statuses = %v", got)
	}
}

func TestReportWriteFormat(t *testing.T) {
	// Arrange
	rep := factorycheck.Report{Checks: []factorycheck.Check{
		{Name: "factory enabled", Status: factorycheck.OK, Detail: "FACTORY=1"},
		{Name: "webhook secret", Status: factorycheck.Warn, Detail: "the secret is 5 characters", Fix: "use a longer secret"},
		{Name: "app installed", Status: factorycheck.Fail, Detail: "not installed", Fix: "install it"},
		{Name: "branch", Status: factorycheck.Skip},
	}}
	var sb strings.Builder

	// Act
	rep.Write(&sb)

	// Assert
	want := "[ OK ] factory enabled: FACTORY=1\n" +
		"[WARN] webhook secret: the secret is 5 characters\n" +
		"       fix: use a longer secret\n" +
		"[FAIL] app installed: not installed\n" +
		"       fix: install it\n" +
		"[SKIP] branch\n"
	if sb.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", sb.String(), want)
	}
}

func TestFailedOnlyCountsFailures(t *testing.T) {
	cases := []struct {
		name   string
		checks []factorycheck.Check
		want   bool
	}{
		{"empty", nil, false},
		{"ok warn skip", []factorycheck.Check{{Status: factorycheck.OK}, {Status: factorycheck.Warn}, {Status: factorycheck.Skip}}, false},
		{"one fail among others", []factorycheck.Check{{Status: factorycheck.OK}, {Status: factorycheck.Fail}, {Status: factorycheck.Warn}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			rep := factorycheck.Report{Checks: tc.checks}

			// Act
			got := rep.Failed()

			// Assert
			if got != tc.want {
				t.Fatalf("Failed() = %v, want %v", got, tc.want)
			}
		})
	}
}
