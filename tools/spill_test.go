package tools_test

import (
	"context"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/sandboxtest"
	"github.com/GhaziBenDahmane/harness/tools"
)

// oneSandbox hands out the same sandbox for every thread.
type oneSandbox struct{ sb sandbox.Sandbox }

func (m oneSandbox) Enabled() bool { return true }
func (m oneSandbox) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	return m.sb, nil
}
func (m oneSandbox) Get(context.Context, string) (sandbox.Sandbox, error) {
	if m.sb == nil {
		return nil, sandbox.ErrNoSandbox
	}
	return m.sb, nil
}
func (m oneSandbox) Kill(context.Context, string) error         { return nil }
func (m oneSandbox) Invalidate(context.Context, string, string) {}

func TestSpillCreatesItsDirectoryItself(t *testing.T) {
	// A large tool output is spilled even when /workspace/.work/tool-output/ is missing and the
	// provider's upload does not create parent directories (a sandbox from before the layout, a wiped .work).
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sb.SetWriteErr(func(p string) error {
		if !sb.HasDir(path.Dir(p)) {
			return sandbox.ErrNotFound
		}
		return nil
	})
	sp := tools.SandboxSpiller{Sandboxes: oneSandbox{sb}}

	// Act
	p, err := sp.SpillOutput(context.Background(), tools.RunContext{ThreadID: "t", Login: "alice"}, "call/1", "big output")

	// Assert
	got, ok := sb.File(p)
	if err != nil || !strings.HasPrefix(p, "/workspace/.work/tool-output/call_1-") || !ok || got != "big output" {
		t.Fatalf("path %q err %v content %q (%v)", p, err, got, ok)
	}
}

// noSandbox is a manager whose thread has no running sandbox; Acquire would create one.
type noSandbox struct{ acquired *int }

func (m noSandbox) Enabled() bool { return true }
func (m noSandbox) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	*m.acquired++
	return sandboxtest.NewSandbox("created"), nil
}
func (m noSandbox) Get(context.Context, string) (sandbox.Sandbox, error) {
	return nil, sandbox.ErrNoSandbox
}
func (m noSandbox) Kill(context.Context, string) error         { return nil }
func (m noSandbox) Invalidate(context.Context, string, string) {}

func TestSpillNeverCreatesASandbox(t *testing.T) {
	// A large result in a thread without a running sandbox (an MCP or web search result) is not spilled: no sandbox is created for it, the agent cuts it instead.
	// Arrange
	acquired := 0
	sp := tools.SandboxSpiller{Sandboxes: noSandbox{acquired: &acquired}}

	// Act
	p, err := sp.SpillOutput(context.Background(), tools.RunContext{ThreadID: "t", Login: "alice"}, "call_1", "big output")

	// Assert
	if !errors.Is(err, tools.ErrNoSpill) || p != "" || acquired != 0 {
		t.Fatalf("path %q err %v acquired %d", p, err, acquired)
	}
}

func TestSpillsOfTheSameCallIDGoToDistinctFiles(t *testing.T) {
	// A provider reusing a tool call id never overwrites an earlier spilled output.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sp := tools.SandboxSpiller{Sandboxes: oneSandbox{sb}}
	rc := tools.RunContext{ThreadID: "t", Login: "alice"}

	// Act
	first, err1 := sp.SpillOutput(context.Background(), rc, "call_1", "first output")
	second, err2 := sp.SpillOutput(context.Background(), rc, "call_1", "second output")

	// Assert
	a, _ := sb.File(first)
	b, _ := sb.File(second)
	if err1 != nil || err2 != nil || first == second || a != "first output" || b != "second output" {
		t.Fatalf("paths %q %q (%v %v) contents %q %q", first, second, err1, err2, a, b)
	}
}
