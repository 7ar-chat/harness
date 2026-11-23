// Package usageledger books what every run consumed: an end-of-run hook that prices the run's per-model usage and writes
// one ledger entry per model (core/usage), and the gate that refuses to start a run once a monthly spend cap is used up.
// Who the work is booked to, and which project and kind of work it belongs to, is the host's to say (Resolve).
package usageledger

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/modelinfo"
)

// Attribution says what a run's cost is booked to.
type Attribution struct {
	// ProjectID is empty for a personal conversation.
	ProjectID string
	// User is who pays: the person who started the conversation.
	User string
	// Source is a usage.Source constant ("": a session); SourceID the schedule or task.
	Source, SourceID string
}

// Options configures the hook.
type Options struct {
	Store usage.Store
	// Price returns the price of a model; nil, or a model it does not know: the entry is flagged unpriced.
	Price func(model string) (modelinfo.Info, bool)
	// Resolve attributes a run. Nil, or an empty User: the run's own user, as a session.
	Resolve func(ctx context.Context, run *store.Run) Attribution
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

const persistTimeout = 10 * time.Second

// subagentSuffix marks the usage of a sub-agent in a run's per-model usage (see engine/agent): the price is its model's.
const subagentSuffix = " (sub-agent)"

// Hook returns the end-of-run hook that writes the ledger. It must be the last hook of the harness (Options.Hooks), so
// the model calls of the title and goal hooks are in the figures it reads.
func Hook(o Options) runtime.Hook { return hook{o} }

type hook struct{ o Options }

// Finishing implements runtime.Hook.
func (h hook) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	if f == nil || f.Run == nil || h.o.Store == nil {
		return nil
	}
	// The run may have been cancelled (its ctx is done): the spend is booked all the same.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
	defer cancel()
	at := time.Now()
	if h.o.Now != nil {
		at = h.o.Now()
	}
	entries := Entries(f.Run, f.TotalUsage(), h.attribution(ctx, f.Run), h.o.Price, at)
	if len(entries) == 0 {
		return nil
	}
	if err := h.o.Store.Record(ctx, entries); err != nil {
		slog.Error("usage: booking a run failed", "run_id", f.Run.ID, "error", err)
	}
	return nil
}

func (h hook) attribution(ctx context.Context, run *store.Run) Attribution {
	var a Attribution
	if h.o.Resolve != nil {
		a = h.o.Resolve(ctx, run)
	}
	if a.User == "" {
		a.User = run.UserLogin
	}
	if a.Source == "" {
		a.Source = usage.SourceSession
	}
	return a
}

// Entries prices the usage of a run: one entry per model that consumed tokens.
func Entries(run *store.Run, u []agui.Usage, a Attribution, price func(string) (modelinfo.Info, bool), at time.Time) []usage.Entry {
	var out []usage.Entry
	for _, m := range u {
		if m.InputTokens == 0 && m.OutputTokens == 0 {
			continue
		}
		name := m.Model
		if name == "" {
			name = run.Model
		}
		e := usage.Entry{
			RunID: run.ID, ThreadID: run.ThreadID, ProjectID: a.ProjectID, User: a.User, Source: a.Source, SourceID: a.SourceID,
			Model: name, InputTokens: int64(m.InputTokens), OutputTokens: int64(m.OutputTokens),
			CacheReadTokens: int64(m.CachedInputTokens), CacheWriteTokens: int64(m.CacheWriteTokens), At: at.UTC(),
		}
		e.CostUSD, e.Unpriced = Cost(m, name, price)
		out = append(out, e)
	}
	return out
}

// Cost prices one model's usage; unpriced is true when the model has no price (the cost is then 0, not free).
func Cost(m agui.Usage, model string, price func(string) (modelinfo.Info, bool)) (usd float64, unpriced bool) {
	if price == nil {
		return 0, true
	}
	info, ok := price(strings.TrimSuffix(model, subagentSuffix))
	if !ok {
		return 0, true
	}
	usd, ok = modelinfo.Cost(info, modelinfo.Usage{
		In: m.InputTokens, Out: m.OutputTokens, Cached: m.CachedInputTokens, Written: m.CacheWriteTokens,
		// The usage is per model over the whole run, so the prompt size that picks the price tier is the best estimate.
		Prompt: m.InputTokens,
	})
	return usd, !ok
}

// Gate returns the check a harness runs before it starts a run: it refuses (with a *usage.CapError) when the
// project's or the user's monthly cap is used up. resolve names the project of the thread (empty for a personal one) and
// the person the work is booked to (empty: user), the same attribution the ledger uses.
// A store that cannot be read does not stop the run: a cap is a guard rail, not a reason to take the product down.
func Gate(st usage.Store, resolve func(ctx context.Context, user, threadID string) (projectID, booked string), now func() time.Time) func(ctx context.Context, user, threadID string) error {
	return func(ctx context.Context, user, threadID string) error {
		project, booked := "", user
		if resolve != nil {
			if p, b := resolve(ctx, user, threadID); b != "" || p != "" {
				project = p
				if b != "" {
					booked = b
				}
			}
		}
		at := time.Now()
		if now != nil {
			at = now()
		}
		err := usage.Check(ctx, st, project, booked, at)
		if err != nil && !errors.Is(err, usage.ErrCapReached) {
			slog.Warn("usage: reading the spend caps failed; the run starts", "error", err)
			return nil
		}
		return err
	}
}
