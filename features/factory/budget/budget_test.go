package budget_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/features/factory/budget"
	"github.com/7ar-chat/harness/modelinfo"
)

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// prices: "m" is cheap and flat, "tiered" doubles above 1000 prompt tokens, anything else is unknown.
var prices = map[string]modelinfo.Info{
	"m":      {ID: "m", Priced: true, InputPerToken: 0.001, OutputPerToken: 0.002, CacheReadPerToken: 0.0001},
	"tiered": {ID: "tiered", Priced: true, InputPerToken: 0.001, OutputPerToken: 0.002, Tier: &modelinfo.Tier{Above: 1000, Input: 0.002, Output: 0.004}},
}

func price(m string) (modelinfo.Info, bool) { i, ok := prices[m]; return i, ok }

// blocker is the Controller: it records the Block calls and moves the task like the tasks service does.
type blocker struct {
	factory.Controller
	st *factorystore.Mem

	mu      sync.Mutex
	reasons map[string][]string
}

func (b *blocker) Block(ctx context.Context, id, reason string) error {
	b.mu.Lock()
	b.reasons[id] = append(b.reasons[id], reason)
	b.mu.Unlock()
	blocked := factory.StateBlocked
	_, err := b.st.UpdateTask(ctx, id, factory.Patch{State: &blocked, BlockedReason: &reason})
	return err
}

func (b *blocker) blocks(id string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.reasons[id]...)
}

type rig struct {
	t    *testing.T
	st   *factorystore.Mem
	ctl  *blocker
	opts budget.Options
	now  time.Time
}

func newRig(t *testing.T, mod func(*budget.Options)) *rig {
	r := &rig{t: t, now: t0}
	r.st = factorystore.NewMem(func() time.Time { return r.now })
	r.ctl = &blocker{st: r.st, reasons: map[string][]string{}}
	r.opts = budget.Options{Store: r.st, Controller: r.ctl, Price: price, Now: func() time.Time { return r.now }}
	if mod != nil {
		mod(&r.opts)
	}
	return r
}

func (r *rig) guard() *budget.Guard { return budget.New(r.opts) }

// task creates a running task with a thread "th-<id>" for it.
func (r *rig) task(project string, b factory.Budget) *factory.Task {
	r.t.Helper()
	tk, _, err := r.st.CreateTask(context.Background(), factory.Task{ProjectID: project, Title: "t", Budget: b, State: factory.StateRunning})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.st.SetThreadInfo(context.Background(), factory.ThreadInfo{ThreadID: "th-" + tk.ID, TaskID: tk.ID}); err != nil {
		r.t.Fatal(err)
	}
	return tk
}

func (r *rig) get(id string) *factory.Task {
	r.t.Helper()
	tk, err := r.st.GetTask(context.Background(), id)
	if err != nil {
		r.t.Fatal(err)
	}
	return tk
}

func finish(task *factory.Task, runID string, usage ...agui.Usage) *runtime.Finish {
	return &runtime.Finish{
		Run:    &store.Run{ID: runID, ThreadID: "th-" + task.ID, Model: "m"},
		Status: store.StatusCompleted,
		Result: agent.Result{Usage: usage},
	}
}

func failed(task *factory.Task, runID, msg string) *runtime.Finish {
	f := finish(task, runID)
	f.Status = store.StatusFailed
	f.Run.Error, f.Run.ErrorCode = msg, "internal"
	return f
}

func use(model string, in, out, cached int) agui.Usage {
	return agui.Usage{Model: model, InputTokens: in, OutputTokens: out, CachedInputTokens: cached}
}

func (r *rig) run(g *budget.Guard, f *runtime.Finish) {
	r.t.Helper()
	g.Hook().Finishing(context.Background(), f)
}

func near(a, b float64) bool { d := a - b; return d < 1e-9 && d > -1e-9 }

