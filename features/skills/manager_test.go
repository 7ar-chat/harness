package skills_test

import (
	"context"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

// staticManager hands out one running sandbox (none when empty) and never creates one.
type staticManager struct{ sb *sandboxtest.Sandbox }

func (m staticManager) Enabled() bool { return true }
func (m staticManager) Acquire(ctx context.Context, threadID, _ string) (sandbox.Sandbox, error) {
	return m.Get(ctx, threadID)
}
func (m staticManager) Get(context.Context, string) (sandbox.Sandbox, error) {
	if m.sb == nil {
		return nil, sandbox.ErrNoSandbox
	}
	return m.sb, nil
}
func (m staticManager) Kill(context.Context, string) error         { return nil }
func (m staticManager) Invalidate(context.Context, string, string) {}
