package product

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/executors"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

type probeMgr struct {
	sb       sandbox.Sandbox
	acquired atomic.Int32
	killed   atomic.Int32
	block    chan struct{}
}

func (m *probeMgr) Enabled() bool { return true }
func (m *probeMgr) Acquire(ctx context.Context, _, _ string) (sandbox.Sandbox, error) {
	m.acquired.Add(1)
	if m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return m.sb, nil
}
func (m *probeMgr) Get(context.Context, string) (sandbox.Sandbox, error) {
	return nil, sandbox.ErrNoSandbox
}
func (m *probeMgr) Kill(context.Context, string) error         { m.killed.Add(1); return nil }
func (m *probeMgr) Invalidate(context.Context, string, string) {}

func TestExecutorAvailabilityProbesTheImageOnceAndCaches(t *testing.T) {
	// Arrange
	mgr := &probeMgr{sb: sandboxtest.NewHostSandbox("p", t.TempDir())}
	a := &ExecutorAPI{AllowInstall: true}
	a.SetSandboxes(mgr)
	list := []executors.Executor{
		{Name: "present", Binary: "sh"},
		{Name: "installable", Binary: "no-such-cli-xyz", InstallCommand: "npm i -g x"},
		{Name: "absent", Binary: "no-such-cli-abc"},
	}

	// Act
	got := a.availability(context.Background(), list)
	again := a.availability(context.Background(), list)

	// Assert
	want := map[string]string{"present": AvailReady, "installable": AvailInstall, "absent": AvailMissing}
	for k, v := range want {
		if got[k] != v || again[k] != v {
			t.Errorf("%s = %q / %q, want %q", k, got[k], again[k], v)
		}
	}
	if mgr.acquired.Load() != 1 || mgr.killed.Load() != 1 {
		t.Fatalf("one short-lived sandbox expected, acquired %d killed %d", mgr.acquired.Load(), mgr.killed.Load())
	}
}

func TestExecutorAvailabilityIsUnknownWithoutASandboxOrWhenItIsSlow(t *testing.T) {
	// Arrange
	list := []executors.Executor{{Name: "present", Binary: "sh"}}
	none := &ExecutorAPI{}
	slow := &ExecutorAPI{}
	block := make(chan struct{})
	defer close(block)
	slow.SetSandboxes(&probeMgr{sb: sandboxtest.NewHostSandbox("p", t.TempDir()), block: block})

	// Act
	start := time.Now()
	gotNone := none.availability(context.Background(), list)
	gotSlow := slow.availability(context.Background(), list)

	// Assert
	if gotNone["present"] != AvailUnknown || gotSlow["present"] != AvailUnknown {
		t.Fatalf("none %v slow %v", gotNone, gotSlow)
	}
	if d := time.Since(start); d > probeWait+2*time.Second {
		t.Fatalf("the request waited %s for the probe", d)
	}
}