func TestHookConvertsUsageToSpend(t *testing.T) {
	tests := []struct {
		name     string
		usage    []agui.Usage
		usd      float64
		tokens   int64
		unpriced int
	}{
		{"plain", []agui.Usage{use("m", 1000, 500, 0)}, 1000*0.001 + 500*0.002, 1500, 0},
		{"cached input is cheaper", []agui.Usage{use("m", 1000, 0, 800)}, 200*0.001 + 800*0.0001, 1000, 0},
		{"cache without read price bills as input", []agui.Usage{use("tiered", 100, 0, 50)}, 100 * 0.001, 100, 0},
		{"tier above its prompt size", []agui.Usage{use("tiered", 2000, 100, 0)}, 2000*0.002 + 100*0.004, 2100, 0},
		{"models add up", []agui.Usage{use("m", 100, 0, 0), use("tiered", 100, 0, 0)}, 0.2, 200, 0},
		{"unpriced model counts tokens, not dollars", []agui.Usage{use("ghost", 400, 100, 0), use("m", 100, 0, 0)}, 0.1, 600, 1},
		{"no usage", nil, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			r := newRig(t, nil)
			tk := r.task("p", factory.Budget{})

			// Act
			r.run(r.guard(), finish(tk, "r1", tc.usage...))

			// Assert
			got := r.get(tk.ID).Spent
			if !near(got.USD, tc.usd) || got.Tokens != tc.tokens || got.Unpriced != tc.unpriced || got.Runs != 1 {
				t.Fatalf("spent = %+v, want usd %v tokens %d unpriced %d runs 1", got, tc.usd, tc.tokens, tc.unpriced)
			}
		})
	}
}

func TestHookWithoutPriceFuncCountsEverythingUnpriced(t *testing.T) {
	// Arrange
	r := newRig(t, func(o *budget.Options) { o.Price = nil })
	tk := r.task("p", factory.Budget{})

	// Act
	r.run(r.guard(), finish(tk, "r1", use("m", 10, 10, 0)))

	// Assert
	if got := r.get(tk.ID).Spent; got.USD != 0 || got.Tokens != 20 || got.Unpriced != 1 {
		t.Fatalf("spent = %+v", got)
	}
}

func TestHookRecordsABudgetEventWithTheAmounts(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{})

	// Act
	r.run(r.guard(), finish(tk, "r1", use("m", 1000, 0, 0)))

	// Assert
	evs, _ := r.st.ListEvents(context.Background(), tk.ID, 10)
	if len(evs) != 1 || evs[0].Kind != factory.EventBudget || evs[0].Data["runId"] != "r1" || !near(evs[0].Data["usd"].(float64), 1) || evs[0].Data["tokens"] != int64(1000) {
		t.Fatalf("events = %+v", evs)
	}
}

func TestHookIgnoresOrdinaryThreadsAndStaleInfo(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{})
	g := r.guard()
	_ = r.st.SetThreadInfo(context.Background(), factory.ThreadInfo{ThreadID: "stale", TaskID: "gone"})
	_ = r.st.SetThreadInfo(context.Background(), factory.ThreadInfo{ThreadID: "no-task"})

	// Act
	for _, th := range []string{"chat", "stale", "no-task"} {
		g.Hook().Finishing(context.Background(), &runtime.Finish{
			Run: &store.Run{ID: "run-" + th, ThreadID: th}, Status: store.StatusCompleted,
			Result: agent.Result{Usage: []agui.Usage{use("m", 100, 100, 0)}},
		})
	}

	// Assert
	if got := r.get(tk.ID).Spent; got != (factory.Spend{}) {
		t.Fatalf("an unrelated task was charged: %+v", got)
	}
	if evs, _ := r.st.ListEvents(context.Background(), tk.ID, 10); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
}

