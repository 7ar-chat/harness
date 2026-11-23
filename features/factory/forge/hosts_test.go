package forge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/githost"
	"github.com/7ar-chat/harness/features/factory/forge"
)

const hostToken = "glpat-secrettoken1234567890"

type fakeHosts struct {
	hosts map[string]githost.Host
	creds map[string]githost.Credential // login|host
}

func (f *fakeHosts) Host(_ context.Context, name string) (githost.Host, error) {
	if h, ok := f.hosts[name]; ok {
		return h, nil
	}
	return githost.Host{}, githost.ErrUnknownHost
}

func (f *fakeHosts) Credential(_ context.Context, login, host string) (githost.Credential, error) {
	if c, ok := f.creds[login+"|"+host]; ok {
		return c, nil
	}
	return githost.Credential{}, githost.ErrNoCredential
}

func hostFor(t *testing.T, srv *httptest.Server, kind githost.Kind) (githost.Host, *fakeHosts) {
	t.Helper()
	name := strings.TrimPrefix(srv.URL, "http://")
	h := githost.Host{Name: name, Kind: kind, BaseURL: srv.URL}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	return h, &fakeHosts{hosts: map[string]githost.Host{h.Name: h},
		creds: map[string]githost.Credential{"ann|" + h.Name: {Token: hostToken}}}
}

func TestGitLabMergeRequestLifecycle(t *testing.T) {
	var seen []string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath())
		auth = r.Header.Get("PRIVATE-TOKEN")
		switch r.Method + " " + r.URL.EscapedPath() {
		case "POST /api/v4/projects/grp%2Fsub%2Fproj/merge_requests":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["title"] != "Draft: Add x" || b["source_branch"] != "har/1" || b["target_branch"] != "main" {
				t.Errorf("body = %v", b)
			}
			writeJSON(w, 201, map[string]any{"iid": 7, "web_url": "https://x/mr/7", "title": b["title"], "state": "opened",
				"source_branch": "har/1", "target_branch": "main", "sha": "abc1234", "detailed_merge_status": "mergeable"})
		case "GET /api/v4/projects/grp%2Fsub%2Fproj/pipelines":
			writeJSON(w, 200, []map[string]any{{"id": 99}})
		case "GET /api/v4/projects/grp%2Fsub%2Fproj/pipelines/99/jobs":
			writeJSON(w, 200, []map[string]any{
				{"id": 5, "name": "test", "status": "failed", "stage": "test", "web_url": "https://x/-/jobs/5", "failure_reason": "script_failure"},
				{"id": 6, "name": "lint", "status": "success", "web_url": "https://x/-/jobs/6"},
				{"id": 7, "name": "deploy", "status": "manual", "web_url": "https://x/-/jobs/7"},
				{"id": 8, "name": "e2e", "status": "running", "web_url": "https://x/-/jobs/8"},
			})
		case "GET /api/v4/projects/grp%2Fsub%2Fproj/jobs/5/trace":
			_, _ = w.Write([]byte("\x1b[31mFAIL\x1b[0m TestX\nusing token " + hostToken + "\n"))
		case "PUT /api/v4/projects/grp%2Fsub%2Fproj/merge_requests/7/merge":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["sha"] != "abc1234" || b["squash"] != true {
				t.Errorf("merge body = %v", b)
			}
			writeJSON(w, 200, map[string]any{})
		case "GET /api/v4/projects/grp%2Fsub%2Fproj/merge_requests/9":
			writeJSON(w, 404, map[string]any{"message": "404 Not found, token " + hostToken})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h, res := hostFor(t, srv, githost.KindGitLab)
	f := forge.NewGitLab(h, res, nil)
	ctx := githost.WithLogin(context.Background(), "ann")
	repo := h.Name + "/grp/sub/proj"

	pr, err := f.OpenPR(ctx, factory.OpenPR{Repo: repo, Title: "Add x", Head: "har/1", Base: "main", Draft: true})
	if err != nil || pr.Number != 7 || !pr.Draft || pr.State != "open" || pr.Mergeable == nil || !*pr.Mergeable {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	if auth != hostToken {
		t.Fatal("the token of the user was not sent")
	}
	checks, err := f.Checks(ctx, repo, "abc1234")
	if err != nil || len(checks) != 3 {
		t.Fatalf("checks = %+v, %v (a manual job is not a check)", checks, err)
	}
	var failed factory.Check
	for _, c := range checks {
		if c.State == factory.CheckFailed {
			failed = c
		}
	}
	if failed.Name != "test" || failed.Detail != "script_failure" {
		t.Fatalf("failed = %+v", failed)
	}
	log, err := f.FailureLog(ctx, repo, failed)
	if err != nil || !strings.Contains(log, "FAIL TestX") || strings.Contains(log, hostToken) || strings.Contains(log, "\x1b") {
		t.Fatalf("log = %q, %v", log, err)
	}
	if err := f.Merge(ctx, repo, 7, "abc1234", factory.MergeSquash); err != nil {
		t.Fatal(err)
	}
	_, err = f.GetPR(ctx, repo, 9)
	if err == nil || strings.Contains(err.Error(), hostToken) || !strings.Contains(err.Error(), "gitlab:") {
		t.Fatalf("error leaks the token or lacks the provider: %v", err)
	}
	if _, err := f.GetPR(ctx, h.Name+"/other", 1); err != nil {
		// a path on the same host is fine; it just 404s
		if !strings.Contains(err.Error(), "404") {
			t.Fatal(err)
		}
	}
	if _, err := f.GetPR(ctx, "other.example.com/a/b", 1); err == nil {
		t.Fatal("a repository of another host must be refused")
	}
}

func TestHostForgeNeedsTheUsersToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("no request without a token") }))
	defer srv.Close()
	h, res := hostFor(t, srv, githost.KindGitea)
	f := forge.NewGitea(h, res, nil)
	_, err := f.GetPR(githost.WithLogin(context.Background(), "bob"), h.Name+"/o/r", 1)
	if err == nil || !strings.Contains(err.Error(), "Git access") {
		t.Fatalf("err = %v", err)
	}
}

