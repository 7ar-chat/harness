package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/core/store"
)

// Store is what the manager needs from the thread store.
type Store interface {
	store.SandboxStore
	// SwapThreadSandbox is SetThreadSandbox reporting whether it changed the row
	// (false: another sandbox was recorded meanwhile, or the thread was deleted).
	SwapThreadSandbox(ctx context.Context, threadID, expected, sandboxID string) (bool, error)
	// UserSandboxes lists the user's live threads with a recorded sandbox.
	UserSandboxes(ctx context.Context, login string) ([]store.UserSandbox, error)
}

// ManagerOptions configures the manager.
type ManagerOptions struct {
	MaxPerUser int
	// Timeout is the idle lifetime given to the provider on each renewal.
	Timeout time.Duration
	// RenewEvery throttles renewals and paces the keep-alive of running
	// commands (default 60 s, at most Timeout/3).
	RenewEvery time.Duration
	// CacheIdle drops cached handles unused for this long (default 10 min).
	CacheIdle time.Duration
	// CacheSize caps cached handles (default 1000).
	CacheSize int
	// LockTTL is the TTL of the creation locks (thread, user quota), renewed
	// every LockTTL/3 while held, so a slow creation never loses them (default 30 s; C-10).
	LockTTL time.Duration
	// LockWait bounds the wait for a creation lock (default 3 min).
	LockWait time.Duration
	// HealthRecheck: a cached handle checked healthy more recently is used without a new check (default 30 s).
	HealthRecheck time.Duration
	// UnhealthyRecreate: a sandbox continuously unhealthy for longer than this,
	// checked at least 3 times (across replicas), is killed and recreated (default 5 min).
	UnhealthyRecreate time.Duration
	// HealthTimeout bounds one connect + health attempt (default 10 s).
	HealthTimeout time.Duration
	// OnCreate runs after a sandbox is created for a thread (workspace restore),
	// before the sandbox is recorded for the thread: nobody else can use it
	// until the restore finished (C-11). The returned notice reaches the model
	// with the next tool result (kept for the next agent Acquire when the
	// creation was Quiet; C-18).
	OnCreate func(ctx context.Context, threadID string, sb Sandbox, recreated bool) string
}

type recreated struct{ Sandbox }

// WriteFileFrom keeps streamed writes through the wrapper.
func (r recreated) WriteFileFrom(ctx context.Context, p string, rd io.Reader) error {
	return WriteFrom(ctx, r.Sandbox, p, rd)
}

// MarkRecreated wraps a sandbox that replaced a gone or unresponsive one (/workspace was reset).
func MarkRecreated(sb Sandbox) Sandbox { return recreated{sb} }

// WasRecreated reports whether Acquire had to replace the thread's sandbox.
func WasRecreated(sb Sandbox) bool {
	if n, ok := sb.(noticed); ok {
		return n.recreated
	}
	_, ok := sb.(recreated)
	return ok
}

type noticed struct {
	Sandbox
	notice    string
	recreated bool
}

// WriteFileFrom keeps streamed writes through the wrapper.
func (n noticed) WriteFileFrom(ctx context.Context, p string, rd io.Reader) error {
	return WriteFrom(ctx, n.Sandbox, p, rd)
}

// NoticeOf returns the OnCreate notice of a freshly created sandbox ("" otherwise).
func NoticeOf(sb Sandbox) string {
	if n, ok := sb.(noticed); ok {
		return n.notice
	}
	return ""
}

// errRecreate: the sandbox was unhealthy for too long and has been forgotten.
var errRecreate = errors.New("sandbox unhealthy for too long; recreating")

type entry struct {
	inst        Instance
	lastUsed    time.Time
	lastRenew   time.Time
	lastHealthy time.Time
}

// DefaultManager implements Manager with a Backend, the thread store and a Redis lock.
type DefaultManager struct {
	backend Backend
	store   Store
	coord   Coordinator
	opts    ManagerOptions

	mu    sync.Mutex
	cache map[string]*entry // threadID -> handle
}

