// Package sandboxtest provides an in-memory sandbox backend for tests.
package sandboxtest

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

// ExecFunc scripts command execution.
type ExecFunc func(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error)

// Sandbox is an in-memory sandbox.
type Sandbox struct {
	mu       sync.Mutex
	id       string
	files    map[string][]byte
	dirs     map[string]bool
	execFn   ExecFunc
	execs    []sandbox.ExecRequest
	mkdirs   []string
	renewals int
	healthy  bool
	gone     bool
	hang     bool
	dead     bool
	writeErr func(path string) error
	// Fakes of the sandbox tools' scripts (protocol.go).
	jobFn JobFunc
	jobs  map[string]*fakeJob
	docs  map[string]Doc
	// links are symlinks (link path → target), followed by ReadFile, WriteFile,
	// Stat and the realpath script.
	links map[string]string
	// realpaths counts the realpath scripts run.
	realpaths int
	// fileReads counts the contents returned by the read-files script.
	fileReads int
	// mtimes are the files' modification times (Put; Backdate moves them back).
	mtimes map[string]time.Time
	// scriptErr makes the next marked script with that marker fail (FailScript).
	scriptErr map[string]error
	// lineTransport delivers Exec output like the OpenSandbox adapter (on by
	// default; SetLineTransport).
	lineTransport bool
}

// NewSandbox returns an empty healthy sandbox, its output delivered like
// OpenSandbox's (SetLineTransport).
func NewSandbox(id string) *Sandbox {
	return &Sandbox{id: id, files: map[string][]byte{}, dirs: map[string]bool{"/": true}, healthy: true, lineTransport: true}
}

func (s *Sandbox) ID() string { return s.id }

// Die makes the sandbox behave like a container removed at the provider:
// every operation fails with OpenSandbox's SANDBOX_NOT_FOUND error.
func (s *Sandbox) Die() {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
}

// deadErr is the error of an operation on a removed sandbox ("" when alive).
func (s *Sandbox) deadErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dead {
		return nil
	}
	return fmt.Errorf("DOCKER::SANDBOX_NOT_FOUND: Sandbox %s not found. (request_id: test)", s.id)
}

// Healthy implements sandbox.Instance.
func (s *Sandbox) Healthy(ctx context.Context) bool {
	s.mu.Lock()
	hang, healthy := s.hang, s.healthy && !s.dead
	s.mu.Unlock()
	if hang {
		<-ctx.Done()
		return false
	}
	return healthy
}

// SetHang makes health checks block until their context ends (unresponsive execd).
func (s *Sandbox) SetHang(v bool) {
	s.mu.Lock()
	s.hang = v
	s.mu.Unlock()
}

// SetHealthy toggles health.
func (s *Sandbox) SetHealthy(v bool) {
	s.mu.Lock()
	s.healthy = v
	s.mu.Unlock()
}

// Gone implements sandbox.Instance.
func (s *Sandbox) Gone(context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gone || s.dead
}

// SetGone marks the sandbox as Terminated/Failed at the provider.
func (s *Sandbox) SetGone(v bool) {
	s.mu.Lock()
	s.gone = v
	s.mu.Unlock()
}

// Renew implements sandbox.Instance.
func (s *Sandbox) Renew(context.Context, time.Duration) error {
	s.mu.Lock()
	s.renewals++
	s.mu.Unlock()
	return nil
}

// Renewals returns how many times Renew was called.
func (s *Sandbox) Renewals() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

// SetExec scripts Exec.
func (s *Sandbox) SetExec(f ExecFunc) {
	s.mu.Lock()
	s.execFn = f
	s.mu.Unlock()
}

// Execs returns recorded commands.
func (s *Sandbox) Execs() []sandbox.ExecRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sandbox.ExecRequest(nil), s.execs...)
}

// SetLineTransport sets whether Exec delivers stdout and stderr like the
// OpenSandbox adapter does (LineMessages; the default): one newline-terminated
// message per line, a last line without its newline getting one.
func (s *Sandbox) SetLineTransport(v bool) {
	s.mu.Lock()
	s.lineTransport = v
	s.mu.Unlock()
}

// LineMessages wraps an Exec output callback the way OpenSandbox output
// reaches it: OpenSandbox sends each line as a message without its newline,
// and the adapter's Exec appends "\n" to every message (withNewline), so a
// last line without a newline arrives with one.
func LineMessages(fn func(string)) func(string) {
	if fn == nil {
		return nil
	}
	return func(t string) {
		for _, line := range strings.SplitAfter(t, "\n") {
			if line == "" {
				continue
			}
			fn(strings.TrimSuffix(line, "\n") + "\n")
		}
	}
}

