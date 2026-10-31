package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// rig is a harness behind the web Handler, with a scripted model.
type rig struct {
	t   *testing.T
	srv *httptest.Server
	h   *harness.Harness
	ctx context.Context
}

type rigOpts struct {
	web       httpapi.Options // Authenticate/User default to the X-User header
	model     *llmtest.Model
	store     harness.Store
	env       map[string]string
	local     bool // local sandbox backend instead of the in-memory fake
	noSandbox bool // no backend at all
	rawAuth   bool // keep the web options' authentication (default: the X-User header)
}

// scripted asks for one tool call, then answers "done" once it has seen the tool result.
func scripted(tool, args string) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 { // title and other utility calls
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", tool, args), "tool_calls")}}
	})
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	if o.local {
		t.Setenv("SANDBOX_BACKEND", "local")
		t.Setenv("LOCAL_WORKSPACE", t.TempDir())
		t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	}
	for k, v := range o.env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if o.model == nil {
		o.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("Hi there")}, llmtest.Turn{Chunks: llmtest.Text("Title")})
	}
	ho := harness.Options{Config: cfg, Models: llmtest.Factory{M: o.model}, Store: o.store}
	if !o.local && !o.noSandbox {
		ho.Backend = sandboxtest.NewBackend()
	}
	h, err := harness.New(ho)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	go func() { _ = h.Run(ctx) }()
	w := o.web
	if !o.rawAuth && w.Authenticate == nil && w.User == nil && w.Token == "" {
		w.Authenticate = func(r *http.Request) (string, error) { return r.Header.Get("X-User"), nil }
	}
	w.DefaultModel = "gpt-test"
	w.Models = cfg.LLMModels
	srv := httptest.NewServer(httpapi.New(h, w))
	t.Cleanup(func() { srv.Close(); cancel() })
	return &rig{t: t, srv: srv, h: h, ctx: ctx}
}

// do sends a request as user (X-User) and returns the response.
func (r *rig) do(method, path, user, body string, hdr ...string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, strings.NewReader(body))
	if user != "" {
		req.Header.Set("X-User", user)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// json performs a request, checks the status and decodes the body into a map.
func (r *rig) json(method, path, user, body string, want int) map[string]any {
	r.t.Helper()
	resp := r.do(method, path, user, body)
	var m map[string]any
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &m)
	if resp.StatusCode != want {
		r.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, b)
	}
	return m
}

func (r *rig) newThread(user string) string {
	r.t.Helper()
	return r.json("POST", "/api/threads", user, `{}`, 201)["id"].(string)
}

func (r *rig) startRun(user, thread, msg string) string {
	r.t.Helper()
	return r.json("POST", "/api/threads/"+thread+"/runs", user, `{"message":`+strconv.Quote(msg)+`}`, 202)["id"].(string)
}

type sse struct{ ID, Data string }

// stream reads a run's event stream until the server closes it.
func (r *rig) stream(runID, user string, hdr ...string) []sse {
	r.t.Helper()
	return readSSE(r.t, r.do("GET", "/api/runs/"+runID+"/events", user, "", hdr...))
}

func readSSE(t *testing.T, resp *http.Response) []sse {
	t.Helper()
	if resp.StatusCode != 200 {
		t.Fatalf("events status %d", resp.StatusCode)
	}
	timer := time.AfterFunc(15*time.Second, func() { resp.Body.Close() })
	defer timer.Stop()
	var out []sse
	var cur sse
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = line[4:]
		case strings.HasPrefix(line, "data: "):
			cur.Data = line[6:]
			out = append(out, cur)
			cur = sse{}
		}
	}
	return out
}

func eventType(e sse) string {
	var m struct{ Type string }
	_ = json.Unmarshal([]byte(e.Data), &m)
	return m.Type
}

// finished returns the RUN_FINISHED payload of a stream.
func finished(t *testing.T, evs []sse) map[string]any {
	t.Helper()
	if len(evs) == 0 || eventType(evs[len(evs)-1]) != "RUN_FINISHED" {
		t.Fatalf("stream did not end with RUN_FINISHED: %d events", len(evs))
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(evs[len(evs)-1].Data), &m)
	return m
}

