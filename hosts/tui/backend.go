package tui

import (
	"context"
	"encoding/json"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/hosts/client"
)

// Decision kinds of a Resume.
const (
	DecisionApprove = client.DecisionApprove
	DecisionDeny    = client.DecisionDeny
	DecisionRespond = client.DecisionRespond
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

// Backend is what the terminal client needs from a harness: one thread, one
// model. ClientBackend implements it over any client.Client (in process or
// remote); tests use a fake.
type Backend interface {
	// Model is the model id runs use (shown in the status line).
	Model() string
	// Send starts a run answering text and returns its id.
	Send(ctx context.Context, text string) (runID string, err error)
	// Events streams the run's events from the start until its terminal event.
	Events(ctx context.Context, runID string) <-chan client.Event
	// Steer adds a message to the running run.
	Steer(ctx context.Context, runID, text string) error
	// Cancel stops the running run.
	Cancel(ctx context.Context, runID string) error
	// Resume answers the interrupt a run ended with, which starts a new run.
	Resume(ctx context.Context, r ResumeRequest) (runID string, err error)
}

// ClientBackend runs one thread through a client.Client.
type ClientBackend struct {
	C        client.Client
	ThreadID string
	ModelID  string
	// Interactive makes risky tool calls wait for approval.
	Interactive bool
	// ReasoningEffort is passed to every run.
	ReasoningEffort string
}

// Model implements Backend.
func (b *ClientBackend) Model() string { return b.ModelID }

// Send implements Backend.
func (b *ClientBackend) Send(ctx context.Context, text string) (string, error) {
	run, err := b.C.Send(ctx, b.ThreadID, text, client.SendOptions{
		Model: b.ModelID, Interactive: b.Interactive, ReasoningEffort: b.ReasoningEffort,
	})
	if err != nil {
		return "", err
	}
	return run.ID, nil
}

// Events implements Backend. A run that cannot be streamed yields a RUN_ERROR event.
func (b *ClientBackend) Events(ctx context.Context, runID string) <-chan client.Event {
	ch, err := b.C.Events(ctx, runID, 0)
	if err != nil {
		out := make(chan client.Event, 1)
		data, _ := json.Marshal(agui.RunErrorEvent("stream_failed", err.Error()))
		out <- client.Event{Type: agui.RunError, Data: data}
		close(out)
		return out
	}
	return ch
}

// Steer implements Backend.
func (b *ClientBackend) Steer(ctx context.Context, runID, text string) error {
	return b.C.Steer(ctx, runID, text)
}

// Cancel implements Backend.
func (b *ClientBackend) Cancel(ctx context.Context, runID string) error {
	return b.C.Cancel(ctx, runID)
}

// Resume implements Backend.
func (b *ClientBackend) Resume(ctx context.Context, r ResumeRequest) (string, error) {
	run, err := b.C.Resume(ctx, b.ThreadID, client.ResumeRequest{
		InterruptID: r.InterruptID, Reason: r.Reason, Decision: r.Decision, Always: r.Always, Text: r.Text,
		Model: b.ModelID, Interactive: b.Interactive, ReasoningEffort: b.ReasoningEffort,
	})
	if err != nil {
		return "", err
	}
	return run.ID, nil
}
