package persistence_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestARunOfACrashedRuntimeIsInterruptedBySweepAndALateFinishCannotOverwriteIt(t *testing.T) {
	// Arrange: two runtimes share one Postgres database and one Redis. Runtime A (one slot) claims the run and its
	// model call is held; runtime B only serves HTTP for now.
	db := newDatabase(t)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	gate := newGatedModel(t)
	poolA := db.open()
	a := startPGProcess(t, poolA, processOpts{model: gate.Model, rdb: rdb, runtimeID: "runtime-a",
		env: map[string]string{"RUNTIME_CONCURRENCY": "1"}})
	b := startPGProcess(t, db.open(), processOpts{model: scriptedModel(), rdb: rdb, runtimeID: "runtime-b", idle: true})
	thread := b.newThread("alice")
	run := b.send("alice", thread, "Take your time")
	gate.waitCalled(t)
	ctx, cancel := context.WithTimeout(context.Background(), failAfter)
	defer cancel()
	claimed, err := b.H.Store.GetRun(ctx, run)
	if err != nil || claimed.Status != "running" || claimed.RuntimeID != "runtime-a" {
		t.Fatalf("run before the crash = %+v, %v", claimed, err)
	}

	// Act: A stops responding (its run is simply never finished). Its database heartbeat gets old, its Redis
	// heartbeat key expires, and B's sweeper runs once.
	if _, err := poolA.Exec(ctx, `UPDATE work_runs SET heartbeat_at = now() - interval '10 minutes' WHERE id = $1`, run); err != nil {
		t.Fatal(err)
	}
	mr.FastForward(2 * time.Minute)
	b.H.Runtime.Sweep(ctx)

	// Assert: the run is interrupted and its stream ends with exactly one RUN_ERROR that says so.
	r := b.run("alice", run)
	if r.Status != "interrupted" || r.ErrorCode != "interrupted" {
		t.Fatalf("run after the sweep = %+v", r)
	}
	interrupted := b.stream("alice", run)
	last := interrupted[len(interrupted)-1]
	if last.Type() != "RUN_ERROR" || terminals(interrupted) != 1 || !strings.Contains(last.Data, `"code":"interrupted"`) || interrupted[0].Type() != "RUN_STARTED" {
		t.Fatalf("events after the sweep = %v, last %s", types(interrupted), last.Data)
	}
	if th := b.thread("alice", thread); th.ActiveRun != nil {
		t.Fatalf("the thread is stuck on %+v", th.ActiveRun)
	}

	// Act: B starts consuming and the thread gets a new message.
	b.start()
	next := b.send("alice", thread, "Are you there?")
	nextEvents := b.finishedRun("alice", next)

	// Assert: B executed the new run to completion.
	owner, err := b.H.Store.GetRun(ctx, next)
	if err != nil || owner.Status != "completed" || owner.RuntimeID != "runtime-b" {
		t.Fatalf("new run = %+v, %v (events %v)", owner, err, types(nextEvents))
	}

	// Act: A's model call is finally released; A tries to finish the run it lost, and stops (stop waits for it).
	gate.open()
	a.stop()

	// Assert: A changed nothing: same status, same code, and the stream is still the one B wrote, with one terminal event.
	after := b.run("alice", run)
	if after.Status != "interrupted" || after.ErrorCode != "interrupted" || after.Error != r.Error {
		t.Fatalf("run after A's late finish = %+v, was %+v", after, r)
	}
	replay := b.stream("alice", run)
	if terminals(replay) != 1 || !sameEvents(replay, interrupted) {
		t.Fatalf("events after A's late finish = %v, were %v", types(replay), types(interrupted))
	}
}