// Exec runs the scripted function (default: exit 0, no output).
func (s *Sandbox) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	if err := s.deadErr(); err != nil {
		return sandbox.ExecResult{}, err
	}
	s.mu.Lock()
	if s.lineTransport {
		req.OnStdout, req.OnStderr = LineMessages(req.OnStdout), LineMessages(req.OnStderr)
	}
	s.mu.Unlock()
	if fake := s.intercept(req); fake != nil {
		first, _, _ := strings.Cut(req.Command, "\n")
		s.mu.Lock()
		err, failing := s.scriptErr[strings.TrimSpace(first)]
		delete(s.scriptErr, strings.TrimSpace(first))
		s.mu.Unlock()
		if failing {
			return sandbox.ExecResult{}, err
		}
		return fake(ctx, req)
	}
	s.mu.Lock()
	s.execs = append(s.execs, req)
	f := s.execFn
	s.mu.Unlock()
	if f == nil {
		return sandbox.ExecResult{}, nil
	}
	return f(ctx, req)
}

// Put writes a file directly (test setup).
func (s *Sandbox) Put(p string, data string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[p] = []byte(data)
	if s.mtimes == nil {
		s.mtimes = map[string]time.Time{}
	}
	s.mtimes[p] = time.Now()
	for d := path.Dir(p); ; d = path.Dir(d) {
		s.dirs[d] = true
		if d == "/" {
			break
		}
	}
}

// Backdate makes the file at p look last modified age ago.
func (s *Sandbox) Backdate(p string, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[p]; ok {
		s.mtimes[p] = time.Now().Add(-age)
	}
}

// Paths lists the files under prefix, sorted.
func (s *Sandbox) Paths(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.files {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// FailScript makes the next run of the sandbox script with this marker (its
// first line, e.g. sandbox.PlaceMarker) fail with err before it does anything.
func (s *Sandbox) FailScript(marker string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scriptErr == nil {
		s.scriptErr = map[string]error{}
	}
	s.scriptErr[marker] = err
}

// File returns a file content and whether it exists.
func (s *Sandbox) File(p string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.files[p]
	return string(d), ok
}

// Mkdirs returns the paths given to MkdirAll, in order.
func (s *Sandbox) Mkdirs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.mkdirs...)
}

// HasDir reports whether a directory exists.
func (s *Sandbox) HasDir(p string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dirs[p]
}

