package product

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
)

// fxStore is a factory.Store whose named operations can be made to fail.
type fxStore struct {
	factory.Store
	mu   sync.Mutex
	errs map[string]error
}

func (s *fxStore) failOn(op string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.errs, op)
		return
	}
	s.errs[op] = err
}

func (s *fxStore) err(op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.errs[op]
}

func (s *fxStore) GetTask(ctx context.Context, id string) (*factory.Task, error) {
	if err := s.err("GetTask"); err != nil {
		return nil, err
	}
	return s.Store.GetTask(ctx, id)
}

func (s *fxStore) ListTasks(ctx context.Context, f factory.Filter) ([]factory.Task, error) {
	if err := s.err("ListTasks"); err != nil {
		return nil, err
	}
	return s.Store.ListTasks(ctx, f)
}

func (s *fxStore) CreateTask(ctx context.Context, t factory.Task) (*factory.Task, bool, error) {
	if err := s.err("CreateTask"); err != nil {
		return nil, false, err
	}
	return s.Store.CreateTask(ctx, t)
}

func (s *fxStore) UpdateTask(ctx context.Context, id string, p factory.Patch) (*factory.Task, error) {
	if err := s.err("UpdateTask"); err != nil {
		return nil, err
	}
	return s.Store.UpdateTask(ctx, id, p)
}

func (s *fxStore) ListEvents(ctx context.Context, id string, limit int) ([]factory.TaskEvent, error) {
	if err := s.err("ListEvents"); err != nil {
		return nil, err
	}
	return s.Store.ListEvents(ctx, id, limit)
}

func (s *fxStore) AppendEvent(ctx context.Context, e factory.TaskEvent) error {
	if err := s.err("AppendEvent"); err != nil {
		return err
	}
	return s.Store.AppendEvent(ctx, e)
}

func (s *fxStore) ListTriggers(ctx context.Context, project string) ([]factory.Trigger, error) {
	if err := s.err("ListTriggers"); err != nil {
		return nil, err
	}
	return s.Store.ListTriggers(ctx, project)
}

func (s *fxStore) PutTrigger(ctx context.Context, t factory.Trigger) (*factory.Trigger, error) {
	if err := s.err("PutTrigger"); err != nil {
		return nil, err
	}
	return s.Store.PutTrigger(ctx, t)
}

func (s *fxStore) GetTrigger(ctx context.Context, id string) (*factory.Trigger, error) {
	if err := s.err("GetTrigger"); err != nil {
		return nil, err
	}
	return s.Store.GetTrigger(ctx, id)
}

func (s *fxStore) DeleteTrigger(ctx context.Context, id string) error {
	if err := s.err("DeleteTrigger"); err != nil {
		return err
	}
	return s.Store.DeleteTrigger(ctx, id)
}

func (s *fxStore) SpendSince(ctx context.Context, project string, t time.Time) (factory.Spend, error) {
	if err := s.err("SpendSince"); err != nil {
		return factory.Spend{}, err
	}
	return s.Store.SpendSince(ctx, project, t)
}

type fxOpts struct {
	model  *llmtest.Model
	run    bool   // run the dispatcher (default: tasks stay ready so states are deterministic)
	secret string // webhook secret
	tg     *cnTG  // a Telegram bot API double: turns the bridge on
	notify func(ctx context.Context, login, text string)
}

// fxEnv is a team server over a harness with the factory feature: alice owns the project "api", bob is a member,
// carol a viewer and dave is outside it.
type fxEnv struct {
	*rig
	tm      *cnFaultTeam
	st      *fxStore
	f       *harness.Factory
	esc     *FactoryEscalator
	hooks   *FactoryHooks
	tgAPI   *cnTG
	project string
}

func newFxEnv(t *testing.T, o fxOpts) *fxEnv {
	t.Helper()
	e := &fxEnv{tgAPI: o.tg, tm: newCnFaultTeam(), st: &fxStore{Store: factorystore.NewMem(nil), errs: map[string]error{}}}
	cnUsers(t, e.tm, "alice", "bob", "carol", "dave")
	e.esc = &FactoryEscalator{Notify: o.notify}
	e.hooks = &FactoryHooks{Team: e.tm}
	fo := harness.FactoryOptions{
		Store: e.st, Run: o.run, PollEvery: 10 * time.Millisecond, WebhookSecret: []byte(o.secret),
		Escalator: e.esc, OnSpawn: e.hooks.OnSpawn, Backoff: func(int) time.Duration { return 0 },
	}
	prod := Options{Team: e.tm, Factory: FactoryOptions{Escalator: e.esc, Hooks: e.hooks}}
	if o.tg != nil {
		prod.SecretKey, prod.PublicURL = cnKey, "https://harness.test/"
		prod.Telegram = &Telegram{Token: cnTGToken, Username: "harness_bot", API: o.tg.srv.URL, PollSeconds: 1}
	}
	e.rig = newRig(t, rigOpts{
		team: true, model: o.model, prod: prod, env: map[string]string{"LLM_DEFAULT_MODEL": "gpt-test"},
		mod: func(ho *harness.Options) { ho.Features = append(harness.ProfileMinimal(), harness.FactoryFeature(fo)) },
	})
	e.f = e.h.FactoryOf()
	if e.f == nil {
		t.Fatal("the factory feature published no service")
	}
	e.project = e.json("POST", "/api/projects", "alice", `{"name":"api"}`, 201)["id"].(string)
	e.json("PUT", "/api/projects/"+e.project+"/members/bob", "alice", `{"role":"member"}`, 204)
	e.json("PUT", "/api/projects/"+e.project+"/members/carol", "alice", `{"role":"viewer"}`, 204)
	return e
}

// repoProject makes the project a GitHub one (tasks then default to its repository).
func (e *fxEnv) repoProject(t *testing.T) {
	t.Helper()
	e.json("PATCH", "/api/projects/"+e.project, "alice", `{"repoUrl":"https://github.com/x/api"}`, 200)
}

func (e *fxEnv) tasks(suffix string) string { return "/api/projects/" + e.project + "/tasks" + suffix }

// newTask creates a task through the API as bob and returns its id.
func (e *fxEnv) newTask(title string) string {
	e.t.Helper()
	return e.json("POST", e.tasks(""), "bob", `{"title":"`+title+`","spec":"do it"}`, 201)["id"].(string)
}

// block blocks a task the way the dispatcher does and waits for the escalation to be delivered.
func (e *fxEnv) block(id, reason string) {
	e.t.Helper()
	if err := e.f.Tasks.Block(e.ctx, id, reason); err != nil {
		e.t.Fatal(err)
	}
	e.f.Tasks.Flush()
}

func (e *fxEnv) stateOf(id string) factory.State {
	e.t.Helper()
	tk, err := e.st.Store.GetTask(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk.State
}

func (e *fxEnv) list(path, user string, want int) []map[string]any {
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

func fxWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fxLine waits for a feed line containing all of want.
func fxLine(t *testing.T, feed <-chan string, want ...string) string {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case l := <-feed:
			ok := true
			for _, w := range want {
				ok = ok && strings.Contains(l, w)
			}
			if ok {
				return l
			}
		case <-timeout:
			t.Fatalf("no feed event containing %q", want)
		}
	}
}

// fxQuiet reports whether the feed stays silent (for lines containing sub) for a moment.
func fxQuiet(feed <-chan string, sub string) bool {
	timeout := time.After(250 * time.Millisecond)
	for {
		select {
		case l := <-feed:
			if strings.Contains(l, sub) {
				return false
			}
		case <-timeout:
			return true
		}
	}
}

// ---- the routes ----