// NewManager builds a manager over a Coordinator (nil: in-process state, a
// single process). A nil backend yields a disabled manager.
func NewManager(backend Backend, st Store, coord Coordinator, opts ManagerOptions) *DefaultManager {
	if coord == nil {
		coord = NewLocalCoordinator()
	}
	if opts.RenewEvery <= 0 {
		opts.RenewEvery = time.Minute
	}
	if opts.CacheIdle <= 0 {
		opts.CacheIdle = 10 * time.Minute
	}
	if opts.CacheSize <= 0 {
		opts.CacheSize = 1000
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = 30 * time.Second
	}
	if opts.LockWait <= 0 {
		opts.LockWait = 3 * time.Minute
	}
	if opts.HealthRecheck <= 0 {
		opts.HealthRecheck = 30 * time.Second
	}
	if opts.MaxPerUser <= 0 {
		opts.MaxPerUser = 5
	}
	if opts.UnhealthyRecreate <= 0 {
		opts.UnhealthyRecreate = 5 * time.Minute
	}
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = 10 * time.Second
	}
	return &DefaultManager{backend: backend, store: st, coord: coord, opts: opts, cache: map[string]*entry{}}
}

var _ Manager = (*DefaultManager)(nil)

func (m *DefaultManager) Enabled() bool { return m != nil && m.backend != nil }

// UserTag is the hashed login stored in sandbox metadata.
func UserTag(login string) string {
	sum := sha256.Sum256([]byte(login))
	return hex.EncodeToString(sum[:])[:40]
}

func (m *DefaultManager) cached(threadID, wantID string) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for k, e := range m.cache {
		if now.Sub(e.lastUsed) > m.opts.CacheIdle {
			delete(m.cache, k)
		}
	}
	e := m.cache[threadID]
	if e == nil || (wantID != "" && e.inst.ID() != wantID) {
		return nil
	}
	e.lastUsed = now
	return e
}

func (m *DefaultManager) put(threadID string, inst Instance, renewed time.Time) *entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.cache) >= m.opts.CacheSize {
		keys := make([]string, 0, len(m.cache))
		for k := range m.cache {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return m.cache[keys[i]].lastUsed.Before(m.cache[keys[j]].lastUsed) })
		for _, k := range keys[:len(keys)-m.opts.CacheSize+1] {
			delete(m.cache, k)
		}
	}
	e := &entry{inst: inst, lastUsed: time.Now(), lastRenew: renewed, lastHealthy: time.Now()}
	m.cache[threadID] = e
	return e
}

func (m *DefaultManager) drop(threadID string) {
	m.mu.Lock()
	delete(m.cache, threadID)
	m.mu.Unlock()
}

func (m *DefaultManager) renew(ctx context.Context, e *entry) {
	m.mu.Lock()
	due := time.Since(e.lastRenew) >= m.opts.RenewEvery
	if due {
		e.lastRenew = time.Now()
	}
	m.mu.Unlock()
	if !due {
		return
	}
	if err := e.inst.Renew(ctx, m.opts.Timeout); err != nil {
		slog.Warn("sandbox renew failed", "sandbox_id", e.inst.ID(), "error", err)
	}
}

// connect returns the thread's live sandbox. ok=false (nil error) means it is
// definitively gone and was forgotten; transient failures return ErrUnavailable
// and keep the sandbox (a reload or a busy sandbox must never wipe /workspace).
func (m *DefaultManager) connect(ctx context.Context, threadID, id string) (Instance, bool, error) {
	// Each attempt is detached from the caller (client disconnects) and bounded,
	// so a tool call fails fast while the sandbox is unavailable.
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.opts.HealthTimeout)
	defer cancel()
	if e := m.cached(threadID, id); e != nil {
		m.mu.Lock()
		fresh := time.Since(e.lastHealthy) < m.opts.HealthRecheck
		m.mu.Unlock()
		if fresh || e.inst.Healthy(hctx) {
			m.mu.Lock()
			e.lastHealthy = time.Now()
			m.mu.Unlock()
			if !fresh {
				// A healthy check on any replica ends the unhealthy streak (C-15).
				_, _ = m.coord.Del(context.WithoutCancel(ctx), unhealthyKey(threadID, id))
			}
			m.renew(ctx, e)
			return e.inst, true, nil
		}
		m.drop(threadID) // re-check from scratch below
	}
	inst, err := m.backend.Connect(hctx, id)
	switch {
	case errors.Is(err, ErrNoSandbox):
		return nil, false, m.forget(ctx, threadID, id)
	case err != nil:
		return nil, false, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if !inst.Healthy(hctx) {
		if inst.Gone(hctx) {
			return nil, false, m.forget(ctx, threadID, id)
		}
		if m.unhealthyTooLong(ctx, threadID, id) {
			slog.Warn("sandbox unhealthy for too long, recreating it", "thread_id", threadID, "sandbox_id", id)
			// Whoever creates the replacement (maybe another replica, maybe later) tells the model.
			if err := m.coord.Set(context.WithoutCancel(ctx), recreatedKey(threadID), "1", 24*time.Hour); err != nil {
				slog.Warn("sandbox: storing recreation marker failed", "thread_id", threadID, "error", err)
			}
			if err := m.forget(ctx, threadID, id); err != nil {
				return nil, false, err
			}
			return nil, false, errRecreate
		}
		return nil, false, ErrUnavailable
	}
	_, _ = m.coord.Del(context.WithoutCancel(ctx), unhealthyKey(threadID, id))
	e := m.put(threadID, inst, time.Time{})
	m.renew(ctx, e)
	return inst, true, nil
}

