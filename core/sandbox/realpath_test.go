package sandbox_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// lineSandbox delivers the output of the wrapped sandbox's commands the way
// the OpenSandbox adapter does (one newline-terminated message per line).
type lineSandbox struct{ sandbox.Sandbox }

func (l lineSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	req.OnStdout, req.OnStderr = sandboxtest.LineMessages(req.OnStdout), sandboxtest.LineMessages(req.OnStderr)
	return l.Sandbox.Exec(ctx, req)
}

// shells are the shells the real realpath script is checked with (the
// sandbox image's sh is dash).
var shells = []string{"bash", "sh"}

// hostSandbox runs the real realpath script with shell under a temporary
// root, its output delivered like OpenSandbox's.
func hostSandbox(t *testing.T, shell string) (*sandboxtest.HostSandbox, sandbox.Sandbox, string) {
	t.Helper()
	for _, tool := range []string{shell, "realpath", "tr"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := sandboxtest.NewHostSandbox("host", root)
	host.Shell = shell
	return host, lineSandbox{host}, root
}

// requireGNURealpath skips the test when realpath lacks GNU coreutils' -m and
// -z (BusyBox, in the CI image; the sandbox image is Debian): the real script
// would fail there whatever its input.
func requireGNURealpath(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("realpath", "-m", "-z", "--", "/").Output(); err != nil || string(out) != "/\x00" {
		t.Skip("realpath does not support -m -z (not GNU coreutils)")
	}
}

// scripted is a sandbox whose realpath script prints stdout and exits with exit.
type scripted struct {
	sandbox.Sandbox
	stdout string
	exit   int
	calls  int
}

func (s *scripted) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	s.calls++
	if req.OnStdout != nil {
		req.OnStdout(s.stdout)
	}
	return sandbox.ExecResult{ExitCode: s.exit}, nil
}

func TestRealPathsResolvesOutputDeliveredLineByLine(t *testing.T) {
	// Through the OpenSandbox transport (a newline appended to every output message), one path and several paths resolve exactly, symlinks followed (ADISTU-6764).
	// Arrange
	sb := sandboxtest.NewSandbox("s")
	sb.SetLineTransport(true)
	sb.Put("/workspace/.work/renders/x/p7.png", "png")
	sb.Symlink("/workspace/src", "/workspace/outputs/link")
	ctx := context.Background()

	// Act
	one, errOne := sandbox.RealPath(ctx, sb, "/workspace/.work/renders/x/p7.png")
	many, errMany := sandbox.RealPaths(ctx, sb, []string{"/workspace/outputs/a.md", "/workspace/outputs/link/b.py", "/workspace/missing/../c"})

	// Assert
	if errOne != nil || one != "/workspace/.work/renders/x/p7.png" {
		t.Fatalf("one %q %v", one, errOne)
	}
	want := []string{"/workspace/outputs/a.md", "/workspace/src/b.py", "/workspace/c"}
	if errMany != nil || !slices.Equal(many, want) {
		t.Fatalf("many %q %v", many, errMany)
	}
}

func TestRealPathsOfTheRealScriptResolveOutputDeliveredLineByLine(t *testing.T) {
	// The real script under bash and sh with the real realpath, its output delivered like OpenSandbox's, resolves one path and several paths (existing, missing, through a symlink) (ADISTU-6764).
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			// Arrange
			_, sb, root := hostSandbox(t, shell)
			requireGNURealpath(t)
			ws := filepath.Join(root, "workspace")
			for _, d := range []string{"src", "outputs"} {
				if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(ws, "outputs", "a.md"), []byte("a"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(ws, "src"), filepath.Join(ws, "outputs", "link")); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()

			// Act
			one, errOne := sandbox.RealPath(ctx, sb, "/workspace/outputs/a.md")
			many, errMany := sandbox.RealPaths(ctx, sb, []string{"/workspace/outputs/a.md", "/workspace/outputs/link/b.py", "/workspace/missing/x/../c"})

			// Assert
			if errOne != nil || one != "/workspace/outputs/a.md" {
				t.Fatalf("one %q %v", one, errOne)
			}
			want := []string{"/workspace/outputs/a.md", "/workspace/src/b.py", "/workspace/missing/c"}
			if errMany != nil || !slices.Equal(many, want) {
				t.Fatalf("many %q %v", many, errMany)
			}
		})
	}
}

