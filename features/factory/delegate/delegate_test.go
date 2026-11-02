package delegate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/features/factory/delegate"
)

// fakeSpawner records requests and answers through fn.
type fakeSpawner struct {
	mu   sync.Mutex
	reqs []factory.SpawnRequest
	fn   func(ctx context.Context, req factory.SpawnRequest) (*factory.SpawnResult, error)

	cur, max atomic.Int32
}

func (f *fakeSpawner) Spawn(ctx context.Context, req factory.SpawnRequest) (*factory.SpawnResult, error) {
	n := f.cur.Add(1)
	defer f.cur.Add(-1)
	for {
		m := f.max.Load()
		if n <= m || f.max.CompareAndSwap(m, n) {
			break
		}
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	id := "sub-" + string(rune('a'+len(f.reqs)-1))
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, req)
	}
	return &factory.SpawnResult{ThreadID: id, RunID: "run-1", Status: "completed", Answer: "done: " + req.Role, Usage: factory.Spend{Tokens: 1234, Runs: 1}}, nil
}

func (f *fakeSpawner) requests() []factory.SpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]factory.SpawnRequest(nil), f.reqs...)
}

func toolByName(t *testing.T, p *delegate.Provider, rc tools.RunContext, name string) ai.InvokableTool {
	t.Helper()
	list, _, err := p.Tools(context.Background(), rc)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	for _, bt := range list {
		info, _ := bt.Info(context.Background())
		if info.Name == name {
			return bt.(ai.InvokableTool)
		}
	}
	return nil
}

func call(t *testing.T, tl ai.InvokableTool, args any) (string, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	return tl.InvokableRun(context.Background(), string(b))
}

func seed(t *testing.T, st *factorystore.Mem, ti factory.ThreadInfo) {
	t.Helper()
	if err := st.SetThreadInfo(context.Background(), ti); err != nil {
		t.Fatal(err)
	}
}

