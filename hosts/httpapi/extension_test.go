package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// testExt is a product extension: it records what the server asks of its policy and takes over a route when the
// request carries X-Takeover.
type testExt struct {
	setupErr, installErr error

	mu      sync.Mutex
	calls   []string
	details []map[string]any
	nResume string
}

func (e *testExt) note(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, s)
}

func (e *testExt) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.calls...)
}

func (e *testExt) Setup(o *httpapi.Options, mux *http.ServeMux) error {
	e.note("setup")
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, _ *http.Request) {
		httpapi.WriteJSON(w, 200, map[string]string{"login": "page"})
	})
	return e.setupErr
}

func (e *testExt) Install(s *httpapi.Server) error {
	e.note("install")
	s.Route("GET /product/limit", func(w http.ResponseWriter, r *http.Request, c httpapi.Caller) {
		httpapi.WriteJSON(w, 200, map[string]any{"limit": httpapi.LimitParam(r, 7), "as": c.Login})
	})
	s.Route("POST /product/echo", func(w http.ResponseWriter, r *http.Request, _ httpapi.Caller) {
		var body struct{ Text string }
		if httpapi.DecodeLimit(w, r, &body, 32) {
			httpapi.WriteJSON(w, 200, map[string]string{"text": body.Text})
		}
	})
	s.Route("POST /product/decode", func(w http.ResponseWriter, r *http.Request, _ httpapi.Caller) {
		var body struct{ Text string }
		if httpapi.Decode(w, r, &body) {
			httpapi.WriteJSON(w, 200, map[string]string{"text": body.Text})
		}
	})
	s.Route("GET /product/fail", func(w http.ResponseWriter, _ *http.Request, _ httpapi.Caller) {
		httpapi.Fail(w, errors.New("pq: password authentication failed for user admin"))
	})
	s.Route("GET /product/refuse", func(w http.ResponseWriter, _ *http.Request, _ httpapi.Caller) {
		httpapi.Fail(w, client.Errorf(client.ErrConflict, "already there"))
	})
	s.Route("GET /product/teapot", func(w http.ResponseWriter, _ *http.Request, _ httpapi.Caller) {
		httpapi.WriteError(w, http.StatusTeapot, "teapot", "short and stout")
	})
	return e.installErr
}

func (e *testExt) Resolve(r *http.Request, _ httpapi.ResourceKind, actor string) (httpapi.Caller, error) {
	if r.Header.Get("X-Deny") != "" {
		return httpapi.Caller{}, errors.New("not a member")
	}
	return httpapi.Caller{Login: actor, Actor: actor}, nil
}

func (e *testExt) FailAccess(w http.ResponseWriter, err error) {
	httpapi.WriteError(w, http.StatusTeapot, "denied", err.Error())
}

func (e *testExt) Author(c httpapi.Caller) harness.Author {
	return harness.Author{Login: "proxy-" + c.Actor, Kind: store.AuthorSupervisor, OnBehalfOf: c.Login}
}

func (e *testExt) takeover(name string, w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Takeover") == "" {
		return false
	}
	e.note(name)
	httpapi.WriteJSON(w, http.StatusAlreadyReported, map[string]string{"by": "extension", "route": name})
	return true
}

func (e *testExt) CreateThread(w http.ResponseWriter, r *http.Request, _ httpapi.Caller) bool {
	return e.takeover("create", w, r)
}
func (e *testExt) ListThreads(w http.ResponseWriter, r *http.Request, _ httpapi.Caller) bool {
	return e.takeover("list", w, r)
}
func (e *testExt) GetThread(w http.ResponseWriter, r *http.Request, _ httpapi.Caller) bool {
	return e.takeover("get", w, r)
}
func (e *testExt) DeleteThread(w http.ResponseWriter, r *http.Request, _ httpapi.Caller) bool {
	return e.takeover("delete", w, r)
}

func (e *testExt) BeforeRun(r *http.Request, threadID string, o *client.SendOptions) {
	e.note("before-run:" + threadID)
	if m := r.Header.Get("X-Force-Model"); m != "" {
		o.Model = m
	}
}

func (e *testExt) BeforeResume(_ *http.Request, _ httpapi.Caller, _ string, _ *client.ResumeRequest) string {
	e.note("before-resume")
	return "audited-tool"
}

func (e *testExt) AfterRun(c httpapi.Caller, run *client.Run, action string, detail map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, "after-run:"+action+":"+c.Login+":"+run.ThreadID)
	e.details = append(e.details, detail)
}