func TestRealPathsOfTheRealScriptFailsClosedWhenRealpathFails(t *testing.T) {
	// A realpath that prints every result but exits non-zero fails with ErrRealPath under bash and sh: the pipe hides its exit status, the missing success marker does not.
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			// Arrange
			host, sb, root := hostSandbox(t, shell)
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			stub := "#!/bin/sh\nshift 3\nfor a; do printf '%s\\000' \"$a\"; done\nexit 1\n"
			if err := os.WriteFile(filepath.Join(bin, "realpath"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			host.Env = map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")}

			// Act
			_, err := sandbox.RealPaths(context.Background(), sb, []string{"/workspace/outputs/a", "/workspace/outputs/b"})

			// Assert
			if !errors.Is(err, sandbox.ErrRealPath) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestRealPathsFailsClosedOnAResolvedPathWithANewlineOrACarriageReturn(t *testing.T) {
	// A symlink whose target holds a newline (inside or at the end of a name) or a carriage return fails with ErrRealPath under bash and sh instead of resolving to a cut or altered path.
	for _, shell := range shells {
		t.Run(shell, func(t *testing.T) {
			// Arrange
			_, sb, root := hostSandbox(t, shell)
			requireGNURealpath(t)
			ws := filepath.Join(root, "workspace")
			links := map[string]string{"inner": "outputs\nx", "trailing": "evil\n", "cr": "evil\r"}
			for link, target := range links {
				if err := os.MkdirAll(filepath.Join(ws, target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(ws, target), filepath.Join(ws, link)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()

			// Act
			_, errInner := sandbox.RealPaths(ctx, sb, []string{"/workspace/inner/a"})
			_, errTrailing := sandbox.RealPaths(ctx, sb, []string{"/workspace/trailing", "/workspace/outputs/a"})
			_, errCR := sandbox.RealPaths(ctx, sb, []string{"/workspace/cr"})

			// Assert
			if !errors.Is(errInner, sandbox.ErrRealPath) || !errors.Is(errTrailing, sandbox.ErrRealPath) || !errors.Is(errCR, sandbox.ErrRealPath) {
				t.Fatalf("inner %v trailing %v cr %v", errInner, errTrailing, errCR)
			}
		})
	}
}

func TestRealPathsRejectsPathsItCannotPassSafely(t *testing.T) {
	// Relative paths and paths holding a NUL, a newline or a carriage return fail with ErrRealPath without running anything.
	// Arrange
	sb := &scripted{}

	// Act
	var errs []error
	for _, p := range []string{"workspace/a", "/workspace/a\x00b", "/workspace/a\nb", "/workspace/a\n", "/workspace/a\rb"} {
		_, err := sandbox.RealPaths(context.Background(), sb, []string{"/workspace/ok", p})
		errs = append(errs, err)
	}

	// Assert
	for i, err := range errs {
		if !errors.Is(err, sandbox.ErrRealPath) {
			t.Fatalf("path %d: %v", i, err)
		}
	}
	if sb.calls != 0 {
		t.Fatalf("%d commands ran", sb.calls)
	}
}

func TestRealPathsFailsClosedOnUnexpectedOutput(t *testing.T) {
	// Missing or misplaced success marker, too few or too many results, a relative result, a result holding a NUL or a carriage return, or a failed script are ErrRealPath.
	// Arrange
	cases := []scripted{
		{stdout: "/workspace/a\n/workspace/b\n"},
		{stdout: "ok\n/workspace/a\n/workspace/b\n"},
		{stdout: "/workspace/a\nok\n"},
		{stdout: "/workspace/a\n/workspace/b\n/workspace/c\nok\n"},
		{stdout: "/workspace/a\nrelative\nok\n"},
		{stdout: "/workspace/a\x00\n/workspace/b\nok\n"},
		{stdout: "/workspace/a\r\n/workspace/b\nok\n"},
		{stdout: "/workspace/a\n/workspace/b\nok\n", exit: 1},
	}

	// Act
	var errs []error
	for i := range cases {
		_, err := sandbox.RealPaths(context.Background(), &cases[i], []string{"/workspace/a", "/workspace/b"})
		errs = append(errs, err)
	}

	// Assert
	for i, err := range errs {
		if !errors.Is(err, sandbox.ErrRealPath) {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}

func TestRealPathsReportsASymlinkEscapingTheWorkspace(t *testing.T) {
	// A path through a symlink out of /workspace resolves to its real target (callers then refuse it).
	// Arrange
	sb := sandboxtest.NewSandbox("s")
	sb.SetLineTransport(true)
	sb.Symlink("/etc", "/workspace/outputs/x")

	// Act
	got, err := sandbox.RealPath(context.Background(), sb, "/workspace/outputs/x/passwd")

	// Assert
	if err != nil || got != "/etc/passwd" {
		t.Fatalf("got %q %v", got, err)
	}
}