func TestHookAccumulatesAndBlocksExactlyOnceAtTheCap(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{MaxUSD: 2.5})
	g := r.guard()

	// Act / Assert: $1 per run
	r.run(g, finish(tk, "r1", use("m", 1000, 0, 0)))
	r.run(g, finish(tk, "r2", use("m", 1000, 0, 0)))
	if got := r.get(tk.ID).Spent; !near(got.USD, 2) || got.Runs != 2 || len(r.ctl.blocks(tk.ID)) != 0 {
		t.Fatalf("under the cap: spent %+v, blocks %v", got, r.ctl.blocks(tk.ID))
	}
	r.run(g, finish(tk, "r3", use("m", 1000, 0, 0)))
	blocks := r.ctl.blocks(tk.ID)
	if len(blocks) != 1 || !strings.HasPrefix(blocks[0], "budget exhausted: ") || !strings.Contains(blocks[0], "$3.00 spent of $2.50") {
		t.Fatalf("blocks = %v", blocks)
	}

	// The same run reported again, and a run of an already blocked task, do not block again.
	r.run(g, finish(tk, "r3", use("m", 1000, 0, 0)))
	r.run(g, finish(tk, "r4", use("m", 1000, 0, 0)))
	if got := r.ctl.blocks(tk.ID); len(got) != 1 {
		t.Fatalf("blocks = %v", got)
	}
	if got := r.get(tk.ID).Spent; !near(got.USD, 4) || got.Runs != 4 {
		t.Fatalf("a repeated run was booked twice or a real one lost: %+v", got)
	}
}

func TestHookBlocksOnTokenCap(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{MaxTokens: 1000})

	// Act
	r.run(r.guard(), finish(tk, "r1", use("ghost", 600, 400, 0)))

	// Assert
	if b := r.ctl.blocks(tk.ID); len(b) != 1 || !strings.Contains(b[0], "1000 tokens used of 1000") {
		t.Fatalf("blocks = %v", b)
	}
}

func TestDailyCapCountsTheProjectNotTheOthers(t *testing.T) {
	// Arrange
	r := newRig(t, func(o *budget.Options) { o.Daily = factory.Budget{MaxUSD: 1.5}; o.PerProject = true })
	a1, a2, b1 := r.task("a", factory.Budget{}), r.task("a", factory.Budget{}), r.task("b", factory.Budget{})
	g := r.guard()

	// Act
	r.run(g, finish(a1, "r1", use("m", 1000, 0, 0)))
	okBefore, _ := g.Check(context.Background(), a2.ID)
	r.run(g, finish(a2, "r2", use("m", 1000, 0, 0)))
	okA, reasonA := g.Check(context.Background(), a2.ID)
	okB, _ := g.Check(context.Background(), b1.ID)

	// Assert
	if !okBefore || okA || !strings.Contains(reasonA, "daily budget exhausted") || !strings.Contains(reasonA, "project a") || !okB {
		t.Fatalf("before %v; project a: %v %q; project b: %v", okBefore, okA, reasonA, okB)
	}
	if len(r.ctl.blocks(a2.ID)) != 1 || len(r.ctl.blocks(b1.ID)) != 0 {
		t.Fatalf("blocks: a2 %v, b1 %v", r.ctl.blocks(a2.ID), r.ctl.blocks(b1.ID))
	}
}

func TestDailyCapIsGlobalWithoutPerProjectAndRollsOffWithTheWindow(t *testing.T) {
	// Arrange
	r := newRig(t, func(o *budget.Options) { o.Daily = factory.Budget{MaxUSD: 1.5}; o.Window = time.Hour })
	a, b := r.task("a", factory.Budget{}), r.task("b", factory.Budget{})
	g := r.guard()
	r.run(g, finish(a, "r1", use("m", 2000, 0, 0)))

	// Act
	okSame, _ := g.Check(context.Background(), b.ID)
	r.now = r.now.Add(2 * time.Hour)
	okLater, _ := g.Check(context.Background(), b.ID)

	// Assert
	if okSame || !okLater {
		t.Fatalf("other project in window %v, after the window %v", okSame, okLater)
	}
}

func TestFingerprintRepetitionBlocksAtTheThirdIdenticalFailure(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{})
	g := r.guard()
	fail := func(run, msg string) {
		r.run(g, failed(tk, run, msg))
		// The tasks service records the fingerprint after the hooks ran.
		_, _ = r.st.UpdateTask(context.Background(), tk.ID, factory.Patch{AddFingerprint: factory.Fingerprint(msg + " internal")})
	}

	// Act / Assert
	fail("r1", "model call failed at line 12")
	fail("r2", "model call failed at line 99") // same failure, other numbers
	if b := r.ctl.blocks(tk.ID); len(b) != 0 {
		t.Fatalf("blocked after 2: %v", b)
	}
	fail("r3", "model call failed at line 7")
	b := r.ctl.blocks(tk.ID)
	if len(b) != 1 || !strings.HasPrefix(b[0], "failing the same way 3 times: ") || !strings.Contains(b[0], "model call failed") {
		t.Fatalf("blocks = %v", b)
	}
	if got := r.get(tk.ID).Fingerprints; len(got) != 3 {
		t.Fatalf("the guard must not record fingerprints itself: %v", got)
	}
}

