package tui

import (
	"context"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/store"
)

// Decision kinds of a Resume.
const (
	DecisionApprove = store.DecisionApprove
	DecisionDeny    = store.DecisionDeny
	DecisionRespond = store.DecisionRespond
)

// ResumeRequest answers the interrupt a run ended with.
type ResumeRequest struct {
	// InterruptID is the interrupt's id (the approval id for tool approvals).
	InterruptID string
	// Reason is the interrupt reason ("tool_approval" or "ask_user").
	Reason string
	// Decision applies to tool approvals: approve, deny or respond.
	Decision string
	// Always approves the tool for the rest of the run chain (scope "run").
	Always bool
	// Text is the answer to an ask_user question, or the free text of a respond.
	Text string
}

// Backend is what the terminal client needs from a harness. HarnessBackend
// implements it over a harness.Harness; tests use a fake.
type Backend interface {
	// Model is the model id runs use (shown in the status line).
	Model() string
	// Send starts a run answering text and returns its id.
	Send(ctx context.Context, text string) (runID string, err error)
	// Events streams the run's events from the start until its terminal event.
	Events(ctx context.Context, runID string) <-chan store.Event
	// Steer adds a message to the running run.
	Steer(ctx context.Context, runID, text string) error
	// Cancel stops the running run.
	Cancel(ctx context.Context, runID string) error
	// Resume answers the interrupt a run ended with, which starts a new run.
	Resume(ctx context.Context, r ResumeRequest) (runID string, err error)
}

// HarnessBackend runs one thread of a harness.Harness.
type HarnessBackend struct {
	H        *harness.Harness
	User     string
	ThreadID string
	ModelID  string
	// Interactive makes risky tool calls wait for approval.
	Interactive bool
	// ReasoningEffort is passed to every run.
	ReasoningEffort string
}

// Model implements Backend.
func (b *HarnessBackend) Model() string { return b.ModelID }

// Send implements Backend.
func (b *HarnessBackend) Send(ctx context.Context, text string) (string, error) {
	run, err := b.H.Send(ctx, b.User, b.ThreadID, text, harness.RunOptions{
		Model: b.ModelID, Interactive: b.Interactive, ReasoningEffort: b.ReasoningEffort,
	})
	if err != nil {
		return "", err
	}
	return run.ID, nil
}

// Events implements Backend.
func (b *HarnessBackend) Events(ctx context.Context, runID string) <-chan store.Event {
	return b.H.Events(ctx, runID, 0)
}

// Steer implements Backend.
func (b *HarnessBackend) Steer(ctx context.Context, runID, text string) error {
	return b.H.Steer(ctx, b.User, runID, text)
}

// Cancel implements Backend.
func (b *HarnessBackend) Cancel(ctx context.Context, runID string) error {
	return b.H.Cancel(ctx, b.User, runID)
}

// Resume implements Backend.
func (b *HarnessBackend) Resume(ctx context.Context, r ResumeRequest) (string, error) {
	run, err := b.H.Resume(ctx, b.User, b.ThreadID, harness.ResumeRequest{
		InterruptID: r.InterruptID, Reason: r.Reason, Decision: r.Decision, Always: r.Always, Text: r.Text,
		Model: b.ModelID, Interactive: b.Interactive, ReasoningEffort: b.ReasoningEffort,
	})
	if err != nil {
		return "", err
	}
	return run.ID, nil
}
