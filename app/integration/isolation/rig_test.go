package isolation_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// hold is a model turn that stays inside the model call until the test releases it, so a run can be observed (and
// attacked) while it is still running.
type hold struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
	opened  sync.Once
}

func (h *hold) open() { h.opened.Do(func() { close(h.release) }) }

// stack is the real engine behind the real HTTP handler, with a scripted model and an in-memory sandbox.
type stack struct {
	t     *testing.T
	h     *harness.Harness
	srv   *httptest.Server
	holds map[string]*hold
	ctx   context.Context
}

type stackOpts struct {
	web     httpapi.Options
	product *product.Options // nil: the plain single-user httpapi
	holds   []string
	// configure runs once the application's address is known (an OIDC redirect URL needs it).
	configure func(appURL string, web *httpapi.Options, prod *product.Options)
}

// scriptedModel is driven by the text of the last user message:
//
//	"write <name>" writes /workspace/outputs/<name>.txt ("<name>-secret") and says "done" afterwards,
//	"hold <name>"  stays in the model call until hold <name> is released, then answers "held done",
//	"block <name>" stays in the model call until the run is cancelled,
//	anything else answers "ok".
//
// Calls without tools are the title call.
func scriptedModel(holds map[string]*hold) *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		text := ""
		for _, m := range c.Messages {
			if m.Role == ai.RoleUser {
				text = m.Content
			}
		}
		verb, name, _ := strings.Cut(text, " ")
		switch verb {
		case "write":
			args := `{"path":"/workspace/outputs/` + name + `.txt","content":"` + name + `-secret"}`
			return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "write_file", args), "tool_calls")}}
		case "block":
			g := holds[name]
			return llmtest.Turn{Block: true, OnCall: func() { g.once.Do(func() { close(g.started) }) }}
		case "hold":
			g := holds[name]
			return llmtest.Turn{Chunks: llmtest.Text("held done"), OnCall: func() {
				g.once.Do(func() { close(g.started) })
				<-g.release
			}}
		}
		return llmtest.Turn{Chunks: llmtest.Text("ok")}
	})
}

func newStack(t *testing.T, o stackOpts) *stack {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	holds := map[string]*hold{}
	for _, name := range o.holds {
		holds[name] = &hold{started: make(chan struct{}), release: make(chan struct{})}
	}
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) }
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: scriptedModel(holds)}, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()

	srv := httptest.NewUnstartedServer(nil)
	web := o.web
	web.DefaultModel = "gpt-test"
	web.Models = cfg.LLMModels
	if o.configure != nil {
		prod := o.product
		o.configure("http://"+srv.Listener.Addr().String(), &web, prod)
	}
	var handler http.Handler
	if o.product != nil {
		handler, err = product.New(h, web, *o.product)
	} else {
		handler, err = httpapi.NewServer(h, web)
	}
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = handler
	srv.Start()
	t.Cleanup(func() {
		for _, g := range holds { // a failing test must not leave a run stuck in the model
			g.open()
		}
		srv.Close()
		cancel()
		<-done
	})
	return &stack{t: t, h: h, srv: srv, holds: holds, ctx: ctx}
}

// resp is a fully read HTTP response.
type resp struct {
	Status int
	Body   []byte
	Header http.Header
}

func (r resp) json() map[string]any {
	var m map[string]any
	_ = json.Unmarshal(r.Body, &m)
	return m
}

func (r resp) list() []map[string]any {
	var l []map[string]any
	_ = json.Unmarshal(r.Body, &l)
	return l
}

// actor is one caller of the API: an HTTP client plus the way it authenticates.
type actor struct {
	s    *stack
	c    *http.Client
	auth func(*http.Request)
}

func (s *stack) as(auth func(*http.Request)) *actor {
	return &actor{s: s, c: &http.Client{}, auth: auth}
}

