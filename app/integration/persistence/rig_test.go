package persistence_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// failAfter only bounds how long a broken test waits; it never decides a pass.
const failAfter = 20 * time.Second

// process is one harness plus its HTTP API, built from scratch over a store. Stopping it is what a clean shutdown does:
// the runtime drains, the server stops and the store is closed.
type process struct {
	t          *testing.T
	H          *harness.Harness
	URL        string
	srv        *httptest.Server
	cancel     context.CancelFunc
	done       chan struct{}
	startOnce  sync.Once
	ctx        context.Context
	closeStore func()
	stopOnce   sync.Once
	hc         *http.Client
}

type processOpts struct {
	store      harness.Store
	closeStore func()
	model      *llmtest.Model
	infra      *harness.Infra
	runtimeID  string
	env        map[string]string
	// idle leaves the runtime stopped until start is called: a web tier that only Sends, or a runtime that must not
	// pick up work yet.
	idle bool
	// rdb, when set, shares run queue, event log and liveness through Redis (pg processes only).
	rdb redis.UniversalClient
}

// startProcess wires a harness over o.store, starts its runtime and serves the HTTP API with X-User authentication.
func startProcess(t *testing.T, o processOpts) *process {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	t.Setenv("RUNTIME_SHUTDOWN_GRACE_SECONDS", "5")
	for k, v := range o.env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, sandboxtest.ShellExec(sb))) } // the artifact listing runs find
	h, err := harness.New(harness.Options{Config: cfg, Store: o.store, Models: llmtest.Factory{M: o.model},
		Backend: backend, Infra: o.infra, RuntimeID: o.runtimeID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &process{t: t, H: h, ctx: ctx, cancel: cancel, done: make(chan struct{}), closeStore: o.closeStore,
		hc: &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}}
	if !o.idle {
		p.start()
	}
	p.srv = httptest.NewServer(httpapi.New(h, httpapi.Options{
		Authenticate: func(r *http.Request) (string, error) { return r.Header.Get("X-User"), nil },
		Models:       cfg.LLMModels, DefaultModel: "gpt-test",
	}))
	p.URL = p.srv.URL
	t.Cleanup(p.stop)
	return p
}

// start runs the runtime (once).
func (p *process) start() {
	p.startOnce.Do(func() { go func() { defer close(p.done); _ = p.H.Run(p.ctx) }() })
}

// stopRuntime cancels the runtime's context and waits until Run returned, which is after its in-flight runs ended.
func (p *process) stopRuntime() {
	p.t.Helper()
	p.cancel()
	p.start() // an idle runtime exits at once; this keeps done closable
	select {
	case <-p.done:
	case <-time.After(failAfter):
		p.t.Error("the runtime did not stop")
	}
}

// stop shuts the process down in order: HTTP server, runtime (waiting for in-flight runs), store. It is idempotent.
func (p *process) stop() {
	p.stopOnce.Do(func() {
		p.srv.Close()
		p.stopRuntime()
		if p.closeStore != nil {
			p.closeStore()
		}
	})
}

// do sends a request as user and returns the status and body.
func (p *process) do(method, path, user, body string, hdr ...string) (int, []byte) {
	p.t.Helper()
	resp := p.request(method, path, user, body, hdr...)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatal(err)
	}
	return resp.StatusCode, b
}

func (p *process) request(method, path, user, body string, hdr ...string) *http.Response {
	p.t.Helper()
	req, err := http.NewRequest(method, p.URL+path, strings.NewReader(body))
	if err != nil {
		p.t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		p.t.Fatal(err)
	}
	return resp
}

// call performs a request, requires the status and decodes the JSON body into out.
func (p *process) call(method, path, user, body string, want int, out any) {
	p.t.Helper()
	code, b := p.do(method, path, user, body)
	if code != want {
		p.t.Fatalf("%s %s: status %d, want %d: %s", method, path, code, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			p.t.Fatalf("%s %s: %v: %s", method, path, err, b)
		}
	}
}

type threadView struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	ActiveRun *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"activeRun"`
	Messages []map[string]any `json:"messages"`
}

type runView struct {
	ID        string `json:"id"`
	ThreadID  string `json:"threadId"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	ErrorCode string `json:"errorCode"`
}

func (p *process) newThread(user string) string {
	p.t.Helper()
	var th threadView
	p.call("POST", "/api/threads", user, `{}`, 201, &th)
	return th.ID
}

func (p *process) thread(user, id string) threadView {
	p.t.Helper()
	var th threadView
	p.call("GET", "/api/threads/"+id, user, "", 200, &th)
	return th
}

func (p *process) threads(user string) []threadView {
	p.t.Helper()
	var out []threadView
	p.call("GET", "/api/threads", user, "", 200, &out)
	return out
}

func (p *process) run(user, id string) runView {
	p.t.Helper()
	var r runView
	p.call("GET", "/api/runs/"+id, user, "", 200, &r)
	return r
}

