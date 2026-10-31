package sandboxtools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/coding/sandboxtools"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestAfterPathNoteIsAppendedToReadWriteAndEditResults(t *testing.T) {
	// Arrange
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	backend := sandboxtest.NewBackend()
	backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.Put("/workspace/pkg/a.go", "package a\n") }
	mgr := sandbox.NewManager(backend, st, nil, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	var paths []string
	p := sandboxtools.New(mgr, sandboxtools.Options{CommandTimeout: time.Second, AfterPath: func(_ context.Context, _ tools.RunContext, _ sandbox.Sandbox, path string) string {
		paths = append(paths, path)
		return "[Instructions for pkg: be careful]"
	}})
	ts, _, err := p.Tools(context.Background(), tools.RunContext{ThreadID: th.ID, RunID: "r", Login: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ai.InvokableTool{}
	for _, x := range ts {
		info, _ := x.Info(context.Background())
		byName[info.Name] = x.(ai.InvokableTool)
	}
	ctx := context.Background()

	// Act
	read, err1 := byName["read_file"].InvokableRun(ctx, `{"path":"pkg/a.go"}`)
	edit, err2 := byName["edit_file"].InvokableRun(ctx, `{"path":"pkg/a.go","oldText":"package a","newText":"package b"}`)
	write, err3 := byName["write_file"].InvokableRun(ctx, `{"path":"pkg/b.go","content":"package b\n"}`)

	// Assert
	if err1 != nil || err2 != nil || err3 != nil {
		t.Fatal(err1, err2, err3)
	}
	for name, out := range map[string]string{"read_file": read, "edit_file": edit, "write_file": write} {
		if !strings.Contains(out, "[Instructions for pkg: be careful]") {
			t.Errorf("%s result without the note: %s", name, out)
		}
	}
	if len(paths) != 3 || paths[0] != "/workspace/pkg/a.go" {
		t.Errorf("hook paths %v", paths)
	}
}
