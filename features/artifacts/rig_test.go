package artifacts_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/artifacts"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// tickClock is a store clock that moves one millisecond at every reading, so
// consecutive store changes are strictly ordered without sleeping.
type tickClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tickClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

// faultStore is a memstore whose artifact operations can be made to fail.
type faultStore struct {
	*memstore.Store
	mu             sync.Mutex
	puts           int
	putErr         func(call int) error
	listErr        error
	clockErr       error
	deleteErr      error
	materializeErr error
	skipErr        error
	appendErr      error
	touchErr       error
	queueErr       error
	purgeErr       error
	candidatesErr  error
	lockErr        error
}

func (f *faultStore) PutArtifactVersion(ctx context.Context, in store.PutArtifact) (*store.Artifact, string, error) {
	f.mu.Lock()
	f.puts++
	call, fn := f.puts, f.putErr
	f.mu.Unlock()
	if fn != nil {
		if err := fn(call); err != nil {
			return nil, "", err
		}
	}
	return f.Store.PutArtifactVersion(ctx, in)
}

func (f *faultStore) ListArtifacts(ctx context.Context, threadID string) ([]*store.Artifact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.ListArtifacts(ctx, threadID)
}

func (f *faultStore) Clock(ctx context.Context) (time.Time, error) {
	if f.clockErr != nil {
		return time.Time{}, f.clockErr
	}
	return f.Store.Clock(ctx)
}

func (f *faultStore) MarkArtifactDeleted(ctx context.Context, id, runID, sbID string, since time.Time) (*store.Artifact, error) {
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return f.Store.MarkArtifactDeleted(ctx, id, runID, sbID, since)
}

func (f *faultStore) MarkArtifactsMaterialized(ctx context.Context, sbID string, ids []string) error {
	if f.materializeErr != nil {
		return f.materializeErr
	}
	return f.Store.MarkArtifactsMaterialized(ctx, sbID, ids)
}

func (f *faultStore) MarkArtifactsRestoreSkipped(ctx context.Context, ids []string) error {
	if f.skipErr != nil {
		return f.skipErr
	}
	return f.Store.MarkArtifactsRestoreSkipped(ctx, ids)
}

func (f *faultStore) AppendRunArtifacts(ctx context.Context, runID string, items []any) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	return f.Store.AppendRunArtifacts(ctx, runID, items)
}

func (f *faultStore) TouchBlob(ctx context.Context, key string) error {
	if f.touchErr != nil {
		return f.touchErr
	}
	return f.Store.TouchBlob(ctx, key)
}

func (f *faultStore) QueueBlob(ctx context.Context, key string) error {
	if f.queueErr != nil {
		return f.queueErr
	}
	return f.Store.QueueBlob(ctx, key)
}

func (f *faultStore) PurgeDeletedThreads(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if f.purgeErr != nil {
		return 0, f.purgeErr
	}
	return f.Store.PurgeDeletedThreads(ctx, olderThan, limit)
}

func (f *faultStore) BlobCandidates(ctx context.Context, grace time.Duration, limit int) ([]string, error) {
	if f.candidatesErr != nil {
		return nil, f.candidatesErr
	}
	return f.Store.BlobCandidates(ctx, grace, limit)
}

func (f *faultStore) AdvisoryLock(ctx context.Context, name string, wait time.Duration) (func(), error) {
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	return f.Store.AdvisoryLock(ctx, name, wait)
}

// failBlobs is a blob backend whose operations can be made to fail.
type failBlobs struct {
	*hookBlobs
	putErr, hasErr, getErr, deleteErr, previewDeleteErr error
}

func (b *failBlobs) Put(ctx context.Context, key string, data []byte) error {
	if b.putErr != nil {
		return b.putErr
	}
	return b.hookBlobs.Put(ctx, key, data)
}

func (b *failBlobs) Has(ctx context.Context, key string) (bool, error) {
	if b.hasErr != nil {
		return false, b.hasErr
	}
	return b.hookBlobs.Has(ctx, key)
}

func (b *failBlobs) Get(ctx context.Context, key string) ([]byte, error) {
	if b.getErr != nil {
		return nil, b.getErr
	}
	return b.hookBlobs.Get(ctx, key)
}

func (b *failBlobs) Delete(ctx context.Context, key string) error {
	if strings.HasPrefix(key, "preview/") {
		if b.previewDeleteErr != nil {
			return b.previewDeleteErr
		}
	} else if b.deleteErr != nil {
		return b.deleteErr
	}
	return b.hookBlobs.Delete(ctx, key)
}

// capBlobs is a blob backend with a size cap.
type capBlobs struct {
	*hookBlobs
	max int64
}

func (b capBlobs) MaxBytes() int64 { return b.max }

// fakeMgr is a scriptable sandbox manager.
type fakeMgr struct {
	mu          sync.Mutex
	disabled    bool
	get         func(call int) (sandbox.Sandbox, error)
	acquire     func(call int) (sandbox.Sandbox, error)
	gets        int
	acquires    int
	invalidated []string
}

func (m *fakeMgr) Enabled() bool { return !m.disabled }

