package sandbox_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GhaziBenDahmane/harness/sandbox"
)

func TestUnhealthySandboxIsNotDestroyed(t *testing.T) {
	// A sandbox failing its health check (e.g. CPU-saturated) is reported unavailable, never forgotten or killed.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	sb, _ := f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.Created()[0].SetHealthy(false)
	fresh := otherProcess(f)

	// Act
	_, acqErr := fresh.Acquire(context.Background(), th.ID, "alice")
	_, getErr := otherProcess(f).Get(context.Background(), th.ID)

	// Assert
	id, _ := f.st.ThreadSandbox(context.Background(), th.ID)
	if !errors.Is(acqErr, sandbox.ErrUnavailable) || !errors.Is(getErr, sandbox.ErrUnavailable) {
		t.Fatalf("acquire %v get %v", acqErr, getErr)
	}
	if id != sb.ID() || len(f.backend.Killed()) != 0 || len(f.backend.Created()) != 1 {
		t.Fatalf("stored %q killed %v created %d", id, f.backend.Killed(), len(f.backend.Created()))
	}
}

func TestTransientConnectFailureKeepsSandbox(t *testing.T) {
	// A provider error other than "not found" (timeout, 5xx, cancelled request) keeps the sandbox id.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	sb, _ := f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.backend.SetConnectErr(errors.New("dial tcp: i/o timeout"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	_, errTimeout := otherProcess(f).Acquire(context.Background(), th.ID, "alice")
	_, errCancelled := otherProcess(f).Get(ctx, th.ID)

	// Assert
	id, _ := f.st.ThreadSandbox(context.Background(), th.ID)
	if errTimeout == nil || errCancelled == nil || errors.Is(errTimeout, sandbox.ErrNoSandbox) || id != sb.ID() || len(f.backend.Killed()) != 0 {
		t.Fatalf("timeout %v cancelled %v stored %q killed %v", errTimeout, errCancelled, id, f.backend.Killed())
	}
}

func TestQuotaHoldsUnderConcurrentCreates(t *testing.T) {
	// Concurrent first uses of different threads of one user never exceed MAX_SANDBOXES_PER_USER.
	// Arrange
	f := newFixture(t, 1)
	f.backend.CreateDelay = 50 * 1e6
	threads := []string{f.thread(t, "alice").ID, f.thread(t, "alice").ID, f.thread(t, "alice").ID}
	errs := make(chan error, len(threads))

	// Act
	for _, id := range threads {
		go func(id string) {
			_, err := otherProcess(f).Acquire(context.Background(), id, "alice")
			errs <- err
		}(id)
	}
	quota := 0
	for range threads {
		if errors.Is(<-errs, sandbox.ErrQuota) {
			quota++
		}
	}

	// Assert
	if n, _ := f.st.CountUserSandboxes(context.Background(), "alice"); n != 1 || quota != 2 || len(f.backend.Created()) != 1 {
		t.Fatalf("sandboxes %d quota errors %d created %d", n, quota, len(f.backend.Created()))
	}
}
