package sandbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

func replica(f *fixture, opts sandbox.ManagerOptions) *sandbox.DefaultManager {
	opts.MaxPerUser, opts.Timeout = f.max, time.Hour
	return sandbox.NewManager(f.backend, f.st, f.rdb, opts)
}

func TestSandboxUnhealthyForTooLongIsRecreated(t *testing.T) {
	// A sandbox unhealthy across replicas for longer than the limit (and checked ≥ 3 times) is killed and replaced, and the new one says so.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	old, _ := f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.Created()[0].SetHealthy(false)
	opts := sandbox.ManagerOptions{UnhealthyRecreate: 50 * time.Millisecond}
	_, first := replica(f, opts).Acquire(context.Background(), th.ID, "alice")
	time.Sleep(60 * time.Millisecond)
	_, second := replica(f, opts).Acquire(context.Background(), th.ID, "alice")

	// Act
	sb, err := replica(f, opts).Acquire(context.Background(), th.ID, "alice")

	// Assert
	if !errors.Is(first, sandbox.ErrUnavailable) || !errors.Is(second, sandbox.ErrUnavailable) {
		t.Fatalf("first %v second %v", first, second)
	}
	if err != nil || sb.ID() == old.ID() || !sandbox.WasRecreated(sb) || len(f.backend.Created()) != 2 {
		t.Fatalf("err %v new %v recreated %v created %d", err, sb, sb != nil && sandbox.WasRecreated(sb), len(f.backend.Created()))
	}
	waitKilled(t, f, old.ID())
}

func TestBrieflyUnhealthySandboxIsKept(t *testing.T) {
	// Before the limit, repeated failed health checks keep the sandbox (unavailable, not recreated).
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.Created()[0].SetHealthy(false)
	opts := sandbox.ManagerOptions{UnhealthyRecreate: time.Hour}

	// Act
	var err error
	for i := 0; i < 5; i++ {
		_, err = replica(f, opts).Acquire(context.Background(), th.ID, "alice")
	}

	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) || len(f.backend.Created()) != 1 || len(f.backend.Killed()) != 0 {
		t.Fatalf("err %v created %d killed %v", err, len(f.backend.Created()), f.backend.Killed())
	}
}

func TestHealthAttemptIsBounded(t *testing.T) {
	// A hanging health check fails fast (HealthTimeout) instead of blocking the tool call.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.Created()[0].SetHang(true)
	m := replica(f, sandbox.ManagerOptions{HealthTimeout: 100 * time.Millisecond})

	// Act
	start := time.Now()
	_, err := m.Acquire(context.Background(), th.ID, "alice")

	// Assert
	if !errors.Is(err, sandbox.ErrUnavailable) || time.Since(start) > time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func waitKilled(t *testing.T, f *fixture, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, k := range f.backend.Killed() {
			if k == id {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s not killed: %v", id, f.backend.Killed())
}

func TestRecreationTriggeredByGetIsAnnouncedByNextAcquire(t *testing.T) {
	// When the files panel (Get) drops a long-unhealthy sandbox, the next Acquire's sandbox is marked recreated.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.Created()[0].SetHealthy(false)
	opts := sandbox.ManagerOptions{UnhealthyRecreate: 30 * time.Millisecond}
	replica(f, opts).Get(context.Background(), th.ID)
	replica(f, opts).Get(context.Background(), th.ID)
	time.Sleep(40 * time.Millisecond)
	_, getErr := replica(f, opts).Get(context.Background(), th.ID)

	// Act
	sb, err := replica(f, opts).Acquire(context.Background(), th.ID, "alice")
	again, _ := replica(f, opts).Acquire(context.Background(), th.ID, "alice")

	// Assert
	if !errors.Is(getErr, sandbox.ErrNoSandbox) || err != nil || !sandbox.WasRecreated(sb) || sandbox.WasRecreated(again) {
		t.Fatalf("get %v acquire %v recreated %v again %v", getErr, err, sb != nil && sandbox.WasRecreated(sb), sandbox.WasRecreated(again))
	}
}
