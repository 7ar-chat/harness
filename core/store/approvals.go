package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Interaction modes of a run: interactive runs pause before risky tools until
// the user approves them (Work's "interactive" mode); automatic runs never pause.
const (
	InteractionAutomatic   = "automatic"
	InteractionInteractive = "interactive"
)

// Approval kinds, statuses and decisions (Work's approval record).
const (
	ApprovalKindRiskyTool = "risky_tool"

	ApprovalPending   = "pending"
	ApprovalResolved  = "resolved"
	ApprovalCancelled = "cancelled"

	DecisionApprove = "approve"
	DecisionDeny    = "deny"
	DecisionRespond = "respond"

	// ReasonToolApproval is the AG-UI interrupt reason of a paused risky tool call.
	ReasonToolApproval = "tool_approval"
)

// Tool results given to the model for a resolved approval (Work's
// build_resume_packet instructions) and for cancelled ones.
const (
	ApprovalDeniedResult = "The user denied the pending action. Do not retry it. Choose a safe alternative and continue."
	// ApprovalCancelledResult is the result of an approval cancelled with the run (stop button).
	ApprovalCancelledResult = "The tool call was cancelled by the user."
	// ImplicitApprovalCancelResult is stored when the user sends a new message instead of deciding.
	ImplicitApprovalCancelResult = "The user did not approve the pending action and sent a new message instead. The action was not run."
)

var (
	// ErrApprovalPending means the run kind cannot start while an approval waits for a decision.
	ErrApprovalPending = errors.New("an approval is pending")
	// ErrApprovalNotPending means the approval is resolved, cancelled, or its run is not awaiting it.
	ErrApprovalNotPending = errors.New("approval is not pending")
)

// ApprovalResponse is the user's answer of a respond decision.
type ApprovalResponse struct {
	SelectedChoice string
	ResponseText   string
}

// Approval is a risky tool call of an interactive run waiting for (or having
// received) the user's decision. One approval at most is pending per thread.
type Approval struct {
	ID               string
	Kind             string
	ThreadID         string
	RunID            string
	ToolCallID       string
	ToolName         string
	ToolInputPreview string
	Status           string
	// Decision is approve, deny or respond once resolved ("" otherwise).
	Decision string
	Response ApprovalResponse
	// ResumeRunID is the run created by the decision (or the cancelling message).
	ResumeRunID string
	// Scope of the decision: once or run (harness v3 §7; "" before a decision is once).
	Scope      string
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

// Payload is the approval in Work's record shape (camelCase), as sent in
// work.approval events, interrupts and thread.pendingApproval.
func (a *Approval) Payload() map[string]any {
	kind := a.Kind
	if kind == "" {
		kind = ApprovalKindRiskyTool
	}
	out := map[string]any{
		"id": a.ID, "kind": kind, "threadId": a.ThreadID, "runId": a.RunID, "toolCallId": a.ToolCallID,
		"toolName": a.ToolName, "toolInputPreview": a.ToolInputPreview, "status": a.Status,
		"createdAt": a.CreatedAt.UTC().Format(time.RFC3339Nano), "resolvedAt": nil, "decision": nil, "response": nil,
	}
	if a.ResolvedAt != nil {
		out["resolvedAt"] = a.ResolvedAt.UTC().Format(time.RFC3339Nano)
	}
	scope := a.Scope
	if scope == "" {
		scope = ApprovalScopeOnce
	}
	out["scope"] = scope
	if a.Decision != "" {
		out["decision"] = a.Decision
		out["response"] = map[string]any{"selectedChoice": nilIfEmpty(a.Response.SelectedChoice), "responseText": nilIfEmpty(a.Response.ResponseText)}
	}
	return out
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ApprovalResult is the tool result of a deny or respond decision (Work's
// build_resume_packet instruction); "" for approve (the tool runs).
func ApprovalResult(decision string, r ApprovalResponse) string {
	switch decision {
	case DecisionApprove:
		return ""
	case DecisionRespond:
		var details []string
		if s := strings.TrimSpace(r.SelectedChoice); s != "" {
			details = append(details, "selected option: "+s)
		}
		if s := strings.TrimSpace(r.ResponseText); s != "" {
			details = append(details, "free text: "+s)
		}
		text := "no extra details provided"
		if len(details) > 0 {
			text = strings.Join(details, "; ")
		}
		return "The user answered the pending question. Use this answer and continue: " + text + "."
	default:
		return ApprovalDeniedResult
	}
}

// SameDecision reports whether a repeated resolve asks for what was already decided.
func (a *Approval) SameDecision(decision string, r ApprovalResponse) bool {
	return a.Status == ApprovalResolved && a.Decision == decision &&
		strings.TrimSpace(a.Response.SelectedChoice) == strings.TrimSpace(r.SelectedChoice) &&
		strings.TrimSpace(a.Response.ResponseText) == strings.TrimSpace(r.ResponseText)
}

// ApprovalStore is the persistence of tool approvals (interactive mode). An
// approval counts as pending for its thread only while its run is completed
// (the run ended waiting for the decision).
type ApprovalStore interface {
	// CreateApproval records a pending approval (ID, CreatedAt are set when empty).
	CreateApproval(ctx context.Context, a *Approval) error
	// GetApproval returns an approval by id (no ownership check; ErrNotFound).
	GetApproval(ctx context.Context, id string) (*Approval, error)
	// ResumedApproval returns the approval a run was created to decide or that
	// its message cancelled, or the latest one cancelled with its waiting run
	// (stop) before this run, which the run announces (nil when none).
	ResumedApproval(ctx context.Context, runID string) (*Approval, error)
	// CancelRunApprovals cancels the pending approvals of a run that did not
	// complete (no tool result is stored: the history marks the call interrupted).
	CancelRunApprovals(ctx context.Context, runID string) error
	// CancelPendingApproval cancels the pending approval of a completed run and
	// stores result as its tool result (ErrApprovalNotPending when none).
	CancelPendingApproval(ctx context.Context, runID, result string) (*Approval, error)
	// ExpireApprovals expires the pending approvals created more than ttl ago
	// (of threadID only when it is not empty): status expired, tool result
	// ApprovalExpiredResult for those whose run waits for them. It returns how many.
	ExpireApprovals(ctx context.Context, ttl time.Duration, threadID string) (int, error)
	// LatestExpiredApproval returns the thread's latest approval when it is
	// expired and belongs to the thread's latest run (nothing happened since);
	// nil when there is none (review round 1: Thread.pendingApproval on reload).
	LatestExpiredApproval(ctx context.Context, threadID string) (*Approval, error)
	// ReopenApproval makes a resolved approval pending again when its resume
	// run finished (completed, failed, cancelled or interrupted) without a tool
	// result for the approved call, is still the thread's latest run and no run
	// is active: the decision never took effect, so the user may decide again.
	// ErrApprovalNotPending when that is not the case.
	ReopenApproval(ctx context.Context, id string) (*Approval, error)
	// SetThreadInteractionMode switches login's thread and its active (queued or
	// running) run to mode in one transaction: the run's later tool calls and
	// every next run of the thread follow it; a pending approval stays pending.
	// ErrNotFound when the thread is not login's or is deleted.
	SetThreadInteractionMode(ctx context.Context, login, threadID, mode string) error
	// RunInteractionMode returns the run's current interaction mode ("" is
	// automatic): the approval gate reads it before every tool call. ErrNotFound.
	RunInteractionMode(ctx context.Context, runID string) (string, error)
}