func TestGiteaPullRequestLifecycle(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/repos/o/r/pulls":
			writeJSON(w, 409, map[string]any{"message": "pull request already exists for these targets"})
		case "GET /api/v1/repos/o/r/pulls":
			writeJSON(w, 200, []map[string]any{{"number": 3, "html_url": "https://g/o/r/pulls/3", "title": "T", "state": "open",
				"head": map[string]any{"ref": "har/2", "sha": "def5678"}, "base": map[string]any{"ref": "main"}, "mergeable": true}})
		case "GET /api/v1/repos/o/r/commits/def5678/statuses":
			writeJSON(w, 200, []map[string]any{
				{"context": "ci/build", "status": "failure", "target_url": "https://g/o/r/actions/runs/1", "description": "build broke"},
				{"context": "ci/build", "status": "success"},
				{"context": "ci/lint", "status": "pending"},
			})
		case "GET /api/v1/repos/o/r/issues/3/comments":
			writeJSON(w, 200, []map[string]any{{"body": "please fix", "user": map[string]any{"login": "rev"}}})
		case "GET /api/v1/repos/o/r/pulls/3/reviews":
			writeJSON(w, 200, []map[string]any{{"id": 1, "state": "REQUEST_CHANGES", "body": "no", "comments_count": 1, "user": map[string]any{"login": "rev"}}})
		case "GET /api/v1/repos/o/r/pulls/3/reviews/1/comments":
			writeJSON(w, 200, []map[string]any{{"body": "nit", "path": "a.go", "line": 4, "user": map[string]any{"login": "rev"}}})
		case "POST /api/v1/repos/o/r/pulls/3/merge":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b["Do"] != "squash" || b["head_commit_id"] != "def5678" {
				t.Errorf("merge body = %v", b)
			}
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h, res := hostFor(t, srv, githost.KindGitea)
	f := forge.NewGitea(h, res, nil)
	ctx := githost.WithLogin(context.Background(), "ann")
	repo := h.Name + "/o/r"

	_, err := f.OpenPR(ctx, factory.OpenPR{Repo: repo, Title: "T", Head: "har/2", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "422") {
		t.Fatalf("a duplicate must read as unprocessable, got %v", err)
	}
	pr, err := f.FindOpenPR(ctx, repo, "har/2", "main")
	if err != nil || pr.Number != 3 || pr.HeadSHA != "def5678" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	if auth != "token "+hostToken {
		t.Fatalf("auth = %q", auth)
	}
	checks, err := f.Checks(ctx, repo, "def5678")
	if err != nil || len(checks) != 2 || checks[0].State != factory.CheckFailed || checks[1].State != factory.CheckPending {
		t.Fatalf("checks = %+v, %v (the newest status of a context wins)", checks, err)
	}
	cs, err := f.Comments(ctx, repo, 3)
	if err != nil || len(cs) != 3 || cs[1].Review != "changes_requested" || cs[2].Path != "a.go" || cs[2].Line != 4 {
		t.Fatalf("comments = %+v, %v", cs, err)
	}
	if err := f.Merge(ctx, repo, 3, "def5678", factory.MergeSquash); err != nil {
		t.Fatal(err)
	}
}

func TestMultiRoutesByRepositoryAndRefusesUnregisteredHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"default_branch": "trunk"})
	}))
	defer srv.Close()
	h, res := hostFor(t, srv, githost.KindGitea)
	gh := newFakeGitHub(t)
	m := &forge.Multi{GitHub: gh.client(), Hosts: res}
	ctx := githost.WithLogin(context.Background(), "ann")

	if b, err := m.DefaultBranch(ctx, h.Name+"/o/r"); err != nil || b != "trunk" {
		t.Fatalf("host repo: %q, %v", b, err)
	}
	if _, err := m.DefaultBranch(ctx, "evil.example.com/o/r"); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("an unregistered host must be refused, got %v", err)
	}
	if _, err := (&forge.Multi{Hosts: res}).DefaultBranch(ctx, "o/r"); err == nil {
		t.Fatal("GitHub repository without GitHub configured")
	}
}

