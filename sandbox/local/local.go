//go:build unix

// Package local is a sandbox.Backend that runs on the host: no container, no
// server. The agent sees the usual "/workspace" (mapped to a host directory,
// typically the project being worked on) and runs commands with bash in it.
//
// It is for a terminal client on the developer's own machine. There is no
// isolation beyond path confinement of the file API: commands run with the
// user's rights. Run the harness in a container or VM, or use the OpenSandbox
// backend, for untrusted work.
//
// The protocol scripts the tools run with GNU tools (place, realpath,
// read-files, sweep and the background-job scripts) are answered natively, so
// macOS works without coreutils.
package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// Options configures the backend.
type Options struct {
	// Workspace is the host directory mapped to /workspace and shared by every
	// sandbox (project mode). Empty: each sandbox gets its own directory under StateDir.
	Workspace string
	// StateDir keeps what is not the user's project: per-sandbox workspaces
	// (without Workspace), /skills and job records. Default: <user cache dir>/harness.
	StateDir string
	// Shell is the shell that runs commands (default bash).
	Shell string
}

// Backend implements sandbox.Backend on the host.
type Backend struct{ o Options }

// New returns the backend; it creates the directories it needs.
func New(o Options) (*Backend, error) {
	if o.Shell == "" {
		o.Shell = "bash"
	}
	if _, err := exec.LookPath(o.Shell); err != nil {
		return nil, fmt.Errorf("local sandbox: %w", err)
	}
	if o.StateDir == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		o.StateDir = filepath.Join(dir, "harness")
	}
	var err error
	if o.StateDir, err = filepath.Abs(o.StateDir); err != nil {
		return nil, err
	}
	if o.Workspace != "" {
		if o.Workspace, err = filepath.Abs(o.Workspace); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(o.Workspace, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(o.StateDir, 0o755); err != nil {
		return nil, err
	}
	return &Backend{o: o}, nil
}

const idPrefix = "local-"

func (b *Backend) instance(id string) *Instance {
	state := filepath.Join(b.o.StateDir, id)
	ws := b.o.Workspace
	if ws == "" {
		ws = filepath.Join(state, "workspace")
	}
	return &Instance{id: id, ws: resolveDir(ws), sys: filepath.Join(state, "sys"), shell: b.o.Shell}
}

// Create makes a sandbox (a directory layout; nothing runs yet).
func (b *Backend) Create(_ context.Context, _ sandbox.CreateOptions) (sandbox.Instance, error) {
	in := b.instance(idPrefix + uuid.NewString()[:8])
	for _, d := range []string{in.ws, in.sys} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// Connect returns an existing sandbox, or sandbox.ErrNoSandbox when its state is gone.
func (b *Backend) Connect(_ context.Context, id string) (sandbox.Instance, error) {
	if !strings.HasPrefix(id, idPrefix) {
		return nil, sandbox.ErrNoSandbox
	}
	in := b.instance(id)
	if _, err := os.Stat(in.sys); err != nil {
		return nil, sandbox.ErrNoSandbox
	}
	return in, nil
}

// Kill forgets a sandbox. A shared project workspace is never deleted.
func (b *Backend) Kill(_ context.Context, id string) error {
	if !strings.HasPrefix(id, idPrefix) || strings.ContainsAny(id, "/\\.") {
		return nil
	}
	return os.RemoveAll(filepath.Join(b.o.StateDir, id))
}

// Instance is one local sandbox.
type Instance struct {
	id, ws, sys, shell string
	mu                 sync.Mutex
}

// ID returns the sandbox id.
func (in *Instance) ID() string { return in.id }

// Healthy reports whether the workspace still exists.
func (in *Instance) Healthy(context.Context) bool {
	_, err := os.Stat(in.ws)
	return err == nil
}

// Gone is always false: a local sandbox does not expire.
func (in *Instance) Gone(context.Context) bool { return false }

// Renew is a no-op.
func (in *Instance) Renew(context.Context, time.Duration) error { return nil }

func resolveDir(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// host maps a sandbox path to the host: /workspace → the workspace, /skills →
// the state directory, /tmp as is. Other paths are refused.
func (in *Instance) host(p string) (string, error) {
	p = cleanVirtual(p)
	switch {
	case p == sandbox.Workspace || strings.HasPrefix(p, sandbox.Workspace+"/"):
		return filepath.Join(in.ws, strings.TrimPrefix(p, sandbox.Workspace)), nil
	case p == "/skills" || strings.HasPrefix(p, "/skills/"):
		return filepath.Join(in.sys, p), nil
	case p == "/tmp" || strings.HasPrefix(p, "/tmp/"):
		return p, nil
	}
	return "", fmt.Errorf("path %s is outside the local sandbox (/workspace, /skills, /tmp)", p)
}

func cleanVirtual(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = sandbox.Workspace + "/" + p
	}
	return filepath.ToSlash(filepath.Clean(p))
}

// virtual maps a host path back to the sandbox view.
func (in *Instance) virtual(p string) string {
	switch {
	case p == in.ws || strings.HasPrefix(p, in.ws+"/"):
		return sandbox.Workspace + strings.TrimPrefix(p, in.ws)
	case strings.HasPrefix(p, in.sys+"/"):
		return strings.TrimPrefix(p, in.sys)
	}
	return p
}

func (in *Instance) virtualText(s string) string {
	s = strings.ReplaceAll(s, in.ws, sandbox.Workspace)
	return strings.ReplaceAll(s, in.sys+"/skills", "/skills")
}

// mapCommand rewrites the sandbox roots of a command to host paths. A root only
// counts where a path starts (not inside a longer name or path).
func (in *Instance) mapCommand(s string) string {
	s = mapRoot(s, sandbox.Workspace, in.ws)
	return mapRoot(s, "/skills", filepath.Join(in.sys, "skills"))
}

// mapEnv maps the sandbox paths the tools pass in HARNESS_* variables (the search
// path, the glob); HARNESS_PATTERN and other free text are left alone.
func (in *Instance) mapEnv(key, v string) string {
	if strings.HasPrefix(key, "HARNESS_") && key != "HARNESS_PATTERN" && !strings.HasSuffix(key, "_B64") {
		return in.mapCommand(v)
	}
	return v
}

func mapRoot(s, from, to string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, from)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(from)
		okBefore := i == 0 || !isPathChar(s[i-1])
		okAfter := end == len(s) || s[end] == '/' || !isPathChar(s[end])
		b.WriteString(s[:i])
		if okBefore && okAfter {
			b.WriteString(to)
		} else {
			b.WriteString(from)
		}
		s = s[end:]
	}
}

