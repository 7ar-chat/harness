package workspace

import (
	"context"
	"errors"
	"log/slog"

	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/engine/tools"
)

// GitPromptHeading heads the local Git section of the system prompt.
const GitPromptHeading = "## Local Git Checkpoints"

// GitPrompt is the local Git prompt section.
const GitPrompt = GitPromptHeading + `

This conversation's /workspace has a local private Git repository used only for organizing and reviewing workspace file changes. You may use local git commands such as status, diff, add, and commit to group meaningful file changes with clear commit messages when helpful. Do not push, fetch, pull, create remotes, or treat this as a user repository. Keep commits small and descriptive so the user can inspect or roll back file changes later.`

// PromptSection adds the local Git section to runs with sandbox tools.
func (s *Service) PromptSection(_ context.Context, in agent.Input) string {
	if !s.Enabled() || !in.Sandbox {
		return ""
	}
	return GitPrompt
}

var _ agent.RunStartHook = (*Service)(nil)

// BeforeRun implements agent.RunStartHook: a run starting while the thread's
// sandbox is live checkpoints it first (never creates one; a sandbox created
// later in the run is checkpointed by OnAcquire).
func (s *Service) BeforeRun(ctx context.Context, run *store.Run) {
	if !s.Enabled() || run.Sandbox == store.SandboxOff {
		return
	}
	sb, err := s.Sandbox.Get(ctx, run.ThreadID)
	if err != nil {
		if !errors.Is(err, sandbox.ErrNoSandbox) {
			slog.Warn("workspace: sandbox unavailable for the checkpoint before the AI turn", "run_id", run.ID, "error", err)
		}
		return
	}
	s.BeforeTurn(ctx, run.ThreadID, run.ID, sb)
}

// OnAcquire is the sandbox tools' acquisition hook: the first acquisition of
// a run in a sandbox takes the "Before AI turn" checkpoint (lazily, when the
// run created the sandbox).
func (s *Service) OnAcquire(ctx context.Context, rc tools.RunContext, sb sandbox.Sandbox) {
	s.BeforeTurn(ctx, rc.ThreadID, rc.RunID, sb)
}

// EndOfRunHook archives the thread's checkpoint history when a run ends
// (register it after artifacts.EndOfRunHook), in the background: the run's
// terminal status and event do not wait for the upload.
type EndOfRunHook struct{ Svc *Service }

// Finishing implements runtime.Hook.
func (h EndOfRunHook) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if h.Svc == nil {
		return nil
	}
	defer h.Svc.forgetRun(f.Run.ID)
	if f.Run.Kind == store.KindCompact || !h.Svc.Enabled() {
		return nil
	}
	sb, err := h.Svc.Sandbox.Get(context.WithoutCancel(ctx), f.Run.ThreadID)
	if err != nil {
		return nil
	}
	h.Svc.ArchiveAsync(f.Run.ThreadID, sb)
	return nil
}

// OnCreate restores the archived history into a new sandbox; call it before
// the artifacts' workspace restore.
func (s *Service) OnCreate(ctx context.Context, threadID string, sb sandbox.Sandbox) {
	s.RestoreHistory(ctx, threadID, sb)
}