func (m *fakeMgr) Acquire(_ context.Context, _, _ string) (sandbox.Sandbox, error) {
	m.mu.Lock()
	m.acquires++
	call := m.acquires
	m.mu.Unlock()
	return m.acquire(call)
}

func (m *fakeMgr) Get(_ context.Context, _ string) (sandbox.Sandbox, error) {
	m.mu.Lock()
	m.gets++
	call := m.gets
	m.mu.Unlock()
	return m.get(call)
}

func (m *fakeMgr) Kill(context.Context, string) error { return nil }

func (m *fakeMgr) Invalidate(_ context.Context, _, sandboxID string) {
	m.mu.Lock()
	m.invalidated = append(m.invalidated, sandboxID)
	m.mu.Unlock()
}

// flakySandbox wraps a sandbox and fails chosen file operations.
type flakySandbox struct {
	*sandboxtest.Sandbox
	readErr func(p string) error
	statErr func(p string) error
}

func (s *flakySandbox) ReadFile(ctx context.Context, p string, maxBytes int64) ([]byte, int64, error) {
	if s.readErr != nil {
		if err := s.readErr(p); err != nil {
			return nil, 0, err
		}
	}
	return s.Sandbox.ReadFile(ctx, p, maxBytes)
}

func (s *flakySandbox) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	if s.statErr != nil {
		if err := s.statErr(p); err != nil {
			return sandbox.FileInfo{}, err
		}
	}
	return s.Sandbox.Stat(ctx, p)
}

var errGone = errors.New("DOCKER::SANDBOX_NOT_FOUND: Sandbox sbx-x not found. (request_id: test)")

// sbRig is a service over a memstore (ticking clock) and an in-memory sandbox manager.
type sbRig struct {
	t       *testing.T
	ctx     context.Context
	mem     *memstore.Store
	backend *sandboxtest.Backend
	mgr     *sandbox.DefaultManager
	svc     *artifacts.Service
	th      *store.Thread
}

func newSBRig(t *testing.T, o artifacts.Options) *sbRig {
	t.Helper()
	r := &sbRig{t: t, ctx: context.Background(), mem: memstore.New(), backend: sandboxtest.NewBackend()}
	clock := &tickClock{t: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	r.mem.Now = clock.now
	r.backend.OnCreate = func(sb *sandboxtest.Sandbox) { sb.SetExec(sandboxtest.FindExec(sb, nil)) }
	r.mgr = sandbox.NewManager(r.backend, r.mem, nil, sandbox.ManagerOptions{MaxPerUser: 5, Timeout: time.Hour})
	r.svc = artifacts.New(r.mem, artifacts.PGBlobs{S: r.mem}, r.mgr, o)
	th, err := r.mem.CreateThread(r.ctx, "alice", "t")
	if err != nil {
		t.Fatal(err)
	}
	r.th = th
	return r
}

// sandbox returns the live sandbox of the rig's thread (created on first use).
func (r *sbRig) sandbox() *sandboxtest.Sandbox {
	r.t.Helper()
	if _, err := r.mgr.Acquire(r.ctx, r.th.ID, "alice"); err != nil {
		r.t.Fatal(err)
	}
	created := r.backend.Created()
	return created[len(created)-1]
}

func (r *sbRig) ref() artifacts.RunRef {
	return artifacts.RunRef{ThreadID: r.th.ID, Login: "alice"}
}

func (r *sbRig) snapshot(sb sandbox.Sandbox) []artifacts.Change {
	r.t.Helper()
	changes, err := r.svc.Snapshot(r.ctx, sb, r.ref())
	if err != nil {
		r.t.Fatal(err)
	}
	return changes
}

func (r *sbRig) all() []*store.Artifact {
	r.t.Helper()
	list, err := r.mem.ListArtifacts(r.ctx, r.th.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	return list
}

func (r *sbRig) art(p string) *store.Artifact {
	r.t.Helper()
	for _, a := range r.all() {
		if a.Path == p {
			return a
		}
	}
	return nil
}

func (r *sbRig) upload(name, content string) *store.Artifact {
	r.t.Helper()
	a, err := r.svc.Upload(r.ctx, artifacts.UploadInput{ThreadID: r.th.ID, Login: "alice", Filename: name, Data: []byte(content)})
	if err != nil {
		r.t.Fatal(err)
	}
	return a
}

// scriptList makes the sandbox answer the snapshot listing with fn's output.
func scriptList(sb *sandboxtest.Sandbox, fn func() (out string, exit int)) {
	sb.SetExec(func(_ context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
		if !strings.HasPrefix(req.Command, "find /workspace") {
			return sandbox.ExecResult{}, nil
		}
		out, exit := fn()
		if req.OnStdout != nil {
			req.OnStdout(out)
		}
		return sandbox.ExecResult{ExitCode: exit}, nil
	})
}

// listLine is one line of the snapshot listing.
func listLine(size int, mtime float64, p string) string {
	return fmt.Sprintf("%d %f %s\n", size, mtime, p)
}

func changePaths(changes []artifacts.Change) map[string]string {
	out := map[string]string{}
	for _, c := range changes {
		out[c.Artifact.Path] = c.Change
	}
	return out
}