func recreatedKey(threadID string) string { return "work:sbx:recreated:" + threadID }

func unhealthyKey(threadID, id string) string { return "work:sbx:unhealthy:" + threadID + ":" + id }

// unhealthyTooLong records a failed health check (shared by all replicas) and
// reports whether the streak lasts longer than UnhealthyRecreate with ≥ 3 checks.
func (m *DefaultManager) unhealthyTooLong(ctx context.Context, threadID, id string) bool {
	ttl := max(2*m.opts.UnhealthyRecreate, time.Hour)
	first, checks, err := m.coord.Unhealthy(context.WithoutCancel(ctx), unhealthyKey(threadID, id), time.Now().UnixMilli(), ttl)
	if err != nil || checks == 0 {
		return false
	}
	return checks >= 3 && time.Since(time.UnixMilli(first)) >= m.opts.UnhealthyRecreate
}

func (m *DefaultManager) forget(ctx context.Context, threadID, id string) error {
	slog.Info("sandbox is gone, forgetting it", "thread_id", threadID, "sandbox_id", id)
	m.drop(threadID)
	if err := m.store.SetThreadSandbox(context.WithoutCancel(ctx), threadID, id, ""); err != nil {
		return err
	}
	go func() {
		kctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.backend.Kill(kctx, id)
	}()
	return nil
}

func (m *DefaultManager) Get(ctx context.Context, threadID string) (Sandbox, error) {
	if !m.Enabled() {
		return nil, ErrNoSandbox
	}
	id, err := m.store.ThreadSandbox(ctx, threadID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNoSandbox
		}
		return nil, err
	}
	if id == "" {
		return nil, ErrNoSandbox
	}
	inst, ok, err := m.connect(ctx, threadID, id)
	if errors.Is(err, errRecreate) {
		return nil, ErrNoSandbox
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNoSandbox
	}
	return m.wrap(inst), nil
}

func lockKey(threadID string) string { return "work:sbx:lock:" + threadID }

