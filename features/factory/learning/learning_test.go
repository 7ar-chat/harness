package learning_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/features/factory/learning"
)

var ctx = context.Background()

func mustTask(t *testing.T, st factory.Store, tk factory.Task) *factory.Task {
	t.Helper()
	if tk.Owner == "" {
		tk.Owner = "alice"
	}
	if tk.Title == "" {
		tk.Title = "t"
	}
	out, _, err := st.CreateTask(ctx, tk)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func finish(t *testing.T, st factory.Store, id string, s factory.State, p factory.Patch) factory.Task {
	t.Helper()
	p.State = &s
	u, err := st.UpdateTask(ctx, id, p)
	if err != nil {
		t.Fatal(err)
	}
	return *u
}

func entry(t *testing.T, ms store.MemoryStore, key, id string, meta map[string]any) {
	t.Helper()
	m := map[string]any{"source": "factory-lessons", "auto": true}
	for k, v := range meta {
		m[k] = v
	}
	imp := 0.7
	_, err := ms.CreateMemoryEntry(ctx, &store.MemoryEntry{
		ID: id, UserLogin: key, Path: "memories/semantic/facts/" + id + ".md", Title: id, Content: "content " + id,
		Importance: &imp, Metadata: m, Source: store.MemorySourceProxy, Status: store.MemoryActive,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}, store.MemoryLimits{})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAttributionAndScoring(t *testing.T) {
	fs, ms := factorystore.NewMem(nil), memstore.New()
	entry(t, ms, "project:p", "m1", nil)
	entry(t, ms, "project:p", "m2", nil)

	l := learning.NewLearner(learning.Options{Store: fs, Memory: ms,
		Skills: func(context.Context, string) []string { return []string{"dokploy"} }})
	rec := &learning.Recorder{Store: fs, PromptVersion: func(factory.Task) string { return "v2" }}
	keys := map[string]string{"m1": "project:p", "m2": "project:p"}

	win := mustTask(t, fs, factory.Task{ProjectID: "p", ThreadID: "th-win", Model: "m"})
	// the same recall twice is recorded once; a thread that is no task's is ignored
	rec.Recall(ctx, "run1", "th-win", []string{"m1", "m2"}, keys)
	rec.Recall(ctx, "run1", "th-win", []string{"m2", "m1"}, keys)
	rec.Recall(ctx, "run9", "no-such-thread", []string{"m1"}, keys)
	ev, _ := fs.ListEvents(ctx, win.ID, 0)
	n := 0
	for _, e := range ev {
		if e.Kind == factory.EventContext {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("context events = %d, want 1", n)
	}

	done := finish(t, fs, win.ID, factory.StateDone, factory.Patch{})
	l.TaskFinished(ctx, done, nil)
	l.TaskFinished(ctx, done, nil) // idempotent

	lose := mustTask(t, fs, factory.Task{ProjectID: "p", ThreadID: "th-lose"})
	rec.Recall(ctx, "run2", "th-lose", []string{"m1"}, keys)
	failed := finish(t, fs, lose.ID, factory.StateFailed, factory.Patch{})
	l.TaskFinished(ctx, failed, nil)

	m1, _ := ms.GetMemoryEntry(ctx, "project:p", "m1")
	m2, _ := ms.GetMemoryEntry(ctx, "project:p", "m2")
	if u := learning.UtilityOf(m1); u != (learning.Utility{Uses: 2, Wins: 1, Losses: 1}) {
		t.Errorf("m1 utility = %+v", u)
	}
	if u := learning.UtilityOf(m2); u != (learning.Utility{Uses: 1, Wins: 1}) {
		t.Errorf("m2 utility = %+v", u)
	}

	rows, err := learning.Collect(ctx, fs, factory.Filter{ProjectID: "p"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %d, err %v", len(rows), err)
	}
	r := rows[0]
	if r.Result != learning.ResultSuccess || len(r.Memory) != 2 || r.PromptVersion != "v2" || len(r.Skills) != 1 || r.Key != "project:p" {
		t.Errorf("row = %+v", r)
	}
	var sb strings.Builder
	if err := learning.WriteJSONL(&sb, rows); err != nil || strings.Count(sb.String(), "\n") != 2 {
		t.Errorf("jsonl = %q, %v", sb.String(), err)
	}
}

func TestBlockedIsProvisional(t *testing.T) {
	fs, ms := factorystore.NewMem(nil), memstore.New()
	entry(t, ms, "alice", "m1", nil)
	l := learning.NewLearner(learning.Options{Store: fs, Memory: ms})
	rec := &learning.Recorder{Store: fs}
	tk := mustTask(t, fs, factory.Task{ThreadID: "th"})
	rec.Recall(ctx, "r", "th", []string{"m1"}, map[string]string{"m1": "alice"})
	l.TaskFinished(ctx, finish(t, fs, tk.ID, factory.StateBlocked, factory.Patch{}), nil)
	if e, _ := ms.GetMemoryEntry(ctx, "alice", "m1"); learning.UtilityOf(e).Uses != 0 {
		t.Fatal("a blocked task scored the memory")
	}
	// the human unblocks it and it succeeds: now it counts, once
	l.TaskFinished(ctx, finish(t, fs, tk.ID, factory.StateDone, factory.Patch{}), nil)
	if e, _ := ms.GetMemoryEntry(ctx, "alice", "m1"); learning.UtilityOf(e) != (learning.Utility{Uses: 1, Wins: 1}) {
		t.Fatalf("utility = %+v", learning.UtilityOf(e))
	}
	// cancelled tasks carry no outcome
	c := mustTask(t, fs, factory.Task{})
	l.TaskFinished(ctx, finish(t, fs, c.ID, factory.StateCancelled, factory.Patch{}), nil)
	ev, _ := fs.ListEvents(ctx, c.ID, 0)
	for _, e := range ev {
		if e.Kind == factory.EventOutcome {
			t.Fatal("outcome recorded for a cancelled task")
		}
	}
}

func TestCurator(t *testing.T) {
	ms := memstore.New()
	entry(t, ms, "k", "bad", map[string]any{"uses": 10, "wins": 0, "losses": 10})
	entry(t, ms, "k", "meh", map[string]any{"uses": 10, "wins": 4, "losses": 6})
	entry(t, ms, "k", "good", map[string]any{"uses": 12, "wins": 12, "losses": 0})
	entry(t, ms, "k", "few", map[string]any{"uses": 3, "wins": 0, "losses": 3})
	entry(t, ms, "k", "pinned", map[string]any{"uses": 10, "wins": 0, "losses": 10, "pinned": true})
	entry(t, ms, "k", "user", map[string]any{"uses": 10, "wins": 0, "losses": 10, "source": "user", "auto": false})
	// "user" is written by a person: the entry's Source is the guard
	u, _ := ms.GetMemoryEntry(ctx, "k", "user")
	u.Source = store.MemorySourceUser
	_, _ = ms.UpdateMemoryEntry(ctx, u, store.MemoryLimits{})

	c := learning.NewCurator(learning.CuratorOptions{Memory: ms,
		Baseline: func(context.Context, string) (float64, int, error) { return 0.6, 40, nil }})
	rep, err := c.Curate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status := func(id string) string { e, _ := ms.GetMemoryEntry(ctx, "k", id); return e.Status }
	if status("bad") != store.MemoryDeleted {
		t.Errorf("bad entry not retired: %+v", rep)
	}
	if e, _ := ms.GetMemoryEntry(ctx, "k", "good"); e.Metadata["proven"] != true || *e.Importance <= 0.7 {
		t.Errorf("good entry not promoted: %+v", e.Metadata)
	}
	for _, id := range []string{"meh", "few", "pinned", "user"} {
		if status(id) != store.MemoryActive {
			t.Errorf("%s was retired", id)
		}
		if e, _ := ms.GetMemoryEntry(ctx, "k", id); *e.Importance != 0.7 {
			t.Errorf("%s importance changed to %v", id, *e.Importance)
		}
	}
	// nothing new since: acted on only once (an entry inside the band is looked at again, but never written)
	if rep2, _ := c.Curate(ctx); len(rep2.Promoted)+len(rep2.Demoted)+len(rep2.Retired) != 0 {
		t.Errorf("acted twice on the same evidence: %+v", rep2)
	}
	// no baseline yet: no judgement
	ms2 := memstore.New()
	entry(t, ms2, "k", "bad", map[string]any{"uses": 10, "wins": 0, "losses": 10})
	c2 := learning.NewCurator(learning.CuratorOptions{Memory: ms2,
		Baseline: func(context.Context, string) (float64, int, error) { return 0.6, 2, nil }})
	if rep, _ := c2.Curate(ctx); len(rep.Retired) != 0 {
		t.Errorf("retired with no baseline: %+v", rep)
	}
}

func TestMine(t *testing.T) {
	rows := []learning.Row{
		{TaskID: "1", Result: learning.ResultFailure, Error: "build failed at line 12", USD: 1, CreatedAt: time.Unix(1, 0)},
		{TaskID: "2", Result: learning.ResultFailure, Error: "build failed at line 99", USD: 2, CreatedAt: time.Unix(2, 0)},
		{TaskID: "3", Result: learning.ResultBlocked, Fingerprints: []string{factory.Fingerprint("build failed at line 1")}, BlockedReason: "failed the same way", CreatedAt: time.Unix(3, 0)},
		{TaskID: "4", Result: learning.ResultBlocked, BlockedReason: "ready to merge: awaiting a human"}, // not a failure
		{TaskID: "5", Result: learning.ResultSuccess},
		{TaskID: "6", Result: learning.ResultFailure, Error: "something else"},
		{TaskID: "7", Result: learning.ResultFailure, Error: "build failed at line 5", Source: factory.SourceDelegate},
	}
	cs := learning.Mine(rows, 2)
	if len(cs) != 1 || cs[0].Count != 3 || cs[0].USD != 3 {
		t.Fatalf("clusters = %+v", cs)
	}
	dir := t.TempDir()
	cands := learning.CandidatesOf(cs)
	if w, err := learning.WriteCandidates(dir, cands); err != nil || len(w) != 1 {
		t.Fatalf("written %v, %v", w, err)
	}
	if w, _ := learning.WriteCandidates(dir, cands); len(w) != 0 {
		t.Fatal("an existing candidate was overwritten")
	}
}

func TestExperiment(t *testing.T) {
	e := learning.Experiment{Variants: map[string]string{"terse": "be brief", "plan": "plan first"}}
	seen := map[string]int{}
	for i := range 300 {
		v := e.Pick(fmt.Sprint("task-", i))
		seen[v]++
		if v != e.Pick(fmt.Sprint("task-", i)) {
			t.Fatal("pick is not stable")
		}
	}
	if len(seen) != 3 {
		t.Fatalf("variants used = %v", seen)
	}
	if e.Instructions(learning.Control) != "" || e.Instructions("plan") != "plan first" {
		t.Fatal("instructions")
	}

	mk := func(variant string, wins, n int) (rows []learning.Row) {
		for i := range n {
			r := learning.ResultFailure
			if i < wins {
				r = learning.ResultSuccess
			}
			rows = append(rows, learning.Row{PromptVersion: variant, Result: r, USD: 1})
		}
		return
	}
	rows := append(mk("control", 12, 40), mk("plan", 30, 40)...)
	rows = append(rows, mk("terse", 13, 40)...)
	if d := learning.Decide(rows, 20, 0.05); d.Winner != "plan" {
		t.Errorf("winner = %q (%s)", d.Winner, d.Why)
	}
	if d := learning.Decide(append(mk("control", 12, 40), mk("terse", 14, 40)...), 20, 0.05); d.Winner != "" {
		t.Errorf("an insignificant gain won: %+v", d)
	}
	if d := learning.Decide(append(mk("control", 12, 5), mk("plan", 5, 5)...), 20, 0.05); d.Winner != "" {
		t.Errorf("won with too few tasks: %+v", d)
	}
}

type fakeController struct {
	factory.Controller
	st factory.Store
}

func (f fakeController) Create(ctx context.Context, t factory.Task) (*factory.Task, bool, error) {
	return f.st.CreateTask(ctx, t)
}

func TestImproverGuardrails(t *testing.T) {
	fs := factorystore.NewMem(nil)
	for i := range 9 { // three failures each of three kinds
		mustTask(t, fs, factory.Task{Title: "x", Owner: "o"})
		tk := mustTask(t, fs, factory.Task{Title: "w", Owner: "o"})
		finish(t, fs, tk.ID, factory.StateFailed, factory.Patch{Error: ptr(fmt.Sprintf("kind %c broke", 'a'+i%3))})
	}
	if _, err := learning.NewImprover(learning.ImproverOptions{Store: fs, Controller: fakeController{st: fs}}); err == nil {
		t.Fatal("an improver without a repository was accepted")
	}
	imp, err := learning.NewImprover(learning.ImproverOptions{Store: fs, Controller: fakeController{st: fs}, Owner: "o", Repo: "acme/app", MaxOpen: 2, MinCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	made, err := imp.Propose(ctx)
	if err != nil || len(made) != 2 {
		t.Fatalf("made %d tasks, err %v (want the 2 allowed open)", len(made), err)
	}
	for _, m := range made {
		if m.AutoMerge || m.Repo != "acme/app" || m.Policy != factory.PolicySandboxed || m.Budget.MaxUSD == 0 || m.Labels[0] != learning.Label {
			t.Errorf("unsafe improvement task: %+v", m)
		}
		for _, f := range learning.DefaultFrozen {
			if !strings.Contains(m.Spec, f) {
				t.Errorf("spec does not name the frozen path %s", f)
			}
		}
		if !strings.Contains(m.Spec, "bench -gate") {
			t.Error("spec does not require the eval gate")
		}
	}
	// still two open: nothing more, and the same failure is never proposed twice
	if again, _ := imp.Propose(ctx); len(again) != 0 {
		t.Fatalf("exceeded MaxOpen: %d", len(again))
	}
	for _, m := range made {
		finish(t, fs, m.ID, factory.StateCancelled, factory.Patch{})
	}
	again, _ := imp.Propose(ctx)
	if len(again) != 1 { // MaxPerDay (3) minus the 2 already created today
		t.Fatalf("after closing: %d new tasks, want 1 (daily cap)", len(again))
	}
	for _, a := range again {
		for _, m := range made {
			if a.Source.Key == m.Source.Key {
				t.Fatal("the same failure was proposed twice")
			}
		}
	}
}

func ptr[T any](v T) *T { return &v }