func TestDelegateRunsRoleAndInheritsFromThread(t *testing.T) {
	// Arrange
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "t1", Login: "ann", TaskID: "task-9", Policy: factory.PolicySandboxed,
		Repo: "acme/app", BaseBranch: "main", Branch: "agent/fix"})
	sp := &fakeSpawner{}
	p := delegate.New(delegate.Options{Spawner: sp, Store: st, Models: []string{"fast"}})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t1", Login: "rc-login"}, "delegate")

	// Act
	out, err := call(t, tl, map[string]any{"role": "Coder", "task": "fix the parser\nsecond line", "context": "see issue 12", "model": "fast"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	reqs := sp.requests()
	if len(reqs) != 1 {
		t.Fatalf("spawns = %d", len(reqs))
	}
	r := reqs[0]
	if r.Role != factory.RoleCoder || r.ReadOnly || r.Instructions != delegate.DefaultRoles()[factory.RoleCoder].Instructions {
		t.Errorf("role fields wrong: %+v", r)
	}
	if r.Login != "ann" || r.TaskID != "task-9" || r.Policy != factory.PolicySandboxed || r.ParentThreadID != "t1" {
		t.Errorf("inherited fields wrong: %+v", r)
	}
	if r.Repo != "acme/app" || r.BaseBranch != "main" || r.Branch != "agent/fix" {
		t.Errorf("repository wrong: %+v", r)
	}
	if r.Model != "fast" {
		t.Errorf("model = %q", r.Model)
	}
	if !strings.Contains(r.Prompt, "fix the parser") || !strings.Contains(r.Prompt, "see issue 12") {
		t.Errorf("prompt = %q", r.Prompt)
	}
	if r.Title != "coder: fix the parser" {
		t.Errorf("title = %q", r.Title)
	}
	for _, want := range []string{"status: completed", "thread: sub-a", "tokens: 1234", "done: coder"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestDelegateUsesLoginOfRunWithoutThreadInfo(t *testing.T) {
	// Arrange
	sp := &fakeSpawner{}
	p := delegate.New(delegate.Options{Spawner: sp, Store: factorystore.NewMem(nil)})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "plain", Login: "bob"}, "delegate")

	// Act
	_, err := call(t, tl, map[string]any{"role": "explorer", "task": "where is X"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if r := sp.requests()[0]; r.Login != "bob" || r.TaskID != "" || r.Repo != "" || r.ParentThreadID != "plain" {
		t.Errorf("request = %+v", r)
	}
}

func TestDelegateReadOnlyRolesSpawnReadOnly(t *testing.T) {
	for role, readOnly := range map[string]bool{
		factory.RolePlanner: true, factory.RoleReviewer: true, factory.RoleExplorer: true,
		factory.RoleCoder: false, factory.RoleTester: false,
	} {
		t.Run(role, func(t *testing.T) {
			// Arrange
			sp := &fakeSpawner{}
			p := delegate.New(delegate.Options{Spawner: sp})
			tl := toolByName(t, p, tools.RunContext{ThreadID: "t", Login: "u"}, "delegate")

			// Act
			_, err := call(t, tl, map[string]any{"role": role, "task": "x"})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := sp.requests()[0].ReadOnly; got != readOnly {
				t.Errorf("ReadOnly = %v, want %v", got, readOnly)
			}
		})
	}
}

func TestDelegateIsolatedBranch(t *testing.T) {
	tests := []struct {
		name     string
		role     string
		isolated bool
		want     string
	}{
		{"writer shares by default", factory.RoleCoder, false, "agent/fix"},
		{"writer isolated", factory.RoleCoder, true, "agent/fix-coder-1"},
		{"reader stays on the branch", factory.RoleReviewer, true, "agent/fix"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			st := factorystore.NewMem(nil)
			seed(t, st, factory.ThreadInfo{ThreadID: "t1", Repo: "acme/app", Branch: "agent/fix"})
			sp := &fakeSpawner{}
			tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp, Store: st}), tools.RunContext{ThreadID: "t1"}, "delegate")

			// Act
			_, err := call(t, tl, map[string]any{"role": tc.role, "task": "x", "isolated": tc.isolated})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := sp.requests()[0].Branch; got != tc.want {
				t.Errorf("branch = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDelegateIsolatedBranchesAreUnique(t *testing.T) {
	// Arrange
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "t1", Repo: "acme/app", Branch: "b"})
	sp := &fakeSpawner{}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp, Store: st}), tools.RunContext{ThreadID: "t1"}, "delegate_many")

	// Act
	_, err := call(t, tl, map[string]any{"items": []map[string]any{
		{"role": "coder", "task": "a", "isolated": true}, {"role": "coder", "task": "b", "isolated": true}}})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range sp.requests() {
		seen[r.Branch] = true
	}
	if len(seen) != 2 {
		t.Errorf("branches = %v, want two distinct", seen)
	}
}

func TestDelegateRejectsBadCalls(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown role", map[string]any{"role": "wizard", "task": "x"}, `unknown role "wizard"; available roles: coder, explorer, planner, reviewer, tester`},
		{"empty task", map[string]any{"role": "coder", "task": "  "}, "task must not be empty"},
		{"model not allowed", map[string]any{"role": "coder", "task": "x", "model": "gpt-big"}, `model "gpt-big" cannot be chosen`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			sp := &fakeSpawner{}
			tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp}), tools.RunContext{ThreadID: "t"}, "delegate")

			// Act
			_, err := call(t, tl, tc.args)

			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			var ue *tools.UserError
			if !errors.As(err, &ue) {
				t.Errorf("error %T is not a tools.UserError", err)
			}
			if n := len(sp.requests()); n != 0 {
				t.Errorf("spawned %d sub-agents for an invalid call", n)
			}
		})
	}
}

