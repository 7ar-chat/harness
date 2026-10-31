//go:build unix

package local_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/local"
	"github.com/7ar-chat/harness/core/sandbox"
)

func TestNewRefusesAMissingShell(t *testing.T) {
	// Arrange / Act
	b, err := local.New(local.Options{StateDir: t.TempDir(), Shell: "no-such-shell-for-harness"})

	// Assert
	if err == nil || b != nil {
		t.Fatalf("New = %v, %v; want an error", b, err)
	}
}

func TestNewCreatesTheWorkspaceAndStateDirectories(t *testing.T) {
	// Arrange
	root := t.TempDir()
	ws, state := filepath.Join(root, "proj", "ws"), filepath.Join(root, "state")

	// Act
	_, err := local.New(local.Options{Workspace: ws, StateDir: state})

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{ws, state} {
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Errorf("%s not created: %v", d, err)
		}
	}
}

func TestNewFailsWhenTheWorkspaceCannotBeCreated(t *testing.T) {
	// Arrange: a regular file where the workspace directory should go
	root := t.TempDir()
	blocker := filepath.Join(root, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := local.New(local.Options{Workspace: filepath.Join(blocker, "ws"), StateDir: filepath.Join(root, "state")})

	// Assert
	if err == nil {
		t.Fatal("New accepted a workspace below a regular file")
	}
}

func TestEachSandboxWithoutASharedWorkspaceGetsItsOwn(t *testing.T) {
	// Arrange
	b, err := local.New(local.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	one, _ := b.Create(ctx, sandbox.CreateOptions{})
	two, _ := b.Create(ctx, sandbox.CreateOptions{})

	// Act
	err = one.WriteFile(ctx, "/workspace/a.txt", []byte("one"))
	_, _, otherErr := two.ReadFile(ctx, "/workspace/a.txt", 10)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if one.ID() == two.ID() || !strings.HasPrefix(one.ID(), "local-") {
		t.Errorf("ids %q %q", one.ID(), two.ID())
	}
	if !errors.Is(otherErr, sandbox.ErrNotFound) {
		t.Errorf("a file written in one sandbox is visible in another: %v", otherErr)
	}
}

func TestSandboxesOfASharedWorkspaceSeeTheSameFiles(t *testing.T) {
	// Arrange
	ws := t.TempDir()
	b, err := local.New(local.Options{Workspace: ws, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	one, _ := b.Create(ctx, sandbox.CreateOptions{})
	two, _ := b.Create(ctx, sandbox.CreateOptions{})

	// Act
	if err := one.WriteFile(ctx, "/workspace/shared.txt", []byte("hi")); err != nil {
		t.Fatal(err)
	}
	data, _, err := two.ReadFile(ctx, "/workspace/shared.txt", 10)

	// Assert
	if err != nil || string(data) != "hi" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
}

func TestConnectFindsOnlyWhatCreateMade(t *testing.T) {
	// Arrange
	b, err := local.New(local.Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created, _ := b.Create(ctx, sandbox.CreateOptions{})
	_ = created.WriteFile(ctx, "/workspace/keep.txt", []byte("kept"))

	// Act
	again, err := b.Connect(ctx, created.ID())
	_, foreign := b.Connect(ctx, "opensandbox-1")
	_, unknown := b.Connect(ctx, "local-deadbeef")

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if data, _, err := again.ReadFile(ctx, "/workspace/keep.txt", 10); err != nil || string(data) != "kept" {
		t.Errorf("reconnected sandbox lost its files: %q, %v", data, err)
	}
	if !errors.Is(foreign, sandbox.ErrNoSandbox) || !errors.Is(unknown, sandbox.ErrNoSandbox) {
		t.Errorf("foreign %v unknown %v; want ErrNoSandbox", foreign, unknown)
	}
}

func TestKillForgetsTheSandboxButNeverTheSharedWorkspace(t *testing.T) {
	// Arrange
	ws := t.TempDir()
	state := t.TempDir()
	b, err := local.New(local.Options{Workspace: ws, StateDir: state})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sb, _ := b.Create(ctx, sandbox.CreateOptions{})
	_ = sb.WriteFile(ctx, "/workspace/project.txt", []byte("mine"))

	// Act
	err = b.Kill(ctx, sb.ID())
	_, connectErr := b.Connect(ctx, sb.ID())

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(connectErr, sandbox.ErrNoSandbox) {
		t.Errorf("a killed sandbox still connects: %v", connectErr)
	}
	if data, err := os.ReadFile(filepath.Join(ws, "project.txt")); err != nil || string(data) != "mine" {
		t.Fatalf("the user's project was deleted: %q, %v", data, err)
	}
}

func TestKillIgnoresIDsThatCouldEscapeTheStateDirectory(t *testing.T) {
	// Arrange
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	b, err := local.New(local.Options{StateDir: state})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	for _, id := range []string{"../victim", "local-../victim", "local-a/b", "victim", ""} {
		if err := b.Kill(context.Background(), id); err != nil {
			t.Errorf("Kill(%q) = %v, want a silent no-op", id, err)
		}
	}

	// Assert
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("a crafted id deleted a directory outside the state dir: %v", err)
	}
}

func TestInstanceLifecycleAnswers(t *testing.T) {
	// Arrange
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	b, err := local.New(local.Options{Workspace: ws, StateDir: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sb, _ := b.Create(ctx, sandbox.CreateOptions{})
	healthyBefore := sb.Healthy(ctx)

	// Act
	if err := os.RemoveAll(ws); err != nil {
		t.Fatal(err)
	}
	healthyAfter := sb.Healthy(ctx)

	// Assert
	if !healthyBefore || healthyAfter {
		t.Errorf("Healthy = %v then %v, want true then false once the workspace is gone", healthyBefore, healthyAfter)
	}
	if sb.Gone(ctx) {
		t.Error("a local sandbox never expires")
	}
	if err := sb.Renew(ctx, 0); err != nil {
		t.Errorf("Renew = %v", err)
	}
}

func TestPathsOutsideTheSandboxRootsAreRefusedByEveryFileOperation(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	outside := "/etc/hosts"

	// Act
	_, _, readErr := sb.ReadFile(ctx, outside, 10)
	writeErr := sb.WriteFile(ctx, outside, []byte("x"))
	mkdirErr := sb.MkdirAll(ctx, "/etc/newdir")
	_, statErr := sb.Stat(ctx, outside)
	_, listErr := sb.List(ctx, "/etc", 1)

	// Assert
	for name, err := range map[string]error{"read": readErr, "write": writeErr, "mkdir": mkdirErr, "stat": statErr, "list": listErr} {
		if err == nil || errors.Is(err, sandbox.ErrNotFound) || !strings.Contains(err.Error(), "outside the local sandbox") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRelativePathsAreRelativeToTheWorkspace(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()

	// Act
	err := sb.WriteFile(ctx, "notes/today.md", []byte("x"))
	fi, statErr := sb.Stat(ctx, "notes/today.md")

	// Assert
	if err != nil || statErr != nil {
		t.Fatal(err, statErr)
	}
	if _, err := os.Stat(filepath.Join(ws, "notes", "today.md")); err != nil {
		t.Errorf("file not in the workspace: %v", err)
	}
	if fi.Path != "/workspace/notes/today.md" {
		t.Errorf("Stat path = %q", fi.Path)
	}
}

func TestFileOperationsRefuseSymlinkEscapes(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(ws, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Act
	_, _, readErr := sb.ReadFile(ctx, "/workspace/out/secret", 10)
	writeErr := sb.WriteFile(ctx, "/workspace/out/new.txt", []byte("x"))
	mkdirErr := sb.MkdirAll(ctx, "/workspace/out/sub")
	missingErr := sb.WriteFile(ctx, "/workspace/out/deep/er/new.txt", []byte("x"))

	// Assert
	for name, err := range map[string]error{"read": readErr, "write": writeErr, "mkdir": mkdirErr, "write below a missing dir": missingErr} {
		if err == nil || !strings.Contains(err.Error(), "resolves outside the sandbox") {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, p := range []string{"new.txt", "sub", "deep"} {
		if _, err := os.Stat(filepath.Join(outside, p)); err == nil {
			t.Errorf("%s was created outside the sandbox", p)
		}
	}
}

func TestTmpIsTheSandboxsOwnScratchDirectory(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	p := "/tmp/harness-local-test-" + filepath.Base(t.TempDir())
	t.Cleanup(func() { _ = os.RemoveAll(p) })

	// Act
	err := sb.WriteFile(ctx, p+"/x.txt", []byte("scratch"))
	data, _, readErr := sb.ReadFile(ctx, p+"/x.txt", 100)

	// Assert
	if err != nil || readErr != nil || string(data) != "scratch" {
		t.Fatalf("write %v read %q %v", err, data, readErr)
	}
}

func TestReadFileCutsAtMaxBytesAndReportsTheFullSize(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/f.txt", []byte("0123456789"))

	// Act
	cut, cutSize, err1 := sb.ReadFile(ctx, "/workspace/f.txt", 4)
	all, allSize, err2 := sb.ReadFile(ctx, "/workspace/f.txt", 0)

	// Assert
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if string(cut) != "0123" || cutSize != 10 {
		t.Errorf("limited read = %q size %d", cut, cutSize)
	}
	if string(all) != "0123456789" || allSize != 10 {
		t.Errorf("unlimited read = %q size %d", all, allSize)
	}
}

func TestReadFileOfADirectoryIsAnErrorButNotMissing(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	_ = sb.MkdirAll(ctx, "/workspace/d")

	// Act
	_, _, err := sb.ReadFile(ctx, "/workspace/d", 10)

	// Assert
	if err == nil || errors.Is(err, sandbox.ErrNotFound) || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteFileReplacesContentAndCreatesParents(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/a/b/c.txt", []byte("long original content"))

	// Act
	err := sb.WriteFile(ctx, "/workspace/a/b/c.txt", []byte("new"))

	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a", "b", "c.txt")); string(b) != "new" {
		t.Fatalf("content = %q, want the old bytes gone", b)
	}
}

func TestWriteFileOverADirectoryFails(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	_ = sb.MkdirAll(ctx, "/workspace/dir")

	// Act
	err := sb.WriteFile(ctx, "/workspace/dir", []byte("x"))

	// Assert
	if err == nil {
		t.Fatal("a directory was overwritten by a file")
	}
}

func TestMkdirAllKeepsTheProjectCleanAndIgnoresTheScratchDirectory(t *testing.T) {
	// Arrange
	sb, ws := newSandbox(t)
	ctx := context.Background()

	// Act
	outputsErr := sb.MkdirAll(ctx, sandbox.OutputsDir)
	scratchErr := sb.MkdirAll(ctx, sandbox.ScratchDir)
	// a user's own .gitignore in the scratch dir is not overwritten
	custom := filepath.Join(ws, ".work", ".gitignore")
	_ = os.WriteFile(custom, []byte("custom\n"), 0o644)
	againErr := sb.MkdirAll(ctx, sandbox.ScratchDir)

	// Assert
	if outputsErr != nil || scratchErr != nil || againErr != nil {
		t.Fatal(outputsErr, scratchErr, againErr)
	}
	if _, err := os.Stat(filepath.Join(ws, "outputs")); err == nil {
		t.Error("outputs/ was created in the project")
	}
	if b, _ := os.ReadFile(custom); string(b) != "custom\n" {
		t.Errorf(".gitignore = %q, want it left alone", b)
	}
}

func TestStatDescribesFilesDirectoriesAndMissingPaths(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/d/f.txt", []byte("12345"))

	// Act
	file, fileErr := sb.Stat(ctx, "/workspace/d/f.txt")
	dir, dirErr := sb.Stat(ctx, "/workspace/d")
	_, missing := sb.Stat(ctx, "/workspace/nope")

	// Assert
	if fileErr != nil || file.Type != "file" || file.Size != 5 || file.Path != "/workspace/d/f.txt" || file.ModifiedAt.IsZero() {
		t.Errorf("file = %+v, %v", file, fileErr)
	}
	if dirErr != nil || dir.Type != "directory" {
		t.Errorf("dir = %+v, %v", dir, dirErr)
	}
	if !errors.Is(missing, sandbox.ErrNotFound) {
		t.Errorf("missing = %v", missing)
	}
}

func TestListHonoursDepthSkipsGitAndSortsByPath(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	for _, p := range []string{"/workspace/b.txt", "/workspace/a/inner.txt", "/workspace/a/deep/x.txt", "/workspace/.git/config"} {
		if err := sb.WriteFile(ctx, p, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	paths := func(fis []sandbox.FileInfo) []string {
		var out []string
		for _, fi := range fis {
			out = append(out, fi.Path)
		}
		return out
	}

	// Act
	d1, err1 := sb.List(ctx, "/workspace", 1)
	d2, err2 := sb.List(ctx, "/workspace", 2)
	d0, err0 := sb.List(ctx, "/workspace", 0)

	// Assert
	if err0 != nil || err1 != nil || err2 != nil {
		t.Fatal(err0, err1, err2)
	}
	if got := strings.Join(paths(d1), ","); got != "/workspace/a,/workspace/b.txt" {
		t.Errorf("depth 1 = %s", got)
	}
	if got := strings.Join(paths(d2), ","); got != "/workspace/a,/workspace/a/deep,/workspace/a/inner.txt,/workspace/b.txt" {
		t.Errorf("depth 2 = %s", got)
	}
	if len(d0) != 0 {
		t.Errorf("depth 0 = %v", paths(d0))
	}
	if d1[0].Type != "directory" || d1[1].Type != "file" {
		t.Errorf("types = %s, %s", d1[0].Type, d1[1].Type)
	}
}

func TestListErrors(t *testing.T) {
	// Arrange
	sb, _ := newSandbox(t)
	ctx := context.Background()
	_ = sb.WriteFile(ctx, "/workspace/f.txt", []byte("x"))

	// Act
	_, missing := sb.List(ctx, "/workspace/none", 1)
	_, notDir := sb.List(ctx, "/workspace/f.txt", 1)

	// Assert
	if !errors.Is(missing, sandbox.ErrNotFound) {
		t.Errorf("missing = %v", missing)
	}
	if notDir == nil || errors.Is(notDir, sandbox.ErrNotFound) || !strings.Contains(notDir.Error(), "not a directory") {
		t.Errorf("file = %v", notDir)
	}
}
