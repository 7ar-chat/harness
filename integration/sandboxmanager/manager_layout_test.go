package sandboxmanager_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
)

// layoutDirs is the /workspace skeleton the prompts and skills advertise.
var layoutDirs = []string{"/workspace/outputs", "/workspace/.work", "/workspace/.work/tool-output", "/workspace/.work/renders"}

func missingDirs(ctx context.Context, sb sandbox.Sandbox) []string {
	var missing []string
	for _, d := range layoutDirs {
		if fi, err := sb.Stat(ctx, d); err != nil || fi.Type != "directory" {
			missing = append(missing, d)
		}
	}
	return missing
}

func TestNewAndRecreatedSandboxesHaveTheWorkspaceLayoutBeforeTheRestore(t *testing.T) {
	// /workspace/outputs and the scratch /workspace/.work (tool-output, renders) exist on every new or recreated
	// sandbox, already when the restore (OnCreate) runs, so the first tool can write there.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	var atRestore [][]string
	mgr := sandbox.NewManager(f.backend, f.st, redisx.NewCoordinator(f.rdb), sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour, RenewEvery: time.Hour,
		OnCreate: func(ctx context.Context, _ string, sb sandbox.Sandbox, _ bool) string {
			atRestore = append(atRestore, missingDirs(ctx, sb))
			return ""
		}})
	first, err := mgr.Acquire(context.Background(), th.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	f.backend.Forget(first.ID()) // removed at the provider, and an operation found it gone
	mgr.Invalidate(context.Background(), th.ID, first.ID())

	// Act
	second, err := mgr.Acquire(context.Background(), th.ID, "alice")

	// Assert
	if err != nil || second.ID() == first.ID() || !sandbox.WasRecreated(second) {
		t.Fatalf("err %v first %s second %v", err, first.ID(), second)
	}
	if len(atRestore) != 2 || len(atRestore[0]) != 0 || len(atRestore[1]) != 0 {
		t.Fatalf("directories missing when the restore ran: %v", atRestore)
	}
	if m := missingDirs(context.Background(), second); len(m) != 0 {
		t.Fatalf("recreated sandbox misses %v", m)
	}
}
