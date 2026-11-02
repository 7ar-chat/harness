package conversation_test

import (
	"bufio"
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

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

// deadline bounds every wait of the suite: conditions are polled, never slept on.
const deadline = 20 * time.Second

// rig is the real stack (harness + HTTP API) over a scripted model, spoken to with a real HTTP client.
type rig struct {
	t    *testing.T
	srv  *httptest.Server
	h    *harness.Harness
	http *http.Client

	once  sync.Once
	close func()
}

type rigOpts struct {
	model *llmtest.Model
	local bool // the local sandbox (unix) instead of the in-memory one
	tools []tools.Provider
}

func newRig(t *testing.T, o rigOpts) *rig {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("LLM_STEP_RETRIES", "0")
	t.Setenv("AGENT_PERMISSIONS", "")
	t.Setenv("AGENT_PERMISSIONS_DENY", "")
	if o.local {
		t.Setenv("SANDBOX_BACKEND", "local")
		t.Setenv("LOCAL_WORKSPACE", t.TempDir())
		t.Setenv("LOCAL_STATE_DIR", t.TempDir())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ho := harness.Options{Config: cfg, Models: llmtest.Factory{M: o.model}, Tools: o.tools}
	if !o.local {
		ho.Backend = sandboxtest.NewBackend()
	}
	h, err := harness.New(ho)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = h.Run(ctx) }()
	api := httpapi.New(h, httpapi.Options{
		Authenticate: func(r *http.Request) (string, error) { return r.Header.Get("X-User"), nil },
		DefaultModel: "gpt-test",
		Models:       cfg.LLMModels,
	})
	srv := httptest.NewServer(api)
	tr := &http.Transport{}
	client := &http.Client{Transport: tr}
	rg := &rig{t: t, srv: srv, h: h, http: client}
	rg.close = func() {
		tr.CloseIdleConnections()
		srv.Close()
		cancel()
		<-done
	}
	t.Cleanup(rg.stop)
	return rg
}

// stop tears the stack down (idempotent): the server, the connections, the harness loop.
func (r *rig) stop() { r.once.Do(r.close) }

// request performs one HTTP request as user and returns the status and body.
func (r *rig) request(method, path, user, body string, hdr ...string) (int, []byte) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.srv.URL+path, strings.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	if user != "" {
		req.Header.Set("X-User", user)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := r.http.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// call performs a request, requires the status and decodes the JSON object answered.
func (r *rig) call(method, path, user, body string, want int) map[string]any {
	r.t.Helper()
	status, b := r.request(method, path, user, body)
	if status != want {
		r.t.Fatalf("%s %s as %q: status %d, want %d: %s", method, path, user, status, want, b)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func (r *rig) newThread(user string) string {
	r.t.Helper()
	return r.call("POST", "/api/threads", user, `{}`, 201)["id"].(string)
}

// send posts a message and returns the id of the run answering it.
func (r *rig) send(user, thread, msg string, interactive bool) string {
	r.t.Helper()
	body := `{"message":` + strconv.Quote(msg) + `,"interactive":` + strconv.FormatBool(interactive) + `}`
	return r.call("POST", "/api/threads/"+thread+"/runs", user, body, 202)["id"].(string)
}

func (r *rig) thread(user, id string) map[string]any {
	r.t.Helper()
	return r.call("GET", "/api/threads/"+id, user, "", 200)
}

func (r *rig) run(user, id string) map[string]any {
	r.t.Helper()
	return r.call("GET", "/api/runs/"+id, user, "", 200)
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for !cond() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitIdle waits until the thread has no active run and returns it.
func (r *rig) waitIdle(user, id string) map[string]any {
	r.t.Helper()
	var th map[string]any
	waitFor(r.t, "thread "+id+" to have no active run", func() bool {
		th = r.thread(user, id)
		return th["activeRun"] == nil
	})
	return th
}

// event is one SSE event of a run stream.
type event struct {
	Seq  int64
	Data string
}

func (e event) fields() map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(e.Data), &m)
	return m
}

func (e event) typ() string {
	s, _ := e.fields()["type"].(string)
	return s
}

// custom returns the name of a CUSTOM event ("" for the others).
func (e event) custom() string {
	if e.typ() != "CUSTOM" {
		return ""
	}
	s, _ := e.fields()["name"].(string)
	return s
}

// stream is an open SSE connection to a run.
type stream struct {
	t      *testing.T
	cancel context.CancelFunc
	sc     *bufio.Scanner
	body   io.Closer
	cur    int64
}

// open connects to a run's event stream; the connection ends with the test or with Close.
func (r *rig) open(runID, user, query string, hdr ...string) *stream {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	req, err := http.NewRequestWithContext(ctx, "GET", r.srv.URL+"/api/runs/"+runID+"/events"+query, nil)
	if err != nil {
		cancel()
		r.t.Fatal(err)
	}
	req.Header.Set("X-User", user)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := r.http.Do(req)
	if err != nil {
		cancel()
		r.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		r.t.Fatalf("events of %s as %q: status %d: %s", runID, user, resp.StatusCode, b)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	s := &stream{t: r.t, cancel: cancel, sc: sc, body: resp.Body}
	r.t.Cleanup(s.Close)
	return s
}

// Close drops the connection.
func (s *stream) Close() {
	s.cancel()
	s.body.Close()
}

// next returns the next event; false when the server closed the stream.
func (s *stream) next() (event, bool) {
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			s.cur, _ = strconv.ParseInt(line[4:], 10, 64)
		case strings.HasPrefix(line, "data: "):
			return event{Seq: s.cur, Data: line[6:]}, true
		}
	}
	return event{}, false
}

// all reads until the server closes the stream.
func (s *stream) all() []event {
	var out []event
	for {
		ev, ok := s.next()
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

// until reads events up to and including the first one satisfying stop.
func (s *stream) until(stop func(event) bool) []event {
	s.t.Helper()
	var out []event
	for {
		ev, ok := s.next()
		if !ok {
			s.t.Fatalf("stream ended before the awaited event; got %v", types(out))
		}
		out = append(out, ev)
		if stop(ev) {
			return out
		}
	}
}

// replay reads a run's whole stream from a fresh connection.
func (r *rig) replay(runID, user string) []event {
	r.t.Helper()
	s := r.open(runID, user, "")
	defer s.Close()
	return s.all()
}

func types(evs []event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.typ()
		if c := e.custom(); c != "" {
			out[i] += ":" + c
		}
	}
	return out
}

// outcome decodes the RUN_FINISHED event ending a stream: its outcome type and interrupts.
func outcome(t *testing.T, evs []event) (typ string, interrupts []map[string]any) {
	t.Helper()
	if len(evs) == 0 || evs[len(evs)-1].typ() != "RUN_FINISHED" {
		t.Fatalf("stream does not end with RUN_FINISHED: %v", types(evs))
	}
	out, _ := evs[len(evs)-1].fields()["outcome"].(map[string]any)
	typ, _ = out["type"].(string)
	list, _ := out["interrupts"].([]any)
	for _, i := range list {
		interrupts = append(interrupts, i.(map[string]any))
	}
	return typ, interrupts
}

// deltas concatenates the TEXT_MESSAGE_CONTENT deltas of a stream.
func deltas(evs []event) (text string, chunks int) {
	var b strings.Builder
	for _, e := range evs {
		if e.typ() == "TEXT_MESSAGE_CONTENT" {
			d, _ := e.fields()["delta"].(string)
			b.WriteString(d)
			chunks++
		}
	}
	return b.String(), chunks
}

// titleCall reports whether a model request is the thread-title call (no tools offered).
func titleCall(c llmtest.Call) bool { return len(c.Tools) == 0 }

func titleTurn() llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text("A title")} }

// agentCalls returns the model requests of the agent loop (the title calls left out).
func agentCalls(m *llmtest.Model) []llmtest.Call {
	var out []llmtest.Call
	for _, c := range m.Calls() {
		if !titleCall(c) {
			out = append(out, c)
		}
	}
	return out
}

// roleContent renders a request history as "role:content" lines without the system prompt.
func history(c llmtest.Call) []string {
	var out []string
	for _, m := range c.Messages {
		if m.Role == ai.RoleSystem {
			continue
		}
		out = append(out, string(m.Role)+":"+m.Content)
	}
	return out
}

// gateTool is a tool provider with one tool: it announces each entry on entered, then waits for release (or the run's ctx).
type gateTool struct {
	name    string
	result  string
	entered chan struct{}
	release chan struct{}
	exited  chan error
	once    sync.Once
}

func newGateTool(name, result string) *gateTool {
	return &gateTool{name: name, result: result, entered: make(chan struct{}, 8), release: make(chan struct{}), exited: make(chan error, 8)}
}

// open lets the tool through, now and for later calls.
func (g *gateTool) open() { g.once.Do(func() { close(g.release) }) }

func (g *gateTool) Name() string { return g.name }

func (g *gateTool) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	return []ai.BaseTool{g}, func() {}, nil
}

func (g *gateTool) Info(context.Context) (*ai.ToolInfo, error) {
	return &ai.ToolInfo{Name: g.name, Desc: "gated " + g.name}, nil
}

func (g *gateTool) InvokableRun(ctx context.Context, _ string) (string, error) {
	g.entered <- struct{}{}
	select {
	case <-g.release:
		g.exited <- nil
		return g.result, nil
	case <-ctx.Done():
		g.exited <- ctx.Err()
		return "", ctx.Err()
	}
}

// waitEntered waits for the tool to be running.
func (g *gateTool) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(deadline):
		t.Fatal("the tool was never called")
	}
}

// waitExit waits for a tool call to return and gives its error.
func (g *gateTool) waitExit(t *testing.T) error {
	t.Helper()
	select {
	case err := <-g.exited:
		return err
	case <-time.After(deadline):
		t.Fatal("the tool never returned")
		return nil
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func seqs(evs []event) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}

// decodeList decodes a JSON array.
func decodeList(t *testing.T, b []byte) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("not a JSON array: %v: %s", err, b)
	}
	return out
}

// decodeObject decodes a JSON object (nil when the body is not one).
func decodeObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}
