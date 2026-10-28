//go:build unix

package local_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
	"github.com/GhaziBenDahmane/harness/sandbox/local"
)

func newSandbox(t *testing.T) (sandbox.Instance, string) {
	t.Helper()
	ws := t.TempDir()
	b, err := local.New(local.Options{Workspace: ws, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := b.Create(context.Background(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return sb, ws
}

func run(t *testing.T, sb sandbox.Sandbox, cmd string) (string, int) {
	t.Helper()
	var out strings.Builder
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: cmd, OnStdout: func(s string) { out.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), res.ExitCode
}

func TestCommandsSeeWorkspaceAsSlashWorkspace(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	if err := sb.WriteFile(ctx, "/workspace/src/a.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}

	// Act
	out, code := run(t, sb, "cat /workspace/src/a.txt; pwd")

	// Assert
	if code != 0 || out != "hi/workspace\n" {
		t.Fatalf("got %q exit %d", out, code)
	}
	if _, err := os.Stat(filepath.Join(ws, "src", "a.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestFileAPIRefusesPathsOutsideTheSandbox(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	if err := os.Symlink("/etc", filepath.Join(ws, "etc-link")); err != nil {
		t.Fatal(err)
	}

	// Act
	_, _, errAbs := sb.ReadFile(ctx, "/etc/passwd", 10)
	_, _, errLink := sb.ReadFile(ctx, "/workspace/etc-link/passwd", 10)
	_, _, errDots := sb.ReadFile(ctx, "/workspace/../../etc/passwd", 10)

	// Assert
	for name, err := range map[string]error{"absolute": errAbs, "symlink": errLink, "dotdot": errDots} {
		if err == nil {
			t.Errorf("%s: read succeeded", name)
		}
	}
}

func TestMissingFileIsErrNotFound(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)

	// Act
	_, _, err := sb.ReadFile(context.Background(), "/workspace/nope", 10)

	// Assert
	if err != sandbox.ErrNotFound {
		t.Fatalf("got %v", err)
	}
}

func TestWriteFileNoFollowPlacesAndRefusesSymlinks(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	if err := os.Symlink("/etc/hosts", filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	// Act
	okErr := sandbox.WriteFileNoFollow(ctx, sb, "/workspace/dir/new.txt", []byte("x"))
	linkErr := sandbox.WriteFileNoFollow(ctx, sb, "/workspace/link", []byte("x"))

	// Assert
	if okErr != nil {
		t.Fatal(okErr)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "dir", "new.txt")); string(b) != "x" {
		t.Fatalf("content %q", b)
	}
	if linkErr == nil {
		t.Fatal("a write through a symlink succeeded")
	}
}

func TestRealPathsAndReadRegularFiles(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/a.txt", []byte("hello"))
	_ = os.Symlink(filepath.Join(ws, "a.txt"), filepath.Join(ws, "ln"))

	// Act
	real, err := sandbox.RealPaths(ctx, sb, []string{"/workspace/ln", "/workspace/missing/x"})
	files, ferr := sandbox.ReadRegularFiles(ctx, sb, []string{"/workspace/a.txt", "/workspace/ln"}, nil, 1<<20)

	// Assert
	if err != nil || real[0] != "/workspace/a.txt" || real[1] != "/workspace/missing/x" {
		t.Fatalf("real %v err %v", real, err)
	}
	if ferr != nil || len(files) != 1 || string(files[0].Data) != "hello" {
		t.Fatalf("files %+v err %v", files, ferr)
	}
}

func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)

	// Act
	start := time.Now()
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: "sleep 30 & sleep 30", Timeout: 300 * time.Millisecond})

	// Assert
	if err != nil || res.ExitCode != 124 || time.Since(start) > 5*time.Second {
		t.Fatalf("res %+v err %v after %v", res, err, time.Since(start))
	}
}

func TestBackgroundJobScriptsRunNatively(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	dir := "/workspace/.work/jobs/j1"
	env := map[string]string{"HARNESS_JOB_DIR": dir, "HARNESS_JOB_CMD_B64": b64("echo started; sleep 0.3; echo done; exit 3"), "HARNESS_JOB_WAIT": "0"}

	// Act
	var status strings.Builder
	script := func(marker string) string { return marker + "\n" }
	if _, err := sb.Exec(ctx, sandbox.ExecRequest{Command: script("# work:job-start"), Env: env}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(700 * time.Millisecond)
	_, err := sb.Exec(ctx, sandbox.ExecRequest{Command: script("# work:job-status"), Env: map[string]string{"HARNESS_JOB_DIR": dir, "HARNESS_HEAD": "100", "HARNESS_TAIL": "100"},
		OnStdout: func(s string) { status.WriteString(s) }})

	// Assert
	if err != nil || !strings.Contains(status.String(), "status finished\ncode 3\n") {
		t.Fatalf("status %q err %v", status.String(), err)
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestToolPathsPassedInTheEnvironmentAreMapped(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	var out strings.Builder

	// Act
	res, err := sb.Exec(context.Background(), sandbox.ExecRequest{Command: `ls -d "$HARNESS_PATH" && echo "$HARNESS_PATTERN"`,
		Env: map[string]string{"HARNESS_PATH": "/workspace", "HARNESS_PATTERN": "/workspace/x"}, OnStdout: func(s string) { out.WriteString(s) }})

	// Assert
	if err != nil || res.ExitCode != 0 || out.String() != "/workspace\n/workspace/x\n" {
		t.Fatalf("exit %d err %v out %q", res.ExitCode, err, out.String())
	}
}

func TestPreparingTheWorkspaceLeavesTheProjectClean(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)

	// Act
	err := sandbox.PrepareWorkspace(context.Background(), sb)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "outputs")); err == nil {
		t.Error("outputs/ was created in the project")
	}
	if b, _ := os.ReadFile(filepath.Join(ws, ".work", ".gitignore")); string(b) != "*\n" {
		t.Errorf(".work/.gitignore = %q", b)
	}
}
