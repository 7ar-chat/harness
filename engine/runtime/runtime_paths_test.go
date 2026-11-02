package runtime_test

import (
	"context"
	"errors"
	"github.com/7ar-chat/harness/ai"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/runtime"
)

// execFunc is an Executor made of a function.
type execFunc func(ctx context.Context, in agent.Input, w eventlog.Writer) (agent.Result, error)

func (f execFunc) Run(ctx context.Context, in agent.Input, w eventlog.Writer) (agent.Result, error) {
	return f(ctx, in, w)
}

// reporter records what the runtime reports as internal failures.
type reporter struct {
	mu     sync.Mutex
	errs   []error
	panics []any
}

func (r *reporter) CaptureError(err error) { r.mu.Lock(); r.errs = append(r.errs, err); r.mu.Unlock() }
func (r *reporter) CapturePanic(p any)     { r.mu.Lock(); r.panics = append(r.panics, p); r.mu.Unlock() }

// runtimeWith builds a runtime around exec with the rig's stores and no hooks.
func (r *rig) runtimeWith(exec runtime.Executor, mutate func(*runtime.Options)) *runtime.Runtime {
	opts := r.opts
	opts.Hooks = nil
	if mutate != nil {
		mutate(&opts)
	}
	return runtime.New(opts, r.st, r.log, r.q, r.q, exec, redisx.NewCoordinator(r.rdb))
}

func (r *rig) processWith(rt *runtime.Runtime, run *store.Run) *store.Run {
	r.t.Helper()
	if err := rt.Process(context.Background(), run.ID); err != nil {
		r.t.Fatal(err)
	}
	return r.reload(run)
}