func TestAlternatingFailuresDoNotBlock(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{})
	g := r.guard()

	// Act
	for i, msg := range []string{"tests fail", "build broke", "tests fail", "build broke", "tests fail"} {
		r.run(g, failed(tk, fmt.Sprintf("r%d", i), msg))
		_, _ = r.st.UpdateTask(context.Background(), tk.ID, factory.Patch{AddFingerprint: factory.Fingerprint(msg + " internal")})
	}

	// Assert
	if b := r.ctl.blocks(tk.ID); len(b) != 0 {
		t.Fatalf("blocks = %v", b)
	}
	if ok, reason := g.Check(context.Background(), tk.ID); !ok {
		t.Fatalf("check: %s", reason)
	}
}

func TestRepeatLimitIsConfigurable(t *testing.T) {
	// Arrange
	r := newRig(t, func(o *budget.Options) { o.RepeatLimit = 2 })
	tk := r.task("p", factory.Budget{})
	g := r.guard()
	_, _ = r.st.UpdateTask(context.Background(), tk.ID, factory.Patch{AddFingerprint: factory.Fingerprint("boom internal")})

	// Act
	r.run(g, failed(tk, "r1", "boom"))

	// Assert
	if b := r.ctl.blocks(tk.ID); len(b) != 1 {
		t.Fatalf("blocks = %v", b)
	}
}

func TestCheckRefusesExhaustedAndRepeatingTasks(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	fresh := r.task("p", factory.Budget{MaxUSD: 5})
	spent := r.task("p", factory.Budget{MaxUSD: 1})
	loop := r.task("p", factory.Budget{})
	_, _ = r.st.AddSpend(context.Background(), spent.ID, factory.Spend{USD: 1, Runs: 1})
	for range 3 {
		_, _ = r.st.UpdateTask(context.Background(), loop.ID, factory.Patch{AddFingerprint: "same"})
	}
	g := r.guard()

	// Act
	okFresh, _ := g.Check(context.Background(), fresh.ID)
	okSpent, reasonSpent := g.Check(context.Background(), spent.ID)
	okLoop, reasonLoop := g.Check(context.Background(), loop.ID)
	okMissing, _ := g.Check(context.Background(), "nope")

	// Assert
	if !okFresh || okSpent || okLoop || okMissing {
		t.Fatalf("fresh %v spent %v loop %v missing %v", okFresh, okSpent, okLoop, okMissing)
	}
	if !strings.HasPrefix(reasonSpent, "budget exhausted") || !strings.Contains(reasonLoop, "same way") {
		t.Fatalf("reasons: %q, %q", reasonSpent, reasonLoop)
	}
}

func TestEscalatesWhenTheTaskCannotBeBlocked(t *testing.T) {
	// Arrange
	var got []string
	r := newRig(t, func(o *budget.Options) {
		o.Controller = nil
		o.Escalator = factory.EscalatorFunc(func(_ context.Context, tk factory.Task, reason string) { got = append(got, tk.ID+": "+reason) })
	})
	tk := r.task("p", factory.Budget{MaxTokens: 10})

	// Act
	r.run(r.guard(), finish(tk, "r1", use("m", 20, 0, 0)))

	// Assert
	if len(got) != 1 || !strings.HasPrefix(got[0], tk.ID+": budget exhausted") {
		t.Fatalf("escalations = %v", got)
	}
}

func runOf(task *factory.Task, id string) *store.Run {
	return &store.Run{ID: id, ThreadID: "th-" + task.ID}
}

func denial(g *budget.Guard, run *store.Run, tool string) string {
	return g.BeforeTool().(agent.ToolDenier).Denial(context.Background(), run, tool, "{}")
}

