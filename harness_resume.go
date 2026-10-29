package harness

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/GhaziBenDahmane/harness/store"
)

// ResumeRequest answers the interrupt a run ended with.
type ResumeRequest struct {
	// InterruptID is the interrupt's id (the approval id for a tool approval).
	InterruptID string
	// Reason is the interrupt reason: store.ReasonToolApproval or the ask_user reason.
	Reason string
	// Decision applies to tool approvals: store.DecisionApprove, DecisionDeny or DecisionRespond.
	Decision string
	// Always approves the tool for the rest of the run chain (scope "run").
	Always bool
	// Text is the answer to an ask_user question, or the free text of a respond.
	Text string
	// Model, Interactive and ReasoningEffort are the RunOptions of the resumed run.
	Model           string
	Interactive     bool
	ReasoningEffort string
	// Mode is ModeBuild or ModePlan; empty keeps the mode of the thread's previous run.
	Mode RunMode
}

// Resume answers a run's interrupt (a tool approval or an ask_user question) and
// queues the run that continues the conversation. Follow it with Events.
func (h *Harness) Resume(ctx context.Context, user, threadID string, r ResumeRequest) (*store.Run, error) {
	if err := h.ownsThread(ctx, user, threadID); err != nil {
		return nil, err
	}
	if r.Model == "" {
		return nil, fmt.Errorf("%w: model is required", ErrInvalidRequest)
	}
	res := &store.Resume{InterruptID: r.InterruptID}
	if r.Reason == store.ReasonToolApproval {
		res.Decision = r.Decision
		if r.Decision == store.DecisionRespond {
			res.Response = store.ApprovalResponse{ResponseText: r.Text}
		}
		if r.Decision == store.DecisionApprove && r.Always {
			res.Scope = store.ApprovalScopeRun
		}
	} else {
		if strings.TrimSpace(r.Text) == "" {
			return nil, fmt.Errorf("%w: an answer is required", ErrInvalidRequest)
		}
		res.Content = r.Text
	}
	runMd := ""
	if r.Mode != "" {
		var err error
		if runMd, err = runMode(r.Mode); err != nil {
			return nil, err
		}
	}
	mode := ""
	if r.Interactive {
		mode = store.InteractionInteractive
	}
	run, _, err := h.Store.CreateRun(ctx, store.NewRun{
		RunID: uuid.NewString(), ThreadID: threadID, UserLogin: user, Model: r.Model,
		Resume: res, InteractionMode: mode, ReasoningEffort: r.ReasoningEffort, Mode: runMd,
	})
	if err != nil {
		return nil, err
	}
	if err := h.Queue.Enqueue(ctx, run.ID); err != nil {
		return nil, err
	}
	return run, nil
}