func (a *actor) do(method, path, body string, hdr ...string) resp {
	a.s.t.Helper()
	req, err := http.NewRequest(method, a.s.srv.URL+path, strings.NewReader(body))
	if err != nil {
		a.s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if a.auth != nil {
		a.auth(req)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := a.c.Do(req)
	if err != nil {
		a.s.t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		a.s.t.Fatal(err)
	}
	return resp{Status: res.StatusCode, Body: b, Header: res.Header}
}

func (a *actor) must(method, path, body string, want int) resp {
	a.s.t.Helper()
	r := a.do(method, path, body)
	if r.Status != want {
		a.s.t.Fatalf("%s %s: status %d, want %d: %s", method, path, r.Status, want, r.Body)
	}
	return r
}

func (a *actor) newThread(title string) string {
	a.s.t.Helper()
	body := `{}`
	if title != "" {
		body = `{"title":` + quote(title) + `}`
	}
	return a.must("POST", "/api/threads", body, 201).json()["id"].(string)
}

func (a *actor) startRun(thread, text string) string {
	a.s.t.Helper()
	return a.must("POST", "/api/threads/"+thread+"/runs", `{"message":`+quote(text)+`}`, 202).json()["id"].(string)
}

// complete follows the run's event stream to its end and returns the events.
func (a *actor) complete(run string) []event {
	a.s.t.Helper()
	evs := parseSSE(a.must("GET", "/api/runs/"+run+"/events", "", 200).Body)
	if len(evs) == 0 || evs[len(evs)-1].Type != "RUN_FINISHED" {
		a.s.t.Fatalf("run %s: stream did not end with RUN_FINISHED: %v", run, evs)
	}
	return evs
}

// waitIdle waits until the thread has no active run.
func (a *actor) waitIdle(thread string) {
	a.s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a.must("GET", "/api/threads/"+thread, "", 200).json()["activeRun"] == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.s.t.Fatalf("thread %s still has an active run", thread)
}

func (a *actor) threadIDs() []string {
	a.s.t.Helper()
	var ids []string
	for _, th := range a.must("GET", "/api/threads", "", 200).list() {
		ids = append(ids, th["id"].(string))
	}
	return ids
}

type event struct {
	Seq  string
	Type string
	Data string
}

func parseSSE(b []byte) []event {
	var out []event
	var cur event
	for _, line := range strings.Split(string(bytes.TrimSpace(b)), "\n") {
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.Seq = line[4:]
		case strings.HasPrefix(line, "data: "):
			cur.Data = line[6:]
			var m struct{ Type string }
			_ = json.Unmarshal([]byte(cur.Data), &m)
			cur.Type = m.Type
			out = append(out, cur)
			cur = event{}
		}
	}
	return out
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// waitStarted blocks until the held model call is entered.
func (s *stack) waitStarted(name string) {
	s.t.Helper()
	select {
	case <-s.holds[name].started:
	case <-time.After(20 * time.Second):
		s.t.Fatalf("hold %q: the model was never called", name)
	}
}

// route is one HTTP request of the API.
type route struct{ method, path, body string }

func (r route) String() string { return r.method + " " + r.path }

// globalRoutes are the routes that name no thread or run.
func globalRoutes() []route {
	return []route{
		{"GET", "/api/models", ""},
		{"GET", "/api/sessions", ""},
		{"POST", "/api/threads", `{}`},
		{"GET", "/api/threads", ""},
		{"GET", "/api/commands", ""},
		{"POST", "/api/commands/review/expand", `{"args":""}`},
	}
}

// idRoutes are the routes that name a thread or a run.
func idRoutes(thread, run string) []route {
	return []route{
		{"GET", "/api/threads/" + thread, ""},
		{"PATCH", "/api/threads/" + thread, `{"title":"pwned"}`},
		{"DELETE", "/api/threads/" + thread, ""},
		{"POST", "/api/threads/" + thread + "/runs", `{"message":"pwned"}`},
		{"POST", "/api/threads/" + thread + "/resume", `{"interruptId":"x","decision":"approve"}`},
		{"GET", "/api/threads/" + thread + "/files", ""},
		{"GET", "/api/threads/" + thread + "/files/download?path=/workspace/outputs/alice.txt", ""},
		{"POST", "/api/threads/" + thread + "/compact", ``},
		{"POST", "/api/threads/" + thread + "/undo", ``},
		{"POST", "/api/threads/" + thread + "/redo", ``},
		{"POST", "/api/threads/" + thread + "/fork", `{}`},
		{"GET", "/api/runs/" + run, ""},
		{"GET", "/api/runs/" + run + "/events", ""},
		{"POST", "/api/runs/" + run + "/cancel", ``},
		{"POST", "/api/runs/" + run + "/steer", `{"text":"pwned"}`},
	}
}
