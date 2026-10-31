package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/GhaziBenDahmane/harness/ai/llm"
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/agent"
	"github.com/GhaziBenDahmane/harness/engine/eventlog"
)

// Hook runs at the end of every run whose log this runtime still owns
// (harness v2: generated titles, goal evaluation).
type Hook interface {
	// Finishing runs after the run's work, before its final status is recorded;
	// it may emit events. The returned func (optional) runs once the status is
	// recorded, before the terminal event (e.g. to start the next run).
	Finishing(ctx context.Context, f *Finish) func(ctx context.Context)
}

// Finish describes a run that is ending.
type Finish struct {
	Run    *store.Run
	Status string
	Result agent.Result
	w      eventlog.Writer
	// extra is the usage of the hooks' own model calls (title, goal judge).
	extra []agui.Usage
}

// AddUsage counts a hook's model call in the run's RUN_FINISHED usage (A-25).
func (f *Finish) AddUsage(u agui.Usage) { f.extra = append(f.extra, u) }

// withUsage adds the hooks' usage to a terminal event's usage (per model).
func (f *Finish) withUsage(final agui.Event) agui.Event {
	usage, ok := final["usage"].([]agui.Usage)
	if !ok || len(f.extra) == 0 {
		return final
	}
	merged := append([]agui.Usage(nil), usage...)
	for _, u := range f.extra {
		found := false
		for i := range merged {
			if merged[i].Model == u.Model {
				merged[i].InputTokens += u.InputTokens
				merged[i].OutputTokens += u.OutputTokens
				merged[i].TotalTokens += u.TotalTokens
				merged[i].ReasoningTokens += u.ReasoningTokens
				merged[i].CachedInputTokens += u.CachedInputTokens
				found = true
				break
			}
		}
		if !found {
			merged = append(merged, u)
		}
	}
	final["usage"] = merged
	return final
}

// Emit appends an event to the run's log (before its terminal event).
func (f *Finish) Emit(ctx context.Context, ev agui.Event) error {
	_, err := f.w.Append(ctx, ev)
	return err
}

// successEvent is the terminal event of a completed run: an interrupt outcome
// when the agent waits for the user (ask_user).
func successEvent(run *store.Run, res agent.Result) agui.Event {
	intr := res.Interrupt
	if intr == nil {
		return agui.RunFinishedEvent(run.ThreadID, run.ID, "success", res.Usage)
	}
	if a := intr.Approval; a != nil {
		return agui.RunInterruptedEvent(run.ThreadID, run.ID, []agui.Interrupt{{
			ID: intr.ID, Reason: store.ReasonToolApproval, Message: a.ToolName, ToolCallID: intr.ToolCallID,
			ResponseSchema: map[string]any{"decisions": []string{store.DecisionApprove, store.DecisionDeny, store.DecisionRespond}, "allowFreeText": true},
			Approval:       a.Payload(),
		}}, res.Usage)
	}
	options := make([]map[string]any, 0, len(intr.Options))
	for _, o := range intr.Options {
		opt := map[string]any{"label": o.Label}
		if o.Description != "" {
			opt["description"] = o.Description
		}
		options = append(options, opt)
	}
	return agui.RunInterruptedEvent(run.ThreadID, run.ID, []agui.Interrupt{{
		ID: intr.ID, Reason: intr.Reason, Message: intr.Question, ToolCallID: intr.ToolCallID,
		ResponseSchema: map[string]any{"options": options, "allowFreeText": intr.AllowFreeText},
	}}, res.Usage)
}

// recordInterrupt opens the thread's pending question of a run that ended on
// ask_user (a pending approval is recorded by the agent when it pauses).
func (r *Runtime) recordInterrupt(ctx context.Context, run *store.Run, status string, res agent.Result) {
	intr := res.Interrupt
	if status != store.StatusCompleted {
		// A run that paused for an approval and then failed or was cancelled
		// does not wait for it any more.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := r.store.CancelRunApprovals(sctx, run.ID); err != nil {
			slog.Warn("cancelling the run's approvals failed", "run_id", run.ID, "error", err)
		}
		return
	}
	if intr == nil || intr.Approval != nil { // the approval was recorded when the run paused
		return
	}
	q := &store.PendingQuestion{InterruptID: intr.ID, RunID: run.ID, ToolCallID: intr.ToolCallID, Question: intr.Question,
		Options: make([]store.QuestionOption, 0, len(intr.Options)), AllowFreeText: intr.AllowFreeText}
	for _, o := range intr.Options {
		q.Options = append(q.Options, store.QuestionOption{Label: o.Label, Description: o.Description})
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := r.store.SetPendingQuestion(sctx, run.ThreadID, q); err != nil {
		slog.Error("recording the pending question failed", "run_id", run.ID, "error", err)
	}
}

// runHooks runs the hooks' Finishing step and returns their continuations.
func (r *Runtime) runHooks(ctx context.Context, f *Finish) []func(context.Context) {
	ctx = llm.WithSession(ctx, f.Run.ThreadID) // model calls carry the session header of providers that route by it
	var after []func(context.Context)
	for _, h := range r.opts.Hooks {
		if next := safeHook(f.Run.ID, func() func(context.Context) { return h.Finishing(ctx, f) }); next != nil {
			after = append(after, func(ctx context.Context) {
				next(llm.WithSession(ctx, f.Run.ThreadID))
			})
		}
	}
	return after
}

func safeHook[T any](runID string, fn func() T) (out T) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("run hook panic", "run_id", runID, "panic", p)
		}
	}()
	return fn()
}
