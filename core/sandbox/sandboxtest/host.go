package sandboxtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/sandbox"
)

// HostSandbox is a sandbox.Instance backed by a host directory: the sandbox
// path P is root+P, and Exec runs real bash with every "/workspace" path of
// the command, its environment and working directory mapped under root (output
// is mapped back). Unlike ShellExec, files persist between commands and
// background processes keep running, so the sandbox tools' own scripts
// (background jobs, search) can be tested for real.
type HostSandbox struct {
	id   string
	root string
	// Env is added to every command's environment.
	Env map[string]string
	// Shell runs the commands (default bash; "sh" checks POSIX scripts).
	Shell string
	mu    sync.Mutex
	n     int
}

// NewHostSandbox returns a host sandbox rooted at root (created with /workspace).
func NewHostSandbox(id, root string) *HostSandbox {
	_ = os.MkdirAll(filepath.Join(root, "workspace"), 0o755)
	return &HostSandbox{id: id, root: root}
}

func (h *HostSandbox) ID() string { return h.id }

func (h *HostSandbox) host(p string) string { return h.root + path.Clean("/"+p) }

func (h *HostSandbox) mapWS(s string) string { return mapWorkspace(s, h.root) }

// Exec runs the command with Shell (bash); stdout and stderr are streamed line by line.
func (h *HostSandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	shell := h.Shell
	if shell == "" {
		shell = "bash"
	}
	cmd := exec.CommandContext(ctx, shell, "-c", h.mapWS(req.Command))
	cmd.Dir = h.root + sandbox.Workspace
	if strings.HasPrefix(req.Cwd, sandbox.Workspace) {
		cmd.Dir = h.root + req.Cwd
	}
	cmd.Env = append(os.Environ(), "HOME="+h.root)
	for _, env := range []map[string]string{req.Env, h.Env} {
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+h.mapWS(v))
		}
	}
	cmd.WaitDelay = time.Second // children holding the pipes must not block Wait
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return sandbox.ExecResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return sandbox.ExecResult{}, err
	}
	var wg sync.WaitGroup
	pump := func(r io.Reader, fn func(string)) {
		defer wg.Done()
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if line != "" && fn != nil {
				fn(strings.ReplaceAll(line, h.root, ""))
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go pump(stdout, req.OnStdout)
	go pump(stderr, req.OnStderr)
	wg.Wait()
	runErr := cmd.Wait()
	if ctx.Err() != nil {
		return sandbox.ExecResult{}, ctx.Err()
	}
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		return sandbox.ExecResult{ExitCode: exitErr.ExitCode()}, nil
	case runErr != nil:
		return sandbox.ExecResult{}, runErr
	}
	return sandbox.ExecResult{}, nil
}

// Commands returns how many commands ran.
func (h *HostSandbox) Commands() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

func (h *HostSandbox) ReadFile(_ context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	data, err := os.ReadFile(h.host(p))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0, sandbox.ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	size := int64(len(data))
	if maxBytes > 0 && size > maxBytes {
		data = data[:maxBytes]
	}
	return data, size, nil
}

func (h *HostSandbox) WriteFile(_ context.Context, p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(h.host(p)), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h.host(p), data, 0o644)
}

func (h *HostSandbox) MkdirAll(_ context.Context, p string) error {
	return os.MkdirAll(h.host(p), 0o755)
}

func (h *HostSandbox) List(_ context.Context, p string, depth int) ([]sandbox.FileInfo, error) {
	base := h.host(p)
	if _, err := os.Stat(base); err != nil {
		return nil, sandbox.ErrNotFound
	}
	var out []sandbox.FileInfo
	err := filepath.WalkDir(base, func(hp string, d fs.DirEntry, err error) error {
		if err != nil || hp == base {
			return err
		}
		rel := path.Clean(p) + "/" + filepath.ToSlash(strings.TrimPrefix(hp, base+string(filepath.Separator)))
		if depthOf(path.Clean(p), rel) > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		fi := sandbox.FileInfo{Path: rel, Type: "file", Size: info.Size(), ModifiedAt: info.ModTime()}
		if d.IsDir() {
			fi.Type, fi.Size = "directory", 0
		}
		out = append(out, fi)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func (h *HostSandbox) Stat(_ context.Context, p string) (sandbox.FileInfo, error) {
	info, err := os.Stat(h.host(p))
	if err != nil {
		return sandbox.FileInfo{}, sandbox.ErrNotFound
	}
	fi := sandbox.FileInfo{Path: p, Type: "file", Size: info.Size(), ModifiedAt: info.ModTime()}
	if info.IsDir() {
		fi.Type, fi.Size = "directory", 0
	}
	return fi, nil
}

func (h *HostSandbox) Healthy(context.Context) bool               { return true }
func (h *HostSandbox) Gone(context.Context) bool                  { return false }
func (h *HostSandbox) Renew(context.Context, time.Duration) error { return nil }

// HostBackend creates HostSandboxes under a base directory.
type HostBackend struct {
	mu      sync.Mutex
	base    string
	live    map[string]*HostSandbox
	created []*HostSandbox
	// Env is given to every created sandbox.
	Env map[string]string
}

// NewHostBackend returns a backend creating sandboxes under base (e.g. t.TempDir()).
func NewHostBackend(base string) *HostBackend {
	return &HostBackend{base: base, live: map[string]*HostSandbox{}}
}

func (b *HostBackend) Create(context.Context, sandbox.CreateOptions) (sandbox.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := fmt.Sprintf("host-%d", len(b.created)+1)
	sb := NewHostSandbox(id, filepath.Join(b.base, id))
	sb.Env = b.Env
	b.live[id] = sb
	b.created = append(b.created, sb)
	return sb, nil
}

func (b *HostBackend) Connect(_ context.Context, id string) (sandbox.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sb, ok := b.live[id]; ok {
		return sb, nil
	}
	return nil, sandbox.ErrNoSandbox
}

func (b *HostBackend) Kill(_ context.Context, id string) error {
	b.mu.Lock()
	delete(b.live, id)
	b.mu.Unlock()
	return nil
}

// Created returns the created sandboxes in order.
func (b *HostBackend) Created() []*HostSandbox {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*HostSandbox(nil), b.created...)
}
