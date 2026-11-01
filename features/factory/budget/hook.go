package budget

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/modelinfo"
)

// Hook returns the end-of-run hook that meters the run and blocks the task
// when its budget, the day's cap or its failure pattern says to stop. Runs of
// threads that are not a task's (ordinary chat) are ignored.
func (g *Guard) Hook() runtime.Hook { return endHook{g} }

type endHook struct{ g *Guard }

// Finishing implements runtime.Hook.
func (h endHook) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if f == nil || f.Run == nil {
		return nil
	}
	h.g.finished(ctx, f)
	return nil
}

func (g *Guard) finished(ctx context.Context, f *runtime.Finish) {
	run := f.Run
	// The run may have been cancelled (its ctx is done): the spend must still be booked.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()

	taskID := g.taskOf(ctx, run)
	g.forget(run.ID)
	if taskID == "" || !g.firstTime(run.ID) {
		return
	}
	spend := g.spendOf(f.Result.Usage, run.Model)
	t, err := g.o.Store.AddSpend(ctx, taskID, spend)
	if err != nil {
		g.unsee(run.ID)
		if !errors.Is(err, factory.ErrNotFound) {
			slog.Error("budget: recording spend failed", "task_id", taskID, "run_id", run.ID, "error", err)
		}
		return
	}
	g.event(ctx, taskID, factory.TaskEvent{
		Kind: factory.EventBudget,
		Text: fmt.Sprintf("run spent $%.4f, %d tokens (task total $%.4f, %d tokens)", spend.USD, spend.Tokens, t.Spent.USD, t.Spent.Tokens),
		Data: map[string]any{
			"runId": run.ID, "usd": spend.USD, "tokens": spend.Tokens, "unpriced": spend.Unpriced,
			"totalUsd": t.Spent.USD, "totalTokens": t.Spent.Tokens,
		},
	})
	reason := g.stopReason(ctx, t, f)
	if reason == "" || t.State == factory.StateBlocked || t.State.Terminal() {
		return // nothing to stop, or already stopped
	}
	g.event(ctx, taskID, factory.TaskEvent{Kind: factory.EventBudget, Text: reason, Data: map[string]any{"reason": reason, "runId": run.ID}})
	g.block(ctx, *t, reason)
}

// stopReason is why the task must not continue after this run ("" to go on).
func (g *Guard) stopReason(ctx context.Context, t *factory.Task, f *runtime.Finish) string {
	if t.Budget.Over(t.Spent) {
		return budgetReason(t.Budget, t.Spent)
	}
	spent, err := g.dailySpent(ctx, t.ProjectID)
	if err != nil {
		slog.Error("budget: reading the daily spend failed", "task_id", t.ID, "error", err)
	} else if g.o.Daily.Over(spent) {
		return g.dailyReason(t.ProjectID, spent)
	}
	if f.Status != store.StatusFailed {
		return ""
	}
	// The failure is not in the task yet (the tasks service records it once the
	// run is over): judge the task's signatures plus this one, never store it.
	text := readable(f.Run.Error + " " + f.Run.ErrorCode)
	fps := append(slices.Clone(t.Fingerprints), factory.Fingerprint(text))
	if factory.Repeating(fps, g.o.RepeatLimit) {
		return repeatReason(g.o.RepeatLimit, text)
	}
	return ""
}

func (g *Guard) event(ctx context.Context, taskID string, e factory.TaskEvent) {
	e.TaskID = taskID
	e.At = g.o.Now()
	if err := g.o.Store.AppendEvent(ctx, e); err != nil {
		slog.Error("budget: appending event failed", "task_id", taskID, "error", err)
	}
}

// spendOf converts per-model usage to a spend of one run. A model without a
// price adds its tokens and counts as unpriced instead of free.
func (g *Guard) spendOf(usage []agui.Usage, fallbackModel string) factory.Spend {
	s := factory.Spend{Runs: 1}
	for _, u := range usage {
		if u.InputTokens == 0 && u.OutputTokens == 0 {
			continue
		}
		s.Tokens += int64(u.InputTokens) + int64(u.OutputTokens)
		model := u.Model
		if model == "" {
			model = fallbackModel
		}
		var info modelinfo.Info
		priced := false
		if g.o.Price != nil {
			info, priced = g.o.Price(model)
		}
		// The usage is per model over the whole run, so the prompt size that picks
		// the price tier is the best available estimate of the largest prompt.
		usd, ok := modelinfo.Cost(info, modelinfo.Usage{In: u.InputTokens, Out: u.OutputTokens, Cached: u.CachedInputTokens, Prompt: u.InputTokens})
		if priced && ok {
			s.USD += usd
		} else {
			s.Unpriced++
		}
	}
	return s
}

// firstTime marks the run as booked and reports false when it already was: a
// run is metered once however often its hook fires.
func (g *Guard) firstTime(runID string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, dup := g.seen[runID]; dup {
		return false
	}
	if len(g.ring) >= maxSeen {
		delete(g.seen, g.ring[0])
		g.ring = g.ring[1:]
	}
	g.seen[runID] = struct{}{}
	g.ring = append(g.ring, runID)
	return true
}

// unsee lets a run whose spend could not be stored be booked by a retry.
func (g *Guard) unsee(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, runID)
}