func (e *testExt) AfterRunAction(_ *http.Request, c httpapi.Caller, action string) {
	e.note("after-action:" + action + ":" + c.Login)
}

func (e *testExt) AfterFork(w http.ResponseWriter, r *http.Request, _ httpapi.Caller, th *client.Thread) bool {
	e.note("after-fork:" + th.ID)
	if r.Header.Get("X-Fork-Fails") != "" {
		httpapi.WriteError(w, http.StatusBadGateway, "fork_hook", "the product could not register the fork")
		return true
	}
	return false
}

func TestAnExtensionSeesEveryRouteAndRunsBeforeTheCoreOnes(t *testing.T) {
	// Arrange
	ext := &testExt{}
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: ext}})

	// Act
	login := r.json("GET", "/login", "", "", 200)
	product := r.json("GET", "/api/product/limit?limit=500", "alice", "", 200)
	legacy := r.json("GET", "/product/limit", "alice", "", 200)
	anon := r.do("GET", "/api/product/limit", "", "")

	// Assert
	if login["login"] != "page" {
		t.Fatalf("a route the extension mounted in Setup is lost: %v", login)
	}
	if product["limit"] != float64(200) || product["as"] != "alice" || legacy["limit"] != float64(7) {
		t.Fatalf("limit helper: %v / %v (at most 200, default when absent)", product, legacy)
	}
	if anon.StatusCode != 401 {
		t.Fatalf("an extension's authenticated route must still require authentication: %d", anon.StatusCode)
	}
	if got := ext.seen(); len(got) < 2 || got[0] != "setup" || got[1] != "install" {
		t.Fatalf("setup must run before install: %v", got)
	}
}

func TestAnExtensionErrorStopsTheServerFromStarting(t *testing.T) {
	// Arrange
	h := bareHarness(t)
	cases := []struct {
		name string
		ext  *testExt
		want string
	}{
		{"setup", &testExt{setupErr: errors.New("oidc discovery failed")}, "oidc discovery failed"},
		{"install", &testExt{installErr: errors.New("route clash")}, "route clash"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Act
			handler, err := httpapi.NewServer(h, httpapi.Options{Extension: c.ext})

			// Assert
			if err == nil || !strings.Contains(err.Error(), c.want) || handler != nil {
				t.Fatalf("handler %v err %v", handler, err)
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Error("New must panic on the error NewServer returns")
					}
				}()
				httpapi.New(h, httpapi.Options{Extension: c.ext})
			}()
		})
	}
}

func TestAnExtensionDecidesWhoMayReachAResource(t *testing.T) {
	// Arrange
	ext := &testExt{}
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: ext}})
	id := r.newThread("alice")

	// Act
	denied := r.do("GET", "/api/threads/"+id, "alice", "", "X-Deny", "1")
	allowed := r.do("GET", "/api/threads/"+id, "alice", "")

	// Assert
	body, _ := io.ReadAll(denied.Body)
	if denied.StatusCode != http.StatusTeapot || !strings.Contains(string(body), "not a member") {
		t.Fatalf("denied: %d %s", denied.StatusCode, body)
	}
	if allowed.StatusCode != 200 {
		t.Fatalf("allowed: %d", allowed.StatusCode)
	}
}

func TestAnExtensionCanTakeOverTheThreadRoutes(t *testing.T) {
	// Arrange
	ext := &testExt{}
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: ext}})
	id := r.newThread("alice")
	routes := []struct{ method, path, body, name string }{
		{"POST", "/api/threads", `{"title":"x"}`, "create"},
		{"GET", "/api/threads", "", "list"},
		{"GET", "/api/threads/" + id, "", "get"},
		{"DELETE", "/api/threads/" + id, "", "delete"},
	}

	for _, c := range routes {
		t.Run(c.name, func(t *testing.T) {
			// Act
			resp := r.do(c.method, c.path, "alice", c.body, "X-Takeover", "1")
			var got map[string]string
			_ = json.NewDecoder(resp.Body).Decode(&got)

			// Assert
			if resp.StatusCode != http.StatusAlreadyReported || got["by"] != "extension" || got["route"] != c.name {
				t.Fatalf("status %d response %v", resp.StatusCode, got)
			}
		})
	}
	if r.do("GET", "/api/threads/"+id, "alice", "").StatusCode != 200 {
		t.Fatal("the intercepted DELETE must not have deleted the thread")
	}
}

