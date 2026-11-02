package product

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
	"testing"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
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
	team      bool // team mode on an in-memory TeamStore
	prod      Options
	sup       *Supervisor
	mod       func(*harness.Options) // last word on the harness options (features...)
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
	if o.sup != nil {
		ho.Tools = append(ho.Tools, o.sup.Tools())
		o.prod.Supervisor = o.sup
	}
	if o.mod != nil {
		o.mod(&ho)
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
	if o.team && o.prod.Team == nil {
		o.prod.Team = teamstore.NewMem()
	}
	w.DefaultModel = "gpt-test"
	w.Models = cfg.LLMModels
	handler, err := New(h, w, o.prod)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
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
