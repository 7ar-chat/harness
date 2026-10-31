package sandboxmanager_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/core/sandbox/sandboxtest"
)

// Sandbox lifecycle correctness (roadmap 1.7: C-09, C-10, C-11, C-15, C-18) and keep-alive (C-17).

type lifecycle struct {
	*fixture
	mr *miniredis.Miniredis
}

func newLifecycle(t *testing.T, maxPerUser int) *lifecycle {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	b := sandboxtest.NewBackend()
	f := &fixture{max: maxPerUser, rdb: rdb, st: st, backend: b, mgr: sandbox.NewManager(b, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: maxPerUser, Timeout: time.Hour})}
	return &lifecycle{fixture: f, mr: mr}
}

// process is a manager of another api/runtime process (no cached handles) with the given options.
func (l *lifecycle) process(opts sandbox.ManagerOptions) *sandbox.DefaultManager {
	if opts.MaxPerUser == 0 {
		opts.MaxPerUser = l.max
	}
	if opts.Timeout == 0 {
		opts.Timeout = time.Hour
	}
	return sandbox.NewManager(l.backend, l.st, redisx.NewCoordinator(l.rdb), opts)
}

func TestQuotaIgnoresSandboxesThatNoLongerExist(t *testing.T) {
	// Expired sandboxes still recorded on other threads do not count against the quota: they are probed and forgotten (C-09).
	// Arrange
	l := newLifecycle(t, 2)
	a, b, c := l.thread(t, "alice"), l.thread(t, "alice"), l.thread(t, "alice")
	for _, th := range []string{a.ID, b.ID} {
		sb, err := l.mgr.Acquire(context.Background(), th, "alice")
		if err != nil {
			t.Fatal(err)
		}
		l.backend.Forget(sb.ID()) // idle-expired at the provider
	}

	// Act
	sb, err := l.process(sandbox.ManagerOptions{}).Acquire(context.Background(), c.ID, "alice")

	// Assert
	staleA, _ := l.st.ThreadSandbox(context.Background(), a.ID)
	if err != nil || sb == nil || staleA != "" {
		t.Fatalf("acquire %v, stale id of thread a %q", err, staleA)
	}
}

