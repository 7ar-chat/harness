package approvals_test

import (
	"context"
	"errors"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/approvals"
)

// threadManager hands out the sandbox of a thread, or fails with err.
type threadManager struct {
	sb  sandbox.Sandbox
	err error
}

func (m threadManager) Enabled() bool { return true }
func (m threadManager) Acquire(context.Context, string, string) (sandbox.Sandbox, error) {
	return m.sb, m.err
}
func (m threadManager) Get(context.Context, string) (sandbox.Sandbox, error) { return m.sb, m.err }
func (m threadManager) Kill(context.Context, string) error                   { return nil }
func (m threadManager) Invalidate(context.Context, string, string)           {}

// fixedResolver answers every RealPaths call with real and err.
type fixedResolver struct {
	real []string
	err  error
}

func (r fixedResolver) RealPaths(context.Context, *store.Run, []string) ([]string, error) {
	return r.real, r.err
}

func interactiveRun() *store.Run {
	return &store.Run{ID: "r1", ThreadID: "t1", InteractionMode: store.InteractionInteractive}
}

func TestGateResolvesSymlinksOfTheWorkAreaInTheSandbox(t *testing.T) {
	// A work-area directory that became a symlink to the project is not a work area any more: the write asks.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sb.Symlink("/workspace/src", "/workspace/outputs/escape")
	gate := &approvals.Gate{Resolver: approvals.SandboxResolver{Sandboxes: threadManager{sb: sb}}}
	run := interactiveRun()

	// Act
	throughLink := gate.NeedsApproval(context.Background(), run, "write_file", `{"path":"/workspace/outputs/escape/main.go","content":"x"}`)
	plain := gate.NeedsApproval(context.Background(), run, "write_file", `{"path":"/workspace/outputs/report.md","content":"x"}`)

	// Assert
	if !throughLink || plain {
		t.Fatalf("through symlink %v, plain work-area file %v", throughLink, plain)
	}
	if sb.RealPathCalls() != 2 {
		t.Fatalf("realpath ran %d times, want one per decision (never cached)", sb.RealPathCalls())
	}
}

func TestGateChecksEveryPathOfADeleteAgainstTheirRealLocation(t *testing.T) {
	// One path of several leading out of the work areas makes the whole call ask.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sb.Symlink("/etc", "/workspace/.work/etc")
	gate := &approvals.Gate{Resolver: approvals.SandboxResolver{Sandboxes: threadManager{sb: sb}}}
	run := interactiveRun()

	// Act
	clean := gate.NeedsApproval(context.Background(), run, "delete_files", `{"paths":["/workspace/outputs/a","/workspace/.work/b"]}`)
	mixed := gate.NeedsApproval(context.Background(), run, "delete_files", `{"paths":["/workspace/outputs/a","/workspace/.work/etc/passwd"]}`)

	// Assert
	if clean || !mixed {
		t.Fatalf("all in work areas %v, one through a symlink %v", clean, mixed)
	}
}

func TestSandboxResolverWithoutASandboxKeepsTheLexicalPaths(t *testing.T) {
	// The gate never starts a sandbox: without one the paths are their own real paths.
	// Arrange
	gate := &approvals.Gate{Resolver: approvals.SandboxResolver{Sandboxes: threadManager{err: sandbox.ErrNoSandbox}}}
	run := interactiveRun()

	// Act
	inArea := gate.NeedsApproval(context.Background(), run, "write_file", `{"path":"outputs/a.md","content":"x"}`)
	outside := gate.NeedsApproval(context.Background(), run, "write_file", `{"path":"src/a.md","content":"x"}`)

	// Assert
	if inArea || !outside {
		t.Fatalf("work-area write %v, other write %v", inArea, outside)
	}
}

