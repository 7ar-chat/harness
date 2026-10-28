package permissions

import (
	"context"
	"log/slog"
	"slices"

	"github.com/cloudwego/eino/schema"

	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/approvals"
	"github.com/GhaziBenDahmane/harness/engine/tools"
	"github.com/GhaziBenDahmane/harness/store"
)

// Hook is the agent's BeforeToolHook for the local/coding profile. Calls the
// policy allows run at once; calls it asks about pause the run with a normal
// approval interrupt (resumed by the approval decision); calls it denies are
// refused with a clear tool error (it implements agent.ToolDenier). Tools the
// policy has no opinion on go to Inner, the interactive-mode approval gate.
type Hook struct {
	Policy *Policy
	Inner  *approvals.Gate
}

// NeedsApproval implements agent.BeforeToolHook.
func (h *Hook) NeedsApproval(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	d, judged := h.Policy.Decide(toolName, arguments)
	if !judged {
		return h.Inner != nil && h.Inner.NeedsApproval(ctx, run, toolName, arguments)
	}
	if d.Action != Ask {
		return false
	}
	// "Approve for the run" covers file edits (their path was shown), never commands.
	if ToolClass(toolName) == classEdit && slices.Contains(run.ApprovedTools, approvals.Kind(toolName)) {
		return false
	}
	if d.Once && slices.Contains(run.ApprovedTools, approvals.Kind(toolName)) {
		return false
	}
	slog.Info("permissions: asking", "tool", toolName, "reason", d.Reason, "run_id", run.ID)
	return true
}

// Risky implements agent.RiskJudge: whether the call would be asked, whatever
// the run's mode, so calls behind a pending approval keep waiting.
func (h *Hook) Risky(ctx context.Context, run *store.Run, toolName, arguments string) bool {
	d, judged := h.Policy.Decide(toolName, arguments)
	if !judged {
		return h.Inner != nil && h.Inner.Risky(ctx, run, toolName, arguments)
	}
	return d.Action == Ask
}

// Pause implements agent.BeforeToolHook: it records the approval like the
// interactive gate does.
func (h *Hook) Pause(ctx context.Context, run *store.Run, tc schema.ToolCall) (*tools.Interrupt, error) {
	return h.Inner.Pause(ctx, run, tc)
}

// Denial implements agent.ToolDenier: a non-empty text refuses the call.
func (h *Hook) Denial(_ context.Context, run *store.Run, toolName, arguments string) string {
	d, judged := h.Policy.Decide(toolName, arguments)
	if !judged || d.Action != Deny {
		return ""
	}
	slog.Info("permissions: denied", "tool", toolName, "reason", d.Reason, "run_id", run.ID)
	return DeniedMessage(d)
}

// DeniedMessage is the tool result of a refused call.
func DeniedMessage(d Decision) string {
	return "Error: permission denied: " + d.Reason + ". The action was not run and the permission policy does not let it run; " +
		"do not retry it. Choose another approach, or tell the user what you wanted to do so they can change " + config.DirName + "/permissions.yml."
}
