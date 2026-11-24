package product

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
)

// issueComments is a GitHub double that records the comments posted on issues.
type issueComments struct {
	mu   sync.Mutex
	list []string
}

func (c *issueComments) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.list...)
}

func rqGitHubComments(t *testing.T) (*GitHubApp, *issueComments) {
	t.Helper()
	got := &issueComments{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/42/access_tokens" && r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 1, "title": "Crash on start", "html_url": "https://github.com/acme/api/issues/1", "user": map[string]any{"login": "zed"}, "labels": []map[string]any{}}})
		case r.URL.Path == "/repos/acme/api/issues/1/comments" && r.Method == "POST":
			b, _ := io.ReadAll(r.Body)
			var body struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(b, &body)
			got.mu.Lock()
			got.list = append(got.list, body.Body)
			got.mu.Unlock()
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	app, _ := testApp(t, api.URL)
	return app, got
}

func waitComments(t *testing.T, c *issueComments, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if l := c.all(); len(l) >= n {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("comments = %v, want %d", c.all(), n)
	return nil
}

func TestPullRequestStatusFollowsTheTaskAndCommentsOnTheSourceIssueOnlyWhenTheOwnerAsked(t *testing.T) {
	app, comments := rqGitHubComments(t)
	e := newRqEnv(t, rqOpts{factory: true, github: app})
	inst, repo := int64(42), "https://github.com/acme/api"
	if _, err := e.tm.UpdateProject(e.ctx, e.project, store.ProjectPatch{GitHubInstallation: &inst, RepoURL: &repo}); err != nil {
		t.Fatal(err)
	}
	imp := e.json("POST", e.path("/import/github"), "bob", `{}`, 200)
	id := imp["requirements"].([]any)[0].(map[string]any)["id"].(string)

	// Off by default: the requirement follows the pull request, nothing is posted on GitHub.
	d := e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{"budget":{"maxUsd":3}}`, 202)
	task := d["taskIds"].([]any)[0].(string)
	if err := e.f.Tasks.Block(e.ctx, task, "needs a key"); err != nil {
		t.Fatal(err)
	}
	if err := e.f.Tasks.Review(e.ctx, task, factory.PullRequest{Number: 7, URL: "https://github.com/acme/api/pull/7"}); err != nil {
		t.Fatal(err)
	}
	if r := e.get(id); r.Status != requirements.StatusInReview || r.PR.State != "open" || r.PR.Number != 7 {
		t.Fatalf("review: %+v", r)
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(comments.all()); n != 0 {
		t.Fatalf("a comment was posted with the setting off: %v", comments.all())
	}

	// Only an owner may turn it on; members read it.
	e.json("PUT", e.path("/settings"), "bob", `{"commentOnIssue":true}`, 403)
	st := e.json("PUT", e.path("/settings"), "alice", `{"commentOnIssue":true}`, 200)
	if st["commentOnIssue"] != true {
		t.Fatalf("settings: %v", st)
	}
	if got := e.json("GET", e.path("/settings"), "carol", "", 200); got["commentOnIssue"] != true {
		t.Fatalf("settings read: %v", got)
	}

	// Merged: "Fixed by #7", once.
	if err := e.f.Tasks.Complete(e.ctx, task, "merged"); err != nil {
		t.Fatal(err)
	}
	if got := waitComments(t, comments, 1); got[0] != "Fixed by #7" {
		t.Fatalf("comments = %v", got)
	}
	if r := e.get(id); r.Status != requirements.StatusDone || r.PR.State != "merged" {
		t.Fatalf("done: %+v", r)
	}
}

func TestAPullRequestClosedWithoutAMergeIsShownAsClosed(t *testing.T) {
	task := factory.Task{PR: factory.PullRequest{Number: 9, URL: "u"}}
	p, ok := taskMove(requirements.Requirement{Status: requirements.StatusInReview}, task, factory.StateCancelled)
	if !ok || p.PR == nil || p.PR.State != "closed" || p.PR.Number != 9 {
		t.Fatalf("patch = %+v ok=%v", p, ok)
	}
	if p, _ := taskMove(requirements.Requirement{Status: requirements.StatusReady}, factory.Task{}, factory.StateCancelled); p.PR != nil {
		t.Fatal("no pull request, nothing to mark closed")
	}
}