func TestGateFailsClosedWhenThePathsCannotBeResolved(t *testing.T) {
	// A sandbox that cannot be reached, a resolver that fails or one that answers the wrong number of paths all make the work-area write ask.
	// Arrange
	dead := sandboxtest.NewSandbox("dead")
	dead.Die()
	args := `{"path":"/workspace/outputs/a.md","content":"x"}`
	cases := map[string]approvals.PathResolver{
		"sandbox lookup fails": approvals.SandboxResolver{Sandboxes: threadManager{err: errors.New("provider down")}},
		"sandbox is gone":      approvals.SandboxResolver{Sandboxes: threadManager{sb: dead}},
		"resolver fails":       fixedResolver{err: errors.New("realpath failed")},
		"too few answers":      fixedResolver{real: nil},
	}
	for name, resolver := range cases {
		t.Run(name, func(t *testing.T) {
			gate := &approvals.Gate{Resolver: resolver}

			// Act
			got := gate.NeedsApproval(context.Background(), interactiveRun(), "write_file", args)

			// Assert
			if !got {
				t.Fatal("an unresolvable path must ask")
			}
		})
	}
}

func TestGateTrustsAResolverOnlyForPathsInTheWorkAreas(t *testing.T) {
	// The resolver is consulted only for paths that look like work areas lexically; its answer is cleaned before the check.
	// Arrange
	run := interactiveRun()
	args := `{"path":"/workspace/outputs/a.md","content":"x"}`

	// Act
	same := (&approvals.Gate{Resolver: fixedResolver{real: []string{"/workspace/outputs/a.md"}}}).NeedsApproval(context.Background(), run, "write_file", args)
	escaped := (&approvals.Gate{Resolver: fixedResolver{real: []string{"/workspace/outputs/../src/a.md"}}}).NeedsApproval(context.Background(), run, "write_file", args)
	lexicalEscape := (&approvals.Gate{Resolver: fixedResolver{real: []string{"/workspace/outputs/a.md"}}}).NeedsApproval(context.Background(), run, "write_file", `{"path":"/etc/a.md"}`)

	// Assert
	if same || !escaped || !lexicalEscape {
		t.Fatalf("same %v, resolved outside %v, lexically outside %v", same, escaped, lexicalEscape)
	}
}

func TestGateAsksForMalformedPathArguments(t *testing.T) {
	// Anything the gate cannot read as "a string, or a list of strings, per path field" asks, even when a work-area path is present.
	// Arrange
	gate := &approvals.Gate{}
	run := interactiveRun()
	cases := map[string]string{
		"empty arguments":     ``,
		"not an object":       `["/workspace/outputs/a"]`,
		"a bare string":       `"/workspace/outputs/a"`,
		"truncated":           `{"path":"/workspace/outputs/a"`,
		"truncated key":       `{"path"`,
		"trailing data":       `{"path":"/workspace/outputs/a"} {"path":"/etc/x"}`,
		"number path":         `{"path":5}`,
		"object path":         `{"path":{"x":"/workspace/outputs/a"}}`,
		"non-string in list":  `{"paths":["/workspace/outputs/a",3]}`,
		"empty path":          `{"path":""}`,
		"blank path":          `{"path":"   "}`,
		"NUL in path":         `{"path":"/workspace/outputs/a\u0000/../../x"}`,
		"no path at all":      `{"content":"x"}`,
		"only null paths":     `{"path":null}`,
		"empty list of paths": `{"paths":[]}`,
		"second path outside": `{"path":"/workspace/outputs/a","to":"/workspace/src/b"}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got := gate.NeedsApproval(context.Background(), run, "write_file", args)

			// Assert
			if !got {
				t.Fatalf("NeedsApproval(%s) = false, want true", args)
			}
		})
	}
}

func TestGateReadsPathListsAndSkipsNullAndOtherFields(t *testing.T) {
	// Paths come from every path field (strings and lists); null fields and fields that name no path are ignored.
	// Arrange
	gate := &approvals.Gate{}
	run := interactiveRun()
	cases := map[string]string{
		"list of work-area paths":    `{"paths":["/workspace/outputs/a","outputs/b","/workspace/.work/c"]}`,
		"null path beside a list":    `{"path":null,"paths":["/workspace/outputs/a"]}`,
		"other fields are not paths": `{"path":"outputs/a","content":"/etc/passwd","mode":755,"flags":["x"]}`,
		"padded and relative":        `{"path":"  outputs/./sub/../a.md  "}`,
		"source and destination":     `{"source":"/workspace/outputs/a","destination":"/workspace/.work/b"}`,
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got := gate.NeedsApproval(context.Background(), run, "move_file", args)

			// Assert
			if got {
				t.Fatalf("NeedsApproval(%s) = true, want false", args)
			}
		})
	}
}