func TestFactoryTasksBelongToTheProjectAndTheRoleDecidesWhatYouMayDo(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	cases := []struct {
		name, method, path, user, body string
		want                           int
	}{
		{"viewer cannot create", "POST", e.tasks(""), "carol", `{"title":"x"}`, 403},
		{"outsider cannot create", "POST", e.tasks(""), "dave", `{"title":"x"}`, 404},
		{"viewer can list", "GET", e.tasks(""), "carol", "", 200},
		{"outsider cannot list", "GET", e.tasks(""), "dave", "", 404},
		{"outsider cannot read the report", "GET", "/api/projects/" + e.project + "/factory/report", "dave", "", 404},
		{"viewer can read the report", "GET", "/api/projects/" + e.project + "/factory/report", "carol", "", 200},
		{"outsider cannot list triggers", "GET", "/api/projects/" + e.project + "/triggers", "dave", "", 404},
		{"viewer can list triggers", "GET", "/api/projects/" + e.project + "/triggers", "carol", "", 200},
		{"member cannot create triggers", "POST", "/api/projects/" + e.project + "/triggers", "bob", `{}`, 403},
		{"member cannot delete triggers", "DELETE", "/api/projects/" + e.project + "/triggers/x", "bob", "", 403},
		{"unknown project", "GET", "/api/projects/nope/tasks", "alice", "", 404},
	}
	for _, c := range cases {
		// Act
		got := e.do(c.method, c.path, c.user, c.body).StatusCode

		// Assert
		if got != c.want {
			t.Errorf("%s: %s %s as %s = %d, want %d", c.name, c.method, c.path, c.user, got, c.want)
		}
	}
	id := e.newTask("viewer-proof")
	for _, p := range []string{"", "/cancel", "/resume"} {
		method := "POST"
		if p == "" {
			method = "GET"
		}
		if got := e.do(method, e.tasks("/"+id+p), "dave", "{}").StatusCode; got != 404 {
			t.Errorf("outsider %s %s = %d, want 404", method, p, got)
		}
	}
	e.json("POST", e.tasks("/"+id+"/cancel"), "carol", "", 403)
	e.json("POST", e.tasks("/"+id+"/resume"), "carol", "{}", 403)
	if got := e.stateOf(id); got != factory.StateReady {
		t.Errorf("a refused request changed the task to %s", got)
	}
}

func TestFactoryCreateTaskDefaultsFromTheProjectAndRejectsBadInput(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	e.repoProject(t)
	project := e.json("GET", "/api/projects/"+e.project, "alice", "", 200)

	// Act
	created := e.json("POST", e.tasks(""), "bob", `{"title":"Fix the build","spec":"it fails","priority":3,"labels":["ci"],"policy":"supervised"}`, 201)

	// Assert: the task is bob's, in the project, on the project's repository and branch.
	if created["owner"] != "bob" || created["projectId"] != e.project || created["state"] != "ready" {
		t.Errorf("task = %v", created)
	}
	if created["repo"] != "x/api" || created["baseBranch"] != project["defaultBranch"] {
		t.Errorf("repo/baseBranch = %v/%v, want x/api/%v", created["repo"], created["baseBranch"], project["defaultBranch"])
	}
	if created["policy"] != nil && created["policy"] != "supervised" {
		t.Errorf("policy = %v", created["policy"])
	}
	explicit := e.json("POST", e.tasks(""), "alice", `{"title":"Other","repo":"x/api","baseBranch":"dev"}`, 201)
	if explicit["repo"] != "x/api" || explicit["baseBranch"] != "dev" {
		t.Errorf("explicit repo/branch were overridden: %v", explicit)
	}
	bad := map[string]struct{ body, code string }{
		"invalid policy": {`{"title":"x","policy":"yolo"}`, "invalid_policy"},
		"no title":       {`{"spec":"x"}`, "invalid_task"},
		"unknown role":   {`{"title":"x","role":"wizard"}`, "invalid_task"},
		"unknown model":  {`{"title":"x","model":"no-such-model"}`, "invalid_task"},
		"unknown dep":    {`{"title":"x","dependsOn":["nope"]}`, "invalid_task"},
		"not json":       {`{`, "invalid_request"},
	}
	for name, c := range bad {
		if got := cnErrCode(e.json("POST", e.tasks(""), "bob", c.body, 400)); got != c.code {
			t.Errorf("%s: code %q, want %q", name, got, c.code)
		}
	}
	e.st.failOn("CreateTask", errCnBoom)
	e.json("POST", e.tasks(""), "bob", `{"title":"x"}`, 500)
}

func TestFactoryListsFiltersAndReadsTasksOfTheProjectOnly(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	a, b := e.newTask("first"), e.newTask("second")
	if err := e.f.Tasks.Cancel(e.ctx, b); err != nil {
		t.Fatal(err)
	}
	other := e.json("POST", "/api/projects", "alice", `{"name":"other"}`, 201)["id"].(string)
	foreign := e.json("POST", "/api/projects/"+other+"/tasks", "alice", `{"title":"foreign"}`, 201)["id"].(string)

	// Act / Assert: the list is the project's, newest filters apply, junk states are ignored.
	if got := e.list(e.tasks(""), "carol", 200); len(got) != 2 {
		t.Errorf("tasks = %d, want the project's 2", len(got))
	}
	if got := e.list(e.tasks("?state=cancelled"), "bob", 200); len(got) != 1 || got[0]["id"] != b {
		t.Errorf("cancelled = %v", got)
	}
	if got := e.list(e.tasks("?state=ready,bogus"), "bob", 200); len(got) != 1 || got[0]["id"] != a {
		t.Errorf("ready,bogus = %v", got)
	}
	if got := e.list(e.tasks("?limit=1"), "bob", 200); len(got) != 1 {
		t.Errorf("limit=1 gave %d", len(got))
	}
	if got := e.list(e.tasks("?state=done"), "bob", 200); got == nil || len(got) != 0 {
		t.Errorf("an empty list must be [], got %v", got)
	}
	// One task with its history; another project's task is not found here, nor is an unknown one.
	got := e.json("GET", e.tasks("/"+a), "carol", "", 200)
	events, _ := got["events"].([]any)
	if task, _ := got["task"].(map[string]any); task["id"] != a || len(events) == 0 {
		t.Errorf("task = %v", got)
	}
	e.json("GET", e.tasks("/"+foreign), "alice", "", 404)
	e.json("POST", e.tasks("/"+foreign+"/cancel"), "alice", "", 404)
	e.json("POST", e.tasks("/"+foreign+"/resume"), "alice", "{}", 404)
	e.json("GET", e.tasks("/unknown"), "alice", "", 404)
	if e.stateOf(foreign) != factory.StateReady {
		t.Error("a cross-project cancel touched the foreign task")
	}
	// Storage failures are 500s, not leaks.
	e.st.failOn("ListTasks", errCnBoom)
	e.json("GET", e.tasks(""), "bob", "", 500)
	e.st.failOn("ListTasks", nil)
	e.st.failOn("GetTask", errCnBoom)
	e.json("GET", e.tasks("/"+a), "bob", "", 500)
	e.st.failOn("GetTask", nil)
	e.st.failOn("ListEvents", errCnBoom)
	e.json("GET", e.tasks("/"+a), "bob", "", 500)
}

func TestFactoryCancelStopsATaskOnceAndAnsweringAgainIsAConflict(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	id := e.newTask("to cancel")
	feed := feedOf(e.rig, "alice")

	// Act
	resp := e.do("POST", e.tasks("/"+id+"/cancel"), "bob", "")

	// Assert
	if resp.StatusCode != 202 || e.stateOf(id) != factory.StateCancelled {
		t.Fatalf("cancel = %d, state %s", resp.StatusCode, e.stateOf(id))
	}
	fxLine(t, feed, "task.updated", "to cancel")
	if got := cnErrCode(e.json("POST", e.tasks("/"+id+"/cancel"), "bob", "", 409)); got != "conflict" {
		t.Errorf("second cancel code %q", got)
	}
	other := e.newTask("storage")
	e.st.failOn("UpdateTask", errCnBoom)
	e.json("POST", e.tasks("/"+other+"/cancel"), "bob", "", 500)
}

