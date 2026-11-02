package sandboxmanager_test

import (
	"context"
	"errors"
	"testing"
)

func TestInvalidateKeepsASandboxTheProviderStillHas(t *testing.T) {
	// An operation error the provider does not confirm (the sandbox still exists, or the provider is unreachable) never forgets the sandbox.
	// Arrange
	f := newFixture(t, 5)
	th := f.thread(t, "alice")
	sb, _ := f.mgr.Acquire(context.Background(), th.ID, "alice")
	f.mgr.Invalidate(context.Background(), th.ID, sb.ID()) // still alive
	f.backend.SetConnectErr(errors.New("provider timeout"))
	f.mgr.Invalidate(context.Background(), th.ID, sb.ID()) // unconfirmed
	f.backend.SetConnectErr(nil)

	// Act
	again, err := f.mgr.Acquire(context.Background(), th.ID, "alice")

	// Assert
	id, _ := f.st.ThreadSandbox(context.Background(), th.ID)
	if err != nil || again.ID() != sb.ID() || id != sb.ID() || len(f.backend.Created()) != 1 {
		t.Fatalf("err %v again %v stored %q created %d", err, again, id, len(f.backend.Created()))
	}
}
