package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/runtime"
)

// sweepDuringFinish plays the sweeper taking the run over while the runtime finishes it
// (its heartbeat was stale for more than StaleAge).
type sweepDuringFinish struct {
	r *rig
}

func (h sweepDuringFinish) Finishing(ctx context.Context, f *runtime.Finish) func(context.Context) {
	h.r.st.SetHeartbeat(f.Run.ID, time.Now().Add(-2*time.Minute))
	sweeper := runtime.New(runtime.Options{ID: "sweeper"}, h.r.st, h.r.log, h.r.q, h.r.q, h.r.agent, nil)
	sweeper.Sweep(ctx)
	return nil
}

func TestProcessThatLostTheRunAtFinishSkipsContinuationsAndTheTerminalEvent(t *testing.T) {
	// When the sweeper interrupted the run meanwhile, the runtime starts no goal continuation and writes no second terminal event.
	// Arrange
	r := newRig(t,
		llmtest.Turn{Chunks: llmtest.Text("I loaded the data.")},
		llmtest.Turn{Chunks: llmtest.Text(`{"met": false, "reason": "Not yet."}`)},
	)
	r.opts.Hooks = append([]runtime.Hook{sweepDuringFinish{r: r}}, r.opts.Hooks...)
	th, _, run := r.goalRun(3)

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID)
	if got.Status != store.StatusInterrupted || last(evs)["type"] != "RUN_ERROR" || len(find(evs, "RUN_FINISHED")) != 0 {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
	if g := r.goal(th.ID); g.TurnsUsed != 1 || r.thread(th.ID).ActiveRun != nil {
		t.Fatalf("a goal continuation started: goal %+v active %+v", g, r.thread(th.ID).ActiveRun)
	}
}

func TestReplayWaitsForTheTerminalEventWhileContinuationsRun(t *testing.T) {
	// A slow end-of-run continuation (longer than TerminalWait) does not make replay invent a terminal event: the client gets the real one.
	// Arrange
	r := newRig(t, llmtest.Turn{Chunks: llmtest.Text("done")})
	r.log.TerminalWait = 50 * time.Millisecond
	r.opts.Hooks = []runtime.Hook{slowContinuation{d: 400 * time.Millisecond}}
	_, run := r.queued("q")
	done := make(chan struct{})
	go func() { defer close(done); r.process(run) }()
	waitFor(t, "run started", func() bool { return len(r.liveTypes(run.ID)) > 0 })

	// Act
	var replayed []store.Event
	for e := range r.log.Replay(context.Background(), run.ID, 0) {
		replayed = append(replayed, e)
	}
	<-done

	// Assert
	durable, _ := r.st.ListEvents(context.Background(), run.ID, 0, 1000)
	if len(replayed) != len(durable) || string(replayed[len(replayed)-1].Data) != string(durable[len(durable)-1].Data) {
		t.Fatalf("replayed %d events ending %s, durable %d ending %s", len(replayed), replayed[len(replayed)-1].Data, len(durable), durable[len(durable)-1].Data)
	}
}

// slowContinuation is an end-of-run continuation that takes d (a slow goal handoff).
type slowContinuation struct{ d time.Duration }

func (s slowContinuation) Finishing(context.Context, *runtime.Finish) func(context.Context) {
	return func(context.Context) { time.Sleep(s.d) }
}
