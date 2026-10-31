package sandboxtools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

// newFixtureWith is newFixture with the provider and manager options adjustable by tune.
func newFixtureWith(t *testing.T, setup func(*sandboxtest.Sandbox), tune func(*sandboxtools.Options, *sandbox.ManagerOptions)) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	f := &fixture{backend: sandboxtest.NewBackend(), tools: map[string]ai.InvokableTool{}}
	f.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		f.sb = sb
		if setup != nil {
			setup(sb)
		}
	}
	mo := sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour}
	po := sandboxtools.Options{CommandTimeout: time.Second, MaxSandboxesPerUser: 5}
	if tune != nil {
		tune(&po, &mo)
	}
	mgr := sandbox.NewManager(f.backend, st, redisx.NewCoordinator(rdb), mo)
	p := sandboxtools.New(mgr, po)
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		f.tools[info.Name] = x.(ai.InvokableTool)
	}
	return f
}

// stubManager is a sandbox.Manager whose Acquire and Get are scripted.
type stubManager struct {
	disabled bool
	acquire  func(ctx context.Context, threadID, login string) (sandbox.Sandbox, error)
	get      func(ctx context.Context, threadID string) (sandbox.Sandbox, error)
}

func (m stubManager) Enabled() bool { return !m.disabled }
func (m stubManager) Acquire(ctx context.Context, threadID, login string) (sandbox.Sandbox, error) {
	return m.acquire(ctx, threadID, login)
}
func (m stubManager) Get(ctx context.Context, threadID string) (sandbox.Sandbox, error) {
	return m.get(ctx, threadID)
}
func (m stubManager) Kill(context.Context, string) error         { return nil }
func (m stubManager) Invalidate(context.Context, string, string) {}

// toolsOf returns the provider's tools by name.
func toolsOf(t *testing.T, p *sandboxtools.Provider, rc tools.RunContext) map[string]ai.InvokableTool {
	t.Helper()
	ts, _, err := p.Tools(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]ai.InvokableTool{}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		out[info.Name] = x.(ai.InvokableTool)
	}
	return out
}

func decodeMap(t *testing.T, out string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("result %q: %v", out, err)
	}
	return m
}

// scripted is a sandbox whose marked sandbox-side scripts (their first line, e.g. "# work:render") are
// answered by the test; everything else goes to the in-memory fake.
type scripted struct {
	*sandboxtest.Sandbox
	scripts map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)
}

func newScripted(scripts map[string]func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)) *scripted {
	sb := sandboxtest.NewSandbox("sbx-scripted")
	_ = sb.MkdirAll(context.Background(), "/workspace")
	return &scripted{Sandbox: sb, scripts: scripts}
}

func (s *scripted) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	first, _, _ := strings.Cut(req.Command, "\n")
	if f := s.scripts[strings.TrimSpace(first)]; f != nil {
		return f(ctx, req)
	}
	return s.Sandbox.Exec(ctx, req)
}

// printing is a script that prints text on stdout and exits with code.
func printing(text string, code int) func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if req.OnStdout != nil {
			req.OnStdout(text)
		}
		return sandbox.ExecResult{ExitCode: code}, nil
	}
}

// toolsOn returns the tools of a provider whose manager always hands out sb.
func toolsOn(t *testing.T, sb sandbox.Sandbox, opts sandboxtools.Options) map[string]ai.InvokableTool {
	t.Helper()
	m := stubManager{
		acquire: func(context.Context, string, string) (sandbox.Sandbox, error) { return sb, nil },
		get:     func(context.Context, string) (sandbox.Sandbox, error) { return sb, nil },
	}
	return toolsOf(t, sandboxtools.New(m, opts), tools.RunContext{ThreadID: "t", Login: "alice"})
}

// withRec is a context whose tool call has the recorder as its emitter (and so its vision).
func withRec(r *recorder) context.Context { return tools.WithCall(context.Background(), "call", r) }