func TestDelegateCustomRolesReplaceDefaults(t *testing.T) {
	// Arrange
	sp := &fakeSpawner{}
	roles := map[string]delegate.Role{"docs": {Name: "docs", Instructions: "write docs", ReadOnly: false, Model: "cheap"}}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp, Roles: roles}), tools.RunContext{ThreadID: "t"}, "delegate")

	// Act
	_, err := call(t, tl, map[string]any{"role": "docs", "task": "x"})
	_, errCoder := call(t, tl, map[string]any{"role": "coder", "task": "x"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if r := sp.requests()[0]; r.Model != "cheap" || r.Instructions != "write docs" {
		t.Errorf("request = %+v", r)
	}
	if errCoder == nil {
		t.Error("coder should be unknown in a custom catalogue")
	}
}

func TestToolsDepthLimit(t *testing.T) {
	// A chain root <- d1 <- d2 <- d3: depth of dN is N.
	tests := []struct {
		thread   string
		maxDepth int
		offered  bool
	}{
		{"root", 0, true},
		{"d1", 0, true},
		{"d2", 0, false},
		{"d3", 0, false},
		{"d2", 3, true},
		{"d3", 3, false},
		{"unknown", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.thread, func(t *testing.T) {
			// Arrange
			st := factorystore.NewMem(nil)
			seed(t, st, factory.ThreadInfo{ThreadID: "root", TaskID: "x"})
			seed(t, st, factory.ThreadInfo{ThreadID: "d1", ParentThreadID: "root"})
			seed(t, st, factory.ThreadInfo{ThreadID: "d2", ParentThreadID: "d1"})
			seed(t, st, factory.ThreadInfo{ThreadID: "d3", ParentThreadID: "d2"})
			p := delegate.New(delegate.Options{Spawner: &fakeSpawner{}, Store: st, MaxDepth: tc.maxDepth})

			// Act
			list, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: tc.thread})

			// Assert
			if err != nil {
				t.Fatal(err)
			}
			if got := len(list) > 0; got != tc.offered {
				t.Errorf("tools offered = %v, want %v", got, tc.offered)
			}
		})
	}
}

func TestToolsSurviveParentCycle(t *testing.T) {
	// Arrange: a corrupt store where two threads are each other's parent.
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "a", ParentThreadID: "b"})
	seed(t, st, factory.ThreadInfo{ThreadID: "b", ParentThreadID: "a"})
	p := delegate.New(delegate.Options{Spawner: &fakeSpawner{}, Store: st})

	// Act
	list, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "a"})

	// Assert: the walk is capped, and the cycle counts as too deep.
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("a cyclic thread must not get the delegate tool, got %d tools", len(list))
	}
}

func TestToolsRequireSpawner(t *testing.T) {
	// Arrange
	p := delegate.New(delegate.Options{})

	// Act
	_, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "t"})

	// Assert
	if err == nil {
		t.Fatal("want an error without a spawner")
	}
}

func TestDelegateManyBoundsConcurrencyAndKeepsOrder(t *testing.T) {
	// Arrange
	release := make(chan struct{})
	sp := &fakeSpawner{}
	var started atomic.Int32
	sp.fn = func(ctx context.Context, req factory.SpawnRequest) (*factory.SpawnResult, error) {
		if started.Add(1) >= 2 {
			// Enough sub-agents are in flight to show the bound; let them all go.
			select {
			case <-release:
			default:
				close(release)
			}
		}
		select {
		case <-release:
		case <-time.After(50 * time.Millisecond):
		}
		time.Sleep(5 * time.Millisecond)
		return &factory.SpawnResult{ThreadID: "th-" + req.Title, Status: "completed", Answer: "answer for " + req.Prompt}, nil
	}
	p := delegate.New(delegate.Options{Spawner: sp, MaxParallel: 2})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t"}, "delegate_many")
	var items []map[string]any
	for _, n := range []string{"one", "two", "three", "four", "five"} {
		items = append(items, map[string]any{"role": "explorer", "task": n})
	}

	// Act
	out, err := call(t, tl, map[string]any{"items": items})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if m := sp.max.Load(); m != 2 {
		t.Errorf("max concurrent = %d, want exactly the bound 2", m)
	}
	last := -1
	for i, n := range []string{"one", "two", "three", "four", "five"} {
		pos := strings.Index(out, "answer for "+n)
		if pos < 0 || pos < last {
			t.Fatalf("report %d (%s) missing or out of order:\n%s", i, n, out)
		}
		last = pos
	}
}