func isPathChar(c byte) bool {
	return c == '_' || c == '-' || c == '.' || c == '/' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// confine fails when the real location of host path p (symlinks resolved, the
// missing tail allowed) leaves the workspace and system directories.
func (in *Instance) confine(p string) error {
	if strings.HasPrefix(p, "/tmp/") || p == "/tmp" {
		return nil
	}
	real := realpathM(p)
	for _, root := range []string{in.ws, resolveDir(in.sys)} {
		if real == root || strings.HasPrefix(real, root+"/") {
			return nil
		}
	}
	return fmt.Errorf("%s resolves outside the sandbox", in.virtual(p))
}

// realpathM is `realpath -m`: symlinks of the existing components resolved, the rest kept.
func realpathM(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent, base := filepath.Split(p)
		if parent == "" || parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(base, rest)
		p = filepath.Clean(parent)
	}
}

func mapErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return sandbox.ErrNotFound
	}
	return err
}

// ReadFile returns at most maxBytes of the file and its size.
func (in *Instance) ReadFile(_ context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	h, err := in.host(p)
	if err != nil {
		return nil, 0, err
	}
	if err := in.confine(h); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(h)
	if err != nil {
		return nil, 0, mapErr(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if st.IsDir() {
		return nil, 0, fmt.Errorf("%s is a directory", p)
	}
	var r io.Reader = f
	if maxBytes > 0 {
		r = io.LimitReader(f, maxBytes)
	}
	data, err := io.ReadAll(r)
	return data, st.Size(), err
}

// WriteFile creates parent directories and writes the file (mode 0644).
func (in *Instance) WriteFile(_ context.Context, p string, data []byte) error {
	h, err := in.host(p)
	if err != nil {
		return err
	}
	if err := in.confine(h); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h), 0o755); err != nil {
		return err
	}
	return os.WriteFile(h, data, 0o644)
}