func TestQuotaStillCountsLiveSandboxes(t *testing.T) {
	// Live sandboxes of other threads keep counting: the quota is still enforced after the probe (C-09).
	// Arrange
	l := newLifecycle(t, 1)
	a, b := l.thread(t, "alice"), l.thread(t, "alice")
	if _, err := l.mgr.Acquire(context.Background(), a.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := l.process(sandbox.ManagerOptions{}).Acquire(context.Background(), b.ID, "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrQuota) || len(l.backend.Killed()) != 0 {
		t.Fatalf("err %v killed %v", err, l.backend.Killed())
	}
}

func TestLockOutlivesASlowCreateSoAThreadGetsOneSandbox(t *testing.T) {
	// The creation lock is renewed while held: time passing beyond its TTL during a slow Create never lets a second process create another sandbox (C-10).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	const ttl = 300 * time.Millisecond
	opts := sandbox.ManagerOptions{LockTTL: ttl, LockWait: 10 * time.Second}
	var once sync.Once
	second := make(chan error, 1)
	var secondID string
	l.backend.OnCreate = func(*sandboxtest.Sandbox) {
		once.Do(func() {
			go func() {
				sb, err := l.process(opts).Acquire(context.Background(), th.ID, "alice")
				if err == nil {
					secondID = sb.ID()
				}
				second <- err
			}()
			for i := 0; i < 30; i++ { // 3 TTLs of time pass while the provider creates the sandbox
				l.mr.FastForward(ttl / 10)
				time.Sleep(ttl / 10)
			}
		})
	}

	// Act
	first, err := l.process(opts).Acquire(context.Background(), th.ID, "alice")

	// Assert
	secondErr := <-second
	if err != nil || secondErr != nil || first.ID() != secondID || len(l.backend.Created()) != 1 || len(l.backend.Killed()) != 0 {
		t.Fatalf("first %v %v second %q %v created %d killed %v", first, err, secondID, secondErr, len(l.backend.Created()), l.backend.Killed())
	}
}

func TestSandboxLosingTheRecordRaceIsKilledAndTheRecordedOneReturned(t *testing.T) {
	// When another sandbox was recorded for the thread meanwhile, the one just created is killed and the recorded one returned, never leaked (C-10).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	winner := sandboxtest.NewSandbox("sbx-winner")
	l.backend.Add(winner)
	l.backend.OnCreate = func(*sandboxtest.Sandbox) {
		_ = l.st.SetThreadSandbox(context.Background(), th.ID, "", winner.ID()) // recorded by another process
	}

	// Act
	sb, err := l.mgr.Acquire(context.Background(), th.ID, "alice")

	// Assert
	stored, _ := l.st.ThreadSandbox(context.Background(), th.ID)
	if err != nil || sb.ID() != winner.ID() || stored != winner.ID() {
		t.Fatalf("got %v %v stored %q", sb, err, stored)
	}
	waitKilled(t, l.fixture, l.backend.Created()[0].ID())
}

func TestSandboxCreatedForADeletedThreadIsKilled(t *testing.T) {
	// A thread deleted while its sandbox is being created gets no sandbox: the new one is killed, not orphaned (C-10).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	l.backend.OnCreate = func(*sandboxtest.Sandbox) { _, _ = l.st.DeleteThread(context.Background(), "alice", th.ID) }

	// Act
	_, err := l.mgr.Acquire(context.Background(), th.ID, "alice")

	// Assert
	if err == nil {
		t.Fatal("acquire succeeded on a deleted thread")
	}
	waitKilled(t, l.fixture, l.backend.Created()[0].ID())
}

func TestSandboxIsNotVisibleUntilItsRestoreFinished(t *testing.T) {
	// While OnCreate restores a new sandbox, other processes neither Get it nor receive it from Acquire; they get it once the restore is done (C-11).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	restoring, release := make(chan struct{}), make(chan struct{})
	creator := l.process(sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string {
		close(restoring)
		<-release
		return ""
	}})
	created := make(chan sandbox.Sandbox, 1)
	go func() {
		sb, _ := creator.Acquire(context.Background(), th.ID, "alice")
		created <- sb
	}()
	<-restoring
	other := l.process(sandbox.ManagerOptions{})
	waiter := make(chan sandbox.Sandbox, 1)
	go func() {
		sb, _ := other.Acquire(context.Background(), th.ID, "alice")
		waiter <- sb
	}()

	// Act
	_, getErr := other.Get(context.Background(), th.ID)
	stored, _ := l.st.ThreadSandbox(context.Background(), th.ID)
	var early sandbox.Sandbox
	select {
	case early = <-waiter:
	case <-time.After(300 * time.Millisecond):
	}
	close(release)

	// Assert
	mine, theirs := <-created, early
	if theirs == nil {
		theirs = <-waiter
	}
	if !errors.Is(getErr, sandbox.ErrNoSandbox) || stored != "" || early != nil {
		t.Fatalf("during the restore: get %v, stored %q, acquire returned %v", getErr, stored, early)
	}
	if mine == nil || theirs == nil || mine.ID() != theirs.ID() || len(l.backend.Created()) != 1 {
		t.Fatalf("after the restore: %v %v created %d", mine, theirs, len(l.backend.Created()))
	}
}

