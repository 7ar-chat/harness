package tools_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

func TestSpillPathIsSafeAndUnique(t *testing.T) {
	// The call id becomes a file name that cannot escape the spill directory, is bounded, never empty, and never reused.
	// Arrange
	suffix := `-[0-9a-f]{8}\.txt$`
	cases := []struct {
		name, id string
		want     string
	}{
		{"plain", "call_1", `^call_1`},
		{"slashes and spaces", "a/b c", `^a_b_c`},
		{"traversal", "../../etc/passwd", `^\.\._\.\._etc_passwd`},
		{"dot", ".", `^output`},
		{"dotdot", "..", `^output`},
		{"empty", "", `^output`},
		{"unicode", "appel-é", `^appel-_`},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			p := tools.SpillPath(tt.id)

			// Assert
			rest, ok := strings.CutPrefix(p, tools.SpillDir)
			if !ok || strings.Contains(rest, "/") || !regexp.MustCompile(tt.want+`.*`+suffix).MatchString(rest) {
				t.Fatalf("SpillPath(%q) = %q", tt.id, p)
			}
		})
	}
	long := tools.SpillPath(strings.Repeat("x", 500))
	if name := strings.TrimPrefix(long, tools.SpillDir); len(name) != 80+len("-12345678.txt") {
		t.Errorf("long id gives a %d-byte name %q", len(name), name)
	}
	if a, b := tools.SpillPath("same"), tools.SpillPath("same"); a == b {
		t.Errorf("two spills of one id share the path %q", a)
	}
}

// offManager is a sandbox manager that is disabled; any use of its sandboxes fails the test.
type offManager struct{ t *testing.T }

func (m offManager) Enabled() bool { return false }
func (m offManager) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	m.t.Error("Acquire called")
	return nil, nil
}
func (m offManager) Get(context.Context, string) (sandbox.Sandbox, error) {
	m.t.Error("Get called")
	return nil, nil
}
func (m offManager) Kill(context.Context, string) error         { return nil }
func (m offManager) Invalidate(context.Context, string, string) {}

// failingManager cannot look the sandbox up.
type failingManager struct{ oneSandbox }

func (failingManager) Get(context.Context, string) (sandbox.Sandbox, error) {
	return nil, errors.New("provider unreachable")
}

func TestSpillIsRefusedWhereNoSandboxMayBeUsed(t *testing.T) {
	// No manager, a disabled one, or a run with sandbox "off" never touches a sandbox: the output is cut instead.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	cases := map[string]struct {
		sp tools.SandboxSpiller
		rc tools.RunContext
	}{
		"no manager":      {tools.SandboxSpiller{}, tools.RunContext{ThreadID: "t"}},
		"disabled":        {tools.SandboxSpiller{Sandboxes: offManager{t}}, tools.RunContext{ThreadID: "t"}},
		"sandbox off run": {tools.SandboxSpiller{Sandboxes: oneSandbox{sb}}, tools.RunContext{ThreadID: "t", Sandbox: tools.SandboxOff}},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			p, err := tt.sp.SpillOutput(context.Background(), tt.rc, "call_1", "big output")

			// Assert
			if !errors.Is(err, tools.ErrNoSpill) || p != "" {
				t.Fatalf("path %q err %v", p, err)
			}
		})
	}
	if got := sb.Paths(tools.SpillDir); len(got) != 0 {
		t.Fatalf("a refused spill wrote %v", got)
	}
}

func TestSpillReportsSandboxFailuresInsteadOfCuttingSilently(t *testing.T) {
	// A sandbox that exists but cannot be reached, listed or written is a real error, distinct from "no sandbox".
	// Arrange
	dead := sandboxtest.NewSandbox("dead")
	dead.Die()
	denied := sandboxtest.NewSandbox("denied")
	denied.SetWriteErr(func(string) error { return errors.New("disk full") })
	rc := tools.RunContext{ThreadID: "t"}
	cases := map[string]struct {
		sp   tools.SandboxSpiller
		want string
	}{
		"lookup fails":       {tools.SandboxSpiller{Sandboxes: failingManager{}}, "provider unreachable"},
		"mkdir fails (gone)": {tools.SandboxSpiller{Sandboxes: oneSandbox{dead}}, "SANDBOX_NOT_FOUND"},
		"write fails (full)": {tools.SandboxSpiller{Sandboxes: oneSandbox{denied}}, "disk full"},
	}
	for name, tt := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			p, err := tt.sp.SpillOutput(context.Background(), rc, "call_1", "big output")

			// Assert
			if err == nil || errors.Is(err, tools.ErrNoSpill) || !strings.Contains(err.Error(), tt.want) || p != "" {
				t.Fatalf("path %q err %v, want an error with %q", p, err, tt.want)
			}
		})
	}
}
