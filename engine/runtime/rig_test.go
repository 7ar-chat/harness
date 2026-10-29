package runtime_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/engine/queue"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/features/goals"
	"github.com/GhaziBenDahmane/harness/features/memory"
	"github.com/GhaziBenDahmane/harness/features/memory/memorytools"
	"github.com/GhaziBenDahmane/harness/features/plan"
	"github.com/GhaziBenDahmane/harness/features/titles"
)

// TestMain runs the suite twice: with the scripted Chat Completions model, then
// through the real Responses client and the fake Responses server (stream R:
// the loop must behave the same with either client).
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	code := m.Run()
	if code == 0 && os.Getenv("LLMTEST_CHAT_ONLY") == "" {
		fmt.Println("runtime: the suite again through the fake Responses server")
		llmtest.UseResponses(true)
		code = m.Run()
		if v := llmtest.Violations(); len(v) > 0 {
			fmt.Println("runtime: requests the Responses API would reject:\n  " + strings.Join(v, "\n  "))
			code = 1
		}
	}
	os.Exit(code)
}

type rig struct {
	t       *testing.T
	st      *memstore.Store
	mr      *miniredis.Miniredis
	rdb     *redis.Client
	log     *redisx.EventLog
	q       *redisx.Queue
	backend *sandboxtest.Backend
	mgr     *sandbox.DefaultManager
	model   *llmtest.Model
	agent   *agent.Agent
	opts    runtime.Options
	window  int
	rt      *runtime.Runtime
	mem     *memory.Service
}

func newRig(t *testing.T, turns ...llmtest.Turn) *rig {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	r := &rig{t: t, st: st, mr: mr, rdb: rdb, backend: sandboxtest.NewBackend(), model: llmtest.New(turns...)}
	r.log = redisx.NewEventLog(rdb, st)
	r.log.TailBlock, r.log.Poll = 100*time.Millisecond, 10*time.Millisecond
	r.q = redisx.NewQueue(rdb)
	r.mgr = sandbox.NewManager(r.backend, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	r.mem = memory.New(st, memory.Options{RecallInjection: true})
	r.agent = &agent.Agent{
		Models: llmtest.Factory{M: r.model},
		Tools: tools.NewRegistry(sandboxtools.New(r.mgr, sandboxtools.Options{CommandTimeout: 5 * time.Second, MaxSandboxesPerUser: 5}),
			harnesstools.New(st), memorytools.New(r.mem)),
		Store:              st,
		Steers:             r.q,
		MaxSteps:           5,
		MaxToolOutputChars: 30000,
		RetryBackoff:       time.Millisecond,
		Now:                func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) },
		Harness: agent.Harness{
			Sections:            []agent.PromptSection{memory.PromptSection(r.mem)},
			Context:             []agent.PromptSection{goals.ContextSection(st), memory.RecallSection(r.mem)},
			Tail:                []agent.PromptSection{goals.TurnsSection(st)},
			Summaries:           st,
			Compactions:         st,
			ContextWindow:       func(string) int { return r.window },
			CompactionThreshold: 0.75,
			SupportsReasoning:   func(m string) bool { return m == "gpt-reasoning" },
		},
	}
	r.window = 128000
	// Every runtime test asserts the AG-UI order of every run log it produced.
	t.Cleanup(func() { checkOrder(t, st) })
	r.opts = runtime.Options{ID: "rt-test", Concurrency: 2, Heartbeat: time.Hour, ShutdownGrace: 100 * time.Millisecond, ReadBlock: 50 * time.Millisecond,
		Hooks: []runtime.Hook{&titles.Hook{Store: st, Models: llmtest.Factory{M: r.model}},
			&goals.Hook{Store: st, Models: llmtest.Factory{M: r.model}, Queue: r.q}}}
	return r
}

// checkOrder fails the test when a run's durable event log breaks the AG-UI
// order (START/END pairing, RUN_STARTED first, nothing after the terminal).
func checkOrder(t *testing.T, st *memstore.Store) {
	t.Helper()
	for runID, evs := range st.EventLogs() {
		if len(evs) == 0 {
			continue
		}
		raw := make([]json.RawMessage, 0, len(evs))
		for _, e := range evs {
			raw = append(raw, e.Data)
		}
		if err := agui.CheckOrderJSON(raw); err != nil {
			t.Errorf("run %s: %v", runID, err)
		}
	}
}