func TestDelegateManyRejectsBadItems(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"empty", map[string]any{"items": []map[string]any{}}, "items must not be empty"},
		{"too many", map[string]any{"items": manyItems(9)}, "at most 8 items"},
		{"one bad role fails all", map[string]any{"items": []map[string]any{{"role": "coder", "task": "a"}, {"role": "nope", "task": "b"}}}, "item 2: unknown role"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			sp := &fakeSpawner{}
			tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp}), tools.RunContext{ThreadID: "t"}, "delegate_many")

			// Act
			_, err := call(t, tl, tc.args)

			// Assert
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if n := len(sp.requests()); n != 0 {
				t.Errorf("spawned %d sub-agents before validation passed", n)
			}
		})
	}
}

func manyItems(n int) []map[string]any {
	var out []map[string]any
	for range n {
		out = append(out, map[string]any{"role": "explorer", "task": "x"})
	}
	return out
}

func TestDelegateLimitsDelegationsPerRun(t *testing.T) {
	// Arrange
	sp := &fakeSpawner{}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp}), tools.RunContext{ThreadID: "t"}, "delegate")
	var err error

	// Act
	for range 30 {
		if _, err = call(t, tl, map[string]any{"role": "explorer", "task": "x"}); err != nil {
			break
		}
	}

	// Assert
	if err == nil || !strings.Contains(err.Error(), "already delegated 24 times") {
		t.Fatalf("err = %v", err)
	}
	if n := len(sp.requests()); n != 24 {
		t.Errorf("spawned %d, want 24", n)
	}
}

func TestDelegateTruncatesLongReports(t *testing.T) {
	// Arrange
	sp := &fakeSpawner{fn: func(context.Context, factory.SpawnRequest) (*factory.SpawnResult, error) {
		return &factory.SpawnResult{ThreadID: "th", Status: "completed", Answer: "HEAD" + strings.Repeat("é", 30000) + "TAIL"}, nil
	}}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp}), tools.RunContext{ThreadID: "t"}, "delegate")

	// Act
	out, err := call(t, tl, map[string]any{"role": "explorer", "task": "x"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(out)); n > 8000 {
		t.Errorf("report has %d characters, want <= 8000", n)
	}
	for _, want := range []string{"HEAD", "TAIL", "report truncated", "thread: th", "status: completed"} {
		if !strings.Contains(out, want) {
			t.Errorf("truncated report lacks %q", want)
		}
	}
}

