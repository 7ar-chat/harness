package sandboxtools_test

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/cloudwego/eino/components/tool"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/coding/sandboxtools"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/engine/tools"
)

type fixture struct {
	backend *sandboxtest.Backend
	tools   map[string]tool.InvokableTool
	sb      *sandboxtest.Sandbox
}

func newFixture(t *testing.T, setup func(*sandboxtest.Sandbox)) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	f := &fixture{backend: sandboxtest.NewBackend(), tools: map[string]tool.InvokableTool{}}
	f.backend.OnCreate = func(sb *sandboxtest.Sandbox) {
		f.sb = sb
		if setup != nil {
			setup(sb)
		}
	}
	mgr := sandbox.NewManager(f.backend, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: time.Second, MaxSandboxesPerUser: 5})
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		f.tools[info.Name] = x.(tool.InvokableTool)
	}
	return f
}

func (f *fixture) call(ctx context.Context, name, args string) (string, error) {
	t, ok := f.tools[name]
	if !ok {
		return "", fmt.Errorf("no tool %s", name)
	}
	return t.InvokableRun(ctx, args)
}

// newHostFixture runs the tools on a real-bash sandbox backed by a temporary host directory.
func newHostFixture(t *testing.T, env map[string]string) (*fixture, *sandboxtest.HostBackend) {
	t.Helper()
	return newHostFixtureAt(t, t.TempDir(), env)
}

// requirePython3 skips the test without python3, which the real read, view
// and search fallback scripts run (the sandbox image has it, the CI image
// does not).
func requirePython3(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is required")
	}
}

// newHostFixtureAt is newHostFixture with the sandboxes under base (the first one is base/host-1).
func newHostFixtureAt(t *testing.T, base string, env map[string]string) (*fixture, *sandboxtest.HostBackend) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	hb := sandboxtest.NewHostBackend(base)
	hb.Env = env
	mgr := sandbox.NewManager(hb, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: 5 * time.Second})
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{tools: map[string]tool.InvokableTool{}}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		f.tools[info.Name] = x.(tool.InvokableTool)
	}
	return f, hb
}

// recorder is an agent emitter: live output, and images for a model that can (vision) or cannot see them.
type recorder struct {
	mu     sync.Mutex
	chunks []string
	ids    []string
	vision bool
	images []tools.Image
}

func (r *recorder) CanViewImages() bool { return r.vision }

func (r *recorder) AttachImage(img tools.Image) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.images = append(r.images, img)
}

func (r *recorder) ToolOutput(id, stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	r.chunks = append(r.chunks, stream+":"+text)
}