func TestToolsAreDeniedOnceTheBudgetIsGoneButReadOnlyOnesStay(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{MaxUSD: 1})
	other := r.task("p", factory.Budget{MaxUSD: 100})
	g := r.guard()
	run := runOf(tk, "next")

	// Act
	before := denial(g, run, "write_file")
	_, _ = r.st.AddSpend(context.Background(), tk.ID, factory.Spend{USD: 1, Runs: 1})
	writes := denial(g, run, "write_file")
	exec := denial(g, run, "execute_command")
	reads := []string{denial(g, run, "read_file"), denial(g, run, "search_files"), denial(g, run, "list_files")}
	otherTask := denial(g, runOf(other, "o1"), "write_file")
	chat := denial(g, &store.Run{ID: "c1", ThreadID: "chat"}, "write_file")

	// Assert
	if before != "" || otherTask != "" || chat != "" {
		t.Fatalf("refused too early: %q %q %q", before, otherTask, chat)
	}
	for _, d := range []string{writes, exec} {
		if !strings.Contains(d, "task budget exhausted; finish with a summary of what is done and what remains") {
			t.Fatalf("denial = %q", d)
		}
	}
	for i, d := range reads {
		if d != "" {
			t.Fatalf("read-only tool %d refused: %q", i, d)
		}
	}
}

func TestGuardCancelsARunThatIgnoresTheRefusals(t *testing.T) {
	// Arrange
	var mu sync.Mutex
	var cancelled []string
	done := make(chan struct{}, 4)
	r := newRig(t, func(o *budget.Options) {
		o.Cancel = func(_ context.Context, id string) error {
			mu.Lock()
			cancelled = append(cancelled, id)
			mu.Unlock()
			done <- struct{}{}
			return nil
		}
	})
	tk := r.task("p", factory.Budget{MaxUSD: 1})
	_, _ = r.st.AddSpend(context.Background(), tk.ID, factory.Spend{USD: 2, Runs: 1})
	g := r.guard()
	run := runOf(tk, "stubborn")

	// Act
	for range 4 {
		denial(g, run, "write_file")
	}
	mu.Lock()
	early := len(cancelled)
	mu.Unlock()
	for range 4 {
		denial(g, run, "write_file")
	}
	<-done

	// Assert
	mu.Lock()
	defer mu.Unlock()
	if early != 0 || len(cancelled) != 1 || cancelled[0] != "stubborn" {
		t.Fatalf("cancelled early %d, final %v", early, cancelled)
	}
}

func TestToolCallsPerRunAreCapped(t *testing.T) {
	// Arrange
	r := newRig(t, func(o *budget.Options) { o.MaxToolCallsPerRun = 3 })
	tk := r.task("p", factory.Budget{})
	g := r.guard()
	run, next := runOf(tk, "r1"), runOf(tk, "r2")
	count := func(run *store.Run) { g.ToolHook().AfterTool(context.Background(), run, "c", "read_file") }

	// Act
	var denied []bool
	for range 5 {
		denied = append(denied, denial(g, run, "read_file") != "")
		count(run)
	}
	freshRun := denial(g, next, "write_file")

	// Assert
	if fmt.Sprint(denied) != "[false false false true true]" {
		t.Fatalf("denied = %v", denied)
	}
	if freshRun != "" {
		t.Fatalf("the cap is per run: %q", freshRun)
	}
	if d := denial(g, run, "write_file"); !strings.Contains(d, "3 tool calls") {
		t.Fatalf("message = %q", d)
	}
}

func TestToolCallCapStateIsDroppedWhenTheRunFinishes(t *testing.T) {
	// Arrange
	r := newRig(t, func(o *budget.Options) { o.MaxToolCallsPerRun = 1 })
	tk := r.task("p", factory.Budget{})
	g := r.guard()
	run := runOf(tk, "r1")
	g.ToolHook().AfterTool(context.Background(), run, "c", "write_file")
	before := denial(g, run, "write_file")

	// Act
	r.run(g, finish(tk, "r1"))
	r2 := denial(g, runOf(tk, "r1"), "write_file") // a resumed run reusing the id starts at zero

	// Assert
	if before == "" || r2 != "" {
		t.Fatalf("before %q, after finish %q", before, r2)
	}
}