func (s *Sandbox) ReadFile(_ context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	if err := s.deadErr(); err != nil {
		return nil, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p = s.realLocked(p)
	d, ok := s.files[p]
	if !ok {
		if s.dirs[p] {
			return nil, 0, fmt.Errorf("%s is a directory", p)
		}
		return nil, 0, sandbox.ErrNotFound
	}
	size := int64(len(d))
	if maxBytes > 0 && size > maxBytes {
		d = d[:maxBytes]
	}
	return append([]byte(nil), d...), size, nil
}

func (s *Sandbox) WriteFile(_ context.Context, p string, data []byte) error {
	if err := s.deadErr(); err != nil {
		return err
	}
	s.mu.Lock()
	fail := s.writeErr
	s.mu.Unlock()
	if fail != nil {
		if err := fail(p); err != nil {
			return err
		}
	}
	s.mu.Lock()
	p = s.realLocked(p)
	s.mu.Unlock()
	s.Put(p, string(data))
	return nil
}

// SetWriteErr makes WriteFile fail for the paths where f returns an error (nil: never).
func (s *Sandbox) SetWriteErr(f func(path string) error) {
	s.mu.Lock()
	s.writeErr = f
	s.mu.Unlock()
}

func (s *Sandbox) MkdirAll(_ context.Context, p string) error {
	if err := s.deadErr(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mkdirs = append(s.mkdirs, p)
	for d := path.Clean(p); ; d = path.Dir(d) {
		s.dirs[d] = true
		if d == "/" {
			break
		}
	}
	return nil
}

func depthOf(base, p string) int {
	rel := strings.TrimPrefix(strings.TrimPrefix(p, base), "/")
	return strings.Count(rel, "/") + 1
}

func (s *Sandbox) List(_ context.Context, p string, depth int) ([]sandbox.FileInfo, error) {
	if err := s.deadErr(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirs[p] {
		return nil, sandbox.ErrNotFound
	}
	prefix := strings.TrimSuffix(p, "/") + "/"
	var out []sandbox.FileInfo
	for d := range s.dirs {
		if strings.HasPrefix(d, prefix) && depthOf(p, d) <= depth {
			out = append(out, sandbox.FileInfo{Path: d, Type: "directory", ModifiedAt: time.Unix(0, 0)})
		}
	}
	for f, data := range s.files {
		if strings.HasPrefix(f, prefix) && depthOf(p, f) <= depth {
			out = append(out, sandbox.FileInfo{Path: f, Type: "file", Size: int64(len(data)), ModifiedAt: time.Unix(0, 0)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (s *Sandbox) Stat(_ context.Context, p string) (sandbox.FileInfo, error) {
	if err := s.deadErr(); err != nil {
		return sandbox.FileInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p = s.realLocked(p)
	if d, ok := s.files[p]; ok {
		return sandbox.FileInfo{Path: p, Type: "file", Size: int64(len(d))}, nil
	}
	if s.dirs[p] {
		return sandbox.FileInfo{Path: p, Type: "directory"}, nil
	}
	return sandbox.FileInfo{}, sandbox.ErrNotFound
}

// Backend is an in-memory sandbox.Backend.
type Backend struct {
	mu      sync.Mutex
	n       int
	live    map[string]*Sandbox
	created []*Sandbox
	killed  []string
	opts    []sandbox.CreateOptions
	// OnCreate customizes new sandboxes.
	OnCreate func(*Sandbox)
	// CreateDelay slows Create down (to expose races).
	CreateDelay time.Duration
	connectErr  error
}

// SetConnectErr makes Connect fail with err (nil restores).
func (b *Backend) SetConnectErr(err error) {
	b.mu.Lock()
	b.connectErr = err
	b.mu.Unlock()
}

// NewBackend returns an empty backend.
func NewBackend() *Backend { return &Backend{live: map[string]*Sandbox{}} }

func (b *Backend) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Instance, error) {
	if b.CreateDelay > 0 {
		select { // like the provider's HTTP call: a cancelled caller stops waiting
		case <-time.After(b.CreateDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	b.mu.Lock()
	b.n++
	sb := NewSandbox(fmt.Sprintf("sbx-%d", b.n))
	b.live[sb.id] = sb
	b.created = append(b.created, sb)
	b.opts = append(b.opts, opts)
	hook := b.OnCreate
	b.mu.Unlock()
	if hook != nil {
		hook(sb)
	}
	return sb, nil
}

func (b *Backend) Connect(_ context.Context, id string) (sandbox.Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.connectErr != nil {
		return nil, b.connectErr
	}
	sb, ok := b.live[id]
	if !ok {
		return nil, sandbox.ErrNoSandbox
	}
	return sb, nil
}

func (b *Backend) Kill(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sb := b.live[id]; sb != nil {
		sb.Die()
	}
	delete(b.live, id)
	b.killed = append(b.killed, id)
	return nil
}

// Created returns created sandboxes in order.
func (b *Backend) Created() []*Sandbox {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*Sandbox(nil), b.created...)
}

// CreateOptions returns options of each creation.
func (b *Backend) CreateOptions() []sandbox.CreateOptions {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]sandbox.CreateOptions(nil), b.opts...)
}

// Killed returns killed sandbox ids.
func (b *Backend) Killed() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.killed...)
}

// Add registers an existing live sandbox.
func (b *Backend) Add(sb *Sandbox) {
	b.mu.Lock()
	b.live[sb.id] = sb
	b.mu.Unlock()
}

// Forget simulates a sandbox that expired on the provider side.
func (b *Backend) Forget(id string) {
	b.mu.Lock()
	if sb := b.live[id]; sb != nil {
		sb.Die()
	}
	delete(b.live, id)
	b.mu.Unlock()
}

// Symlink makes link a symbolic link to target (absolute, or relative to the
// link's directory), like `ln -s target link`.
func (s *Sandbox) Symlink(target, link string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.links == nil {
		s.links = map[string]string{}
	}
	link = path.Clean(link)
	if !strings.HasPrefix(target, "/") {
		target = path.Join(path.Dir(link), target)
	}
	s.links[link] = path.Clean(target)
	for d := path.Dir(link); ; d = path.Dir(d) {
		s.dirs[d] = true
		if d == "/" {
			break
		}
	}
}

// realLocked resolves the symlinks of p component by component (realpath -m).
func (s *Sandbox) realLocked(p string) string {
	p = path.Clean("/" + p)
	for hops := 0; hops < 40 && len(s.links) > 0; hops++ {
		parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
		changed := false
		for i := range parts {
			prefix := "/" + strings.Join(parts[:i+1], "/")
			if target, ok := s.links[prefix]; ok {
				p = path.Clean(target + "/" + strings.Join(parts[i+1:], "/"))
				changed = true
				break
			}
		}
		if !changed {
			break
		}
	}
	return p
}