func TestHealthyCheckOnAnyReplicaResetsTheUnhealthyStreak(t *testing.T) {
	// Failed health checks followed by a healthy one (on a replica's cached handle) start over: a later single blip never recreates the sandbox (C-15).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	opts := sandbox.ManagerOptions{UnhealthyRecreate: 50 * time.Millisecond, HealthRecheck: time.Millisecond}
	cachedReplica := l.process(opts)
	if _, err := cachedReplica.Acquire(context.Background(), th.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	sb := l.backend.Created()[0]
	sb.SetHealthy(false)
	_, _ = l.process(opts).Acquire(context.Background(), th.ID, "alice") // failed check 1
	_, _ = l.process(opts).Acquire(context.Background(), th.ID, "alice") // failed check 2
	sb.SetHealthy(true)
	time.Sleep(60 * time.Millisecond)
	if _, err := cachedReplica.Acquire(context.Background(), th.ID, "alice"); err != nil { // healthy, cached path
		t.Fatal(err)
	}
	sb.SetHealthy(false)

	// Act
	_, err := l.process(opts).Acquire(context.Background(), th.ID, "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) || len(l.backend.Created()) != 1 || len(l.backend.Killed()) != 0 {
		t.Fatalf("err %v created %d killed %v", err, len(l.backend.Created()), l.backend.Killed())
	}
}

func TestRestoreNoticeOfASandboxTheAPICreatedReachesTheNextAgentCall(t *testing.T) {
	// When the api recreates a thread's sandbox (preview, restore), its restore notice is kept and given once to the next agent Acquire (C-18).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	notice := "Notice: this conversation's sandbox was restarted."
	api := l.process(sandbox.ManagerOptions{OnCreate: func(context.Context, string, sandbox.Sandbox, bool) string { return notice }})
	apiSb, err := api.Acquire(sandbox.Quiet(context.Background()), th.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	runtimeMgr := l.process(sandbox.ManagerOptions{})

	// Act
	first, err1 := runtimeMgr.Acquire(context.Background(), th.ID, "alice")
	second, err2 := runtimeMgr.Acquire(context.Background(), th.ID, "alice")

	// Assert
	if sandbox.NoticeOf(apiSb) != "" || err1 != nil || err2 != nil {
		t.Fatalf("api notice %q err %v %v", sandbox.NoticeOf(apiSb), err1, err2)
	}
	if sandbox.NoticeOf(first) != notice || sandbox.NoticeOf(second) != "" || first.ID() != apiSb.ID() {
		t.Fatalf("first %q second %q", sandbox.NoticeOf(first), sandbox.NoticeOf(second))
	}
}

func TestExecRenewsTheSandboxWhileTheCommandRuns(t *testing.T) {
	// A command running longer than the renewal interval keeps renewing the sandbox's lifetime, so a long command never outlives its sandbox (C-17).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	m := l.process(sandbox.ManagerOptions{RenewEvery: 20 * time.Millisecond})
	sb, err := m.Acquire(context.Background(), th.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	inner := l.backend.Created()[0]
	inner.SetExec(func(ctx context.Context, _ sandbox.ExecRequest) (sandbox.ExecResult, error) {
		time.Sleep(200 * time.Millisecond)
		return sandbox.ExecResult{}, nil
	})
	before := inner.Renewals()

	// Act
	_, err = sb.Exec(context.Background(), sandbox.ExecRequest{Command: "sleep 0.2"})

	// Assert
	time.Sleep(50 * time.Millisecond)
	after := inner.Renewals()
	if err != nil || after-before < 4 {
		t.Fatalf("err %v renewals during the command %d", err, after-before)
	}
	time.Sleep(60 * time.Millisecond)
	if inner.Renewals() != after {
		t.Fatalf("renewals continue after the command: %d → %d", after, inner.Renewals())
	}
}

func TestKeepAliveRenewsUntilStopped(t *testing.T) {
	// sandbox.KeepAlive renews a managed sandbox until stopped (background jobs build on it; C-17).
	// Arrange
	l := newLifecycle(t, 5)
	th := l.thread(t, "alice")
	m := l.process(sandbox.ManagerOptions{RenewEvery: 20 * time.Millisecond})
	sb, err := m.Acquire(context.Background(), th.ID, "alice")
	if err != nil {
		t.Fatal(err)
	}
	inner := l.backend.Created()[0]
	before := inner.Renewals()

	// Act
	stop := sandbox.KeepAlive(context.Background(), sb)
	time.Sleep(150 * time.Millisecond)
	stop()

	// Assert
	after := inner.Renewals()
	time.Sleep(60 * time.Millisecond)
	if after-before < 4 || inner.Renewals() != after {
		t.Fatalf("renewals %d → %d → %d", before, after, inner.Renewals())
	}
}