func (r *rig) waitIdle(thread, user string) {
	r.t.Helper()
	for i := 0; i < 200; i++ {
		if r.json("GET", "/api/threads/"+thread, user, "", 200)["activeRun"] == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	r.t.Fatal("thread still has an active run")
}

func TestThreadsCanBeCreatedListedReadWithMessagesRenamedAndDeleted(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "hello")
	finished(t, r.stream(run, "alice"))
	r.waitIdle(id, "alice")

	// Act
	list := r.do("GET", "/api/threads", "alice", "")
	var threads []map[string]any
	_ = json.NewDecoder(list.Body).Decode(&threads)
	got := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	renamed := r.json("PATCH", "/api/threads/"+id, "alice", `{"title":"  My chat "}`, 200)
	del := r.do("DELETE", "/api/threads/"+id, "alice", "")
	after := r.do("GET", "/api/threads/"+id, "alice", "")

	// Assert
	if len(threads) != 1 || threads[0]["id"] != id {
		t.Fatalf("list = %v", threads)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) < 2 || msgs[0].(map[string]any)["role"] != "user" || msgs[1].(map[string]any)["content"] != "Hi there" {
		t.Fatalf("messages = %v", msgs)
	}
	if renamed["title"] != "My chat" {
		t.Fatalf("renamed = %v", renamed)
	}
	if del.StatusCode != 204 || after.StatusCode != 404 {
		t.Fatalf("delete %d, get after delete %d", del.StatusCode, after.StatusCode)
	}
}

func TestLegacyUnprefixedRoutesStillWork(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.json("POST", "/threads", "alice", `{}`, 201)["id"].(string)
	run := r.json("POST", "/threads/"+id+"/runs", "alice", `{"message":"hi"}`, 202)["id"].(string)
	finished(t, r.stream(run, "alice"))
	if resp := r.do("GET", "/threads", "alice", ""); resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestEveryRouteAnswersNotFoundForAnotherUsersIds(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "hello")
	finished(t, r.stream(run, "alice"))
	r.waitIdle(id, "alice")
	cases := []struct{ method, path, body string }{
		{"GET", "/api/threads/" + id, ""},
		{"PATCH", "/api/threads/" + id, `{"title":"x"}`},
		{"DELETE", "/api/threads/" + id, ""},
		{"POST", "/api/threads/" + id + "/runs", `{"message":"hi"}`},
		{"POST", "/api/threads/" + id + "/resume", `{"interruptId":"x","decision":"approve"}`},
		{"GET", "/api/threads/" + id + "/files", ""},
		{"GET", "/api/threads/" + id + "/files/download?path=/workspace/a.txt", ""},
		{"GET", "/api/runs/" + run, ""},
		{"GET", "/api/runs/" + run + "/events", ""},
		{"POST", "/api/runs/" + run + "/cancel", ""},
		{"POST", "/api/runs/" + run + "/steer", `{"text":"x"}`},
	}

	for _, c := range cases {
		// Act
		resp := r.do(c.method, c.path, "mallory", c.body)

		// Assert
		if resp.StatusCode != 404 {
			t.Errorf("%s %s as mallory: status %d, want 404", c.method, c.path, resp.StatusCode)
		}
	}
	if r.do("GET", "/api/threads/"+id, "alice", "").StatusCode != 200 {
		t.Fatal("the owner lost the thread")
	}
	var mine []any
	_ = json.NewDecoder(r.do("GET", "/api/threads", "mallory", "").Body).Decode(&mine)
	if len(mine) != 0 {
		t.Fatalf("mallory's list = %v", mine)
	}
}

func TestInvalidRequestsAreRefused(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/api/threads/" + id + "/runs", `{"message":"  "}`, 400},
		{"POST", "/api/threads/" + id + "/runs", `{not json`, 400},
		{"POST", "/api/threads/" + id + "/runs", `{"message":"hi","model":"nope"}`, 400},
		{"PATCH", "/api/threads/" + id, `{}`, 400},
		{"PATCH", "/api/threads/" + id, `{"title":"   "}`, 400},
		{"POST", "/api/threads/" + id + "/resume", `{}`, 400},
		{"POST", "/api/threads/" + id + "/resume", `{"interruptId":"x","decision":"maybe"}`, 400},
		{"POST", "/api/threads/" + id + "/resume", `{"interruptId":"x","reason":"ask_user"}`, 400},
		{"GET", "/api/threads/nope", ``, 404},
		{"GET", "/api/runs/nope/events", ``, 404},
	} {
		if resp := r.do(c.method, c.path, "alice", c.body); resp.StatusCode != c.want {
			t.Errorf("%s %s %s: status %d, want %d", c.method, c.path, c.body, resp.StatusCode, c.want)
		}
	}
}

func TestSteeringAFinishedRunConflictsAndCancelIsAccepted(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "hello")
	finished(t, r.stream(run, "alice"))
	r.waitIdle(id, "alice")

	steer := r.do("POST", "/api/runs/"+run+"/steer", "alice", `{"text":"more"}`)
	empty := r.do("POST", "/api/runs/"+run+"/steer", "alice", `{"text":""}`)
	cancel := r.do("POST", "/api/runs/"+run+"/cancel", "alice", "")
	state := r.json("GET", "/api/runs/"+run, "alice", "", 200)

	if steer.StatusCode != 409 || empty.StatusCode != 400 || cancel.StatusCode != 202 {
		t.Fatalf("steer %d, empty steer %d, cancel %d", steer.StatusCode, empty.StatusCode, cancel.StatusCode)
	}
	if state["status"] != "completed" || state["threadId"] != id {
		t.Fatalf("run = %v", state)
	}
}

