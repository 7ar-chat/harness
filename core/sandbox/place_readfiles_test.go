package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func TestWriteFileNoFollowPlacesTheFileAndLeavesNoStagedUpload(t *testing.T) {
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")

	// Act
	err := sandbox.WriteFileNoFollow(context.Background(), sb, "/workspace/out/a.txt", []byte("data"))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := sb.File("/workspace/out/a.txt"); got != "data" {
		t.Errorf("target = %q", got)
	}
	if left := sb.Paths(sandbox.StagingDir); len(left) != 0 {
		t.Errorf("staged uploads left behind: %v", left)
	}
}

func TestWriteFileNoFollowRefusesSymlinksAndNamesTheRealDirectory(t *testing.T) {
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sb.Symlink("/etc", "/workspace/link")
	sb.Put("/workspace/real/f.txt", "orig")
	sb.Symlink("/workspace/real/f.txt", "/workspace/real/alias")

	// Act
	viaDir := sandbox.WriteFileNoFollow(context.Background(), sb, "/workspace/link/passwd", []byte("x"))
	onLink := sandbox.WriteFileNoFollow(context.Background(), sb, "/workspace/real/alias", []byte("x"))

	// Assert
	var se *sandbox.SymlinkError
	if !errors.As(viaDir, &se) || se.RealDir != "/etc" || !errors.Is(viaDir, sandbox.ErrSymlinkTarget) {
		t.Fatalf("through a symlinked directory: %v", viaDir)
	}
	if !strings.Contains(viaDir.Error(), "/etc") || !strings.Contains(viaDir.Error(), sandbox.ErrSymlinkTarget.Error()) {
		t.Errorf("message %q lacks the real directory or the reason", viaDir.Error())
	}
	if !errors.Is(onLink, sandbox.ErrSymlinkTarget) {
		t.Errorf("onto a symlink: %v", onLink)
	}
	if got, _ := sb.File("/workspace/real/f.txt"); got != "orig" {
		t.Errorf("the symlink target was overwritten: %q", got)
	}
	if left := sb.Paths(sandbox.StagingDir); len(left) != 0 {
		t.Errorf("staged uploads left behind after a refusal: %v", left)
	}
}

func TestWriteFileNoFollowReportsTransportFailuresAndCleansUp(t *testing.T) {
	boom := errors.New("exec transport down")
	for _, c := range []struct {
		name  string
		setup func(sb *sandboxtest.Sandbox)
	}{
		{"staging upload fails", func(sb *sandboxtest.Sandbox) { sb.SetWriteErr(func(string) error { return boom }) }},
		{"the place script cannot run", func(sb *sandboxtest.Sandbox) { sb.FailScript(sandbox.PlaceMarker, boom) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			sb := sandboxtest.NewSandbox("sbx")
			c.setup(sb)

			// Act
			err := sandbox.WriteFileNoFollow(context.Background(), sb, "/workspace/a.txt", []byte("x"))

			// Assert
			if !errors.Is(err, boom) {
				t.Errorf("err = %v, want the transport error", err)
			}
			if _, ok := sb.File("/workspace/a.txt"); ok {
				t.Error("the target was written despite the failure")
			}
			if left := sb.Paths(sandbox.StagingDir); len(left) != 0 {
				t.Errorf("staged upload left behind: %v", left)
			}
		})
	}
}

func TestWriteFileNoFollowTurnsEveryUnexpectedExitCodeIntoAnError(t *testing.T) {
	// A script that exits 3 (could not move) or anything else but 0/4 is a failure naming the code, and a refused
	// move is not retried as a symlink error.
	for _, code := range []int{1, 3, 9} {
		// Arrange
		sb := &exitSandbox{Sandbox: sandboxtest.NewSandbox("sbx"), code: code}

		// Act
		err := sandbox.WriteFileNoFollow(context.Background(), sb, "/workspace/a.txt", []byte("x"))

		// Assert
		if err == nil || errors.Is(err, sandbox.ErrSymlinkTarget) || !strings.Contains(err.Error(), "failed (exit ") {
			t.Errorf("exit %d: %v", code, err)
		}
	}
}

// exitSandbox answers every script with a fixed exit code.
type exitSandbox struct {
	sandbox.Sandbox
	code int
}

func (e *exitSandbox) Exec(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{ExitCode: e.code}, nil
}

func TestWriteFileNoFollowCleansUpEvenWhenTheCallerIsCancelled(t *testing.T) {
	// The unstage after a failed write must not be skipped because the caller's context ended.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	ctx, cancel := context.WithCancel(context.Background())
	sb.FailScript(sandbox.PlaceMarker, context.Canceled)
	cancel()

	// Act
	err := sandbox.WriteFileNoFollow(ctx, sb, "/workspace/a.txt", []byte("x"))

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if left := sb.Paths(sandbox.StagingDir); len(left) != 0 {
		t.Errorf("staged upload left behind: %v", left)
	}
}

