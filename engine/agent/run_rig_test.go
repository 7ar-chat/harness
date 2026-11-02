package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// runRig drives Agent.Run end to end on a memory store and a scripted model.
type runRig struct {
	t     *testing.T
	st    *memstore.Store
	model *llmtest.Model
	a     *Agent
	w     *recordingWriter
	th    *store.Thread
}

func newRunRig(t *testing.T, turns ...llmtest.Turn) *runRig {
	t.Helper()
	st := memstore.New()
	model := llmtest.New(turns...)
	th, err := st.CreateThread(context.Background(), "alice", "t")
	if err != nil {
		t.Fatal(err)
	}
	return &runRig{t: t, st: st, model: model, th: th, w: &recordingWriter{},
		a: &Agent{Models: llmtest.Factory{M: model}, Store: st, MaxSteps: 6, RetryBackoff: 1, MaxToolOutputChars: 30000,
			Sleep: func(context.Context, time.Duration) error { return nil }}}
}

// newRun creates a run whose user message is content (empty: a message-less resume run).
func (r *runRig) newRun(in store.NewRun) *store.Run {
	r.t.Helper()
	in.RunID, in.ThreadID, in.UserLogin = uuid.NewString(), r.th.ID, "alice"
	if in.Model == "" {
		in.Model = "gpt-test"
	}
	run, _, err := r.st.CreateRun(context.Background(), in)
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

func (r *runRig) userRun(content string) *store.Run {
	return r.newRun(store.NewRun{Message: store.Message{Role: store.RoleUser, Content: content}})
}

// run executes the run and returns its result, error and the event log.
func (r *runRig) run(run *store.Run) (Result, error) {
	r.t.Helper()
	r.w = &recordingWriter{}
	return r.a.Run(context.Background(), Input{Run: run, FirstName: "Alice"}, r.w)
}

// do creates a run with content and executes it.
func (r *runRig) do(content string) (Result, error) {
	r.t.Helper()
	return r.run(r.userRun(content))
}

func (r *runRig) types() []string {
	var out []string
	for _, e := range r.w.events {
		s := e.Type()
		if s == agui.Custom {
			s += ":" + fmt.Sprint(e["name"])
		}
		out = append(out, s)
	}
	return out
}

func (r *runRig) events(typ string) []agui.Event {
	var out []agui.Event
	for _, e := range r.w.events {
		s := e.Type()
		if s == agui.Custom {
			s += ":" + fmt.Sprint(e["name"])
		}
		if s == typ {
			out = append(out, e)
		}
	}
	return out
}

func (r *runRig) messages() []*store.Message {
	r.t.Helper()
	msgs, err := r.st.ListMessages(context.Background(), r.th.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return msgs
}

func (r *runRig) roles() []string {
	var out []string
	for _, m := range r.messages() {
		out = append(out, m.Role)
	}
	return out
}

func (r *runRig) lastMessage(role string) *store.Message {
	msgs := r.messages()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == role {
			return msgs[i]
		}
	}
	r.t.Fatalf("no %s message in %v", role, r.roles())
	return nil
}

// withTools adds a provider with the given tools.
func (r *runRig) withTools(ts ...ai.BaseTool) *provider {
	p := &provider{tools: ts}
	r.a.Tools = p
	return p
}

// errCode returns the code of an *Error, failing on any other error.
func errCode(t *testing.T, err error) string {
	t.Helper()
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("error %v is not an *Error", err)
	}
	return ae.Code
}

// funcTool is a scripted tool.
type funcTool struct {
	name string
	run  func(ctx context.Context, args string) (string, error)
	ro   bool
	mu   sync.Mutex
	args []string
}

func (f *funcTool) Info(context.Context) (*ai.ToolInfo, error) {
	return &ai.ToolInfo{Name: f.name, Desc: "fake " + f.name}, nil
}

func (f *funcTool) InvokableRun(ctx context.Context, args string) (string, error) {
	f.mu.Lock()
	f.args = append(f.args, args)
	f.mu.Unlock()
	if f.run == nil {
		return "ok", nil
	}
	return f.run(ctx, args)
}

func (f *funcTool) ReadOnly() bool { return f.ro }

func (f *funcTool) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.args...)
}

func tool(name string, result string) *funcTool {
	return &funcTool{name: name, run: func(context.Context, string) (string, error) { return result, nil }}
}

type provider struct {
	tools   []ai.BaseTool
	err     error
	cleaned int
}

func (p *provider) Name() string { return "test" }

func (p *provider) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	if p.err != nil {
		return nil, nil, p.err
	}
	return p.tools, func() { p.cleaned++ }, nil
}

// callTurn is a model turn with one tool call.
func callTurn(id, name, args string) llmtest.Turn {
	return llmtest.Turn{Chunks: []*ai.Message{llmtest.ToolCall(0, id, name, args)}}
}

func textTurn(s string) llmtest.Turn { return llmtest.Turn{Chunks: llmtest.Text(s)} }

// scriptedSteers is a SteerSource with queued batches.
type scriptedSteers struct {
	mu        sync.Mutex
	drains    [][]queue.Steer
	finals    [][]queue.Steer
	drainErr  error
	finalErr  error
	reopenErr error
	reopened  int
	finalized int
}

func (s *scriptedSteers) DrainSteers(context.Context, string) ([]queue.Steer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.drainErr != nil {
		return nil, s.drainErr
	}
	if len(s.drains) == 0 {
		return nil, nil
	}
	out := s.drains[0]
	s.drains = s.drains[1:]
	return out, nil
}

func (s *scriptedSteers) FinalizeSteers(context.Context, string) ([]queue.Steer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalized++
	if s.finalErr != nil {
		return nil, s.finalErr
	}
	if len(s.finals) == 0 {
		return nil, nil
	}
	out := s.finals[0]
	s.finals = s.finals[1:]
	return out, nil
}

func (s *scriptedSteers) ReopenSteers(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reopened++
	return s.reopenErr
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var _ = strings.TrimSpace