func TestEventsResumeAfterLastEventIDAndAfterParameter(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "hello")
	all := r.stream(run, "alice")
	finished(t, all)
	if len(all) < 4 {
		t.Fatalf("only %d events", len(all))
	}
	cut := all[len(all)/2].ID

	// Act
	byHeader := r.stream(run, "alice", "Last-Event-ID", cut)
	byQuery := readSSE(t, r.do("GET", "/api/runs/"+run+"/events?after="+cut, "alice", ""))

	// Assert: exactly the events after the cut, in order, without duplicates.
	n, _ := strconv.Atoi(cut)
	want := all[n:]
	for name, got := range map[string][]sse{"Last-Event-ID": byHeader, "after": byQuery} {
		if len(got) != len(want) {
			t.Fatalf("%s: %d events, want %d", name, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: event %d = %v, want %v", name, i, got[i], want[i])
			}
		}
	}
}

func TestModelsAndHealth(t *testing.T) {
	r := newRig(t, rigOpts{web: httpapi.Options{Token: "s3cret"}})

	health := r.do("GET", "/healthz", "", "")
	models := r.do("GET", "/api/models", "", "", "Authorization", "Bearer s3cret")
	var m struct {
		Models  []string
		Default string
	}
	_ = json.NewDecoder(models.Body).Decode(&m)

	if health.StatusCode != 200 {
		t.Fatalf("healthz %d without a token", health.StatusCode)
	}
	if !slices.Contains(m.Models, "gpt-test") || m.Default != "gpt-test" {
		t.Fatalf("models = %+v", m)
	}
}

func TestSessionsListComesFromTheDurableStoreOnly(t *testing.T) {
	// Arrange
	mem := newRig(t, rigOpts{})
	st, err := harness.OpenLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	durable := newRig(t, rigOpts{store: st})
	id := durable.newThread("alice")
	finished(t, durable.stream(durable.startRun("alice", id, "remember me"), "alice"))

	// Act
	unsupported := mem.do("GET", "/api/sessions", "alice", "")
	resp := durable.do("GET", "/api/sessions", "alice", "")
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	foreign := durable.do("GET", "/api/sessions", "mallory", "")
	var none []any
	_ = json.NewDecoder(foreign.Body).Decode(&none)

	// Assert
	if unsupported.StatusCode != 501 {
		t.Fatalf("memory store: status %d", unsupported.StatusCode)
	}
	if len(list) != 1 || list[0]["id"] != id || !strings.Contains(list[0]["preview"].(string), "Hi there") && !strings.Contains(list[0]["preview"].(string), "remember") {
		t.Fatalf("sessions = %v", list)
	}
	if len(none) != 0 {
		t.Fatalf("mallory sees %v", none)
	}
}

func TestSessionsReportThePinnedFlag(t *testing.T) {
	// Arrange
	st, err := harness.OpenLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := newRig(t, rigOpts{store: st})
	id := r.newThread("alice")
	finished(t, r.stream(r.startRun("alice", id, "pin me"), "alice"))
	r.json("PATCH", "/api/threads/"+id, "alice", `{"pinned":true}`, 200)

	// Act
	resp := r.do("GET", "/api/sessions", "alice", "")
	var list []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&list)

	// Assert
	if len(list) != 1 || list[0]["pinned"] != true {
		t.Fatalf("sessions = %v", list)
	}
}

func TestFilesAreListedAndDownloadedReadOnlyInsideTheWorkspace(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	sb, err := r.h.Sandboxes.Acquire(r.ctx, id, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(r.ctx, "/workspace/outputs/report.txt", []byte("the report")); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(r.ctx, "/workspace/we\"ird;name.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := sb.WriteFile(r.ctx, "/outside.txt", []byte("secret")); err != nil {
		t.Fatal(err)
	}

	// Act
	root := r.do("GET", "/api/threads/"+id+"/files", "alice", "")
	var rootList []map[string]any
	_ = json.NewDecoder(root.Body).Decode(&rootList)
	sub := r.do("GET", "/api/threads/"+id+"/files?path=/workspace/outputs", "alice", "")
	var subList []map[string]any
	_ = json.NewDecoder(sub.Body).Decode(&subList)
	dl := r.do("GET", "/api/threads/"+id+"/files/download?path=/workspace/outputs/report.txt", "alice", "")
	body, _ := io.ReadAll(dl.Body)
	weird := r.do("GET", "/api/threads/"+id+"/files/download?path="+url.QueryEscape("we\"ird;name.txt"), "alice", "")

	// Assert
	names := map[string]string{}
	for _, f := range rootList {
		names[f["name"].(string)] = f["type"].(string)
	}
	if names["outputs"] != "directory" {
		t.Fatalf("root listing = %v", rootList)
	}
	if len(subList) != 1 || subList[0]["name"] != "report.txt" || subList[0]["path"] != "/workspace/outputs/report.txt" {
		t.Fatalf("sub listing = %v", subList)
	}
	if dl.StatusCode != 200 || string(body) != "the report" || !strings.HasPrefix(dl.Header.Get("Content-Disposition"), "attachment;") ||
		dl.Header.Get("Content-Type") != "application/octet-stream" || dl.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("download: %d %q %v", dl.StatusCode, body, dl.Header)
	}
	if cd := weird.Header.Get("Content-Disposition"); weird.StatusCode != 200 || strings.ContainsAny(cd, ";") && strings.Count(cd, ";") != 1 || strings.Contains(cd, `we"ird`) {
		t.Fatalf("unsafe disposition %q (status %d)", cd, weird.StatusCode)
	}
}