func TestDelegateFailuresAreReadableText(t *testing.T) {
	tests := []struct {
		name string
		res  *factory.SpawnResult
		err  error
		want []string
	}{
		{"spawn error", nil, errors.New("sandbox quota exceeded"), []string{"DELEGATION FAILED", "sandbox quota exceeded", "do the work yourself"}},
		{"failed run", &factory.SpawnResult{ThreadID: "th-f", Status: "failed", Answer: "model unavailable", Usage: factory.Spend{Tokens: 7}},
			nil, []string{"DELEGATION FAILED", "status failed", "thread: th-f", "tokens: 7", "model unavailable"}},
		{"cancelled run", &factory.SpawnResult{ThreadID: "th-c", Status: "cancelled"}, nil, []string{"DELEGATION FAILED", "status cancelled"}},
		{"nil result", nil, nil, []string{"DELEGATION FAILED", "no result"}},
		{"waiting for a human", &factory.SpawnResult{ThreadID: "th-w", Status: "completed", WaitingFor: "approval", Answer: "half"}, nil, []string{"waiting for a human (approval)", "half"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			sp := &fakeSpawner{fn: func(context.Context, factory.SpawnRequest) (*factory.SpawnResult, error) { return tc.res, tc.err }}
			tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp}), tools.RunContext{ThreadID: "t"}, "delegate")

			// Act
			out, err := call(t, tl, map[string]any{"role": "coder", "task": "x"})

			// Assert
			if err != nil {
				t.Fatalf("a sub-run problem must be text for the model, got Go error %v", err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestDelegateTimeout(t *testing.T) {
	// Arrange
	sp := &fakeSpawner{fn: func(ctx context.Context, _ factory.SpawnRequest) (*factory.SpawnResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	p := delegate.New(delegate.Options{Spawner: sp, Timeout: 20 * time.Millisecond})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t"}, "delegate")

	// Act
	out, err := call(t, tl, map[string]any{"role": "coder", "task": "x"})

	// Assert
	if err != nil {
		t.Fatalf("timeout must be text for the model, got %v", err)
	}
	if !strings.Contains(out, "timed out after 20ms") {
		t.Errorf("output = %q", out)
	}
}

func TestDelegateCallerCancellationIsAnError(t *testing.T) {
	// Arrange
	sp := &fakeSpawner{fn: func(ctx context.Context, _ factory.SpawnRequest) (*factory.SpawnResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	tl := toolByName(t, delegate.New(delegate.Options{Spawner: sp}), tools.RunContext{ThreadID: "t"}, "delegate_many")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()

	// Act
	_, err := tl.InvokableRun(ctx, `{"items":[{"role":"coder","task":"x"}]}`)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled so the run itself stops", err)
	}
}

func TestDelegateReportsSpendAndAddsTaskContext(t *testing.T) {
	// Arrange
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "t1", TaskID: "task-1"})
	sp := &fakeSpawner{}
	var mu sync.Mutex
	got := map[string]factory.Spend{}
	p := delegate.New(delegate.Options{
		Spawner: sp, Store: st,
		Task: func(_ context.Context, id string) (*factory.Task, error) {
			return &factory.Task{ID: id, Title: "Fix login", Spec: "users cannot log in", Repo: "acme/app", BaseBranch: "main", Branch: "fix", Policy: factory.PolicyAutonomous}, nil
		},
		OnSpend: func(_ context.Context, taskID string, d factory.Spend) {
			mu.Lock()
			defer mu.Unlock()
			got[taskID] = d
		},
	})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t1"}, "delegate")

	// Act
	_, err := call(t, tl, map[string]any{"role": "planner", "task": "plan it"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got["task-1"].Tokens != 1234 {
		t.Errorf("spend = %+v", got)
	}
	r := sp.requests()[0]
	if !strings.Contains(r.Prompt, "Fix login") || !strings.Contains(r.Prompt, "users cannot log in") {
		t.Errorf("prompt lacks task context: %q", r.Prompt)
	}
	if r.Repo != "acme/app" || r.Branch != "fix" || r.Policy != factory.PolicyAutonomous {
		t.Errorf("task fills what the thread lacks: %+v", r)
	}
}

func TestDelegateWithoutTaskDoesNotReportSpend(t *testing.T) {
	// Arrange
	var calls atomic.Int32
	p := delegate.New(delegate.Options{Spawner: &fakeSpawner{}, OnSpend: func(context.Context, string, factory.Spend) { calls.Add(1) }})
	tl := toolByName(t, p, tools.RunContext{ThreadID: "t"}, "delegate")

	// Act
	_, err := call(t, tl, map[string]any{"role": "coder", "task": "x"})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Error("spend reported for a thread without a task")
	}
}

func TestToolIsNotReadOnly(t *testing.T) {
	// Arrange
	p := delegate.New(delegate.Options{Spawner: &fakeSpawner{}})
	list, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: "t"})
	if err != nil {
		t.Fatal(err)
	}

	// Act / Assert: the engine only runs tools that say ReadOnly() in parallel.
	for _, bt := range list {
		if ro, ok := bt.(interface{ ReadOnly() bool }); ok && ro.ReadOnly() {
			t.Errorf("%T must not be read-only", bt)
		}
	}
	if len(list) != 2 {
		t.Errorf("tools = %d, want delegate and delegate_many", len(list))
	}
}

func TestDefaultRolesCatalogue(t *testing.T) {
	// Act
	roles := delegate.DefaultRoles()

	// Assert
	want := map[string]bool{factory.RolePlanner: true, factory.RoleCoder: false, factory.RoleReviewer: true, factory.RoleTester: false, factory.RoleExplorer: true}
	if len(roles) != len(want) {
		t.Fatalf("roles = %d, want %d", len(roles), len(want))
	}
	for name, ro := range want {
		r, ok := roles[name]
		if !ok || r.Name != name || strings.TrimSpace(r.Instructions) == "" || r.Description == "" || r.ReadOnly != ro {
			t.Errorf("role %s = %+v, want read-only %v with instructions", name, r, ro)
		}
	}
	if !strings.Contains(roles[factory.RoleReviewer].Instructions, "VERDICT:") {
		t.Error("reviewer must have a fixed findings format")
	}
}

func sectionFor(t *testing.T, s agent.PromptSection, thread string) string {
	t.Helper()
	return s(context.Background(), agent.Input{Run: &store.Run{ThreadID: thread}})
}

func TestSection(t *testing.T) {
	// Arrange
	st := factorystore.NewMem(nil)
	seed(t, st, factory.ThreadInfo{ThreadID: "role", Role: factory.RoleReviewer, TaskID: "task-5", Policy: factory.PolicySupervised,
		Repo: "acme/app", BaseBranch: "main", Branch: "agent/x", ReadOnly: true})
	seed(t, st, factory.ThreadInfo{ThreadID: "custom", Role: factory.RoleCoder, Instructions: "Only touch docs/."})
	seed(t, st, factory.ThreadInfo{ThreadID: "bare"})
	s := delegate.Section(st, nil)

	// Act
	role := sectionFor(t, s, "role")
	custom := sectionFor(t, s, "custom")
	ordinary := sectionFor(t, s, "ordinary")
	noThread := sectionFor(t, s, "")
	bare := sectionFor(t, s, "bare")

	// Assert
	for _, w := range []string{"role: reviewer", "task: task-5", "acme/app", "branch agent/x", "based on main", "policy: supervised", "read-only", "VERDICT:"} {
		if !strings.Contains(role, w) {
			t.Errorf("role section lacks %q:\n%s", w, role)
		}
	}
	if !strings.Contains(custom, "Only touch docs/.") || strings.Contains(custom, "small, focused commits") {
		t.Errorf("thread instructions must win over the role's:\n%s", custom)
	}
	if ordinary != "" || noThread != "" || bare != "" {
		t.Errorf("ordinary threads get nothing, got %q %q %q", ordinary, noThread, bare)
	}
}

func TestSectionNilStoreAndRun(t *testing.T) {
	// Act / Assert
	if got := delegate.Section(nil, nil)(context.Background(), agent.Input{Run: &store.Run{ThreadID: "t"}}); got != "" {
		t.Errorf("nil store: %q", got)
	}
	if got := delegate.Section(factorystore.NewMem(nil), nil)(context.Background(), agent.Input{}); got != "" {
		t.Errorf("nil run: %q", got)
	}
}

func TestRoster(t *testing.T) {
	// Act
	def := delegate.Roster()(context.Background(), agent.Input{})
	custom := delegate.Roster(map[string]delegate.Role{"docs": {Name: "docs", Description: "writes docs"}})(context.Background(), agent.Input{})

	// Assert
	for _, w := range []string{"planner", "coder", "reviewer", "tester", "explorer", "delegate_many"} {
		if !strings.Contains(def, w) {
			t.Errorf("roster lacks %q:\n%s", w, def)
		}
	}
	if !strings.Contains(custom, "docs: writes docs") || strings.Contains(custom, "planner") {
		t.Errorf("custom roster = %s", custom)
	}
}
