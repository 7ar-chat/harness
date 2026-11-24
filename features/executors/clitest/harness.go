package clitest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

// Sandbox is a sandbox.Sandbox over a temporary directory standing for /workspace. Commands run through `sh -c`
// with a CLEAN environment: PATH and HOME of the test, the Base variables, and what the runner passes in the request
// (nothing of the developer's own environment leaks in). A cancelled or timed out command kills its whole process
// group, like a real sandbox ends a command.
type Sandbox struct {
	// Root is the host directory /workspace maps to.
	Root string
	// Base is the environment every command gets (PATH, HOME, CODEX_HOME, ...).
	Base map[string]string

	mu   sync.Mutex
	cmds []string
	caps []Capture
}

// Capture is the raw output of one command the runner executed.
type Capture struct {
	Command, Stdout, Stderr string
	ExitCode                int
}

// Captures returns the raw stdout and stderr of every command so far (the CLI's own run is the one that is not a
// probe: use CLIRun).
func (s *Sandbox) Captures() []Capture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Capture(nil), s.caps...)
}

// Commands returns every command the runner executed.
func (s *Sandbox) Commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

func (s *Sandbox) host(p string) string {
	if p == sandbox.Workspace {
		return s.Root
	}
	if strings.HasPrefix(p, sandbox.Workspace+"/") {
		return filepath.Join(s.Root, strings.TrimPrefix(p, sandbox.Workspace+"/"))
	}
	return p
}

// ID implements sandbox.Sandbox.
func (s *Sandbox) ID() string { return "clitest" }

// Exec implements sandbox.Sandbox.
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, req.Command)
	s.mu.Unlock()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.Command("sh", "-c", req.Command)
	cmd.Dir = s.host(req.Cwd)
	cmd.Env = nil
	for k, v := range s.Base {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stream := func(w func(string)) io.Writer {
		if w == nil {
			return io.Discard
		}
		return writerFunc(w)
	}
	var so, se bytes.Buffer
	cmd.Stdout = io.MultiWriter(&so, stream(req.OnStdout))
	cmd.Stderr = io.MultiWriter(&se, stream(req.OnStderr))
	code := -1
	defer func() {
		s.mu.Lock()
		s.caps = append(s.caps, Capture{Command: req.Command, Stdout: so.String(), Stderr: se.String(), ExitCode: code})
		s.mu.Unlock()
	}()
	if err := cmd.Start(); err != nil {
		return sandbox.ExecResult{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
			return sandbox.ExecResult{ExitCode: code}, nil
		}
		if err == nil {
			code = 0
		}
		return sandbox.ExecResult{}, err
	case <-ctx.Done():
		// A container ends every process of the command. The CLIs start their commands in a session of their own
		// (omp, codex), which a process-group kill does not reach, so the descendants are collected first.
		pids := descendants(cmd.Process.Pid)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		for _, p := range pids {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		<-done
		return sandbox.ExecResult{ExitCode: -1}, ctx.Err()
	}
}

type writerFunc func(string)

func (f writerFunc) Write(p []byte) (int, error) { f(string(p)); return len(p), nil }

// ReadFile implements sandbox.Sandbox.
func (s *Sandbox) ReadFile(_ context.Context, p string, max int64) ([]byte, int64, error) {
	b, err := os.ReadFile(s.host(p))
	if err != nil {
		return nil, 0, sandbox.ErrNotFound
	}
	if int64(len(b)) > max {
		b = b[:max]
	}
	return b, int64(len(b)), nil
}

// WriteFile implements sandbox.Sandbox.
func (s *Sandbox) WriteFile(_ context.Context, p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(s.host(p)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.host(p), data, 0o644)
}

// MkdirAll implements sandbox.Sandbox.
func (s *Sandbox) MkdirAll(_ context.Context, p string) error { return os.MkdirAll(s.host(p), 0o755) }

// List implements sandbox.Sandbox.
func (s *Sandbox) List(context.Context, string, int) ([]sandbox.FileInfo, error) { return nil, nil }

// Stat implements sandbox.Sandbox.
func (s *Sandbox) Stat(_ context.Context, p string) (sandbox.FileInfo, error) {
	fi, err := os.Stat(s.host(p))
	if err != nil {
		return sandbox.FileInfo{}, sandbox.ErrNotFound
	}
	t := "file"
	if fi.IsDir() {
		t = "directory"
	}
	return sandbox.FileInfo{Path: p, Type: t, Size: fi.Size()}, nil
}

// Manager hands out one Sandbox.
type Manager struct{ SB *Sandbox }

// Enabled implements sandbox.Manager.
func (m Manager) Enabled() bool { return true }

// Acquire implements sandbox.Manager.
func (m Manager) Acquire(context.Context, string, string) (sandbox.Sandbox, error) { return m.SB, nil }

// Get implements sandbox.Manager.
func (m Manager) Get(context.Context, string) (sandbox.Sandbox, error) { return m.SB, nil }

// Kill implements sandbox.Manager.
func (m Manager) Kill(context.Context, string) error { return nil }

// Invalidate implements sandbox.Manager.
func (m Manager) Invalidate(context.Context, string, string) {}

// Creds is a executors.CredentialSource with fixed values.
type Creds map[string]string

// Credentials implements executors.CredentialSource.
func (c Creds) Credentials(context.Context, string, string) (map[string]string, error) { return c, nil }

// NewWorkspace makes a throwaway /workspace holding a tiny git repo at repo/ (one committed file a.txt) and a HOME
// directory for the CLI, and returns the Sandbox over it. path is the directory that holds the CLI binaries
// (prepended to PATH, with the node and bun that the npm and bun CLIs need).
func NewWorkspace(t testing.TB, binDirs ...string) *Sandbox {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "..", filepath.Base(root)+"-home")
	home = filepath.Clean(home)
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		c := exec.Command("git", append([]string{"-c", "user.email=t@example.test", "-c", "user.name=test"}, args...)...)
		c.Dir = repo
		c.Env = []string{"PATH=/usr/bin:/bin:/opt/homebrew/bin", "HOME=" + home}
		var out bytes.Buffer
		c.Stdout, c.Stderr = &out, &out
		if err := c.Run(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out.String())
		}
	}
	git("init", "-q", ".")
	git("add", ".")
	git("commit", "-q", "-m", "init")
	path := strings.Join(append(append([]string(nil), binDirs...), "/opt/homebrew/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"), ":")
	return &Sandbox{Root: root, Base: map[string]string{
		"PATH": path, "HOME": home, "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_AUTHOR_NAME": "test", "GIT_AUTHOR_EMAIL": "t@example.test",
		"GIT_COMMITTER_NAME": "test", "GIT_COMMITTER_EMAIL": "t@example.test",
	}}
}

// Home is the HOME of the CLI.
func (s *Sandbox) Home() string { return s.Base["HOME"] }

// Repo is the host path of the repository.
func (s *Sandbox) Repo() string { return filepath.Join(s.Root, "repo") }

// Wait polls cond until it holds or d passes.
func Wait(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// HomeFile writes a file under the CLI's HOME (a config.toml, a models.yml).
func (s *Sandbox) HomeFile(t testing.TB, rel, content string) {
	t.Helper()
	p := filepath.Join(s.Home(), rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