func TestFactoryResumeOnlyUnblocksABlockedTaskAndPassesTheHumansWords(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	ready, blocked, again := e.newTask("ready"), e.newTask("blocked"), e.newTask("again")
	e.block(blocked, "needs a decision")
	e.block(again, "needs a decision")

	// Act / Assert: a task that is not blocked is a conflict and stays as it was.
	if got := cnErrCode(e.json("POST", e.tasks("/"+ready+"/resume"), "bob", `{"feedback":"go"}`, 409)); got != "conflict" {
		t.Errorf("code %q", got)
	}
	e.json("POST", e.tasks("/"+ready+"/resume"), "bob", `{`, 400)
	if got := e.stateOf(ready); got != factory.StateReady {
		t.Errorf("state %s", got)
	}
	// A blocked task goes back to work with the human's words, or a default nudge.
	e.json("POST", e.tasks("/"+blocked+"/resume"), "bob", `{"feedback":"  use the v2 API  "}`, 202)
	tk, _ := e.st.Store.GetTask(e.ctx, blocked)
	if tk.State != factory.StateReady || tk.Feedback != "A human says: use the v2 API" {
		t.Errorf("resumed = %s %q", tk.State, tk.Feedback)
	}
	e.json("POST", e.tasks("/"+again+"/resume"), "bob", `{}`, 202)
	if tk, _ := e.st.Store.GetTask(e.ctx, again); tk.Feedback != "A human unblocked this task. Continue." {
		t.Errorf("default feedback = %q", tk.Feedback)
	}
	third := e.newTask("storage")
	e.block(third, "r")
	e.st.failOn("UpdateTask", errCnBoom)
	e.json("POST", e.tasks("/"+third+"/resume"), "bob", `{}`, 500)
}

func TestFactoryTriggersAreManagedByOwnersOnly(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	e.repoProject(t)
	triggers := "/api/projects/" + e.project + "/triggers"

	// Act
	cron := e.json("POST", triggers, "alice", `{"name":"nightly","kind":"cron","schedule":"0 2 * * *","template":{"title":"Nightly cleanup","spec":"tidy"}}`, 201)
	hook := e.json("POST", triggers, "alice", `{"name":"labelled","kind":"webhook","event":"issues.labeled","match":{"label":"agent"},"template":{"title":"Fix {{.title}}"},"enabled":false}`, 201)

	// Assert: enabled by default, repo from the project, a cron trigger knows when it fires next.
	if cron["enabled"] != true || cron["owner"] != "alice" || cron["projectId"] != e.project {
		t.Errorf("cron = %v", cron)
	}
	if tpl, _ := cron["template"].(map[string]any); tpl["repo"] == "" || tpl["repo"] != "x/api" {
		t.Errorf("template = %v", cron["template"])
	}
	if cron["nextFireAt"] == nil || cron["nextFireAt"] == "0001-01-01T00:00:00Z" {
		t.Errorf("nextFireAt = %v", cron["nextFireAt"])
	}
	if hook["enabled"] != false {
		t.Errorf("an explicit enabled:false was ignored: %v", hook)
	}
	if got := e.list(triggers, "carol", 200); len(got) != 2 {
		t.Errorf("triggers = %d", len(got))
	}
	// The other project's trigger cannot be deleted through this project; an unknown one is a 404.
	other := e.json("POST", "/api/projects", "alice", `{"name":"other"}`, 201)["id"].(string)
	foreign := e.json("POST", "/api/projects/"+other+"/triggers", "alice", `{"name":"x","kind":"cron","schedule":"@every 1h","template":{"title":"t"}}`, 201)["id"].(string)
	e.json("DELETE", triggers+"/"+foreign, "alice", "", 404)
	e.json("DELETE", triggers+"/nope", "alice", "", 404)
	e.st.failOn("DeleteTrigger", errCnBoom)
	e.json("DELETE", triggers+"/"+cron["id"].(string), "alice", "", 500)
	e.st.failOn("DeleteTrigger", nil)
	e.st.failOn("GetTrigger", errCnBoom)
	e.json("DELETE", triggers+"/"+cron["id"].(string), "alice", "", 500)
	e.st.failOn("GetTrigger", nil)
	if got := e.do("DELETE", triggers+"/"+cron["id"].(string), "alice", "").StatusCode; got != 204 {
		t.Fatalf("delete = %d", got)
	}
	if got := e.list(triggers, "alice", 200); len(got) != 1 || got[0]["id"] != hook["id"] {
		t.Errorf("after delete: %v", got)
	}
	e.st.failOn("ListTriggers", errCnBoom)
	e.json("GET", triggers, "alice", "", 500)
}

func TestFactoryTriggerInputIsValidated(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	triggers := "/api/projects/" + e.project + "/triggers"
	bad := map[string]string{
		"no name":               `{"kind":"cron","schedule":"@every 1h","template":{"title":"t"}}`,
		"no title":              `{"name":"n","kind":"cron","schedule":"@every 1h"}`,
		"bad schedule":          `{"name":"n","kind":"cron","schedule":"every now and then","template":{"title":"t"}}`,
		"no schedule":           `{"name":"n","kind":"cron","template":{"title":"t"}}`,
		"too many fields":       `{"name":"n","kind":"cron","schedule":"* * * * * *","template":{"title":"t"}}`,
		"webhook without event": `{"name":"n","kind":"webhook","template":{"title":"t"}}`,
		"unknown kind":          `{"name":"n","kind":"carrier-pigeon","template":{"title":"t"}}`,
		"broken template":       `{"name":"n","kind":"cron","schedule":"@every 1h","template":{"title":"{{.x"}}`,
		"not json":              `[`,
	}
	for name, body := range bad {
		// Act
		got := e.json("POST", triggers, "alice", body, 400)

		// Assert
		if code := cnErrCode(got); code != "invalid_trigger" && code != "invalid_request" {
			t.Errorf("%s: code %q", name, code)
		}
	}
	if got := e.list(triggers, "alice", 200); len(got) != 0 {
		t.Errorf("a rejected trigger was stored: %v", got)
	}
	e.st.failOn("PutTrigger", errCnBoom)
	e.json("POST", triggers, "alice", `{"name":"n","kind":"cron","schedule":"@every 1h","template":{"title":"t"}}`, 500)
}

func TestFactoryReportCountsTheProjectsTasksAndSpend(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	id := e.newTask("costly")
	if _, err := e.st.Store.AddSpend(e.ctx, id, factory.Spend{USD: 0.5, Tokens: 1000, Runs: 1}); err != nil {
		t.Fatal(err)
	}
	e.newTask("free")
	path := "/api/projects/" + e.project + "/factory/report"

	// Act
	rep := e.json("GET", path+"?days=30", "carol", "", 200)
	clamped := e.json("GET", path+"?days=100000", "carol", "", 200)
	junk := e.json("GET", path+"?days=abc", "carol", "", 200)

	// Assert
	total, _ := rep["Total"].(map[string]any)
	byState, _ := rep["ByState"].(map[string]any)
	top, _ := rep["Top"].([]any)
	if total["tokens"] != float64(1000) || byState["ready"] != float64(2) || len(top) != 1 {
		t.Errorf("report = %v", rep)
	}
	if clamped["Since"] == nil || junk["Since"] == nil {
		t.Errorf("clamped %v junk %v", clamped, junk)
	}
	e.st.failOn("SpendSince", errCnBoom)
	e.json("GET", path, "carol", "", 500)
}

func TestFactoryRoutesAreAbsentWithoutTheFeature(t *testing.T) {
	// Arrange: a team server over a harness without the factory feature.
	r := newRig(t, rigOpts{team: true, prod: Options{Factory: FactoryOptions{Escalator: &FactoryEscalator{}, Hooks: &FactoryHooks{}}}})

	// Act
	got := r.do("GET", "/api/projects/p/tasks", "alice", "").StatusCode
	hook := r.do("POST", "/hooks/github", "", "{}").StatusCode

	// Assert
	if got != 404 || hook != 404 && hook != 405 {
		t.Errorf("tasks route = %d, webhook = %d; want them absent", got, hook)
	}
}

// ---- the webhook ----

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

const fxIssueLabeled = `{"action":"labeled","label":{"name":"agent"},"repository":{"full_name":"x/api"},"sender":{"login":"eve"},` +
	`"issue":{"number":7,"title":"Crash on start","body":"stack trace","html_url":"https://github.com/x/api/issues/7","labels":[{"name":"agent"}]}}`