// lock takes a Redis lock (SET NX PX LockTTL), polling until LockWait, and
// renews it every LockTTL/3 until released (a creation slower than the TTL
// keeps it; C-10). While waiting, shortcut may return a result that makes the
// lock unnecessary.
func (m *DefaultManager) lock(ctx context.Context, key string, shortcut func() (Instance, bool)) (func(), Instance, error) {
	token := uuid.NewString()
	deadline := time.Now().Add(m.opts.LockWait)
	for {
		ok, err := m.coord.SetNX(ctx, key, token, m.opts.LockTTL)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox lock: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if shortcut != nil {
			if inst, ok := shortcut(); ok {
				return nil, inst, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("%w: timed out waiting for the sandbox lock", ErrUnavailable)
		}
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(max(m.opts.LockTTL/3, 10*time.Millisecond))
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				held, err := m.coord.Renew(rctx, key, token, m.opts.LockTTL)
				cancel()
				if err == nil && !held {
					slog.Warn("sandbox: creation lock lost", "key", key)
					return // the compare-and-set on the thread record still decides
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-stopped
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = m.coord.Unlock(uctx, key, token)
	}, nil, nil
}

func (m *DefaultManager) Acquire(ctx context.Context, threadID, login string) (Sandbox, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	id, err := m.store.ThreadSandbox(ctx, threadID)
	if err != nil {
		return nil, err
	}
	wasRecreated := false
	if id != "" {
		inst, ok, err := m.connect(ctx, threadID, id)
		if errors.Is(err, errRecreate) {
			wasRecreated, err = true, nil
		}
		if err != nil {
			return nil, err
		}
		if ok {
			return m.deliver(ctx, threadID, inst), nil
		}
	}

	unlockThread, inst, err := m.lock(ctx, lockKey(threadID), func() (Instance, bool) {
		// Someone else is creating it: reuse theirs as soon as it is recorded
		// (only once its restore finished).
		if cur, err := m.store.ThreadSandbox(ctx, threadID); err == nil && cur != "" {
			if inst, ok, err := m.connect(ctx, threadID, cur); err == nil && ok {
				return inst, true
			}
		}
		return nil, false
	})
	if err != nil {
		return nil, err
	}
	if inst != nil {
		return m.deliver(ctx, threadID, inst), nil
	}
	defer unlockThread()

	cur, err := m.store.ThreadSandbox(ctx, threadID)
	if err != nil {
		return nil, err
	}
	if cur != "" {
		inst, ok, err := m.connect(ctx, threadID, cur)
		if errors.Is(err, errRecreate) {
			wasRecreated, err = true, nil
		}
		if err != nil {
			return nil, err
		}
		if ok {
			return m.deliver(ctx, threadID, inst), nil
		}
	}
	// count + create + record must be atomic per user, across threads and
	// processes: the user lock is held (and renewed) until the sandbox is recorded.
	unlockUser, _, err := m.lock(ctx, "work:sbx:user:"+UserTag(login), nil)
	if err != nil {
		return nil, err
	}
	defer unlockUser()
	n, err := m.store.CountUserSandboxes(ctx, login)
	if err != nil {
		return nil, err
	}
	if n >= m.opts.MaxPerUser {
		if n, err = m.reclaim(ctx, login, threadID); err != nil {
			return nil, err
		}
		if n >= m.opts.MaxPerUser {
			return nil, ErrQuota
		}
	}
	created, err := m.backend.Create(ctx, CreateOptions{Metadata: map[string]string{
		"work-thread": threadID,
		"work-user":   UserTag(login),
	}})
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	// Before the restore and the first tool (new and recreated sandboxes alike).
	if err := PrepareWorkspace(ctx, created); err != nil {
		slog.Warn("sandbox: creating the workspace layout failed", "sandbox_id", created.ID(), "error", err)
	}
	if existed, err := m.coord.Del(context.WithoutCancel(ctx), recreatedKey(threadID)); err == nil && existed {
		wasRecreated = true // consumed: only this creation carries the notice
	}
	slog.Info("sandbox created", "thread_id", threadID, "sandbox_id", created.ID(), "replaces_unhealthy", wasRecreated)
	notice := ""
	if m.opts.OnCreate != nil {
		// Restored before anyone else can see it: the sandbox is recorded below (C-11).
		notice = m.opts.OnCreate(ctx, threadID, created, wasRecreated)
	}
	recorded, err := m.store.SwapThreadSandbox(context.WithoutCancel(ctx), threadID, "", created.ID())
	if err != nil || !recorded {
		m.killAsync(created.ID())
		if wasRecreated {
			_ = m.coord.Set(context.WithoutCancel(ctx), recreatedKey(threadID), "1", 24*time.Hour)
		}
		if err != nil {
			return nil, err
		}
		return m.recordedInstead(ctx, threadID, created.ID())
	}
	m.put(threadID, created, time.Now())
	sb := m.wrap(created)
	if Quieted(ctx) {
		if notice != "" || wasRecreated {
			m.keepNotice(ctx, threadID, notice, wasRecreated)
		}
		return sb, nil
	}
	if notice != "" {
		return noticed{Sandbox: sb, notice: notice, recreated: wasRecreated}, nil
	}
	if wasRecreated {
		return MarkRecreated(sb), nil
	}
	return sb, nil
}

// recordedInstead is the outcome of a creation that lost the compare-and-set:
// the sandbox recorded for the thread meanwhile, or ErrNoSandbox when the
// thread was deleted (the created sandbox is already being killed; C-10).
func (m *DefaultManager) recordedInstead(ctx context.Context, threadID, createdID string) (Sandbox, error) {
	cur, err := m.store.ThreadSandbox(ctx, threadID)
	if errors.Is(err, store.ErrNotFound) {
		slog.Info("sandbox: thread deleted during creation; sandbox killed", "thread_id", threadID, "sandbox_id", createdID)
		return nil, fmt.Errorf("%w: the conversation was deleted", ErrNoSandbox)
	}
	if err != nil {
		return nil, err
	}
	slog.Warn("sandbox: another sandbox was recorded for the thread during creation; keeping it", "thread_id", threadID,
		"sandbox_id", cur, "killed", createdID)
	if cur == "" {
		return nil, fmt.Errorf("%w: sandbox creation raced; retry", ErrUnavailable)
	}
	inst, ok, err := m.connect(ctx, threadID, cur)
	if errors.Is(err, errRecreate) || (err == nil && !ok) {
		return nil, fmt.Errorf("%w: sandbox creation raced; retry", ErrUnavailable)
	}
	if err != nil {
		return nil, err
	}
	return m.deliver(ctx, threadID, inst), nil
}

func (m *DefaultManager) killAsync(id string) {
	go func() {
		kctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := m.backend.Kill(kctx, id); err != nil {
			slog.Warn("sandbox: killing an unrecorded sandbox failed", "sandbox_id", id, "error", err)
		}
	}()
}

// reclaim forgets the user's recorded sandboxes (other threads) that no longer
// exist at the provider and returns the new count: the quota only counts live
// sandboxes (C-09). Called with the user lock held.
func (m *DefaultManager) reclaim(ctx context.Context, login, threadID string) (int, error) {
	refs, err := m.store.UserSandboxes(ctx, login)
	if err != nil {
		return 0, err
	}
	live := 0
	for _, r := range refs {
		if r.ThreadID == threadID {
			continue
		}
		hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.opts.HealthTimeout)
		inst, err := m.backend.Connect(hctx, r.SandboxID)
		gone := errors.Is(err, ErrNoSandbox) || (err == nil && !inst.Healthy(hctx) && inst.Gone(hctx))
		cancel()
		if !gone {
			live++ // alive, or unknown (transient error): it keeps counting
			continue
		}
		if ok, err := m.store.SwapThreadSandbox(context.WithoutCancel(ctx), r.ThreadID, r.SandboxID, ""); err != nil {
			return 0, err
		} else if ok {
			slog.Info("sandbox: quota reconcile forgot a sandbox that no longer exists", "thread_id", r.ThreadID, "sandbox_id", r.SandboxID)
			m.mu.Lock()
			if e := m.cache[r.ThreadID]; e != nil && e.inst.ID() == r.SandboxID {
				delete(m.cache, r.ThreadID)
			}
			m.mu.Unlock()
			m.killAsync(r.SandboxID)
		}
	}
	return m.store.CountUserSandboxes(ctx, login)
}

// --- notices kept for the agent (C-18) ---

type quietKey struct{}

// Quiet marks an Acquire made on behalf of the API (an Office preview, a
// version or workspace restore): when it creates the sandbox, the notice for
// the model (workspace restored / recreated) is kept for the thread and given
// to the next Acquire that is not quiet (the agent's next tool call).
func Quiet(ctx context.Context) context.Context { return context.WithValue(ctx, quietKey{}, true) }

// Quieted reports a Quiet context.
func Quieted(ctx context.Context) bool { v, _ := ctx.Value(quietKey{}).(bool); return v }

func noticeKey(threadID string) string { return "work:sbx:notice:" + threadID }

func (m *DefaultManager) keepNotice(ctx context.Context, threadID, notice string, wasRecreated bool) {
	flag := "0"
	if wasRecreated {
		flag = "1"
	}
	if err := m.coord.Set(context.WithoutCancel(ctx), noticeKey(threadID), flag+"|"+notice, 24*time.Hour); err != nil {
		slog.Warn("sandbox: keeping the restore notice failed", "thread_id", threadID, "error", err)
	}
}

// deliver wraps a live sandbox and, for the agent, attaches the notice a
// quiet creation kept for the thread (consumed once).
func (m *DefaultManager) deliver(ctx context.Context, threadID string, inst Instance) Sandbox {
	sb := m.wrap(inst)
	if Quieted(ctx) {
		return sb
	}
	v, err := m.coord.Take(context.WithoutCancel(ctx), noticeKey(threadID))
	if err != nil || v == "" {
		return sb
	}
	flag, notice, _ := strings.Cut(v, "|")
	switch {
	case notice != "":
		return noticed{Sandbox: sb, notice: notice, recreated: flag == "1"}
	case flag == "1":
		return MarkRecreated(sb)
	}
	return sb
}

// --- keep-alive (C-17) ---

// managed is a sandbox handed out by the manager: its commands keep the
// sandbox alive while they run.
type managed struct {
	Instance
	m *DefaultManager
}

func (m *DefaultManager) wrap(inst Instance) Sandbox { return managed{Instance: inst, m: m} }

// Exec renews the sandbox's lifetime while the command runs, so a command
// longer than SANDBOX_TIMEOUT_SECONDS never outlives its sandbox.
func (s managed) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	stop := s.m.keepAlive(ctx, s.Instance)
	defer stop()
	return s.Instance.Exec(ctx, req)
}