// MkdirAll creates a directory and its parents. /workspace/outputs (the document
// deliverables folder of the default layout) is not created in a project, and the
// scratch folder /workspace/.work is kept out of git by a .gitignore of its own.
func (in *Instance) MkdirAll(_ context.Context, p string) error {
	if cleanVirtual(p) == sandbox.OutputsDir {
		return nil
	}
	h, err := in.host(p)
	if err != nil {
		return err
	}
	if err := in.confine(h); err != nil {
		return err
	}
	if err := os.MkdirAll(h, 0o755); err != nil {
		return err
	}
	if cleanVirtual(p) == sandbox.ScratchDir {
		ignore := filepath.Join(h, ".gitignore")
		if _, err := os.Stat(ignore); err != nil {
			_ = os.WriteFile(ignore, []byte("*\n"), 0o644)
		}
	}
	return nil
}

func info(virtual string, st fs.FileInfo) sandbox.FileInfo {
	t := "file"
	if st.IsDir() {
		t = "directory"
	}
	return sandbox.FileInfo{Path: virtual, Type: t, Size: st.Size(), ModifiedAt: st.ModTime()}
}

// Stat describes a file or directory.
func (in *Instance) Stat(_ context.Context, p string) (sandbox.FileInfo, error) {
	h, err := in.host(p)
	if err != nil {
		return sandbox.FileInfo{}, err
	}
	st, err := os.Stat(h)
	if err != nil {
		return sandbox.FileInfo{}, mapErr(err)
	}
	return info(cleanVirtual(p), st), nil
}

// List lists the entries under p down to depth levels, sorted by path.
func (in *Instance) List(_ context.Context, p string, depth int) ([]sandbox.FileInfo, error) {
	h, err := in.host(p)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(h); err != nil {
		return nil, mapErr(err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", p)
	}
	var out []sandbox.FileInfo
	_ = filepath.WalkDir(h, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == h {
			return nil
		}
		rel, _ := filepath.Rel(h, path)
		if strings.Count(rel, string(filepath.Separator))+1 > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".git" {
			return filepath.SkipDir
		}
		if st, err := d.Info(); err == nil {
			out = append(out, info(cleanVirtual(p)+"/"+filepath.ToSlash(rel), st))
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Exec runs a command with bash, or answers a protocol script natively.
func (in *Instance) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	first, _, _ := strings.Cut(req.Command, "\n")
	if h := in.native(strings.TrimSpace(first)); h != nil {
		return h(ctx, req)
	}
	return in.run(ctx, req)
}

type lineWriter struct {
	mu sync.Mutex
	f  func(string)
	in *Instance
}

func (w *lineWriter) Write(p []byte) (int, error) {
	if w.f != nil && len(p) > 0 {
		w.mu.Lock()
		w.f(w.in.virtualText(string(p)))
		w.mu.Unlock()
	}
	return len(p), nil
}

func (in *Instance) run(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	dir := in.ws
	if req.Cwd != "" {
		d, err := in.host(req.Cwd)
		if err != nil {
			return sandbox.ExecResult{}, err
		}
		dir = d
	}
	cmd := exec.CommandContext(runCtx, in.shell, "-c", in.mapCommand(req.Command))
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+in.ws)
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+in.mapEnv(k, v))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = &lineWriter{f: req.OnStdout, in: in}
	cmd.Stderr = &lineWriter{f: req.OnStderr, in: in}
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return sandbox.ExecResult{}, nil
	case ctx.Err() != nil:
		return sandbox.ExecResult{}, ctx.Err()
	case runCtx.Err() != nil: // the command's own timeout
		return sandbox.ExecResult{ExitCode: 124}, nil
	case errors.As(err, &ee):
		return sandbox.ExecResult{ExitCode: ee.ExitCode()}, nil
	}
	return sandbox.ExecResult{}, err
}

func (in *Instance) out(req sandbox.ExecRequest, s string) {
	if req.OnStdout != nil && s != "" {
		req.OnStdout(s)
	}
}