func TestFactoryWebhookNeedsNoSessionButAValidSignature(t *testing.T) {
	// Arrange
	const secret = "whsec"
	e := newFxEnv(t, fxOpts{secret: secret})
	e.repoProject(t)
	e.json("POST", "/api/projects/"+e.project+"/triggers", "alice",
		`{"name":"labelled","kind":"webhook","event":"issues.labeled","match":{"label":"agent"},"template":{"title":"Fix #{{.number}}: {{.title}}","spec":"{{.body}}"}}`, 201)
	deliver := func(path, event, delivery, body, sig string) *http.Response {
		return e.do("POST", path, "", body, "X-GitHub-Event", event, "X-GitHub-Delivery", delivery, "X-Hub-Signature-256", sig)
	}

	// Act
	unsigned := deliver("/hooks/github", "issues", "d0", fxIssueLabeled, "")
	forged := deliver("/hooks/github", "issues", "d0", fxIssueLabeled, sign("other", fxIssueLabeled))
	signed := deliver("/hooks/github", "issues", "d1", fxIssueLabeled, sign(secret, fxIssueLabeled))
	again := deliver("/api/hooks/github", "issues", "d1", fxIssueLabeled, sign(secret, fxIssueLabeled))
	unrelated := `{"action":"labeled","label":{"name":"docs"},"repository":{"full_name":"x/api"},"issue":{"number":8,"title":"t"}}`
	skipped := deliver("/hooks/github", "issues", "d2", unrelated, sign(secret, unrelated))

	// Assert
	if unsigned.StatusCode != 401 || forged.StatusCode != 401 {
		t.Errorf("unsigned %d, forged %d; want 401", unsigned.StatusCode, forged.StatusCode)
	}
	if signed.StatusCode != 202 || again.StatusCode != 200 || skipped.StatusCode != 202 {
		t.Fatalf("signed %d, redelivered %d, unrelated %d; want 202, 200, 202", signed.StatusCode, again.StatusCode, skipped.StatusCode)
	}
	tasks := e.list(e.tasks(""), "alice", 200)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %v, want exactly the one the matching delivery created", tasks)
	}
	src, _ := tasks[0]["source"].(map[string]any)
	if tasks[0]["title"] != "Fix #7: Crash on start" || tasks[0]["owner"] != "alice" || tasks[0]["projectId"] != e.project ||
		tasks[0]["spec"] != "stack trace" || src["kind"] != factory.SourceGitHub || !strings.HasPrefix(fmt.Sprint(src["key"]), "github:issue:x/api#7") {
		t.Errorf("task = %v", tasks[0])
	}
}

func TestFactoryWebhookWithoutASecretRefusesEveryDelivery(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})

	// Act
	got := e.do("POST", "/hooks/github", "", `{}`, "X-GitHub-Event", "ping", "X-Hub-Signature-256", sign("", "{}")).StatusCode

	// Assert
	if got != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", got)
	}
}

// ---- the GitHub App adapter ----