func TestGenericHostHasNoPullRequestAPI(t *testing.T) {
	h := githost.Host{Name: "git.example.com", Kind: githost.KindGeneric}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	m := &forge.Multi{Hosts: &fakeHosts{hosts: map[string]githost.Host{h.Name: h}}}
	_, err := m.OpenPR(context.Background(), factory.OpenPR{Repo: "git.example.com/a/b", Title: "t", Head: "x", Base: "main"})
	if err == nil || !strings.Contains(err.Error(), "no pull request API") {
		t.Fatalf("err = %v", err)
	}
}

func TestIssueSources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v4/projects/g%2Fp/issues", "/api/v4/projects/g/p/issues":
			if r.URL.Query().Get("labels") != "bug" {
				t.Errorf("labels = %q", r.URL.Query().Get("labels"))
			}
			writeJSON(w, 200, []map[string]any{{"iid": 4, "title": "Broken", "description": "d", "web_url": "https://x/4", "labels": []string{"bug"}, "author": map[string]any{"username": "ann"}}})
		case "/api/v1/repos/o/r/issues":
			writeJSON(w, 200, []map[string]any{{"number": 2, "title": "Idea", "body": "b", "html_url": "https://x/2", "user": map[string]any{"login": "bob"}, "labels": []map[string]any{{"name": "bug"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	for _, tc := range []struct {
		kind githost.Kind
		repo string
		want string
		src  string
	}{{githost.KindGitLab, "g/p", "Broken", "gitlab_issue"}, {githost.KindGitea, "o/r", "Idea", "gitea_issue"}} {
		h, res := hostFor(t, srv, tc.kind)
		is, ok := forge.IssueSourceFor(h, res, nil)
		if !ok || is.Source() != tc.src {
			t.Fatalf("%s: %v %v", tc.kind, ok, is)
		}
		got, err := is.OpenIssues(githost.WithLogin(context.Background(), "ann"), h.Name+"/"+tc.repo, "bug", 10)
		if err != nil || len(got) != 1 || got[0].Title != tc.want {
			t.Fatalf("%s: %+v, %v", tc.kind, got, err)
		}
	}
	if _, ok := forge.IssueSourceFor(githost.Host{Kind: githost.KindGeneric}, nil, nil); ok {
		t.Fatal("a plain git host has no issues")
	}
}

func TestCheckoutOfAHostRepositoryUsesTheUsersCredentialAndNeverTheCommandText(t *testing.T) {
	h := githost.Host{Name: "gitlab.example.com", Kind: githost.KindGitLab, KnownHosts: []string{"gitlab.example.com ssh-ed25519 AAAAC3"}}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----"
	hosts := &fakeHosts{hosts: map[string]githost.Host{h.Name: h}, creds: map[string]githost.Credential{
		"ann|" + h.Name: {Token: hostToken, Username: "oauth2"}}}
	c, sb := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Login: "ann", Repo: "gitlab.example.com/grp/proj", BaseBranch: "main", Branch: "har/abc"})
	c.Hosts = hosts

	notice := c.Setup(context.Background(), "th1", sb)
	if !strings.Contains(notice, "is checked out") {
		t.Fatal(notice)
	}
	cmd := sb.cmds[0]
	if cmd.Env["REPO_URL"] != "https://gitlab.example.com/grp/proj.git" || cmd.Env["GH_TOKEN"] != hostToken || cmd.Env["GIT_USER"] != "oauth2" {
		t.Fatalf("env = %v", cmd.Env)
	}
	if strings.Contains(cmd.Command, hostToken) || !strings.Contains(cmd.Command, "$GIT_USER") {
		t.Fatalf("the token must be in the environment only:\n%s", cmd.Command)
	}

	// With an SSH key the checkout goes over ssh and checks the registered host key.
	hosts.creds["ann|"+h.Name] = githost.Credential{SSHKey: []byte(key)}
	c2, sb2 := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Login: "ann", Repo: "gitlab.example.com/grp/proj", BaseBranch: "main", Branch: "har/abc"})
	c2.Hosts = hosts
	c2.Setup(context.Background(), "th1", sb2)
	cmd = sb2.cmds[0]
	if cmd.Env["REPO_URL"] != "ssh://git@gitlab.example.com:22/grp/proj.git" || cmd.Env["GIT_SSH_KEY"] != key || !strings.Contains(cmd.Env["GIT_KNOWN_HOSTS"], "ssh-ed25519") {
		t.Fatalf("env = %v", cmd.Env)
	}
	if strings.Contains(cmd.Command, "AAAA") || !strings.Contains(cmd.Command, "StrictHostKeyChecking=yes") {
		t.Fatalf("script:\n%s", cmd.Command)
	}

	// An unregistered host is refused before anything runs.
	c3, sb3 := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Login: "ann", Repo: "evil.example.com/a/b", BaseBranch: "main", Branch: "har/abc"})
	c3.Hosts = hosts
	if n := c3.Setup(context.Background(), "th1", sb3); !strings.Contains(n, "not a registered git host") || len(sb3.cmds) != 0 {
		t.Fatalf("notice = %q, cmds = %d", n, len(sb3.cmds))
	}
	// SSH without a registered host key is refused.
	h2 := githost.Host{Name: "nokeys.example.com", Kind: githost.KindGitea}
	_ = h2.Validate()
	hosts.hosts[h2.Name] = h2
	hosts.creds["ann|"+h2.Name] = githost.Credential{SSHKey: []byte(key)}
	c4, sb4 := checkoutEnv(t, factory.ThreadInfo{ThreadID: "th1", TaskID: "t", Login: "ann", Repo: "nokeys.example.com/a/b", BaseBranch: "main", Branch: "har/abc"})
	c4.Hosts = hosts
	if n := c4.Setup(context.Background(), "th1", sb4); !strings.Contains(n, "host key") || len(sb4.cmds) != 0 {
		t.Fatalf("notice = %q", n)
	}
}