func TestAnExtensionIsToldAboutRunsAndRecordsWhoTheyWereSentBy(t *testing.T) {
	// Arrange
	ext := &testExt{}
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: ext}})
	id := r.newThread("alice")

	// Act
	refused := r.do("POST", "/api/threads/"+id+"/runs", "alice", `{"message":"hi"}`, "X-Force-Model", "not-allowed")
	run := r.json("POST", "/api/threads/"+id+"/runs", "alice", `{"message":"hello"}`, 202)["id"].(string)
	finished(t, r.stream(run, "alice"))
	r.waitIdle(id, "alice")
	cancel := r.do("POST", "/api/runs/"+run+"/cancel", "alice", "")

	// Assert
	if refused.StatusCode != 400 {
		t.Fatalf("BeforeRun's changes must reach the run request: %d", refused.StatusCode)
	}
	if cancel.StatusCode != 202 {
		t.Fatalf("cancel %d", cancel.StatusCode)
	}
	got := strings.Join(ext.seen(), " ")
	for _, want := range []string{"before-run:" + id, "after-run:run.send:alice:" + id, "after-action:run.cancel:alice"} {
		if !strings.Contains(got, want) {
			t.Errorf("extension calls %q lack %q", got, want)
		}
	}
	msgs, _ := r.h.Store.ListMessages(r.ctx, id)
	if len(msgs) == 0 || msgs[0].AuthorLogin != "proxy-alice" || msgs[0].AuthorKind != store.AuthorSupervisor || msgs[0].OnBehalfOf != "alice" {
		t.Fatalf("the message must carry the author the policy named: %+v", msgs)
	}
}

func TestAnExtensionIsToldAboutResumesWithTheToolTheyApprove(t *testing.T) {
	// Arrange
	ext := &testExt{}
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: ext},
		model: scripted("ask_user", `{"question":"Which colour?","options":[{"label":"red"},{"label":"blue"}]}`)})
	id := r.newThread("alice")
	out := finished(t, r.stream(r.startRun("alice", id, "pick a colour"), "alice"))
	r.waitIdle(id, "alice")
	intr := out["outcome"].(map[string]any)["interrupts"].([]any)[0].(map[string]any)

	// Act
	r.json("POST", "/api/threads/"+id+"/resume", "alice",
		`{"interruptId":`+strconv.Quote(intr["id"].(string))+`,"reason":"ask_user","text":"blue","always":true}`, 202)

	// Assert
	got := strings.Join(ext.seen(), " ")
	if !strings.Contains(got, "before-resume") || !strings.Contains(got, "after-run:run.resume:alice:"+id) {
		t.Fatalf("extension calls %q", got)
	}
	ext.mu.Lock()
	defer ext.mu.Unlock()
	d := ext.details[len(ext.details)-1]
	if d["tool"] != "audited-tool" || d["reason"] != "ask_user" || d["always"] != true {
		t.Fatalf("audit detail %v", d)
	}
}

func TestAnExtensionCanRefuseAForkItCannotRegister(t *testing.T) {
	// Arrange
	ext := &testExt{}
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: ext}})
	id := r.newThread("alice")
	finished(t, r.stream(r.startRun("alice", id, "hello"), "alice"))
	r.waitIdle(id, "alice")

	// Act
	refused := r.do("POST", "/api/threads/"+id+"/fork", "alice", `{}`, "X-Fork-Fails", "1")
	accepted := r.json("POST", "/api/threads/"+id+"/fork", "alice", `{}`, 201)

	// Assert
	if refused.StatusCode != http.StatusBadGateway {
		t.Fatalf("a fork the product refuses must answer with the product's error: %d", refused.StatusCode)
	}
	if accepted["id"] == id || accepted["title"] == nil {
		t.Fatalf("fork %v", accepted)
	}
}

