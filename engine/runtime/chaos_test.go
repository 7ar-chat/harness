package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

// viaSlowRedis routes the rig's event log and queue through a proxy that can
// delay one reply past the client's read timeout (go-redis then retries).
func (r *rig) viaSlowRedis(delay, readTimeout time.Duration) *slowProxy {
	r.t.Helper()
	p := newSlowProxy(r.t, r.mr.Addr(), delay)
	rdb := redis.NewClient(&redis.Options{Addr: p.Addr(), ReadTimeout: readTimeout})
	r.t.Cleanup(func() { _ = rdb.Close() })
	r.rdb = rdb
	r.log = redisx.NewEventLog(rdb, r.st)
	r.log.TailBlock, r.log.Poll = 100*time.Millisecond, 10*time.Millisecond
	r.q = redisx.NewQueue(rdb)
	r.agent.Steers = r.q
	r.rt = nil
	return p
}

func TestChaosSlowRedisReplyDuringAnAppendLosesNoEvent(t *testing.T) {
	// One XADD reply delayed past the read timeout (applied by Redis, retried by go-redis) is an idempotent retry: the run completes and Postgres has every seq.
	// Arrange
	r := newRig(t)
	p := r.viaSlowRedis(400*time.Millisecond, 150*time.Millisecond)
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("first part, then more"), OnCall: func() { p.delayNext("xadd") }})
	r.agent.Models = llmtest.Factory{M: r.model}
	_, run := r.queued("q")

	// Act
	got := r.process(run)

	// Assert
	evs := r.events(run.ID) // fails on any seq gap
	live := r.liveTypes(run.ID)
	if got.Status != store.StatusCompleted || p.delayed.Load() != 1 || last(evs)["type"] != "RUN_FINISHED" || len(live) != len(evs) {
		t.Fatalf("status %s (%s) delayed %d types %v live %v", got.Status, got.ErrorCode, p.delayed.Load(), types(evs), live)
	}
}

// pgOutage makes the run heartbeat fail like Postgres being unreachable.
type pgOutage struct {
	*memstore.Store
	down atomic.Bool
}

func (p *pgOutage) Heartbeat(ctx context.Context, id, runtimeID string) (bool, bool, error) {
	if p.down.Load() {
		return true, false, errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	}
	return p.Store.Heartbeat(ctx, id, runtimeID)
}

func TestChaosPostgresOutageDoesNotGetLiveRunsInterrupted(t *testing.T) {
	// After a Postgres outage longer than StaleAge, another runtime's sweeper leaves alone a run whose runtime kept heartbeating in Redis; the run completes.
	// Arrange
	r := newRig(t)
	st := &pgOutage{Store: r.st}
	r.opts.Heartbeat = 20 * time.Millisecond
	var runID string
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("still working"), OnCall: func() {
		st.down.Store(true)
		time.Sleep(60 * time.Millisecond)                         // heartbeats fail meanwhile
		r.st.SetHeartbeat(runID, time.Now().Add(-90*time.Second)) // the last one Postgres saw
		other := runtime.New(runtime.Options{ID: "other"}, r.st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
		other.Sweep(context.Background()) // first sweep after Postgres is back
		st.down.Store(false)
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	rt := runtime.New(r.opts, st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	_, run := r.queued("q")
	runID = run.ID

	// Act
	_ = rt.Process(context.Background(), run.ID)

	// Assert
	evs := r.events(run.ID)
	if got := r.reload(run); got.Status != store.StatusCompleted || last(evs)["type"] != "RUN_FINISHED" {
		t.Fatalf("status %s types %v", got.Status, types(evs))
	}
}

// killSwitch is the Postgres view of a runtime process: after kill, every
// write fails (the process is gone); after revive, it finds its run lost.
type killSwitch struct {
	*memstore.Store
	killed, revived atomic.Bool
}

var errKilled = errors.New("runtime process killed")

func (k *killSwitch) Heartbeat(ctx context.Context, id, rt string) (bool, bool, error) {
	switch {
	case k.revived.Load():
		return false, false, nil
	case k.killed.Load():
		return true, false, errKilled
	}
	return k.Store.Heartbeat(ctx, id, rt)
}

func (k *killSwitch) AppendEvents(ctx context.Context, id string, evs []store.Event) error {
	if k.killed.Load() {
		return errKilled
	}
	return k.Store.AppendEvents(ctx, id, evs)
}

func (k *killSwitch) FinishRun(ctx context.Context, id, rt, status, code, msg string) error {
	if k.killed.Load() {
		return errKilled
	}
	return k.Store.FinishRun(ctx, id, rt, status, code, msg)
}

func TestChaosRuntimeKilledMidRunIsRecoveredByTheSweeper(t *testing.T) {
	// A runtime killed mid-run (no more heartbeats or writes) gets its run interrupted by another runtime's sweeper with a valid log; the thread's next message runs normally.
	// Arrange
	r := newRig(t)
	stale := 300 * time.Millisecond
	dead := &killSwitch{Store: r.st}
	deadRdb := redis.NewClient(&redis.Options{Addr: r.mr.Addr()})
	deadLog := redisx.NewEventLog(deadRdb, dead)
	deadLog.CloseRetry = 50 * time.Millisecond
	streaming := make(chan struct{})
	r.model = llmtest.New(
		llmtest.Turn{Chunks: llmtest.Text("working on it"), Block: true, OnCall: func() { close(streaming) }},
		llmtest.Turn{Chunks: llmtest.Text("Back to work.")},
	)
	r.agent.Models = llmtest.Factory{M: r.model}
	deadRT := runtime.New(runtime.Options{ID: "dead-rt", Heartbeat: 20 * time.Millisecond, StaleAge: stale}, dead, deadLog, r.q, r.q, r.agent, redisx.NewCoordinator(deadRdb))
	th, run := r.queued("long job")
	deadDone := make(chan struct{})
	go func() { defer close(deadDone); _ = deadRT.Process(context.Background(), run.ID) }()
	<-streaming
	dead.killed.Store(true)
	_ = deadRdb.Close() // its Redis heartbeat stops and expires after StaleAge
	r.mr.FastForward(stale + 100*time.Millisecond)
	r.st.SetHeartbeat(run.ID, time.Now().Add(-time.Minute))
	survivor := runtime.New(runtime.Options{ID: "survivor", StaleAge: stale}, r.st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))

	// Act
	survivor.Sweep(context.Background())
	next := r.queuedOn(th, "are you still there?")
	_ = survivor.Process(context.Background(), next.ID)

	// Assert
	dead.revived.Store(true) // the zombie finds its run lost and stops without writing
	<-deadDone
	if got := r.reload(run); got.Status != store.StatusInterrupted || last(r.events(run.ID))["code"] != "interrupted" {
		t.Fatalf("killed run %s types %v", got.Status, types(r.events(run.ID)))
	}
	if got := r.reload(next); got.Status != store.StatusCompleted || last(r.events(next.ID))["type"] != "RUN_FINISHED" {
		t.Fatalf("next run %s types %v", got.Status, types(r.events(next.ID)))
	}
}