// WriteFileFrom keeps streamed writes through the wrapper.
func (s managed) WriteFileFrom(ctx context.Context, p string, r io.Reader) error {
	return WriteFrom(ctx, s.Instance, p, r)
}

// keepAlive renews inst every keep-alive interval until stop or ctx ends.
func (m *DefaultManager) keepAlive(ctx context.Context, inst Instance) func() {
	if m.opts.Timeout <= 0 {
		return func() {}
	}
	every := m.opts.RenewEvery
	if third := m.opts.Timeout / 3; third > 0 && third < every {
		every = third
	}
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				if err := inst.Renew(rctx, m.opts.Timeout); err != nil {
					slog.Warn("sandbox keep-alive renew failed", "sandbox_id", inst.ID(), "error", err)
				}
				cancel()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-stopped
		})
	}
}

// KeepAlive renews a sandbox handed out by a DefaultManager (Acquire, Get)
// until the returned stop is called or ctx ends — for work that outlives one
// Exec call (background jobs). Other sandboxes get a no-op.
func KeepAlive(ctx context.Context, sb Sandbox) func() {
	for {
		switch v := sb.(type) {
		case managed:
			return v.m.keepAlive(ctx, v.Instance)
		case noticed:
			sb = v.Sandbox
		case recreated:
			sb = v.Sandbox
		default:
			return func() {}
		}
	}
}