func TestAnAgentErrorFailsTheRunWithItsCodeAndNeverReachesTheErrorTracker(t *testing.T) {
	// Arrange
	r := newRig(t)
	rep := &reporter{}
	rt := r.runtimeWith(execFunc(func(context.Context, agent.Input, eventlog.Writer) (agent.Result, error) {
		return agent.Result{}, &agent.Error{Code: agui.CodeRateLimited, Message: agent.MsgRateLimited, Detail: "429 from api.internal"}
	}), func(o *runtime.Options) { o.Reporter = rep })
	_, run := r.queued("q")

	// Act
	got := r.processWith(rt, run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusFailed || got.ErrorCode != agui.CodeRateLimited || got.Error != agent.MsgRateLimited ||
		fin["type"] != "RUN_ERROR" || fin["code"] != agui.CodeRateLimited || fin["message"] != agent.MsgRateLimited {
		t.Fatalf("run %+v last %v", got, fin)
	}
	if strings.Contains(got.Error, "api.internal") {
		t.Error("provider detail stored on the run")
	}
	if len(rep.errs) != 0 || len(rep.panics) != 0 {
		t.Errorf("expected failures were reported: %v %v", rep.errs, rep.panics)
	}
}

func TestAnUnexpectedErrorFailsTheRunWithAGenericMessageAndIsReported(t *testing.T) {
	// Arrange
	r := newRig(t)
	rep := &reporter{}
	boom := errors.New("pq: password authentication failed for user admin")
	rt := r.runtimeWith(execFunc(func(context.Context, agent.Input, eventlog.Writer) (agent.Result, error) {
		return agent.Result{}, boom
	}), func(o *runtime.Options) { o.Reporter = rep })
	_, run := r.queued("q")

	// Act
	got := r.processWith(rt, run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusFailed || got.Error != agent.MsgInternal || fin["code"] != agui.CodeInternal || fin["message"] != agent.MsgInternal {
		t.Fatalf("run %+v last %v", got, fin)
	}
	if len(rep.errs) != 1 || !errors.Is(rep.errs[0], boom) {
		t.Fatalf("reported %v", rep.errs)
	}
}

func TestAPanickingExecutorFailsOnlyItsRunAndIsReported(t *testing.T) {
	// Arrange
	r := newRig(t)
	rep := &reporter{}
	rt := r.runtimeWith(execFunc(func(context.Context, agent.Input, eventlog.Writer) (agent.Result, error) {
		panic("nil map in the agent")
	}), func(o *runtime.Options) { o.Reporter = rep })
	_, run := r.queued("q")

	// Act
	got := r.processWith(rt, run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusFailed || fin["type"] != "RUN_ERROR" || fin["message"] != agent.MsgInternal || strings.Contains(got.Error, "nil map") {
		t.Fatalf("run %+v last %v", got, fin)
	}
	if len(rep.panics) != 1 || rep.panics[0] != "nil map in the agent" {
		t.Fatalf("reported panics %v", rep.panics)
	}
	// The runtime survives: the next run is processed.
	_, next := r.queued("again")
	if got := r.processWith(rt, next); got.Status != store.StatusFailed {
		t.Fatalf("next run %s", got.Status)
	}
}

func TestAPanicWithoutAnErrorTrackerStillFailsTheRun(t *testing.T) {
	// Arrange
	r := newRig(t)
	rt := r.runtimeWith(execFunc(func(context.Context, agent.Input, eventlog.Writer) (agent.Result, error) { panic("x") }), nil)
	_, run := r.queued("q")

	// Act + Assert
	if got := r.processWith(rt, run); got.Status != store.StatusFailed {
		t.Fatalf("status %s", got.Status)
	}
}

func TestARunCancelledBeforeItStartsFinishesCancelledWithoutWork(t *testing.T) {
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	_, _ = r.st.RequestCancel(context.Background(), run.ID)
	var ran atomic.Bool
	rt := r.runtimeWith(execFunc(func(ctx context.Context, _ agent.Input, _ eventlog.Writer) (agent.Result, error) {
		ran.Store(true)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}), nil)

	// Act
	got := r.processWith(rt, run)

	// Assert
	fin := last(r.events(run.ID))
	if got.Status != store.StatusCancelled || fin["outcome"].(map[string]any)["type"] != "cancelled" {
		t.Fatalf("status %s last %v", got.Status, fin)
	}
	if !ran.Load() {
		t.Fatal("the executor never saw the cancelled context")
	}
}

// deadPubSub fails every cancel subscription, like Redis pub/sub being down.
type deadPubSub struct{ queue.Bus }

func (deadPubSub) SubscribeCancel(context.Context, string, func()) (func(), error) {
	return func() {}, errors.New("pubsub down") // like the Redis bus: an unsubscribe func even on error
}

func TestACancelStillReachesARunThroughTheHeartbeatWhenPubSubIsDown(t *testing.T) {
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	started := make(chan struct{})
	opts := r.opts
	opts.Hooks, opts.Heartbeat = nil, 10*time.Millisecond
	rt := runtime.New(opts, r.st, r.log, r.q, deadPubSub{r.q}, execFunc(func(ctx context.Context, _ agent.Input, _ eventlog.Writer) (agent.Result, error) {
		close(started)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	}), redisx.NewCoordinator(r.rdb))
	done := make(chan *store.Run)
	go func() { done <- r.processWith(rt, run) }()
	<-started

	// Act
	_, _ = r.st.RequestCancel(context.Background(), run.ID)

	// Assert
	select {
	case got := <-done:
		if got.Status != store.StatusCancelled {
			t.Fatalf("status %s", got.Status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel never reached the run")
	}
}

func TestARunWhoseLogWasTakenOverStopsWithoutWritingATerminalEvent(t *testing.T) {
	// Arrange: while the run works, the sweeper (another writer) finishes its log.
	r := newRig(t)
	_, run := r.queued("q")
	rt := r.runtimeWith(execFunc(func(ctx context.Context, _ agent.Input, w eventlog.Writer) (agent.Result, error) {
		other := r.log.Writer(run.ID, w.LastSeq())
		if _, err := other.Append(ctx, agui.RunErrorEvent(agui.CodeInterrupted, runtime.InterruptedMessage)); err != nil {
			return agent.Result{}, err
		}
		_ = other.Close(ctx)
		_, err := w.Append(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": "step-1"})) // finds its seq taken
		return agent.Result{}, err
	}), nil)

	// Act
	rt.Process(context.Background(), run.ID)

	// Assert
	evs := r.events(run.ID)
	terminals := 0
	for _, e := range evs {
		if e["type"] == "RUN_ERROR" || e["type"] == "RUN_FINISHED" {
			terminals++
		}
	}
	if terminals != 1 || last(evs)["code"] != agui.CodeInterrupted {
		t.Fatalf("events %v", types(evs))
	}
}

func TestProcessReturnsTheStoreErrorWhenTheRunCannotBeClaimed(t *testing.T) {
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	boom := errors.New("connection refused")
	var ran atomic.Bool
	st := &claimFails{Full: r.st, err: boom}
	st.left.Store(1)
	rt := runtime.New(r.opts, st, r.log, r.q, r.q, execFunc(func(context.Context, agent.Input, eventlog.Writer) (agent.Result, error) {
		ran.Store(true)
		return agent.Result{}, nil
	}), nil)

	// Act
	err := rt.Process(context.Background(), run.ID)

	// Assert
	if !errors.Is(err, boom) || ran.Load() {
		t.Fatalf("err %v ran %v", err, ran.Load())
	}
}

// claimFails fails the first n ClaimRun calls with err.
type claimFails struct {
	store.Full
	err   error
	left  atomic.Int32
	calls atomic.Int32
}

func (c *claimFails) ClaimRun(ctx context.Context, id, rt string) (*store.Run, error) {
	c.calls.Add(1)
	if c.left.Add(-1) >= 0 {
		return nil, c.err
	}
	return c.Full.ClaimRun(ctx, id, rt)
}

func TestTheQueueLoopSurvivesAClaimErrorAndKeepsServingTheQueue(t *testing.T) {
	// Arrange: the claim of the first entry fails (database blip); it is acked all the same and the loop goes on.
	r := newRig(t)
	_, bad := r.queued("first")
	st := &claimFails{Full: r.st, err: errors.New("connection refused")}
	st.left.Store(1)
	rt := runtime.New(r.opts, st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rt.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	_ = r.q.Enqueue(context.Background(), bad.ID)
	waitFor(t, "claim attempted", func() bool { return st.calls.Load() >= 1 })

	// Act
	th, _ := r.st.CreateThread(context.Background(), "alice", "t2")
	second := r.queuedOn(th, "second")
	_ = r.q.Enqueue(context.Background(), second.ID)

	// Assert
	waitFor(t, "second run completed", func() bool { return r.reload(second).Status == store.StatusCompleted })
	if got := r.reload(bad); got.Status != store.StatusQueued {
		t.Fatalf("the unclaimed run is %s", got.Status)
	}
	pending, _ := r.rdb.XPending(context.Background(), queue.Stream, queue.Group).Result()
	if pending.Count != 0 {
		t.Fatalf("entries left pending: %+v", pending)
	}
}

func TestHealthReportsAQueueLoopThatStoppedReading(t *testing.T) {
	// Arrange
	r := newRig(t)
	r.opts.StuckAfter = 150 * time.Millisecond
	rt := r.runtime()
	if err := rt.Health(); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("before Run: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rt.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "healthy", func() bool { return rt.Health() == nil })

	// Act: Redis stops answering reads.
	r.mr.SetError("LOADING Redis is loading the dataset in memory")

	// Assert
	waitFor(t, "stuck", func() bool {
		err := rt.Health()
		return err != nil && strings.Contains(err.Error(), "stuck")
	})
	r.mr.SetError("")
	waitFor(t, "recovered", func() bool { return rt.Health() == nil })
}

func TestTheRunningRuntimeSweepsOnItsOwn(t *testing.T) {
	// Arrange: a run of a dead runtime, and a runtime whose sweeper ticks every few milliseconds.
	r := newRig(t)
	r.opts.SweepEvery = 20 * time.Millisecond
	_, run := r.queued("q")
	ctx := context.Background()
	_, _ = r.st.ClaimRun(ctx, run.ID, "dead-runtime")
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = r.runtime().Run(rctx); close(done) }()
	defer func() { cancel(); <-done }()

	// Assert
	waitFor(t, "run interrupted by the sweeper loop", func() bool { return r.reload(run).Status == store.StatusInterrupted })
}

func TestJobsRunIndependentlyAndAFailingJobWaitsForItsNextPeriod(t *testing.T) {
	// Arrange
	r := newRig(t)
	var failing, healthy atomic.Int32
	r.opts.Jobs = []runtime.Job{
		{Name: "no-run", Every: time.Hour},
		{Name: "no-period", Run: func(context.Context) error { t.Error("job without a period ran"); return nil }},
		{Name: "failing", Every: time.Hour, Run: func(context.Context) error { failing.Add(1); return errors.New("boom") }},
		{Name: "healthy", Every: time.Hour, Run: func(context.Context) error { healthy.Add(1); return nil }},
	}
	rt := r.runtime()

	// Act
	rt.Sweep(context.Background())
	r.mr.FastForward(time.Minute)
	rt.Sweep(context.Background())

	// Assert
	if failing.Load() != 1 || healthy.Load() != 1 {
		t.Fatalf("failing ran %d, healthy ran %d", failing.Load(), healthy.Load())
	}
}

type countingPurger struct {
	runs atomic.Int32
	err  error
}

func (p *countingPurger) Purge(context.Context) error { p.runs.Add(1); return p.err }

func TestThePurgeRunsOncePerPeriodAcrossRuntimesAndItsFailureDoesNotStopTheSweep(t *testing.T) {
	// Arrange
	r := newRig(t)
	p := &countingPurger{err: errors.New("blob store down")}
	r.opts.Purger, r.opts.PurgeEvery = p, time.Hour
	_, queued := r.queued("q")
	r.st.SetRunCreatedAt(queued.ID, time.Now().Add(-time.Minute))
	other := runtime.New(runtime.Options{ID: "rt-other", Purger: p, PurgeEvery: time.Hour}, r.st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))

	// Act
	r.runtime().Sweep(context.Background())
	r.mr.FastForward(time.Minute) // the sweeper lock expires, the purge lock does not
	other.Sweep(context.Background())

	// Assert
	if p.runs.Load() != 1 {
		t.Fatalf("purge ran %d times", p.runs.Load())
	}
	entries, _ := r.rdb.XRange(context.Background(), queue.Stream, "-", "+").Result()
	if len(entries) == 0 {
		t.Fatal("the sweep stopped after the purge failed")
	}
}

func TestTheSweeperAcksSteersOfRunsThatNoLongerExistAndLeavesThoseOfRunningRuns(t *testing.T) {
	// Arrange
	r := newRig(t)
	ctx := context.Background()
	_, running := r.queued("q")
	_, _ = r.st.ClaimRun(ctx, running.ID, "alive")
	for _, id := range []string{"ghost-run", running.ID} {
		if err := r.q.PushSteer(ctx, id, queue.Steer{ID: "s-" + id, Text: "also B"}); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	r.runtime().Sweep(ctx)

	// Assert
	ids, _ := r.q.SteerRuns(ctx)
	if len(ids) != 1 || ids[0] != running.ID {
		t.Fatalf("runs with pending steers: %v", ids)
	}
}

func TestSweepingDoesNotNeedRedisLocksInASingleProcess(t *testing.T) {
	// Arrange
	r := newRig(t)
	_, run := r.queued("q")
	_, _ = r.st.ClaimRun(context.Background(), run.ID, "dead-runtime")
	r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))
	opts := r.opts
	opts.Hooks = nil
	rt := runtime.New(opts, r.st, r.log, r.q, r.q, r.agent, nil)

	// Act
	rt.Sweep(context.Background())

	// Assert
	if got := r.reload(run); got.Status != store.StatusInterrupted {
		t.Fatalf("status %s", got.Status)
	}
}

func TestARunProcessesWithoutRedisLocks(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("hi")})
	opts := r.opts
	opts.Hooks = nil
	rt := runtime.New(opts, r.st, r.log, r.q, r.q, r.agent, nil)
	_, run := r.queued("q")

	// Act
	got := r.processWith(rt, run)

	// Assert
	if got.Status != store.StatusCompleted {
		t.Fatalf("status %s", got.Status)
	}
}

func TestTheSweeperCopiesTheOutcomeOfALogThatAlreadyEndedToARunStillMarkedRunning(t *testing.T) {
	// A runtime that wrote its terminal event but died before recording the status: the log is the truth, no second terminal event is added.
	for name, tc := range map[string]struct {
		final   func(*store.Run) agui.Event
		status  string
		errCode string
	}{
		"finished":  {func(run *store.Run) agui.Event { return agui.RunFinishedEvent(run.ThreadID, run.ID, "success", nil) }, store.StatusCompleted, ""},
		"cancelled": {func(run *store.Run) agui.Event { return agui.RunFinishedEvent(run.ThreadID, run.ID, "cancelled", nil) }, store.StatusCancelled, ""},
		"failed":    {func(*store.Run) agui.Event { return agui.RunErrorEvent(agui.CodeRateLimited, agent.MsgRateLimited) }, store.StatusFailed, agui.CodeRateLimited},
		"interrupted": {func(*store.Run) agui.Event {
			return agui.RunErrorEvent(agui.CodeInterrupted, runtime.InterruptedMessage)
		}, store.StatusInterrupted, agui.CodeInterrupted},
	} {
		t.Run(name, func(t *testing.T) {
			// Arrange
			r := newRig(t)
			_, run := r.queued("q")
			ctx := context.Background()
			_, _ = r.st.ClaimRun(ctx, run.ID, "dead-runtime")
			w := r.log.Writer(run.ID, 0)
			_, _ = w.Append(ctx, agui.RunStartedEvent(run.ThreadID, run.ID))
			_, _ = w.Append(ctx, tc.final(run))
			_ = w.Close(ctx)
			r.st.SetHeartbeat(run.ID, time.Now().Add(-2*time.Minute))

			// Act
			r.runtime().Sweep(ctx)

			// Assert
			got := r.reload(run)
			if got.Status != tc.status || got.ErrorCode != tc.errCode {
				t.Fatalf("status %s code %q", got.Status, got.ErrorCode)
			}
			if n := len(r.events(run.ID)); n != 2 {
				t.Fatalf("%d events: a second terminal event was added", n)
			}
		})
	}
}

type panickyHook struct{}

func (panickyHook) Finishing(context.Context, *runtime.Finish) func(context.Context) {
	panic("hook bug")
}

type usageHook struct{ continued *atomic.Bool }

func (h usageHook) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	f.AddUsage(agui.Usage{Model: "gpt-test", InputTokens: 5, OutputTokens: 2, TotalTokens: 7})
	f.AddUsage(agui.Usage{Model: "title-model", InputTokens: 3, OutputTokens: 1, TotalTokens: 4})
	return func(context.Context) { h.continued.Store(true) }
}

func TestAPanickingHookNeverStopsTheRunNorTheHooksAfterIt(t *testing.T) {
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: []*ai.Message{llmtest.WithUsage(&ai.Message{Content: "hi"}, 100, 10)}})
	var continued atomic.Bool
	r.opts.Hooks = []runtime.Hook{panickyHook{}, usageHook{&continued}}
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	if got.Status != store.StatusCompleted || !continued.Load() {
		t.Fatalf("status %s continuation ran %v", got.Status, continued.Load())
	}
	usage := last(r.events(run.ID))["usage"].([]any)
	byModel := map[string]map[string]any{}
	for _, u := range usage {
		m := u.(map[string]any)
		byModel[m["model"].(string)] = m
	}
	if byModel["gpt-test"]["inputTokens"] != float64(105) || byModel["gpt-test"]["totalTokens"] != float64(117) || byModel["title-model"]["inputTokens"] != float64(3) {
		t.Fatalf("usage %v", usage)
	}
}