// send starts a run answering message and returns its id.
func (p *process) send(user, thread, message string) string {
	p.t.Helper()
	var r runView
	p.call("POST", "/api/threads/"+thread+"/runs", user, `{"message":`+strconv.Quote(message)+`}`, 202, &r)
	return r.ID
}

// sseEvent is one event of a stream as it travels on the wire: the id line and the raw data line.
type sseEvent struct{ ID, Data string }

func (e sseEvent) Type() string {
	var m struct{ Type string }
	_ = json.Unmarshal([]byte(e.Data), &m)
	return m.Type
}

// stream reads a run's events until the server closes the stream (it does so after the terminal event).
func (p *process) stream(user, runID string, hdr ...string) []sseEvent {
	p.t.Helper()
	resp := p.request("GET", "/api/runs/"+runID+"/events", user, "", hdr...)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		p.t.Fatalf("events of %s: status %d", runID, resp.StatusCode)
	}
	return readSSE(p.t, resp.Body)
}

func readSSE(t *testing.T, body io.ReadCloser) []sseEvent {
	t.Helper()
	timer := time.AfterFunc(failAfter, func() { body.Close() })
	defer timer.Stop()
	var out []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.ID = line[4:]
		case strings.HasPrefix(line, "data: "):
			cur.Data = line[6:]
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out
}

func types(evs []sseEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type()
	}
	return out
}

// terminals counts the events that end a run stream.
func terminals(evs []sseEvent) int {
	n := 0
	for _, e := range evs {
		if ty := e.Type(); ty == "RUN_FINISHED" || ty == "RUN_ERROR" {
			n++
		}
	}
	return n
}

// finishedRun streams the run to its end and requires RUN_FINISHED.
func (p *process) finishedRun(user, runID string) []sseEvent {
	p.t.Helper()
	evs := p.stream(user, runID)
	if len(evs) == 0 || evs[len(evs)-1].Type() != "RUN_FINISHED" {
		p.t.Fatalf("run %s did not finish: %v", runID, types(evs))
	}
	return evs
}

// equalJSON compares two decoded JSON values by their canonical encoding.
func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Scripted conversation. A message containing "[write v1]" (or "[write v2]") makes the model write /workspace/notes.txt
// with that content; any other message gets a plain answer. After a tool result the model concludes. Calls without
// tools are the title call.
const (
	notesPath = "/workspace/notes.txt"
	notesV1   = "notes v1\n"
	notesV2   = "notes v2\n"
)

func toolCall(id, name, args string) llmtest.Turn {
	return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, id, name, args), "tool_calls")}}
}

func scriptedModel() *llmtest.Model {
	return llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		last := c.Messages[len(c.Messages)-1]
		switch {
		case last.Role == ai.RoleTool:
			return llmtest.Turn{Chunks: llmtest.Text("Tool finished")}
		case strings.Contains(last.Content, "[write v1]"):
			return toolCall("call-v1", "write_file", `{"path":"`+notesPath+`","content":"notes v1\n"}`)
		case strings.Contains(last.Content, "[write v2]"):
			return toolCall("call-v2", "write_file", `{"path":"`+notesPath+`","content":"notes v2\n"}`)
		}
		return llmtest.Turn{Chunks: llmtest.Text("Reply to: " + last.Content)}
	})
}

// gatedModel holds the first model call that has tools (signalling on called) until open is called, then answers
// "Late answer"; every later call answers "Answered after the gate". Title calls answer immediately.
type gatedModel struct {
	*llmtest.Model
	called  chan struct{}
	release chan struct{}
	first   sync.Once
	gate    sync.Once
}

func newGatedModel(t *testing.T) *gatedModel {
	g := &gatedModel{called: make(chan struct{}), release: make(chan struct{})}
	g.Model = llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) == 0 {
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		turn := llmtest.Turn{Chunks: llmtest.Text("Answered after the gate")}
		g.first.Do(func() {
			turn = llmtest.Turn{Chunks: llmtest.Text("Late answer"), OnCall: func() {
				close(g.called)
				<-g.release
			}}
		})
		return turn
	})
	t.Cleanup(g.open)
	return g
}

// open lets the held model call proceed (idempotent).
func (g *gatedModel) open() { g.gate.Do(func() { close(g.release) }) }

// waitCalled blocks until the model call is in flight.
func (g *gatedModel) waitCalled(t *testing.T) {
	t.Helper()
	select {
	case <-g.called:
	case <-time.After(failAfter):
		t.Fatal("the model was never called")
	}
}

// mainCalls are the recorded calls of the agent loop (those that carry tools).
func mainCalls(m *llmtest.Model) []llmtest.Call {
	var out []llmtest.Call
	for _, c := range m.Calls() {
		if len(c.Tools) > 0 {
			out = append(out, c)
		}
	}
	return out
}