func TestReadRegularFilesRefusesPathsThatAreNotCleanAbsolute(t *testing.T) {
	// Nothing runs for a path that could smuggle another file into the script.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sb.Put("/workspace/ok.txt", "x")

	for _, p := range []string{"relative.txt", "/workspace/../etc/passwd", "/workspace//a", "/workspace/a\nb", "/workspace/a\x00b", "/workspace/"} {
		// Act
		files, err := sandbox.ReadRegularFiles(context.Background(), sb, []string{"/workspace/ok.txt", p}, nil, 100)

		// Assert
		if err == nil || files != nil {
			t.Errorf("%q accepted: %v %v", p, files, err)
		}
	}
	if len(sb.Execs()) != 0 {
		t.Errorf("%d commands ran", len(sb.Execs()))
	}
}

func TestReadRegularFilesWithoutPathsDoesNothing(t *testing.T) {
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")

	// Act
	files, err := sandbox.ReadRegularFiles(context.Background(), sb, nil, nil, 100)

	// Assert
	if files != nil || err != nil {
		t.Fatalf("%v %v", files, err)
	}
}

// printing is a sandbox whose read-files script prints stdout and exits with code.
type printing struct {
	sandbox.Sandbox
	stdout string
	code   int
	err    error
}

func (p *printing) Exec(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if p.err != nil {
		return sandbox.ExecResult{}, p.err
	}
	req.OnStdout(p.stdout)
	return sandbox.ExecResult{ExitCode: p.code}, nil
}

func TestReadRegularFilesIgnoresMalformedLinesAndReportsScriptFailures(t *testing.T) {
	// Arrange
	boom := errors.New("transport down")
	good := "0 3 100 F YWJj\n"
	cases := []struct {
		name    string
		sb      *printing
		want    int // files returned
		wantErr bool
	}{
		{"valid line", &printing{stdout: good}, 1, false},
		{"too few fields", &printing{stdout: "0 3 100\n"}, 0, false},
		{"non numeric index", &printing{stdout: "x 3 100 F YWJj\n"}, 0, false},
		{"index out of range", &printing{stdout: "5 3 100 F YWJj\n-1 3 100 F YWJj\n"}, 0, false},
		{"bad base64 is skipped, the good line kept", &printing{stdout: "0 3 100 F !!!\n1 3 100 F YWJj\n"}, 1, false},
		{"non zero exit", &printing{stdout: good, code: 2}, 0, true},
		{"exec error", &printing{err: boom}, 0, true},
	}
	for _, c := range cases {
		c.sb.Sandbox = sandboxtest.NewSandbox("sbx")

		// Act
		files, err := sandbox.ReadRegularFiles(context.Background(), c.sb, []string{"/workspace/a", "/workspace/b"}, nil, 100)

		// Assert
		if (err != nil) != c.wantErr || len(files) != c.want {
			t.Errorf("%s: %d files, err %v", c.name, len(files), err)
		}
	}
}

func TestReadRegularFilesParsesStampsDataAndUnchangedMarkers(t *testing.T) {
	// Arrange
	sb := &printing{Sandbox: sandboxtest.NewSandbox("sbx"), stdout: "0 3 100 F YWJj\n1 7 200 U -\n2 0 300 F -\n"}

	// Act
	files, err := sandbox.ReadRegularFiles(context.Background(), sb, []string{"/workspace/a", "/workspace/b", "/workspace/c"}, nil, 100)

	// Assert
	if err != nil || len(files) != 3 {
		t.Fatalf("%v %v", files, err)
	}
	if files[0].Path != "/workspace/a" || files[0].Stamp != "3 100" || string(files[0].Data) != "abc" || files[0].Unchanged {
		t.Errorf("file 0 = %+v", files[0])
	}
	if files[1].Path != "/workspace/b" || !files[1].Unchanged || files[1].Data != nil || files[1].Stamp != "7 200" {
		t.Errorf("file 1 = %+v", files[1])
	}
	if files[2].Unchanged || files[2].Data != nil {
		t.Errorf("file 2 (empty content marker) = %+v", files[2])
	}
}

func TestReadRegularFilesFakeEndToEnd(t *testing.T) {
	// The protocol the fake sandbox answers is the one ReadRegularFiles speaks: known stamps skip the content.
	// Arrange
	sb := sandboxtest.NewSandbox("sbx")
	sb.Put("/workspace/a.txt", "hello")
	first, err := sandbox.ReadRegularFiles(context.Background(), sb, []string{"/workspace/a.txt", "/workspace/missing"}, nil, 1000)
	if err != nil || len(first) != 1 {
		t.Fatalf("first read %v %v", first, err)
	}

	// Act
	again, err := sandbox.ReadRegularFiles(context.Background(), sb, []string{"/workspace/a.txt"}, []string{first[0].Stamp}, 1000)

	// Assert
	if err != nil || len(again) != 1 || !again[0].Unchanged || again[0].Data != nil {
		t.Fatalf("second read %+v %v", again, err)
	}
}
