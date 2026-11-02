package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
)

// Refusal is the result of a tool call refused because the task has no budget
// left. The model gets one chance to answer with a summary.
const Refusal = "Error: task budget exhausted; finish with a summary of what is done and what remains. Tools that change anything are refused."

// ToolLimitRefusal is the result of a call beyond MaxToolCallsPerRun (%d: the cap).
const ToolLimitRefusal = "Error: this run used its %d tool calls; finish with a summary of what is done and what remains, the next attempt continues from there."

// BeforeTool returns the hook that refuses tools that change something once
// the task's budget is spent or the run used its tool calls. Read-only tools
// stay available so the model can still look things up for its summary.
func (g *Guard) BeforeTool() agent.BeforeToolHook { return denier{g} }

// ToolHook returns the observer that counts the tool calls of a run.
func (g *Guard) ToolHook() agent.ToolHook { return counter{g} }

// StepNote returns the hook that tells the model when 80% of the task's
// budget is used (once per run).
func (g *Guard) StepNote() agent.StepNote { return g.stepNote }

type denier struct{ g *Guard }

// NeedsApproval implements agent.BeforeToolHook: the guard refuses, it never asks.
func (denier) NeedsApproval(context.Context, *store.Run, string, string) bool { return false }

// Pause implements agent.BeforeToolHook; it is never reached.
func (denier) Pause(context.Context, *store.Run, ai.ToolCall) (*tools.Interrupt, error) {
	return nil, errors.New("budget: the guard never pauses a run")
}

// Denial implements agent.ToolDenier.
func (d denier) Denial(ctx context.Context, run *store.Run, toolName, _ string) string {
	return d.g.denial(ctx, run, toolName)
}

type counter struct{ g *Guard }

// AfterTool implements agent.ToolHook.
func (c counter) AfterTool(ctx context.Context, run *store.Run, _, _ string) map[string]any {
	if run == nil {
		return nil
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	if e := c.g.entry(ctx, run); e != nil && e.taskID != "" {
		e.calls++
	}
	return nil
}

func (g *Guard) denial(ctx context.Context, run *store.Run, tool string) string {
	if run == nil {
		return ""
	}
	g.mu.Lock()
	e := g.entry(ctx, run)
	if e == nil || e.taskID == "" {
		g.mu.Unlock()
		return ""
	}
	taskID, calls := e.taskID, e.calls
	g.mu.Unlock()

	refusal := ""
	switch {
	// The call cap holds for read-only tools too: a run looping on reads spends as well.
	case g.o.MaxToolCallsPerRun > 0 && calls >= g.o.MaxToolCallsPerRun:
		refusal = fmt.Sprintf(ToolLimitRefusal, g.o.MaxToolCallsPerRun)
	case g.allow[tool]:
		return ""
	default:
		if t, err := g.o.Store.GetTask(ctx, taskID); err == nil && t.Budget.Over(t.Spent) {
			refusal = Refusal
		}
	}
	if refusal == "" {
		return ""
	}
	g.refused(run.ID)
	return refusal
}

// refused counts a refusal and cancels the run when the model ignores them.
func (g *Guard) refused(runID string) {
	g.mu.Lock()
	e := g.runs[runID]
	if e == nil {
		g.mu.Unlock()
		return
	}
	e.refusals++
	cancel := e.refusals >= refusalsBeforeCancel && !e.cancelled && g.o.Cancel != nil
	if cancel {
		e.cancelled = true
	}
	g.mu.Unlock()
	if !cancel {
		return
	}
	go func() {
		ctx, done := context.WithTimeout(context.Background(), persistTimeout)
		defer done()
		if err := g.o.Cancel(ctx, runID); err != nil {
			slog.Error("budget: cancelling the run failed", "run_id", runID, "error", err)
		}
	}()
}

func (g *Guard) stepNote(ctx context.Context, in agent.Input) string {
	if in.Run == nil {
		return ""
	}
	g.mu.Lock()
	e := g.entry(ctx, in.Run)
	if e == nil || e.taskID == "" || e.noted {
		g.mu.Unlock()
		return ""
	}
	taskID := e.taskID
	g.mu.Unlock()

	t, err := g.o.Store.GetTask(ctx, taskID)
	if err != nil || (t.Budget == factory.Budget{}) {
		return ""
	}
	f := fraction(t.Budget, t.Spent)
	if f < warnAt {
		return ""
	}
	g.mu.Lock()
	if e = g.runs[in.Run.ID]; e == nil || e.noted {
		g.mu.Unlock()
		return ""
	}
	e.noted = true
	g.mu.Unlock()
	if f >= 1 {
		return "The task budget is exhausted (" + spendOf(t.Budget, t.Spent) + "). Tools that change anything are refused: finish with a summary of what is done and what remains."
	}
	return fmt.Sprintf("%.0f%% of the task budget is used (%s). Prioritise what is essential, and leave a clear summary of what is done and what remains.", f*100, spendOf(t.Budget, t.Spent))
}

// taskOf is the task a run works on ("" for an ordinary thread).
func (g *Guard) taskOf(ctx context.Context, run *store.Run) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if e := g.entry(ctx, run); e != nil {
		return e.taskID
	}
	return ""
}

// entry returns the state of the run, resolving its thread on first sight.
// g.mu must be held; the store call under it is a point lookup, and holding
// the lock makes concurrent first calls of a run resolve it once.
func (g *Guard) entry(ctx context.Context, run *store.Run) *runEntry {
	if e := g.runs[run.ID]; e != nil {
		return e
	}
	e := &runEntry{}
	if ti, err := g.o.Store.GetThreadInfo(ctx, run.ThreadID); err == nil && ti != nil {
		e.taskID = ti.TaskID
	} else if err != nil && !errors.Is(err, factory.ErrNotFound) {
		slog.Warn("budget: reading the thread info failed", "thread_id", run.ThreadID, "error", err)
		return &runEntry{} // not remembered: the next call asks again
	}
	if len(g.runs) >= maxRuns {
		g.evictOldest()
	}
	g.seq++
	e.seq = g.seq
	g.runs[run.ID] = e
	return e
}

func (g *Guard) evictOldest() {
	oldest, id := ^uint64(0), ""
	for k, e := range g.runs {
		if e.seq < oldest {
			oldest, id = e.seq, k
		}
	}
	delete(g.runs, id)
}

// forget drops the state of a finished run.
func (g *Guard) forget(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.runs, runID)
}
