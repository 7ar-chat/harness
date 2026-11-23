package product

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/reqstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/store"
)

// rqEnv is a team server with requirements on: alice owns the project "api", bob is a member, carol a viewer.
type rqEnv struct {
	*rig
	tm      *cnFaultTeam
	reqs    *reqstore.Mem
	f       *harness.Factory
	hooks   *FactoryHooks
	project string
}

type rqOpts struct {
	factory bool
	github  *GitHubApp
	model   *llmtest.Model
}

func newRqEnv(t *testing.T, o rqOpts) *rqEnv {
	t.Helper()
	e := &rqEnv{tm: newCnFaultTeam(), reqs: reqstore.NewMem(nil)}
	cnUsers(t, e.tm, "alice", "bob", "carol", "dave")
	prod := Options{Team: e.tm, Requirements: e.reqs, GitHub: o.github}
	mod := func(ho *harness.Options) { ho.Features = append(harness.ProfileMinimal(), RequirementsFeature(e.reqs)) }
	if o.factory {
		e.hooks = &FactoryHooks{Team: e.tm}
		esc := &FactoryEscalator{}
		fo := harness.FactoryOptions{
			Store: factorystore.NewMem(nil), PollEvery: 10 * time.Millisecond, Escalator: esc,
			OnSpawn: e.hooks.OnSpawn, OnTaskState: e.hooks.OnTaskState, Backoff: func(int) time.Duration { return 0 },
		}
		prod.Factory = FactoryOptions{Escalator: esc, Hooks: e.hooks}
		mod = func(ho *harness.Options) {
			ho.Features = append(harness.ProfileMinimal(), RequirementsFeature(e.reqs), harness.FactoryFeature(fo))
		}
	}
	e.rig = newRig(t, rigOpts{team: true, model: o.model, prod: prod, env: map[string]string{"LLM_DEFAULT_MODEL": "gpt-test"}, mod: mod})
	if o.factory {
		if e.f = e.h.FactoryOf(); e.f == nil {
			t.Fatal("no factory")
		}
	}
	e.project = e.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	e.json("PUT", "/api/projects/"+e.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	e.json("PUT", "/api/projects/"+e.project+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	return e
}

func (e *rqEnv) path(s string) string { return "/api/projects/" + e.project + "/requirements" + s }

// create makes a requirement as bob and returns its id.
func (e *rqEnv) create(title string) string {
	e.t.Helper()
	return e.json("POST", e.path(""), "bob", fmt.Sprintf(`{"title":%q,"body":"details"}`, title), 201)["id"].(string)
}

func (e *rqEnv) list(path, user string, want int) []map[string]any {
	e.t.Helper()
	resp := e.do("GET", path, user, "")
	body := rawBody(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("GET %s as %s: status %d, want %d: %s", path, user, resp.StatusCode, want, body)
	}
	var out []map[string]any
	if want == 200 {
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			e.t.Fatalf("GET %s: %v: %s", path, err, body)
		}
	}
	return out
}

func (e *rqEnv) get(id string) requirements.Requirement {
	e.t.Helper()
	r, err := e.reqs.Get(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return *r
}

func TestRequirementsCRUDListFiltersAndRoles(t *testing.T) {
	e := newRqEnv(t, rqOpts{})
	e.json("POST", e.path(""), "carol", `{"title":"viewer cannot"}`, 403)
	e.json("POST", e.path(""), "dave", `{"title":"outsider cannot"}`, 404)
	e.json("POST", e.path(""), "bob", `{"title":"  "}`, 400)
	e.json("POST", e.path(""), "bob", `{"title":"x","status":"bogus"}`, 400)

	a := e.json("POST", e.path(""), "bob", `{"title":"Dark mode","body":"All pages","priority":2,"labels":["ui","ui"," "]}`, 201)
	if a["status"] != "backlog" || a["author"] != "bob" || len(a["labels"].([]any)) != 1 {
		t.Fatalf("created: %v", a)
	}
	id := a["id"].(string)
	e.create("Billing")

	if got := e.list(e.path("?label=ui"), "carol", 200); len(got) != 1 || got[0]["title"] != "Dark mode" {
		t.Fatalf("label filter: %v", got)
	}
	if got := e.list(e.path("?q=billing"), "carol", 200); len(got) != 1 {
		t.Fatalf("search: %v", got)
	}
	if got := e.list(e.path(""), "alice", 200); len(got) != 2 || got[0]["title"] != "Dark mode" { // priority first
		t.Fatalf("list: %v", got)
	}
	e.list(e.path(""), "dave", 404)

	up := e.json("PATCH", e.path("/"+id), "bob", `{"title":"Dark theme","status":"rejected"}`, 200)
	if up["title"] != "Dark theme" || up["status"] != "rejected" {
		t.Fatalf("patch: %v", up)
	}
	if got := e.list(e.path("?status=backlog,ready"), "bob", 200); len(got) != 1 {
		t.Fatalf("status filter: %v", got)
	}
	e.json("PATCH", e.path("/"+id), "bob", `{"status":"nope"}`, 400)
	e.json("PATCH", e.path("/"+id), "carol", `{"title":"x"}`, 403)
	e.json("GET", e.path("/missing"), "bob", "", 404)

	// another project's requirement is not reachable through this one
	other := e.json("POST", "/api/projects", "alice", `{"name":"other"}`, 201)["id"].(string)
	e.json("GET", "/api/projects/"+other+"/requirements/"+id, "alice", "", 404)

	e.do("DELETE", e.path("/"+id), "bob", "")
	if e.get(id).ID == "" {
		t.Fatal("a member deleted a requirement")
	}
	if r := e.do("DELETE", e.path("/"+id), "alice", ""); r.StatusCode != 204 {
		t.Fatalf("owner delete: %d", r.StatusCode)
	}
	e.json("GET", e.path("/"+id), "alice", "", 404)
}

func TestRequirementsAreAbsentWithoutTheStore(t *testing.T) {
	e := newFxEnv(t, fxOpts{})
	if r := e.do("GET", "/api/projects/"+e.project+"/requirements", "alice", ""); r.StatusCode != 404 {
		t.Fatalf("status %d", r.StatusCode)
	}
}

func TestRequirementDispatchCreatesAFactoryTaskAndCompletionFollowsTheMerge(t *testing.T) {
	e := newRqEnv(t, rqOpts{factory: true})
	id := e.create("Add export")

	d := e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{"policy":"sandboxed","budget":{"maxUsd":3}}`, 202)
	if d["status"] != "in_progress" || len(d["taskIds"].([]any)) != 1 {
		t.Fatalf("dispatched: %v", d)
	}
	taskID := d["taskIds"].([]any)[0].(string)
	task, err := e.f.Tasks.Get(e.ctx, taskID)
	if err != nil || task.ProjectID != e.project || task.Owner != "bob" || task.Budget.MaxUSD != 3 || task.Policy != "sandboxed" ||
		!strings.Contains(task.Spec, id) || !strings.Contains(task.Spec, "details") {
		t.Fatalf("task: %+v %v", task, err)
	}
	e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{}`, 409) // already being worked on
	e.json("POST", e.path("/"+id+"/dispatch"), "carol", `{}`, 403)
	e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{"budget":{"maxUsd":-1}}`, 400)

	// the detail shows the task
	det := e.json("GET", e.path("/"+id), "carol", "", 200)
	tasks := det["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != taskID {
		t.Fatalf("detail: %v", det)
	}

	// blocked: still in progress, flagged
	if err := e.f.Tasks.Block(e.ctx, taskID, "needs a key"); err != nil {
		t.Fatal(err)
	}
	if r := e.get(id); r.Status != requirements.StatusInProgress || !strings.Contains(r.Flag, "needs a key") {
		t.Fatalf("blocked: %+v", r)
	}
	// the PR opens: in review, with the PR, flag cleared
	if err := e.f.Tasks.Review(e.ctx, taskID, factory.PullRequest{Number: 7, URL: "https://github.com/x/api/pull/7"}); err != nil {
		t.Fatal(err)
	}
	if r := e.get(id); r.Status != requirements.StatusInReview || r.Flag != "" || r.PR.Number != 7 || r.PR.State != "open" {
		t.Fatalf("review: %+v", r)
	}
	// the merge: the task completes, the requirement is done
	if err := e.f.Tasks.Complete(e.ctx, taskID, "merged"); err != nil {
		t.Fatal(err)
	}
	if r := e.get(id); r.Status != requirements.StatusDone || r.PR.State != "merged" || r.PR.URL == "" {
		t.Fatalf("done: %+v", r)
	}
	e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{}`, 409) // closed
}