func TestRouteHelpersAnswerWithTheAPIsErrorShape(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{web: httpapi.Options{Extension: &testExt{}}})

	// Act
	internal := r.do("GET", "/api/product/fail", "alice", "")
	client409 := r.do("GET", "/api/product/refuse", "alice", "")
	teapot := r.do("GET", "/api/product/teapot", "alice", "")
	tooBig := r.do("POST", "/api/product/echo", "alice", `{"text":"`+strings.Repeat("x", 100)+`"}`)
	small := r.do("POST", "/api/product/echo", "alice", `{"text":"ok"}`)
	broken := r.do("POST", "/api/product/decode", "alice", `{"text":`)
	empty := r.do("POST", "/api/product/decode", "alice", "")

	// Assert
	read := func(resp *http.Response) string { b, _ := io.ReadAll(resp.Body); return string(b) }
	if body := read(internal); internal.StatusCode != 500 || strings.Contains(body, "password") || !strings.Contains(body, `"code":"internal"`) {
		t.Fatalf("an unexpected error must be a generic 500 that hides its detail: %d %s", internal.StatusCode, body)
	}
	if body := read(client409); client409.StatusCode != 409 || !strings.Contains(body, "already there") {
		t.Fatalf("a client error keeps its status and message: %d %s", client409.StatusCode, body)
	}
	if body := read(teapot); teapot.StatusCode != http.StatusTeapot || !strings.Contains(body, `"code":"teapot"`) || !strings.Contains(body, "short and stout") {
		t.Fatalf("WriteError: %d %s", teapot.StatusCode, body)
	}
	if body := read(tooBig); tooBig.StatusCode != 400 || !strings.Contains(body, "invalid_request") {
		t.Fatalf("DecodeLimit must refuse a body over its limit: %d %s", tooBig.StatusCode, body)
	}
	if body := read(small); small.StatusCode != 200 || !strings.Contains(body, `"ok"`) {
		t.Fatalf("DecodeLimit within the limit: %d %s", small.StatusCode, body)
	}
	if broken.StatusCode != 400 || empty.StatusCode != 200 {
		t.Fatalf("Decode: broken JSON %d (want 400), empty body %d (want 200)", broken.StatusCode, empty.StatusCode)
	}
}

func TestMalformedBodiesAreRefusedBeforeAnythingIsChanged(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "hello")
	finished(t, r.stream(run, "alice"))
	r.waitIdle(id, "alice")
	cases := []struct{ method, path string }{
		{"POST", "/api/threads"},
		{"PATCH", "/api/threads/" + id},
		{"POST", "/api/threads/" + id + "/resume"},
		{"POST", "/api/runs/" + run + "/steer"},
		{"POST", "/api/threads/" + id + "/fork"},
		{"POST", "/api/commands/init/expand"},
	}

	for _, c := range cases {
		// Act
		resp := r.do(c.method, c.path, "alice", `{"x":`)

		// Assert
		if resp.StatusCode != 400 {
			t.Errorf("%s %s with broken JSON: status %d, want 400", c.method, c.path, resp.StatusCode)
		}
	}
	if th := r.json("GET", "/api/threads/"+id, "alice", "", 200); th["title"] == "" {
		t.Fatal("the thread was altered")
	}
}

func TestHistoryRoutesMapTheirOutcomesToStatusCodes(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{})
	id := r.newThread("alice")

	// Act / Assert: nothing to undo or redo yet is a conflict, not a server error
	if got := r.do("POST", "/api/threads/"+id+"/undo", "alice", ""); got.StatusCode != 409 {
		t.Fatalf("undo of an empty thread: %d", got.StatusCode)
	}
	if got := r.do("POST", "/api/threads/"+id+"/redo", "alice", ""); got.StatusCode != 409 {
		t.Fatalf("redo with nothing undone: %d", got.StatusCode)
	}
	finished(t, r.stream(r.startRun("alice", id, "hello"), "alice"))
	r.waitIdle(id, "alice")

	undo := r.json("POST", "/api/threads/"+id+"/undo", "alice", "", 200)
	if undo["prompt"] != "hello" || undo["canRedo"] != true {
		t.Fatalf("undo %v", undo)
	}
	redo := r.json("POST", "/api/threads/"+id+"/redo", "alice", "", 200)
	if redo["prompt"] != "hello" || redo["canRedo"] != false {
		t.Fatalf("redo %v", redo)
	}
	compact := r.json("POST", "/api/threads/"+id+"/compact", "alice", "", 200)
	if _, ok := compact["compacted"]; !ok {
		t.Fatalf("compact %v", compact)
	}
	for _, p := range []string{"undo", "redo", "compact", "fork"} {
		if got := r.do("POST", "/api/threads/"+id+"/"+p, "mallory", ""); got.StatusCode != 404 {
			t.Errorf("%s as another user: %d, want 404", p, got.StatusCode)
		}
	}
	if got := r.do("POST", "/api/threads/"+id+"/fork", "alice", `{"messageId":"nope"}`); got.StatusCode != 404 {
		t.Errorf("fork of an unknown message: %d, want 404", got.StatusCode)
	}
}