func TestFilesRefusePathsOutsideTheWorkspace(t *testing.T) {
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	sb, _ := r.h.Sandboxes.Acquire(r.ctx, id, "alice")
	_ = sb.WriteFile(r.ctx, "/etc/passwd", []byte("root"))
	_ = sb.WriteFile(r.ctx, "/workspace/dir/a.txt", []byte("a"))

	for _, p := range []string{"/etc/passwd", "../etc/passwd", "/workspace/../etc/passwd", "/workspace/%2e%2e/etc", "/workspacefoo/x", "/workspace\x00/a"} {
		for _, route := range []string{"files", "files/download"} {
			resp := r.do("GET", "/api/threads/"+id+"/"+route+"?path="+strings.ReplaceAll(p, "\x00", "%00"), "alice", "")
			if resp.StatusCode != 400 {
				t.Errorf("%s?path=%q: status %d, want 400", route, p, resp.StatusCode)
			}
		}
	}
	if resp := r.do("GET", "/api/threads/"+id+"/files/download?path=/workspace/dir", "alice", ""); resp.StatusCode != 400 {
		t.Errorf("directory download: status %d", resp.StatusCode)
	}
	if resp := r.do("GET", "/api/threads/"+id+"/files/download?path=/workspace/missing.txt", "alice", ""); resp.StatusCode != 404 {
		t.Errorf("missing file: status %d", resp.StatusCode)
	}
	// A relative path is taken under /workspace.
	if resp := r.do("GET", "/api/threads/"+id+"/files/download?path=dir/a.txt", "alice", ""); resp.StatusCode != 200 {
		t.Errorf("relative path: status %d", resp.StatusCode)
	}
}

func TestFilesNeedASandbox(t *testing.T) {
	r := newRig(t, rigOpts{})
	// A fresh thread has no sandbox yet: an empty listing, not an error.
	id := r.newThread("alice")
	resp := r.do("GET", "/api/threads/"+id+"/files", "alice", "")
	var list []any
	_ = json.NewDecoder(resp.Body).Decode(&list)
	if resp.StatusCode != 200 || len(list) != 0 {
		t.Fatalf("status %d list %v", resp.StatusCode, list)
	}
}

func TestAskUserAnswerResumesTheRunThroughTheAPI(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{model: scripted("ask_user", `{"question":"Which colour?","options":[{"label":"red"},{"label":"blue"}]}`)})
	id := r.newThread("alice")
	evs := r.stream(r.startRun("alice", id, "pick a colour"), "alice", "Accept", "text/event-stream")
	out := finished(t, evs)
	r.waitIdle(id, "alice")

	// Act
	outcome := out["outcome"].(map[string]any)
	intr := outcome["interrupts"].([]any)[0].(map[string]any)
	th := r.json("GET", "/api/threads/"+id, "alice", "", 200)
	resumed := r.json("POST", "/api/threads/"+id+"/resume", "alice",
		`{"interruptId":`+strconv.Quote(intr["id"].(string))+`,"reason":"ask_user","text":"blue"}`, 202)
	final := finished(t, r.stream(resumed["id"].(string), "alice"))

	// Assert
	if outcome["type"] != "interrupt" || intr["reason"] != "ask_user" {
		t.Fatalf("outcome = %v", outcome)
	}
	q, _ := th["pendingQuestion"].(map[string]any)
	if q["question"] != "Which colour?" || q["interruptId"] != intr["id"] {
		t.Fatalf("pendingQuestion = %v", th["pendingQuestion"])
	}
	if final["outcome"].(map[string]any)["type"] == "interrupt" {
		t.Fatalf("the resumed run is still waiting: %v", final)
	}
}