// Invalidate implements Manager.
func (m *DefaultManager) Invalidate(ctx context.Context, threadID, sandboxID string) {
	if !m.Enabled() || sandboxID == "" {
		return
	}
	m.mu.Lock()
	if e := m.cache[threadID]; e != nil && e.inst.ID() == sandboxID {
		delete(m.cache, threadID)
	}
	m.mu.Unlock()
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), m.opts.HealthTimeout)
	defer cancel()
	inst, err := m.backend.Connect(hctx, sandboxID)
	switch {
	case errors.Is(err, ErrNoSandbox):
	case err != nil:
		slog.Warn("sandbox: confirming a gone sandbox failed; keeping it", "thread_id", threadID, "sandbox_id", sandboxID, "error", err)
		return
	case inst.Healthy(hctx) || !inst.Gone(hctx):
		return // still there: the next call reconnects
	}
	if cur, err := m.store.ThreadSandbox(hctx, threadID); err != nil || cur != sandboxID {
		return // already replaced (or unknown): nothing to forget
	}
	// The replacement tells the model that /workspace was reset.
	if err := m.coord.Set(context.WithoutCancel(ctx), recreatedKey(threadID), "1", 24*time.Hour); err != nil {
		slog.Warn("sandbox: storing recreation marker failed", "thread_id", threadID, "error", err)
	}
	if err := m.forget(ctx, threadID, sandboxID); err != nil {
		slog.Warn("sandbox: forgetting a gone sandbox failed", "thread_id", threadID, "sandbox_id", sandboxID, "error", err)
	}
}

func (m *DefaultManager) Kill(ctx context.Context, sandboxID string) error {
	if !m.Enabled() || sandboxID == "" {
		return nil
	}
	m.mu.Lock()
	for k, e := range m.cache {
		if e.inst.ID() == sandboxID {
			delete(m.cache, k)
		}
	}
	m.mu.Unlock()
	return m.backend.Kill(ctx, sandboxID)
}