func TestFactoryAuthMintsATokenLimitedToTheRepository(t *testing.T) {
	// Arrange
	var mu sync.Mutex
	var scoped []any
	var api *httptest.Server
	api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/api/installation":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42})
		case r.URL.Path == "/app/installations/42/access_tokens" && r.Method == "POST":
			var body struct {
				Repositories []any `json:"repositories"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			scoped = body.Repositories
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_factory", "expires_at": time.Now().Add(time.Hour)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	app, _ := testApp(t, api.URL)
	auth := FactoryAuth{App: app}
	ctx := context.Background()

	// Act
	tok, err := auth.Token(ctx, "acme/api")
	_, notInstalled := auth.Token(ctx, "acme/other")

	// Assert
	if err != nil || tok != "ghs_factory" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	mu.Lock()
	if len(scoped) != 1 || scoped[0] != "api" {
		t.Errorf("token scope = %v, want only the repository", scoped)
	}
	mu.Unlock()
	if !errors.Is(notInstalled, ErrNotInstalled) || !strings.Contains(notInstalled.Error(), "acme/other") {
		t.Errorf("uninstalled repository: %v", notInstalled)
	}
	for _, repo := range []string{"", "acme", "/api", "acme/", "acme//"} {
		if _, err := auth.Token(ctx, repo); err == nil || !strings.Contains(err.Error(), "owner/name") {
			t.Errorf("Token(%q) = %v, want a not-owner/name error", repo, err)
		}
	}
	api.Close()
	if _, err := auth.Token(ctx, "acme/api"); err == nil {
		t.Error("an unreachable GitHub gave a token")
	}
}

func TestFactoryAuthReportsAFailedTokenMint(t *testing.T) {
	// Arrange
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/installation") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7})
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer api.Close()
	app, _ := testApp(t, api.URL)

	// Act
	tok, err := FactoryAuth{App: app}.Token(context.Background(), "acme/api")

	// Assert
	if err == nil || tok != "" {
		t.Errorf("token %q, err %v; want the mint failure", tok, err)
	}
}

// ---- the escalator ----

func TestFactoryEscalatorPublishesToTheProjectFeedAndNotifies(t *testing.T) {
	// Arrange
	type call struct{ login, text string }
	calls := make(chan call, 4)
	e := newFxEnv(t, fxOpts{notify: func(_ context.Context, login, text string) { calls <- call{login, text} }})
	id := e.newTask("Ship it")
	alice, dave := feedOf(e.rig, "alice"), feedOf(e.rig, "dave")

	// Act
	e.block(id, "ready to merge: awaiting a human")

	// Assert: members hear it on the live feed, outsiders do not, the owner is notified.
	line := fxLine(t, alice, "task.blocked", "Ship it", "ready to merge")
	if !strings.Contains(line, e.project) {
		t.Errorf("event %s lacks the project", line)
	}
	if !fxQuiet(dave, "task.blocked") {
		t.Error("an outsider saw the escalation")
	}
	select {
	case c := <-calls:
		if c.login != "bob" || !strings.Contains(c.text, "Ship it") || !strings.Contains(c.text, "ready to merge") {
			t.Errorf("notified %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Notify was not called")
	}
}

func TestFactoryEscalatorBeforeTheServerBindsOnlyLogs(t *testing.T) {
	// Arrange
	var notified []string
	unbound := &FactoryEscalator{Notify: func(_ context.Context, login, text string) { notified = append(notified, login+": "+text) }}
	silent := &FactoryEscalator{}

	// Act: neither may panic although no server (hub) is bound.
	unbound.Escalate(context.Background(), factory.Task{ID: "t1", Owner: "bob", Title: "Stuck", ProjectID: "p"}, "why")
	silent.Escalate(context.Background(), factory.Task{ID: "t2", Title: "Quiet"}, "why")

	// Assert
	if len(notified) != 1 || !strings.HasPrefix(notified[0], "bob: ") || !strings.Contains(notified[0], "Stuck") {
		t.Errorf("notified = %v", notified)
	}
}

func TestFactoryEscalationReachesTheOwnersLinkedTelegramChat(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	e := newFxEnv(t, fxOpts{tg: tg})
	e.linkTelegram(t, "bob", 222)
	id := e.newTask("Ship it")

	// Act
	e.block(id, "ready to merge: awaiting a human")

	// Assert: the owner's chat gets the title, the reason and a link to the project.
	msg := tg.waitSent(t, 222, "Ship it")["text"].(string)
	for _, want := range []string{"ready to merge: awaiting a human", "https://harness.test/#/projects/" + e.project} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestFactoryEscalationWithoutALinkedChatSendsNothing(t *testing.T) {
	// Arrange: bob is linked, alice (the task's owner) is not.
	tg := newCnTG(t)
	e := newFxEnv(t, fxOpts{tg: tg})
	e.linkTelegram(t, "bob", 222)
	id := e.json("POST", e.tasks(""), "alice", `{"title":"Alice's task"}`, 201)["id"].(string)
	before := tg.count("sendMessage")

	// Act
	e.block(id, "stuck")

	// Assert
	if got := tg.count("sendMessage"); got != before {
		t.Errorf("sendMessage calls %d -> %d, want none for an owner without a chat", before, got)
	}
}

func TestFactoryEscalationOmitsTheLinkWithoutAProjectOrAPublicAddressAndSurvivesTelegramErrors(t *testing.T) {
	// Arrange
	tg := newCnTG(t)
	e := newFxEnv(t, fxOpts{tg: tg})
	e.linkTelegram(t, "bob", 222)

	// Act: a task outside any project has nowhere to link to.
	e.esc.Escalate(e.ctx, factory.Task{ID: "t1", Owner: "bob", Title: "Personal"}, "stuck")

	// Assert
	if msg := tg.waitSent(t, 222, "Personal")["text"].(string); strings.Contains(msg, "#/projects") {
		t.Errorf("message %q links to a project that does not exist", msg)
	}
	// Without a configured address the link would point nowhere useful: it is left out.
	e.esc.mu.Lock()
	s := e.esc.s
	e.esc.mu.Unlock()
	s.opt.PublicURL = ""
	e.esc.Escalate(e.ctx, factory.Task{ID: "t2", Owner: "bob", Title: "No base", ProjectID: e.project}, "stuck")
	if msg := tg.waitSent(t, 222, "No base")["text"].(string); strings.Contains(msg, "http") {
		t.Errorf("message %q has a link without a public address", msg)
	}
	// A refused message is logged, never raised, and the feed event still goes out.
	feed := feedOf(e.rig, "alice")
	tg.setFail("sendMessage", "Forbidden: bot was blocked by the user")
	e.esc.Escalate(e.ctx, factory.Task{ID: "t3", Owner: "bob", Title: "Refused", ProjectID: e.project}, "stuck")
	fxLine(t, feed, "task.blocked", "Refused")
}

// linkTelegram links user's account to a Telegram chat through the bot, as the profile page does.
func (e *fxEnv) linkTelegram(t *testing.T, user string, chat int) {
	t.Helper()
	code := e.json("POST", "/api/connectors/telegram/link", user, `{}`, 200)["code"].(string)
	e.tgAPI.say(chat, "/start "+code)
	e.tgAPI.waitSent(t, chat, "Linked")
}

// ---- threads join the project ----

// fxDelegatingModel makes the task's run delegate one exploration and then report; the sub-agent answers at once.
func fxDelegatingModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		last := c.Messages[len(c.Messages)-1]
		if last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("report: delegated and done")}
		}
		var user string
		for _, m := range c.Messages {
			if m.Role == ai.RoleUser {
				user = m.Content
			}
		}
		if strings.HasPrefix(user, "# Task:") {
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "d1", "delegate", `{"role":"explorer","task":"look around"}`), "tool_calls")}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("child report")}
	})
}

func TestFactoryTaskThreadsAreVisibleToTheProjectsMembers(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{run: true, model: fxDelegatingModel()})
	feed := feedOf(e.rig, "alice")
	outsider := feedOf(e.rig, "dave")

	// Act
	id := e.newTask("Explore the repo")
	fxWait(t, "the task to finish", func() bool { return e.stateOf(id) == factory.StateDone })

	// Assert: the task's thread and the delegated child's thread are both in the project's thread list.
	tk, _ := e.st.Store.GetTask(e.ctx, id)
	threads := e.list("/api/threads?project="+e.project, "carol", 200)
	byID := map[string]map[string]any{}
	for _, th := range threads {
		byID[th["id"].(string)] = th
	}
	if len(threads) != 2 || byID[tk.ThreadID] == nil {
		t.Fatalf("project threads = %v, want the task's thread (%s) and its delegate's", threads, tk.ThreadID)
	}
	var child map[string]any
	for _, th := range threads {
		if th["id"] != tk.ThreadID {
			child = th
		}
	}
	if title, _ := child["title"].(string); !strings.HasPrefix(title, "explorer:") {
		t.Errorf("child thread title = %q", title)
	}
	for _, th := range threads {
		if th["createdBy"] != "bob" || th["projectId"] != e.project {
			t.Errorf("thread %v is not bob's, in the project", th)
		}
	}
	// Members open them, an outsider cannot; the live feed announced both to the project.
	for _, th := range threads {
		e.json("GET", "/api/threads/"+th["id"].(string), "alice", "", 200)
		e.json("GET", "/api/threads/"+th["id"].(string), "carol", "", 200)
		e.json("GET", "/api/threads/"+th["id"].(string), "dave", "", 404)
	}
	announced := map[string]bool{}
	for len(announced) < len(threads) {
		l := fxLine(t, feed, "thread.created", e.project)
		for _, th := range threads {
			if strings.Contains(l, th["id"].(string)) {
				announced[th["id"].(string)] = true
			}
		}
	}
	if !fxQuiet(outsider, "thread.created") {
		t.Error("an outsider heard about the project's task threads")
	}
	// A task of no project keeps its thread private to its owner.
	personal, _, err := e.f.Tasks.Create(e.ctx, factory.Task{Owner: "bob", Title: "Private", Spec: "x"})
	if err != nil {
		t.Fatal(err)
	}
	fxWait(t, "the private task to finish", func() bool { return e.stateOf(personal.ID) == factory.StateDone })
	pt, _ := e.st.Store.GetTask(e.ctx, personal.ID)
	if _, err := e.tm.GetThreadMeta(e.ctx, pt.ThreadID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a task without a project was linked to one: %v", err)
	}
}

func TestFactoryOnSpawnLogsAndContinuesWhenTheLinkCannotBeWritten(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	feed := feedOf(e.rig, "alice")
	task := factory.Task{ID: "t1", ProjectID: e.project, Owner: "bob", Title: "x"}
	req := factory.SpawnRequest{Login: "bob", Title: "x"}
	e.tm.failOn("SetThreadMeta", errCnBoom)

	// Act
	e.hooks.OnSpawn(e.ctx, "thread-1", task, req)

	// Assert: nothing is linked or announced, and nothing panicked.
	if _, err := e.tm.GetThreadMeta(e.ctx, "thread-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("meta after a failed write: %v", err)
	}
	if !fxQuiet(feed, "thread.created") {
		t.Error("a thread.created event went out for a thread that was not linked")
	}
	// A hook without a store, or a task without a project, does nothing.
	(&FactoryHooks{}).OnSpawn(e.ctx, "thread-2", task, req)
	e.tm.failOn("SetThreadMeta", nil)
	e.hooks.OnSpawn(e.ctx, "thread-3", factory.Task{ID: "t2", Owner: "bob"}, req)
	if _, err := e.tm.GetThreadMeta(e.ctx, "thread-3"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a personal task's thread was linked: %v", err)
	}
	// Before the server binds, the link is still written (only the feed announcement is skipped).
	unbound := &FactoryHooks{Team: e.tm}
	unbound.OnSpawn(e.ctx, "thread-4", task, req)
	if m, err := e.tm.GetThreadMeta(e.ctx, "thread-4"); err != nil || m.ProjectID != e.project || m.Owner != "bob" || m.CreatedBy != "bob" || m.Kind != store.ThreadChat {
		t.Errorf("unbound hook meta = %+v, %v", m, err)
	}
}

// ---- a project's tasks and triggers stay inside its own repository ----

// newProject creates a project owned by alice (and bob as a member when asked) on the given repository URL.
func (e *fxEnv) newProject(name, repoURL string, bobMember bool) string {
	e.t.Helper()
	id := e.json("POST", "/api/projects", "alice", `{"name":"`+name+`","repoUrl":"`+repoURL+`"}`, 201)["id"].(string)
	if bobMember {
		e.json("PUT", "/api/projects/"+id+"/members/bob", "alice", `{"role":"member"}`, 204)
	}
	return id
}

func (e *fxEnv) tasksOf(project string) []map[string]any {
	e.t.Helper()
	return e.list("/api/projects/"+project+"/tasks", "alice", 200)
}

func TestFactoryTaskRepositoryIsAlwaysTheProjectsOwn(t *testing.T) {
	// Arrange: bob is a member of "api" (x/api) only; "lib" (y/lib) is another tenant's project.
	e := newFxEnv(t, fxOpts{})
	e.repoProject(t)
	e.newProject("lib", "https://github.com/y/lib", false)
	bare := e.newProject("bare", "", true)
	cases := []struct {
		name, project, body string
		want                int
		repo                string
	}{
		{"another project's repository", e.project, `{"title":"t","repo":"y/lib"}`, 400, ""},
		{"another owner's repository", e.project, `{"title":"t","repo":"evil/secrets"}`, 400, ""},
		{"a repository of the same owner", e.project, `{"title":"t","repo":"x/other"}`, 400, ""},
		{"own repository in another case", e.project, `{"title":"t","repo":"X/API"}`, 201, "x/api"},
		{"own repository spelled exactly", e.project, `{"title":"t","repo":"x/api"}`, 201, "x/api"},
		{"no repository named", e.project, `{"title":"t"}`, 201, "x/api"},
		{"a repository on a project without one", bare, `{"title":"t","repo":"y/lib"}`, 400, ""},
		{"no repository on a project without one", bare, `{"title":"t"}`, 201, ""},
	}
	for _, c := range cases {
		// Act
		resp := e.do("POST", "/api/projects/"+c.project+"/tasks", "bob", c.body)
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)

		// Assert
		if resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d: %v", c.name, resp.StatusCode, c.want, got)
			continue
		}
		if c.want == 400 && cnErrCode(got) != "invalid_repo" {
			t.Errorf("%s: code %q, want invalid_repo", c.name, cnErrCode(got))
		}
		if c.want == 201 && (got["repo"] == nil && c.repo != "" || got["repo"] != nil && got["repo"] != c.repo) {
			t.Errorf("%s: task repo = %v, want %q", c.name, got["repo"], c.repo)
		}
	}
	created := 0
	for _, c := range cases {
		if c.want == 201 {
			created++
		}
	}
	if got := len(e.tasksOf(e.project)) + len(e.tasksOf(bare)); got != created {
		t.Errorf("tasks stored = %d, want %d: a refused request created a task", got, created)
	}
}

func TestFactoryOnlyAnOwnerMayAllowAutomaticMerging(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	e.repoProject(t)

	// Act
	refused := e.do("POST", e.tasks(""), "bob", `{"title":"t","autoMerge":true}`)
	var body map[string]any
	_ = json.NewDecoder(refused.Body).Decode(&body)
	afterRefusal := len(e.tasksOf(e.project))
	member := e.json("POST", e.tasks(""), "bob", `{"title":"t","autoMerge":false}`, 201)
	owner := e.json("POST", e.tasks(""), "alice", `{"title":"t","autoMerge":true}`, 201)

	// Assert
	if refused.StatusCode != 403 || cnErrCode(body) != "forbidden" {
		t.Errorf("member with autoMerge: %d %v, want 403 forbidden", refused.StatusCode, body)
	}
	if afterRefusal != 0 {
		t.Errorf("the refused request created %d task(s)", afterRefusal)
	}
	if member["autoMerge"] == true {
		t.Errorf("a member's task has autoMerge: %v", member)
	}
	if owner["autoMerge"] != true {
		t.Errorf("an owner's task lost autoMerge: %v", owner)
	}
}

func TestFactoryTriggersAreBoundToTheProjectsRepository(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	e.repoProject(t)
	bare := e.newProject("bare", "", false)
	hook := func(template, match string) string {
		return `{"name":"n","kind":"webhook","event":"issues.labeled","match":` + match + `,"template":` + template + `}`
	}
	cases := []struct {
		name, project, body, code string
		want                      int
	}{
		{"another repository in the template", e.project, hook(`{"title":"t","repo":"y/lib"}`, `{"label":"agent"}`), "invalid_repo", 400},
		{"a template in the repository", e.project, hook(`{"title":"t","repo":"{{.Repo}}"}`, `{"label":"agent"}`), "invalid_repo", 400},
		{"another repository on a cron trigger", e.project, `{"name":"n","kind":"cron","schedule":"@every 1h","template":{"title":"t","repo":"y/lib"}}`, "invalid_repo", 400},
		{"a webhook trigger without a repository", bare, hook(`{"title":"t"}`, `{"label":"agent"}`), "invalid_trigger", 400},
		{"a repository named on a project without one", bare, `{"name":"n","kind":"cron","schedule":"@every 1h","template":{"title":"t","repo":"y/lib"}}`, "invalid_repo", 400},
		{"a cron trigger without a repository", bare, `{"name":"n","kind":"cron","schedule":"@every 1h","template":{"title":"t"}}`, "", 201},
	}
	for _, c := range cases {
		// Act
		resp := e.do("POST", "/api/projects/"+c.project+"/triggers", "alice", c.body)
		var got map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&got)

		// Assert
		if resp.StatusCode != c.want || c.code != "" && cnErrCode(got) != c.code {
			t.Errorf("%s: %d %v, want %d %s", c.name, resp.StatusCode, got, c.want, c.code)
		}
	}
	if got := e.list("/api/projects/"+e.project+"/triggers", "alice", 200); len(got) != 0 {
		t.Errorf("a rejected trigger was stored: %v", got)
	}

	// Act: the caller's own repository, and a "repo" matcher that tries to widen or redirect the trigger.
	own := e.json("POST", "/api/projects/"+e.project+"/triggers", "alice",
		hook(`{"title":"t","repo":"X/Api"}`, `{"label":"agent","repo":"y/lib"}`), 201)
	cron := e.json("POST", "/api/projects/"+e.project+"/triggers", "alice",
		`{"name":"c","kind":"cron","schedule":"@every 1h","template":{"title":"t"}}`, 201)

	// Assert
	match, _ := own["match"].(map[string]any)
	tpl, _ := own["template"].(map[string]any)
	if match["repo"] != "x/api" || match["label"] != "agent" || tpl["repo"] != "x/api" {
		t.Errorf("webhook trigger: match %v template %v, want repo forced to x/api and label kept", match, tpl)
	}
	if ctpl, _ := cron["template"].(map[string]any); ctpl["repo"] != "x/api" {
		t.Errorf("cron template = %v, want repo x/api", cron["template"])
	}
	stored, err := e.st.Store.GetTrigger(e.ctx, own["id"].(string))
	if err != nil || stored.Match["repo"] != "x/api" {
		t.Errorf("stored trigger = %+v (%v), want Match[repo] x/api", stored, err)
	}
}

func TestFactoryWebhookEventsReachOnlyTheProjectOfTheirRepository(t *testing.T) {
	// Arrange: two projects on different repositories, each with the same "agent" label trigger.
	const secret = "whsec"
	e := newFxEnv(t, fxOpts{secret: secret})
	a := e.newProject("a", "https://github.com/acme/alpha", false)
	b := e.newProject("b", "https://github.com/acme/beta.git", false)
	for _, p := range []string{a, b} {
		e.json("POST", "/api/projects/"+p+"/triggers", "alice",
			`{"name":"labelled","kind":"webhook","event":"issues.labeled","match":{"label":"agent"},"template":{"title":"Fix #{{.number}} in {{.Repo}}"}}`, 201)
	}
	issue := func(repo string, n int) string {
		return fmt.Sprintf(`{"action":"labeled","label":{"name":"agent"},"repository":{"full_name":%q},"sender":{"login":"eve"},`+
			`"issue":{"number":%d,"title":"t","body":"b","html_url":"https://github.com/%s/issues/%d","labels":[{"name":"agent"}]}}`, repo, n, repo, n)
	}
	deliver := func(delivery, body string) int {
		return e.do("POST", "/hooks/github", "", body, "X-GitHub-Event", "issues", "X-GitHub-Delivery", delivery,
			"X-Hub-Signature-256", sign(secret, body)).StatusCode
	}
	titles := func(project string) []string {
		var out []string
		for _, tk := range e.tasksOf(project) {
			out = append(out, fmt.Sprint(tk["title"], "|", tk["repo"]))
		}
		return out
	}

	// Act: an event of B's repository, then one of A's, then one of a repository the App serves for somebody else.
	forB := deliver("d-b", issue("acme/beta", 1))
	afterB := [2][]string{titles(a), titles(b)}
	forA := deliver("d-a", issue("acme/alpha", 2))
	foreign := deliver("d-x", issue("stranger/secrets", 3))

	// Assert
	if forB != 202 || forA != 202 || foreign != 202 {
		t.Fatalf("deliveries: %d %d %d, want 202", forB, forA, foreign)
	}
	if len(afterB[0]) != 0 || len(afterB[1]) != 1 || afterB[1][0] != "Fix #1 in acme/beta|acme/beta" {
		t.Errorf("after B's event: A %v, B %v; want only B's task, on acme/beta", afterB[0], afterB[1])
	}
	if got := titles(a); len(got) != 1 || got[0] != "Fix #2 in acme/alpha|acme/alpha" {
		t.Errorf("A's tasks = %v, want only the one for acme/alpha", got)
	}
	if got := titles(b); len(got) != 1 {
		t.Errorf("B's tasks = %v, want only the one for acme/beta", got)
	}
}

// ---- budgets are chosen by whoever creates the task ----

// budgetTask creates a task as bob with the given budget JSON, books spend on it and blocks it like the dispatcher
// does when the cap is reached (spent: $1.20, 40000 tokens).
func (e *fxEnv) budgetTask(budget string) string {
	e.t.Helper()
	id := e.json("POST", e.tasks(""), "bob", `{"title":"capped","spec":"do it","budget":`+budget+`}`, 201)["id"].(string)
	if _, err := e.st.Store.AddSpend(e.ctx, id, factory.Spend{USD: 1.2, Tokens: 40000, Runs: 1}); err != nil {
		e.t.Fatal(err)
	}
	e.block(id, "budget exhausted")
	return id
}

func (e *fxEnv) taskOfID(id string) *factory.Task {
	e.t.Helper()
	tk, err := e.st.Store.GetTask(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

func TestFactoryCreateTaskStoresTheCreatorsBudgetAndNeverInventsOne(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	cases := []struct {
		name, body string
		want       factory.Budget
	}{
		{"both caps", `"budget":{"maxUsd":2.5,"maxTokens":1000}`, factory.Budget{MaxUSD: 2.5, MaxTokens: 1000}},
		{"dollars only", `"budget":{"maxUsd":3}`, factory.Budget{MaxUSD: 3}},
		{"tokens only", `"budget":{"maxTokens":500}`, factory.Budget{MaxTokens: 500}},
		{"explicit zeros", `"budget":{"maxUsd":0,"maxTokens":0}`, factory.Budget{}},
		{"omitted", `"title":"x"`, factory.Budget{}},
	}
	for _, c := range cases {
		// Act
		got := e.json("POST", e.tasks(""), "bob", `{"title":"t","spec":"s",`+c.body+`}`, 201)

		// Assert
		if stored := e.taskOfID(got["id"].(string)).Budget; stored != c.want {
			t.Errorf("%s: stored budget %+v, want %+v", c.name, stored, c.want)
		}
		returned, has := got["budget"].(map[string]any)
		if c.want == (factory.Budget{}) && has && len(returned) != 0 {
			t.Errorf("%s: an uncapped task came back with budget %v", c.name, returned)
		}
		if c.want.MaxUSD > 0 && returned["maxUsd"] != c.want.MaxUSD {
			t.Errorf("%s: returned budget %v", c.name, returned)
		}
	}
}

func TestFactoryRejectsANegativeBudgetOnTasksAndTriggerTemplates(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	triggers := "/api/projects/" + e.project + "/triggers"
	cases := []struct{ name, budget, field string }{
		{"negative dollars", `{"maxUsd":-1}`, "budget.maxUsd must be a number >= 0"},
		{"negative tokens", `{"maxTokens":-5}`, "budget.maxTokens must be a number >= 0"},
		{"negative dollars beside valid tokens", `{"maxUsd":-0.01,"maxTokens":10}`, "budget.maxUsd must be a number >= 0"},
	}
	for _, c := range cases {
		// Act
		task := e.json("POST", e.tasks(""), "bob", `{"title":"t","budget":`+c.budget+`}`, 400)
		trigger := e.json("POST", triggers, "alice", `{"name":"n","kind":"cron","schedule":"@every 1h","template":{"title":"t","budget":`+c.budget+`}}`, 400)

		// Assert
		for what, got := range map[string]map[string]any{"task": task, "trigger": trigger} {
			msg, _ := got["error"].(map[string]any)["message"].(string)
			if cnErrCode(got) != "invalid_budget" || !strings.Contains(msg, c.field) {
				t.Errorf("%s %s: %v", c.name, what, got)
			}
		}
	}
	if got := e.list(e.tasks(""), "alice", 200); len(got) != 0 {
		t.Errorf("a rejected task was stored: %v", got)
	}
	if got := e.list(triggers, "alice", 200); len(got) != 0 {
		t.Errorf("a rejected trigger was stored: %v", got)
	}
}

func TestFactoryTriggerTemplateKeepsItsBudgetAndDefaultsToNone(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	triggers := "/api/projects/" + e.project + "/triggers"

	// Act
	capped := e.json("POST", triggers, "alice", `{"name":"a","kind":"cron","schedule":"@every 1h","template":{"title":"t","budget":{"maxUsd":4,"maxTokens":9000}}}`, 201)
	open := e.json("POST", triggers, "alice", `{"name":"b","kind":"cron","schedule":"@every 1h","template":{"title":"t"}}`, 201)

	// Assert
	tpl := func(tr map[string]any) factory.Budget {
		b, _ := tr["template"].(map[string]any)["budget"].(map[string]any)
		usd, _ := b["maxUsd"].(float64)
		tokens, _ := b["maxTokens"].(float64)
		return factory.Budget{MaxUSD: usd, MaxTokens: int64(tokens)}
	}
	if got := tpl(capped); got != (factory.Budget{MaxUSD: 4, MaxTokens: 9000}) {
		t.Errorf("capped template budget = %+v", got)
	}
	if got := tpl(open); got != (factory.Budget{}) {
		t.Errorf("a template without a budget got %+v", got)
	}
}

func TestValidBudgetAcceptsZeroAndFiniteNonNegativeCaps(t *testing.T) {
	// Arrange
	cases := []struct {
		name string
		b    factory.Budget
		ok   bool
	}{
		{"none", factory.Budget{}, true},
		{"both", factory.Budget{MaxUSD: 1.5, MaxTokens: 1}, true},
		{"negative usd", factory.Budget{MaxUSD: -1}, false},
		{"negative tokens", factory.Budget{MaxTokens: -1}, false},
		{"NaN", factory.Budget{MaxUSD: math.NaN()}, false},
		{"+Inf", factory.Budget{MaxUSD: math.Inf(1)}, false},
		{"-Inf", factory.Budget{MaxUSD: math.Inf(-1)}, false},
	}
	for _, c := range cases {
		// Act
		err := validBudget(c.b)

		// Assert
		if (err == nil) != c.ok {
			t.Errorf("%s: validBudget(%+v) = %v", c.name, c.b, err)
		}
	}
}

func TestFactoryResumeBudgetIsChangedByTheCreatorOrAProjectOwnerOnly(t *testing.T) {
	// Arrange: dave is a plain member, who neither created the tasks (bob did) nor owns the project (alice does).
	e := newFxEnv(t, fxOpts{})
	e.json("PUT", "/api/projects/"+e.project+"/members/dave", "alice", `{"role":"member"}`, 204)
	old := factory.Budget{MaxUSD: 1, MaxTokens: 50000}
	raised := `{"maxUsd":5,"maxTokens":100000}`
	cases := []struct {
		name, user, body string
		want             int
		budget           factory.Budget // after the request
		state            factory.State
	}{
		{"the creator raises it", "bob", `{"budget":` + raised + `}`, 202, factory.Budget{MaxUSD: 5, MaxTokens: 100000}, factory.StateReady},
		{"a project owner raises it", "alice", `{"budget":` + raised + `}`, 202, factory.Budget{MaxUSD: 5, MaxTokens: 100000}, factory.StateReady},
		{"another member may not", "dave", `{"budget":` + raised + `}`, 403, old, factory.StateBlocked},
		{"a viewer may not", "carol", `{"budget":` + raised + `}`, 403, old, factory.StateBlocked},
		{"zero removes the caps", "bob", `{"budget":{}}`, 202, factory.Budget{}, factory.StateReady},
		{"zero on one axis removes only that cap", "bob", `{"budget":{"maxUsd":5}}`, 202, factory.Budget{MaxUSD: 5}, factory.StateReady},
		{"feedback alone leaves the budget", "bob", `{"feedback":"go on"}`, 202, old, factory.StateReady},
		{"another member may resume without touching it", "dave", `{"feedback":"go on"}`, 202, old, factory.StateReady},
		{"dollars at the amount spent", "bob", `{"budget":{"maxUsd":1.2}}`, 400, old, factory.StateBlocked},
		{"dollars below the amount spent", "bob", `{"budget":{"maxUsd":0.5}}`, 400, old, factory.StateBlocked},
		{"tokens at the amount spent", "bob", `{"budget":{"maxTokens":40000}}`, 400, old, factory.StateBlocked},
		{"tokens below the amount spent", "alice", `{"budget":{"maxUsd":9,"maxTokens":39999}}`, 400, old, factory.StateBlocked},
		{"negative dollars", "bob", `{"budget":{"maxUsd":-1}}`, 400, old, factory.StateBlocked},
		{"negative tokens", "bob", `{"budget":{"maxTokens":-1}}`, 400, old, factory.StateBlocked},
	}
	for _, c := range cases {
		id := e.budgetTask(`{"maxUsd":1,"maxTokens":50000}`)

		// Act
		got := e.json("POST", e.tasks("/"+id+"/resume"), c.user, c.body, c.want)

		// Assert
		tk := e.taskOfID(id)
		if tk.Budget != c.budget || tk.State != c.state {
			t.Errorf("%s: budget %+v state %s, want %+v %s", c.name, tk.Budget, tk.State, c.budget, c.state)
		}
		code := map[int]string{400: "invalid_budget", 403: "forbidden"}[c.want]
		if cnErrCode(got) != code {
			t.Errorf("%s: code %q, want %q", c.name, cnErrCode(got), code)
		}
	}
}

func TestFactoryResumeRefusesABudgetTheTaskAlreadySpentAndSaysWhy(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	id := e.budgetTask(`{"maxUsd":1}`)

	// Act
	got := e.json("POST", e.tasks("/"+id+"/resume"), "bob", `{"budget":{"maxUsd":1}}`, 400)

	// Assert
	msg, _ := got["error"].(map[string]any)["message"].(string)
	if want := "the task already spent $1.20 and 40000 tokens; set a higher cap, or 0 to remove it"; !strings.Contains(msg, want) {
		t.Errorf("message %q does not say %q", msg, want)
	}
}

func TestFactoryResumeWithABudgetRecordsWhoChangedItAndFrom(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	raise := e.budgetTask(`{"maxUsd":1,"maxTokens":50000}`)
	remove := e.budgetTask(`{"maxUsd":1}`)
	refused := e.budgetTask(`{"maxUsd":1}`)

	// Act
	e.json("POST", e.tasks("/"+raise+"/resume"), "alice", `{"budget":{"maxUsd":5,"maxTokens":100000}}`, 202)
	e.json("POST", e.tasks("/"+remove+"/resume"), "bob", `{"budget":{}}`, 202)
	e.json("POST", e.tasks("/"+refused+"/resume"), "bob", `{"budget":{"maxUsd":0.1}}`, 400)

	// Assert
	texts := func(id string) []string {
		var out []string
		evs, err := e.f.Tasks.Events(e.ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Kind == factory.EventBudget {
				out = append(out, ev.Text)
			}
		}
		return out
	}
	if got := texts(raise); len(got) != 1 || got[0] != "budget changed by alice: $1.00, 50000 tokens -> $5.00, 100000 tokens" {
		t.Errorf("raise events = %q", got)
	}
	if got := texts(remove); len(got) != 1 || got[0] != "budget changed by bob: $1.00, no token cap -> no $ cap, no token cap" {
		t.Errorf("remove events = %q", got)
	}
	if got := texts(refused); len(got) != 0 {
		t.Errorf("a refused change was recorded: %q", got)
	}
}

func TestFactoryTaskBlockedOnItsBudgetRunsAgainOnlyAfterTheCapIsRaised(t *testing.T) {
	// Arrange: two tasks blocked for budget exhaustion with the same spend; the dispatcher refuses an over-budget one.
	e := newFxEnv(t, fxOpts{})
	raised := e.budgetTask(`{"maxUsd":1}`)
	untouched := e.budgetTask(`{"maxUsd":1}`)

	// Act
	e.json("POST", e.tasks("/"+raised+"/resume"), "bob", `{"budget":{"maxUsd":5}}`, 202)
	e.json("POST", e.tasks("/"+untouched+"/resume"), "bob", `{}`, 202)
	started, err := e.f.Tasks.Dispatch(e.ctx)

	// Assert: only the one with room under its new cap started; the other went straight back to blocked.
	if err != nil {
		t.Fatal(err)
	}
	if started != 1 {
		t.Errorf("dispatch started %d tasks, want 1", started)
	}
	if got := e.stateOf(untouched); got != factory.StateBlocked {
		t.Errorf("the task resumed without a higher cap is %s, want blocked again", got)
	}
	if got := e.taskOfID(raised).Attempts; got != 1 {
		t.Errorf("the task with the raised cap has %d attempts, want it started once", got)
	}
	if got := e.taskOfID(untouched).Attempts; got != 0 {
		t.Errorf("the task still over its cap started anyway (%d attempts)", got)
	}
}

func TestFactoryResumeBudgetFailuresAreReportedAndNeverStrandTheTask(t *testing.T) {
	// Arrange
	e := newFxEnv(t, fxOpts{})
	body := `{"budget":{"maxUsd":5}}`
	conflict := e.budgetTask(`{"maxUsd":1}`)
	broken := e.budgetTask(`{"maxUsd":1}`)
	noHistory := e.budgetTask(`{"maxUsd":1}`)
	ready := e.newTask("not blocked")

	// Act / Assert: the task left Blocked between the read and the update.
	e.st.failOn("UpdateTask", factory.ErrConflict)
	if got := cnErrCode(e.json("POST", e.tasks("/"+conflict+"/resume"), "bob", body, 409)); got != "conflict" {
		t.Errorf("conflict code %q", got)
	}
	e.st.failOn("UpdateTask", errCnBoom)
	e.json("POST", e.tasks("/"+broken+"/resume"), "bob", body, 500)
	e.st.failOn("UpdateTask", nil)
	if tk := e.taskOfID(broken); tk.Budget.MaxUSD != 1 || tk.State != factory.StateBlocked {
		t.Errorf("a failed update changed the task: %+v %s", tk.Budget, tk.State)
	}
	// A history line that cannot be written does not undo the stored change nor keep the task blocked.
	e.st.failOn("AppendEvent", errCnBoom)
	e.json("POST", e.tasks("/"+noHistory+"/resume"), "bob", body, 202)
	e.st.failOn("AppendEvent", nil)
	if tk := e.taskOfID(noHistory); tk.Budget.MaxUSD != 5 || tk.State != factory.StateReady {
		t.Errorf("after a lost history line: %+v %s", tk.Budget, tk.State)
	}
	// A task that is not blocked is a conflict whoever asks, and its budget stays.
	for _, user := range []string{"bob", "alice"} {
		e.json("POST", e.tasks("/"+ready+"/resume"), user, body, 409)
	}
	if tk := e.taskOfID(ready); tk.Budget != (factory.Budget{}) {
		t.Errorf("a refused resume changed the budget to %+v", tk.Budget)
	}
}