type handler func(context.Context, sandbox.ExecRequest) (sandbox.ExecResult, error)

func (in *Instance) native(marker string) handler {
	switch marker {
	case sandbox.PlaceMarker:
		return in.place
	case sandbox.UnstageMarker:
		return in.unstage
	case sandbox.RealPathMarker:
		return in.realpath
	case sandbox.ReadFilesMarker:
		return in.readFiles
	case "# work:sweep":
		return in.sweep
	case "# work:job-start":
		return in.jobStart
	case "# work:job-wait":
		return in.jobWait
	case "# work:job-status":
		return in.jobStatus
	case "# work:job-kill":
		return in.jobKill
	}
	return nil
}

func exit(code int) (sandbox.ExecResult, error) { return sandbox.ExecResult{ExitCode: code}, nil }

// place moves HARNESS_STAGED onto HARNESS_TARGET unless the target is a symlink or
// its directory resolves elsewhere (exit 4, the real directory on stdout).
func (in *Instance) place(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	target, err1 := in.host(req.Env["HARNESS_TARGET"])
	staged, err2 := in.host(req.Env["HARNESS_STAGED"])
	if err1 != nil || err2 != nil {
		return exit(3)
	}
	dir := filepath.Dir(target)
	real := realpathM(dir)
	want := realpathM(dir)
	if rel, err := filepath.Rel(in.ws, dir); err == nil && !strings.HasPrefix(rel, "..") {
		want = filepath.Join(resolveDir(in.ws), rel)
	}
	if st, err := os.Lstat(target); (err == nil && st.Mode()&fs.ModeSymlink != 0) || real != want {
		_ = os.Remove(staged)
		in.out(req, in.virtual(real))
		return exit(4)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		_ = os.Remove(staged)
		return exit(3)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return exit(3)
	}
	return exit(0)
}

func (in *Instance) unstage(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if p, err := in.host(req.Env["HARNESS_STAGED"]); err == nil {
		_ = os.Remove(p)
	}
	return exit(0)
}

func (in *Instance) realpath(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	n := atoi(req.Env["HARNESS_N"])
	var b strings.Builder
	for i := 0; i < n; i++ {
		h, err := in.host(req.Env[fmt.Sprintf("HARNESS_PATH_%d", i)])
		if err != nil {
			return exit(1)
		}
		b.WriteString(strings.ReplaceAll(in.virtual(realpathM(h)), "\n", "\x00") + "\n")
	}
	b.WriteString("ok\n")
	in.out(req, b.String())
	return exit(0)
}

func (in *Instance) readFiles(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	budget := int64(atoi(req.Env["HARNESS_BUDGET"]))
	var b strings.Builder
	for i := 0; i < atoi(req.Env["HARNESS_N"]); i++ {
		vp := req.Env[fmt.Sprintf("HARNESS_PATH_%d", i)]
		h, err := in.host(vp)
		if err != nil {
			continue
		}
		st, err := os.Lstat(h)
		if err != nil || !st.Mode().IsRegular() || in.virtual(realpathM(h)) != cleanVirtual(vp) {
			continue
		}
		stamp := fmt.Sprintf("%d %d", st.Size(), st.ModTime().Unix())
		if k := req.Env[fmt.Sprintf("HARNESS_KNOWN_%d", i)]; k != "" && (k == stamp || k == fmt.Sprintf("%d -", st.Size())) {
			fmt.Fprintf(&b, "%d %s U -\n", i, stamp)
			continue
		}
		if budget <= 0 {
			continue
		}
		f, err := os.Open(h)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(f, budget))
		f.Close()
		fmt.Fprintf(&b, "%d %s F %s\n", i, stamp, b64(data))
		budget = max(0, budget-st.Size())
	}
	in.out(req, b.String())
	return exit(0)
}

func (in *Instance) sweep(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	dir, err := in.host(req.Env["HARNESS_STAGING_DIR"])
	if err != nil {
		return exit(0)
	}
	cutoff := time.Now().Add(-time.Duration(atoi(req.Env["HARNESS_STAGING_MINUTES"])) * time.Minute)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if st, err := e.Info(); err == nil && st.Mode().IsRegular() && st.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return exit(0)
}
