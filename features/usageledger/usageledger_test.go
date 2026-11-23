package usageledger_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/usagestore"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/usageledger"
	"github.com/7ar-chat/harness/modelinfo"
)

var ctx = context.Background()

func price(model string) (modelinfo.Info, bool) {
	if model != "m" {
		return modelinfo.Info{}, false
	}
	return modelinfo.Info{Priced: true, InputPerToken: 1e-5, OutputPerToken: 5e-5, CacheReadPerToken: 1e-6, CacheWritePerToken: 1.25e-5}, true
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestEntriesPriceEachModelWithTheCacheSplit(t *testing.T) {
	run := &store.Run{ID: "r1", ThreadID: "t1", Model: "m"}
	at := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

	got := usageledger.Entries(run, []agui.Usage{
		{Model: "m", InputTokens: 1000, OutputTokens: 200, CachedInputTokens: 600, CacheWriteTokens: 100},
		{Model: "m (sub-agent)", InputTokens: 100, OutputTokens: 10},
		{InputTokens: 10, OutputTokens: 1}, // no model name: the run's
		{Model: "mystery", InputTokens: 50, OutputTokens: 5},
		{Model: "idle"}, // nothing consumed: no entry
	}, usageledger.Attribution{ProjectID: "p", User: "ann", Source: usage.SourceSchedule, SourceID: "s1"}, price, at)

	if len(got) != 4 {
		t.Fatalf("entries: %+v", got)
	}
	main := got[0]
	want := 300*1e-5 + 600*1e-6 + 100*1.25e-5 + 200*5e-5
	if !near(main.CostUSD, want) || main.Unpriced || main.CacheReadTokens != 600 || main.CacheWriteTokens != 100 ||
		main.ProjectID != "p" || main.User != "ann" || main.Source != usage.SourceSchedule || main.SourceID != "s1" || main.ThreadID != "t1" || !main.At.Equal(at) {
		t.Fatalf("main: %+v (want cost %v)", main, want)
	}
	if sub := got[1]; sub.Model != "m (sub-agent)" || sub.Unpriced || sub.CostUSD <= 0 {
		t.Fatalf("sub-agent is priced as its model: %+v", sub)
	}
	if got[2].Model != "m" {
		t.Fatalf("fallback model: %+v", got[2])
	}
	if mystery := got[3]; !mystery.Unpriced || mystery.CostUSD != 0 || mystery.InputTokens != 50 {
		t.Fatalf("unpriced: %+v", mystery)
	}
	if none := usageledger.Entries(run, []agui.Usage{{InputTokens: 5}}, usageledger.Attribution{}, nil, at); len(none) != 1 || !none[0].Unpriced {
		t.Fatalf("without a price table: %+v", none)
	}
}

func TestHookBooksTheRunOnceUnderItsAttribution(t *testing.T) {
	st := usagestore.NewMem()
	h := usageledger.Hook(usageledger.Options{
		Store: st, Price: price,
		Resolve: func(_ context.Context, run *store.Run) usageledger.Attribution {
			return usageledger.Attribution{ProjectID: "p1", User: "ann"}
		},
		Now: func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) },
	})
	f := &runtime.Finish{Run: &store.Run{ID: "r1", ThreadID: "t1", Model: "m", UserLogin: "owner"}, Status: store.StatusCompleted,
		Result: agent.Result{Usage: []agui.Usage{{Model: "m", InputTokens: 100, OutputTokens: 10}}}}

	h.Finishing(ctx, f)
	h.Finishing(ctx, f) // a hook that fires twice must not book twice
	h.Finishing(ctx, nil)
	h.Finishing(ctx, &runtime.Finish{})

	rep, _ := st.Report(ctx, usage.Query{ProjectID: "p1", User: "ann"})
	if rep.Total.Runs != 1 || rep.Total.InputTokens != 100 || rep.Total.CostUSD <= 0 || rep.BySource[usage.SourceSession].Runs != 1 {
		t.Fatalf("report: %+v", rep)
	}
}

type failingStore struct{ usage.Store }

func (failingStore) Cap(context.Context, string, string) (float64, error) {
	return 0, errors.New("database down")
}

func TestGateRefusesOverTheCapButNeverBecauseTheStoreIsDown(t *testing.T) {
	st := usagestore.NewMem()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	must(t, st.Record(ctx, []usage.Entry{{RunID: "a", User: "ann", ProjectID: "p1", Model: "m", CostUSD: 10, At: now.Add(-time.Hour)}}))
	must(t, st.SetCap(ctx, usage.ScopeProject, "p1", 10))
	resolve := func(_ context.Context, _, thread string) (string, string) {
		if thread == "t-project" {
			return "p1", "ann"
		}
		return "", ""
	}
	gate := usageledger.Gate(st, resolve, func() time.Time { return now })

	err := gate(ctx, "ann", "t-project")
	var capErr *usage.CapError
	if !errors.As(err, &capErr) || capErr.Scope != usage.ScopeProject || capErr.CapUSD != 10 || capErr.SpentUSD != 10 {
		t.Fatalf("over the cap: %v", err)
	}
	if err := gate(ctx, "ann", "t-personal"); err != nil {
		t.Fatalf("a personal thread is not under the project's cap: %v", err)
	}
	down := usageledger.Gate(failingStore{st}, resolve, func() time.Time { return now })
	if err := down(ctx, "ann", "t-project"); err != nil {
		t.Fatalf("a store that is down stopped a run: %v", err)
	}
	if msg := (&usage.CapError{Scope: usage.ScopeUser, CapUSD: 5, SpentUSD: 5}).Error(); msg == "" {
		t.Fatal("empty message")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