func TestStepNoteWarnsOnceAtEightyPercent(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{MaxUSD: 10})
	g := r.guard()
	note := g.StepNote()
	in := agent.Input{Run: runOf(tk, "r1")}
	ctx := context.Background()

	// Act
	low := note(ctx, in)
	_, _ = r.st.AddSpend(ctx, tk.ID, factory.Spend{USD: 8, Runs: 1})
	warn := note(ctx, in)
	again := note(ctx, in)
	otherRun := note(ctx, agent.Input{Run: runOf(tk, "r2")})
	chat := note(ctx, agent.Input{Run: &store.Run{ID: "c", ThreadID: "chat"}})

	// Assert
	if low != "" || again != "" || chat != "" {
		t.Fatalf("notes: low %q again %q chat %q", low, again, chat)
	}
	if !strings.Contains(warn, "80% of the task budget") || otherRun == "" {
		t.Fatalf("warn %q, other run %q", warn, otherRun)
	}
}

func TestStepNoteNeedsACap(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{})
	_, _ = r.st.AddSpend(context.Background(), tk.ID, factory.Spend{USD: 1000, Runs: 1})

	// Act
	note := r.guard().StepNote()(context.Background(), agent.Input{Run: runOf(tk, "r1")})

	// Assert
	if note != "" {
		t.Fatalf("note = %q", note)
	}
}

func TestConcurrentRunsAreAllBookedAndBlockOnce(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	tk := r.task("p", factory.Budget{MaxUSD: 10.5})
	g := r.guard()

	// Act
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := fmt.Sprintf("r%d", i)
			denial(g, runOf(tk, run), "write_file")
			g.ToolHook().AfterTool(context.Background(), runOf(tk, run), "c", "write_file")
			r.run(g, finish(tk, run, use("m", 1000, 0, 0)))
			r.run(g, finish(tk, run, use("m", 1000, 0, 0))) // duplicate delivery
		}()
	}
	wg.Wait()

	// Assert
	if got := r.get(tk.ID).Spent; got.Runs != 40 || !near(got.USD, 40) {
		t.Fatalf("spent = %+v", got)
	}
	if b := r.ctl.blocks(tk.ID); len(b) == 0 {
		t.Fatal("never blocked")
	}
}

func TestSummary(t *testing.T) {
	// Arrange
	r := newRig(t, nil)
	ctx := context.Background()
	var ids []string
	for i := range 7 {
		tk := r.task("p", factory.Budget{})
		ids = append(ids, tk.ID)
		_, _ = r.st.AddSpend(ctx, tk.ID, factory.Spend{USD: float64(i + 1), Tokens: int64(100 * (i + 1)), Runs: 1})
	}
	_, _ = r.st.AddSpend(ctx, ids[0], factory.Spend{Unpriced: 2, Tokens: 50, Runs: 1})
	done := factory.StateDone
	_, _ = r.st.UpdateTask(ctx, ids[6], factory.Patch{State: &done})
	other := r.task("q", factory.Budget{})
	_, _ = r.st.AddSpend(ctx, other.ID, factory.Spend{USD: 99, Runs: 1})

	// Act
	rep, err := budget.Summary(ctx, r.st, "p", t0.Add(-time.Hour))
	old, err2 := budget.Summary(ctx, r.st, "p", t0.Add(time.Hour))

	// Assert
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if !near(rep.Total.USD, 28) || rep.Total.Unpriced != 2 || rep.Total.Runs != 8 {
		t.Fatalf("total = %+v", rep.Total)
	}
	if rep.ByState[factory.StateReady] != 6 || rep.ByState[factory.StateDone] != 1 {
		t.Fatalf("by state = %v", rep.ByState)
	}
	if len(rep.Top) != 5 || rep.Top[0].ID != ids[6] || rep.Top[0].State != factory.StateDone || rep.Top[4].ID != ids[2] {
		t.Fatalf("top = %+v", rep.Top)
	}
	if old.Total != (factory.Spend{}) || len(old.Top) != 0 || len(old.ByState) != 0 {
		t.Fatalf("future window = %+v", old)
	}
}
