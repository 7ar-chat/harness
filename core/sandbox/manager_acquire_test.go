package sandbox_test

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
	"github.com/GhaziBenDahmane/harness/core/store"
)

type fixture struct {
	max     int
	rdb     *redis.Client
	st      *memstore.Store
	backend *sandboxtest.Backend
	mgr     *sandbox.DefaultManager
}

func newFixture(t *testing.T, maxPerUser int) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	b := sandboxtest.NewBackend()
	return &fixture{max: maxPerUser, rdb: rdb, st: st, backend: b, mgr: sandbox.NewManager(b, st, redisx.NewCoordinator(rdb), sandbox.ManagerOptions{MaxPerUser: maxPerUser, Timeout: time.Hour, RenewEvery: time.Hour})}
}

func (f *fixture) thread(t *testing.T, login string) *store.Thread {
	t.Helper()
	th, err := f.st.CreateThread(context.Background(), login, "t")
	if err != nil {
		t.Fatal(err)
	}
	return th
}

func TestAcquireCreatesSandboxLazilyAndReusesIt(t *testing.T) {
	// The first Acquire creates a tagged sandbox with /workspace and stores its id; later calls reuse it.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")

	// Act
	first, err1 := f.mgr.Acquire(context.Background(), th.ID, "alice")
	second, err2 := f.mgr.Acquire(context.Background(), th.ID, "alice")

	// Assert
	if err1 != nil || err2 != nil || first.ID() != second.ID() || len(f.backend.Created()) != 1 {
		t.Fatalf("err %v %v ids %s %s created %d", err1, err2, first.ID(), second.ID(), len(f.backend.Created()))
	}
	id, _ := f.st.ThreadSandbox(context.Background(), th.ID)
	md := f.backend.CreateOptions()[0].Metadata
	if id != first.ID() || md["work-thread"] != th.ID || md["work-user"] != sandbox.UserTag("alice") || !f.backend.Created()[0].HasDir("/workspace") {
		t.Fatalf("stored %q metadata %v", id, md)
	}
}

func TestAcquireReplacesExpiredSandbox(t *testing.T) {
	// When the stored sandbox no longer exists, a new one is created and its id replaces the old one.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	old, _ := f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.Forget(old.ID())
	fresh := otherProcess(f) // no cached handle

	// Act
	sb, err := fresh.Acquire(context.Background(), th.ID, "alice")

	// Assert
	id, _ := f.st.ThreadSandbox(context.Background(), th.ID)
	if err != nil || sb.ID() == old.ID() || id != sb.ID() || len(f.backend.Created()) != 2 {
		t.Fatalf("err %v new %s old %s stored %s", err, sb.ID(), old.ID(), id)
	}
}

// otherProcess is a manager of another api/runtime process sharing Redis, Postgres and the provider.
func otherProcess(f *fixture) *sandbox.DefaultManager {
	return sandbox.NewManager(f.backend, f.st, redisx.NewCoordinator(f.rdb), sandbox.ManagerOptions{MaxPerUser: f.max, Timeout: time.Hour})
}

func TestAcquireEnforcesPerUserQuota(t *testing.T) {
	// A user cannot hold more sandboxes than MAX_SANDBOXES_PER_USER.
	// Arrange
	f := newFixture(t, 1)
	a, b := f.thread(t, "alice"), f.thread(t, "alice")
	if _, err := f.mgr.Acquire(context.Background(), a.ID, "alice"); err != nil {
		t.Fatal(err)
	}

	// Act
	_, err := f.mgr.Acquire(context.Background(), b.ID, "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrQuota) || len(f.backend.Created()) != 1 {
		t.Fatalf("err %v created %d", err, len(f.backend.Created()))
	}
}

func TestAcquireConcurrentCallsCreateOneSandbox(t *testing.T) {
	// Concurrent first uses of a thread from several processes create a single sandbox (Redis lock).
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	var wg sync.WaitGroup
	ids := make([]string, 8)

	// Act
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sb, err := otherProcess(f).Acquire(context.Background(), th.ID, "alice")
			if err == nil {
				ids[i] = sb.ID()
			}
		}(i)
	}
	wg.Wait()

	// Assert
	for _, id := range ids {
		if id != ids[0] || id == "" {
			t.Fatalf("ids %v", ids)
		}
	}
	if len(f.backend.Created()) != 1 {
		t.Fatalf("created %d", len(f.backend.Created()))
	}
}

func TestGetNeverCreatesSandbox(t *testing.T) {
	// Get reports ErrNoSandbox for a thread without sandbox and does not create one.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")

	// Act
	_, err := f.mgr.Get(context.Background(), th.ID)

	// Assert
	if !errors.Is(err, sandbox.ErrNoSandbox) || len(f.backend.Created()) != 0 {
		t.Fatalf("err %v created %d", err, len(f.backend.Created()))
	}
}

func TestKillDestroysSandbox(t *testing.T) {
	// Kill destroys the sandbox at the provider.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	sb, _ := f.mgr.Acquire(context.Background(), th.ID, "alice")

	// Act
	err := f.mgr.Kill(context.Background(), sb.ID())

	// Assert
	if err != nil || len(f.backend.Killed()) != 1 || f.backend.Killed()[0] != sb.ID() {
		t.Fatalf("err %v killed %v", err, f.backend.Killed())
	}
}