func TestRequirementTaskFailureKeepsItInProgressWithAFlag(t *testing.T) {
	e := newRqEnv(t, rqOpts{factory: true})
	id := e.create("Fragile")
	d := e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{}`, 202)
	taskID := d["taskIds"].([]any)[0].(string)
	if err := e.f.Tasks.Fail(e.ctx, taskID, "tests never passed"); err != nil {
		t.Fatal(err)
	}
	r := e.get(id)
	if r.Status != requirements.StatusInProgress || !strings.Contains(r.Flag, "tests never passed") {
		t.Fatalf("failed: %+v", r)
	}
	// a flagged requirement may be dispatched again, which clears the flag
	d = e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{}`, 202)
	if d["flag"] != nil && d["flag"] != "" || len(d["taskIds"].([]any)) != 2 {
		t.Fatalf("redispatched: %v", d)
	}
}

func TestRequirementTaskHookStartsReviewsAndIgnoresClosedOnes(t *testing.T) {
	e := newRqEnv(t, rqOpts{factory: true})
	id := e.create("Linked by hand")
	// a task that is not dispatched from the requirement but linked to it
	if _, err := e.reqs.Update(e.ctx, id, requirements.Patch{AddTaskID: "t-1"}); err != nil {
		t.Fatal(err)
	}
	task := factory.Task{ID: "t-1", ProjectID: e.project, Owner: "bob", Title: "x", ThreadID: "th-1", PR: factory.PullRequest{Number: 3, URL: "u"}}
	e.hooks.OnTaskState(task, factory.StateReady, factory.StateRunning)
	if r := e.get(id); r.Status != requirements.StatusInProgress || len(r.ThreadIDs) != 1 {
		t.Fatalf("running: %+v", r)
	}
	e.hooks.OnTaskState(task, factory.StateRunning, factory.StateReview)
	if r := e.get(id); r.Status != requirements.StatusInReview || r.PR.Number != 3 {
		t.Fatalf("review: %+v", r)
	}
	e.hooks.OnTaskState(task, factory.StateReview, factory.StateCancelled)
	if r := e.get(id); r.Status != requirements.StatusInReview || r.Flag != "task cancelled" {
		t.Fatalf("cancelled: %+v", r)
	}
	// a task of another project never touches it
	other := task
	other.ProjectID = "elsewhere"
	e.hooks.OnTaskState(other, factory.StateReview, factory.StateDone)
	if r := e.get(id); r.Status == requirements.StatusDone {
		t.Fatal("another project's task completed the requirement")
	}
	// a rejected requirement stays rejected
	rej := requirements.StatusRejected
	if _, err := e.reqs.Update(e.ctx, id, requirements.Patch{Status: &rej}); err != nil {
		t.Fatal(err)
	}
	e.hooks.OnTaskState(task, factory.StateReview, factory.StateDone)
	if r := e.get(id); r.Status != requirements.StatusRejected {
		t.Fatalf("rejected: %+v", r)
	}
}