func (r *rig) runtime() *runtime.Runtime {
	if r.rt == nil {
		r.rt = runtime.New(r.opts, r.st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	}
	return r.rt
}

// queued creates a thread with a queued run, like POST /threads/{id}/runs.
func (r *rig) queued(content string) (*store.Thread, *store.Run) {
	r.t.Helper()
	ctx := context.Background()
	th, err := r.st.CreateThread(ctx, "alice", "t")
	if err != nil {
		r.t.Fatal(err)
	}
	return th, r.queuedOn(th, content)
}

func (r *rig) queuedOn(th *store.Thread, content string) *store.Run {
	r.t.Helper()
	run, _, err := r.st.CreateRun(context.Background(), store.NewRun{
		RunID: newID(), ThreadID: th.ID, UserLogin: "alice", Model: "gpt-test",
		Message: store.Message{Role: store.RoleUser, Content: content},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

func (r *rig) process(run *store.Run) *store.Run {
	r.t.Helper()
	if err := r.runtime().Process(context.Background(), run.ID); err != nil {
		r.t.Fatal(err)
	}
	return r.reload(run)
}

func (r *rig) reload(run *store.Run) *store.Run {
	got, err := r.st.GetRun(context.Background(), run.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

type event map[string]any

// events returns the durable (Postgres) events of a run.
func (r *rig) events(runID string) []event {
	r.t.Helper()
	evs, err := r.st.ListEvents(context.Background(), runID, 0, 10000)
	if err != nil {
		r.t.Fatal(err)
	}
	out := make([]event, 0, len(evs))
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			r.t.Fatalf("seq gap: event %d has seq %d", i, e.Seq)
		}
		var m event
		_ = json.Unmarshal(e.Data, &m)
		out = append(out, m)
	}
	return out
}

func (r *rig) messages(threadID string) []*store.Message {
	msgs, _ := r.st.ListMessages(context.Background(), threadID)
	return msgs
}

func types(evs []event) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		t, _ := e["type"].(string)
		if t == "CUSTOM" {
			t += ":" + e["name"].(string)
		}
		out = append(out, t)
	}
	return out
}

func find(evs []event, typ string) []event {
	var out []event
	for _, e := range evs {
		t, _ := e["type"].(string)
		if t == "CUSTOM" {
			t += ":" + e["name"].(string)
		}
		if t == typ {
			out = append(out, e)
		}
	}
	return out
}

func last(evs []event) event {
	if len(evs) == 0 {
		return event{}
	}
	return evs[len(evs)-1]
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// liveTypes reads the live Redis stream (events not yet flushed to Postgres).
func (r *rig) liveTypes(runID string) []string {
	msgs, _ := r.rdb.XRange(context.Background(), redisx.EventKey(runID), "-", "+").Result()
	var out []string
	for _, m := range msgs {
		var e event
		_ = json.Unmarshal([]byte(m.Values["data"].(string)), &e)
		out = append(out, e["type"].(string))
	}
	return out
}

// create queues a run with custom harness fields on th (defaults like queuedOn).
func (r *rig) create(th *store.Thread, in store.NewRun) *store.Run {
	r.t.Helper()
	in.RunID, in.ThreadID = newID(), th.ID
	if in.UserLogin == "" {
		in.UserLogin = "alice"
	}
	if in.Model == "" {
		in.Model = "gpt-test"
	}
	run, _, err := r.st.CreateRun(context.Background(), in)
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

// thread reloads a thread of alice.
func (r *rig) thread(id string) *store.Thread {
	r.t.Helper()
	th, err := r.st.GetThread(context.Background(), "alice", id)
	if err != nil {
		r.t.Fatal(err)
	}
	return th
}

func index(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return -1
}

// fakeTool is a scripted tool added next to the rig's tools.
type fakeTool struct {
	name string
	run  func(ctx context.Context, args string) (string, error)
}

func (f *fakeTool) Info(context.Context) (*schema.ToolInfo, error) {
	return &schema.ToolInfo{Name: f.name, Desc: "fake " + f.name}, nil
}

func (f *fakeTool) InvokableRun(ctx context.Context, args string, _ ...tool.Option) (string, error) {
	if f.run == nil {
		return "ok", nil
	}
	return f.run(ctx, args)
}

// guardTool is a fakeTool implementing tools.StopGuard.
type guardTool struct {
	fakeTool
	reminder func() string
}

func (g *guardTool) StopReminder(context.Context) string { return g.reminder() }

// withTools adds fake tools to the run's tools.
func (r *rig) withTools(ts ...tool.BaseTool) {
	r.agent.Tools = tools.NewRegistry(r.agent.Tools, staticTools(ts))
}

// countingSteers counts the agent's FinalizeSteers calls.
type countingSteers struct {
	agent.SteerSource
	finalizes atomic.Int32
}

func (c *countingSteers) FinalizeSteers(ctx context.Context, runID string) ([]queue.Steer, error) {
	c.finalizes.Add(1)
	return c.SteerSource.FinalizeSteers(ctx, runID)
}