func TestIdleEventStreamsAreKeptAliveWithPings(t *testing.T) {
	// Arrange: the model never answers, so the stream has nothing to send but pings
	block := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text("..."), Block: true} })
	r := newRig(t, rigOpts{model: block, web: httpapi.Options{PingEvery: 10 * time.Millisecond}})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "hello")
	resp := r.do("GET", "/api/runs/"+run+"/events", "alice", "")

	// Act: read until the second ping
	pings := 0
	sc := bufio.NewScanner(resp.Body)
	timer := time.AfterFunc(10*time.Second, func() { resp.Body.Close() })
	defer timer.Stop()
	for sc.Scan() && pings < 2 {
		if sc.Text() == ": ping" {
			pings++
		}
	}

	// Assert
	if pings != 2 || resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("pings %d headers %v", pings, resp.Header)
	}
	if err := r.h.Cancel(r.ctx, "alice", run); err != nil {
		t.Fatal(err)
	}
}

// noFlush hides the Flusher of a ResponseWriter.
type noFlush struct{ http.ResponseWriter }

func bareHarness(t *testing.T) *harness.Harness {
	t.Helper()
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestEventsNeedAWriterThatCanStream(t *testing.T) {
	// Arrange
	h := bareHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.Run(ctx) }()
	th, _ := h.NewThread(ctx, "local", "")
	run, err := h.Send(ctx, "local", th.ID, "hi", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.New(h, httpapi.Options{NoUI: true})
	rec := httptest.NewRecorder()

	// Act
	handler.ServeHTTP(noFlush{rec}, httptest.NewRequest("GET", "/api/runs/"+run.ID+"/events", nil))

	// Assert
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "Streaming is not supported") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestTheUIIsLeftOutWhenAskedAndServedOtherwise(t *testing.T) {
	// Arrange
	h := bareHarness(t)

	// Act
	with := httptest.NewRecorder()
	httpapi.New(h, httpapi.Options{}).ServeHTTP(with, httptest.NewRequest("GET", "/", nil))
	without := httptest.NewRecorder()
	httpapi.New(h, httpapi.Options{NoUI: true}).ServeHTTP(without, httptest.NewRequest("GET", "/", nil))

	// Assert
	if with.Code == 404 && !strings.Contains(with.Body.String(), "not built") {
		t.Fatalf("with the UI: %d %s", with.Code, with.Body.String())
	}
	if without.Code != 404 {
		t.Fatalf("NoUI must not serve the UI: %d", without.Code)
	}
}

// brokenStore fails thread creation and listing like a database that went away.
type brokenStore struct {
	harness.Store
}

func (brokenStore) CreateThread(context.Context, string, string) (*store.Thread, error) {
	return nil, errors.New("pq: connection refused to 10.0.0.5")
}

func (brokenStore) ListThreads(context.Context, string, int, *store.ThreadCursor) ([]*store.Thread, error) {
	return nil, errors.New("pq: connection refused to 10.0.0.5")
}

func TestStorageFailuresAreGeneric500sThatLeakNothing(t *testing.T) {
	// Arrange
	r := newRig(t, rigOpts{store: brokenStore{memstore.New()}})

	for _, c := range []struct{ method, path, body string }{
		{"POST", "/api/threads", `{}`},
		{"GET", "/api/threads", ""},
	} {
		// Act
		resp := r.do(c.method, c.path, "alice", c.body)
		body, _ := io.ReadAll(resp.Body)

		// Assert
		if resp.StatusCode != 500 || !strings.Contains(string(body), `"code":"internal"`) || strings.Contains(string(body), "10.0.0.5") {
			t.Errorf("%s %s: %d %s", c.method, c.path, resp.StatusCode, body)
		}
	}
}

func TestSteeringARunningRunIsAcceptedAndReportedToTheExtension(t *testing.T) {
	// Arrange: the first model call waits until the steer has been accepted
	release, started := make(chan struct{}), make(chan struct{})
	var once sync.Once
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		return llmtest.Turn{Chunks: llmtest.Text("done")}
	})
	ext := &testExt{}
	r := newRig(t, rigOpts{model: model, web: httpapi.Options{Extension: ext}})
	id := r.newThread("alice")
	run := r.startRun("alice", id, "work on it")
	<-started

	// Act
	steer := r.json("POST", "/api/runs/"+run+"/steer", "alice", `{"text":"use the blue one"}`, 202)
	close(release)
	finished(t, r.stream(run, "alice"))

	// Assert
	if steer["status"] != "accepted" {
		t.Fatalf("steer %v", steer)
	}
	if got := strings.Join(ext.seen(), " "); !strings.Contains(got, "after-action:run.steer:alice") {
		t.Fatalf("extension calls %q", got)
	}
}