func TestRequirementTaskMovesBySessionThreadToo(t *testing.T) {
	e := newRqEnv(t, rqOpts{factory: true})
	id := e.create("From a chat")
	e.reqs.Update(e.ctx, id, requirements.Patch{AddThreadID: "chat-1"})
	// the chat's agent opens a PR: the session becomes a task on the same thread
	task := factory.Task{ID: "t-9", ProjectID: e.project, Owner: "bob", ThreadID: "chat-1", PR: factory.PullRequest{Number: 5}}
	e.hooks.OnTaskState(task, "", factory.StateRunning)
	e.hooks.OnTaskState(task, factory.StateRunning, factory.StateDone)
	r := e.get(id)
	if r.Status != requirements.StatusDone || len(r.TaskIDs) != 1 || r.TaskIDs[0] != "t-9" {
		t.Fatalf("%+v", r)
	}
}

func TestRequirementDispatchWithoutTheFactoryStartsAProjectSession(t *testing.T) {
	e := newRqEnv(t, rqOpts{})
	id := e.create("Plain session")
	d := e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{}`, 202)
	threads := d["threadIds"].([]any)
	if d["status"] != "in_progress" || len(threads) != 1 || d["taskIds"] != nil {
		t.Fatalf("dispatched: %v", d)
	}
	th := e.json("GET", "/api/threads/"+threads[0].(string), "carol", "", 200)
	if th["projectId"] != e.project {
		t.Fatalf("thread: %v", th)
	}
	det := e.json("GET", e.path("/"+id), "carol", "", 200)
	if ts := det["threads"].([]any); len(ts) != 1 || ts[0].(map[string]any)["id"] != threads[0] {
		t.Fatalf("detail: %v", det)
	}
}

func TestRequirementShowsOnTheBoardCardOfItsSession(t *testing.T) {
	e := newRqEnv(t, rqOpts{})
	id := e.create("Board link")
	d := e.json("POST", e.path("/"+id+"/dispatch"), "bob", `{}`, 202)
	thread := d["threadIds"].([]any)[0].(string)
	board := e.json("GET", "/api/projects/"+e.project+"/board", "carol", "", 200)
	found := false
	for _, col := range board["columns"].([]any) {
		for _, c := range col.(map[string]any)["cards"].([]any) {
			card := c.(map[string]any)
			if card["threadId"] == thread {
				found = true
				if card["requirementId"] != id || card["requirementTitle"] != "Board link" {
					t.Fatalf("card: %v", card)
				}
			}
		}
	}
	if !found {
		t.Fatalf("the session is not on the board: %v", board)
	}
}

func TestRequirementAutoDispatchNeedsABudgetAndTheFactoryAndOwnerAndIsOffByDefault(t *testing.T) {
	e := newRqEnv(t, rqOpts{factory: true})
	if s := e.json("GET", e.path("/settings"), "carol", "", 200); s["autoDispatch"] != false {
		t.Fatalf("default: %v", s)
	}
	e.json("PUT", e.path("/settings"), "bob", `{"autoDispatch":true,"budget":{"maxUsd":1}}`, 403)
	e.json("PUT", e.path("/settings"), "alice", `{"autoDispatch":true}`, 400)
	e.json("PUT", e.path("/settings"), "alice", `{"autoDispatch":true,"budget":{"maxUsd":0}}`, 400)
	e.json("PUT", e.path("/settings"), "alice", `{"autoDispatch":true,"policy":"yolo","budget":{"maxUsd":1}}`, 400)

	// off: moving to ready dispatches nothing
	id := e.create("Manual")
	if r := e.json("PATCH", e.path("/"+id), "bob", `{"status":"ready"}`, 200); r["status"] != "ready" {
		t.Fatalf("off: %v", r)
	}
	if ts, _ := e.f.Tasks.List(e.ctx, factory.Filter{ProjectID: e.project}); len(ts) != 0 {
		t.Fatalf("a task was created with auto-dispatch off: %v", ts)
	}

	s := e.json("PUT", e.path("/settings"), "alice", `{"autoDispatch":true,"policy":"sandboxed","budget":{"maxUsd":2.5,"maxTokens":1000}}`, 200)
	if s["autoDispatch"] != true || s["projectId"] != e.project {
		t.Fatalf("saved: %v", s)
	}
	id2 := e.create("Automatic")
	r := e.json("PATCH", e.path("/"+id2), "bob", `{"status":"ready"}`, 200)
	if r["status"] != "in_progress" || len(r["taskIds"].([]any)) != 1 {
		t.Fatalf("auto: %v", r)
	}
	task, err := e.f.Tasks.Get(e.ctx, r["taskIds"].([]any)[0].(string))
	if err != nil || task.Budget.MaxUSD != 2.5 || task.Budget.MaxTokens != 1000 || task.Policy != "sandboxed" || task.AutoMerge {
		t.Fatalf("task: %+v %v", task, err)
	}
	// created ready is dispatched too; patching ready to ready is not a move
	c := e.json("POST", e.path(""), "bob", `{"title":"Born ready","status":"ready"}`, 201)
	if c["status"] != "in_progress" {
		t.Fatalf("born ready: %v", c)
	}
	n := len(e.json("GET", e.path("/"+id2), "bob", "", 200)["tasks"].([]any))
	e.json("PATCH", e.path("/"+id2), "bob", `{"title":"Automatic 2"}`, 200)
	if m := len(e.json("GET", e.path("/"+id2), "bob", "", 200)["tasks"].([]any)); m != n {
		t.Fatalf("a patch dispatched again: %d -> %d", n, m)
	}
}

func TestRequirementAutoDispatchIsRefusedWithoutTheFactory(t *testing.T) {
	e := newRqEnv(t, rqOpts{})
	e.json("PUT", e.path("/settings"), "alice", `{"autoDispatch":true,"budget":{"maxUsd":1}}`, 400)
	e.json("PUT", e.path("/settings"), "alice", `{"autoDispatch":false,"budget":{"maxUsd":1}}`, 200)
}

func TestRequirementAutoDispatchWithAMissingBudgetFlagsInsteadOfRunning(t *testing.T) {
	e := newRqEnv(t, rqOpts{factory: true})
	// settings written behind the route's back (an older record) must still never dispatch without a budget
	if err := e.reqs.PutSettings(e.ctx, requirements.Settings{ProjectID: e.project, AutoDispatch: true}); err != nil {
		t.Fatal(err)
	}
	id := e.create("Unbudgeted")
	r := e.json("PATCH", e.path("/"+id), "bob", `{"status":"ready"}`, 200)
	if r["status"] != "ready" || !strings.Contains(fmt.Sprint(r["flag"]), "no budget") {
		t.Fatalf("%v", r)
	}
	if ts, _ := e.f.Tasks.List(e.ctx, factory.Filter{ProjectID: e.project}); len(ts) != 0 {
		t.Fatal("dispatched without a budget")
	}
}

func TestRequirementLinksAConversationAndShowsItsIdToTheAgent(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		mu.Lock()
		b, _ := json.Marshal(c.Messages)
		seen = append(seen, string(b))
		mu.Unlock()
		return llmtest.Turn{Chunks: llmtest.Text("ok")}
	})
	e := newRqEnv(t, rqOpts{model: model})
	id := e.create("Wire it up")

	th := e.json("POST", "/api/threads", "bob", fmt.Sprintf(`{"projectId":%q}`, e.project), 201)["id"].(string)
	personal := e.newThread("bob")
	e.json("POST", "/api/threads/"+personal+"/requirement", "bob", fmt.Sprintf(`{"requirementId":%q}`, id), 400) // not a project session
	e.json("POST", "/api/threads/"+th+"/requirement", "bob", `{"requirementId":"nope"}`, 404)
	e.json("POST", "/api/threads/"+th+"/requirement", "carol", fmt.Sprintf(`{"requirementId":%q}`, id), 403)
	linked := e.json("POST", "/api/threads/"+th+"/requirement", "bob", fmt.Sprintf(`{"requirementId":%q}`, id), 200)
	if linked["status"] != "in_progress" || len(linked["threadIds"].([]any)) != 1 {
		t.Fatalf("linked: %v", linked)
	}
	// a requirement of another project cannot be linked
	other := e.json("POST", "/api/projects", "alice", `{"name":"other"}`, 201)["id"].(string)
	foreign, _, err := e.reqs.Create(e.ctx, requirements.Requirement{ProjectID: other, Title: "foreign"})
	if err != nil {
		t.Fatal(err)
	}
	e.json("POST", "/api/threads/"+th+"/requirement", "bob", fmt.Sprintf(`{"requirementId":%q}`, foreign.ID), 404)

	e.startRun("bob", th, "hello")
	e.waitIdle(th, "bob")
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, s := range seen {
		found = found || strings.Contains(s, "requirement "+id)
	}
	if !found {
		t.Fatalf("the agent never saw the requirement id %s in %d calls", id, len(seen))
	}
}

// ---- import from GitHub ----

func rqGitHub(t *testing.T) (*GitHubApp, *int) {
	t.Helper()
	var issueCalls int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/42/access_tokens" && r.Method == "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_x", "expires_at": time.Now().Add(time.Hour)})
		case r.URL.Path == "/repos/acme/api/issues":
			issueCalls++
			if r.URL.Query().Get("state") != "open" || r.Header.Get("Authorization") != "Bearer ghs_x" {
				t.Errorf("issues request: %s %v", r.URL, r.Header)
			}
			items := []map[string]any{
				{"number": 1, "title": "Crash on start", "body": "stack", "html_url": "https://github.com/acme/api/issues/1", "user": map[string]any{"login": "zed"}, "labels": []map[string]any{{"name": "bug"}}},
				{"number": 2, "title": "A pull request", "pull_request": map[string]any{"url": "x"}, "labels": []map[string]any{}, "user": map[string]any{"login": "zed"}},
				{"number": 3, "title": "Feature", "html_url": "https://github.com/acme/api/issues/3", "user": map[string]any{"login": "zed"}, "labels": []map[string]any{{"name": "enhancement"}}},
			}
			if l := r.URL.Query().Get("labels"); l != "" {
				var keep []map[string]any
				for _, it := range items {
					for _, lb := range it["labels"].([]map[string]any) {
						if lb["name"] == l {
							keep = append(keep, it)
						}
					}
				}
				items = keep
			}
			_ = json.NewEncoder(w).Encode(items)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)
	app, _ := testApp(t, api.URL)
	return app, &issueCalls
}

func TestRequirementImportFromGitHubSkipsPullRequestsAndDedupes(t *testing.T) {
	app, _ := rqGitHub(t)
	e := newRqEnv(t, rqOpts{github: app})
	e.json("POST", e.path("/import/github"), "bob", `{}`, 409) // no repository yet

	inst, repo := int64(42), "https://github.com/acme/api"
	if _, err := e.tm.UpdateProject(e.ctx, e.project, store.ProjectPatch{GitHubInstallation: &inst, RepoURL: &repo}); err != nil {
		t.Fatal(err)
	}
	e.json("POST", e.path("/import/github"), "carol", `{}`, 403)
	out := e.json("POST", e.path("/import/github"), "bob", `{"label":"bug"}`, 200)
	if out["imported"] != float64(1) || out["skipped"] != float64(0) {
		t.Fatalf("label import: %v", out)
	}
	out = e.json("POST", e.path("/import/github"), "bob", `{}`, 200)
	if out["imported"] != float64(1) || out["skipped"] != float64(1) { // #1 is known, #3 is new, the PR is not an issue
		t.Fatalf("second import: %v", out)
	}
	list := e.list(e.path(""), "bob", 200)
	if len(list) != 2 {
		t.Fatalf("list: %v", list)
	}
	var crash map[string]any
	for _, r := range list {
		if r["title"] == "Crash on start" {
			crash = r
		}
	}
	src := crash["source"].(map[string]any)
	if src["kind"] != "github_issue" || src["repo"] != "acme/api" || src["number"] != float64(1) || src["url"] == "" || crash["body"] != "stack" || crash["author"] != "bob" {
		t.Fatalf("imported: %v", crash)
	}
}

func TestRequirementImportNeedsAConfiguredAndConnectedGitHub(t *testing.T) {
	e := newRqEnv(t, rqOpts{})
	if r := e.json("POST", e.path("/import/github"), "bob", `{}`, 409); r["error"] == nil && r["code"] == nil {
		t.Logf("answer: %v", r)
	}
}

func TestTaskMoveMapping(t *testing.T) {
	task := factory.Task{PR: factory.PullRequest{Number: 1, URL: "u"}, Error: "boom", BlockedReason: "why"}
	for _, c := range []struct {
		from requirements.Status
		to   factory.State
		want requirements.Status // "" when unchanged
		flag string
		ok   bool
	}{
		{requirements.StatusReady, factory.StateRunning, requirements.StatusInProgress, "", true},
		{requirements.StatusInReview, factory.StateRunning, requirements.StatusInProgress, "", true},
		{requirements.StatusInProgress, factory.StateReview, requirements.StatusInReview, "", true},
		{requirements.StatusInProgress, factory.StateDone, requirements.StatusDone, "", true},
		{requirements.StatusInReview, factory.StateFailed, "", "task failed: boom", true},
		{requirements.StatusReady, factory.StateFailed, requirements.StatusInProgress, "task failed: boom", true},
		{requirements.StatusInProgress, factory.StateBlocked, "", "task blocked: why", true},
		{requirements.StatusDone, factory.StateRunning, "", "", false},
		{requirements.StatusRejected, factory.StateDone, "", "", false},
	} {
		p, ok := taskMove(requirements.Requirement{Status: c.from}, task, c.to)
		if ok != c.ok {
			t.Errorf("%s->%s: ok=%v", c.from, c.to, ok)
			continue
		}
		if !ok {
			continue
		}
		var got requirements.Status
		if p.Status != nil {
			got = *p.Status
		}
		if got != c.want || p.Flag == nil || *p.Flag != c.flag {
			t.Errorf("%s->%s: status %q flag %v, want %q %q", c.from, c.to, got, p.Flag, c.want, c.flag)
		}
	}
}
