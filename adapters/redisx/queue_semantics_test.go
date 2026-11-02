package redisx_test

import (
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/engine/queue"
)

func pendingCount(t *testing.T, rdb redis.UniversalClient) int64 {
	t.Helper()
	p, err := rdb.XPending(bg, queue.Stream, queue.Group).Result()
	if err != nil {
		t.Fatal(err)
	}
	return p.Count
}

func TestReadDeliversEnqueuedRunsOnceAndAckClearsThem(t *testing.T) {
	// A run enqueued before the group exists is still delivered (the group reads from the start); it stays pending until acked and is never delivered twice.
	// Arrange
	_, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	_ = q.Enqueue(bg, "run-1")
	_ = q.Enqueue(bg, "run-2")
	if err := q.EnsureGroup(bg); err != nil {
		t.Fatal(err)
	}
	if err := q.EnsureGroup(bg); err != nil {
		t.Fatalf("EnsureGroup must be idempotent: %v", err)
	}

	// Act
	first, err := q.Read(bg, "rt-a", 1, 10*time.Millisecond)
	second, _ := q.Read(bg, "rt-b", 10, 10*time.Millisecond)
	empty, emptyErr := q.Read(bg, "rt-a", 10, 10*time.Millisecond)
	pendingBefore := pendingCount(t, rdb)
	ackErr := q.Ack(bg, first[0].StreamID)
	_ = q.Ack(bg, second[0].StreamID)

	// Assert
	if err != nil || len(first) != 1 || first[0].RunID != "run-1" || len(second) != 1 || second[0].RunID != "run-2" {
		t.Fatalf("first %v (%v) second %v", first, err, second)
	}
	if len(empty) != 0 || emptyErr != nil {
		t.Fatalf("an empty queue must read as nothing, not an error: %v %v", empty, emptyErr)
	}
	if pendingBefore != 2 || ackErr != nil || pendingCount(t, rdb) != 0 {
		t.Fatalf("pending before ack %d, after %d (ack %v)", pendingBefore, pendingCount(t, rdb), ackErr)
	}
}

func TestEnsureGroupFailsWhenTheStreamKeyHoldsSomethingElse(t *testing.T) {
	// Only an existing group is tolerated: a real Redis error (wrong type at the queue key) is returned.
	// Arrange
	_, rdb := newRedis(t)
	rdb.Set(bg, queue.Stream, "not a stream", 0)

	// Act
	err := redisx.NewQueue(rdb).EnsureGroup(bg)

	// Assert
	if err == nil {
		t.Fatal("expected the WRONGTYPE error")
	}
}

func TestReclaimMovesOnlyEntriesIdleLongEnoughToTheCaller(t *testing.T) {
	// Entries a stopped runtime read but never acked move to the caller once idle for minIdle; fresher ones stay with their consumer.
	// Arrange
	mr, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	_ = q.EnsureGroup(bg)
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	mr.SetTime(t0)
	_ = q.Enqueue(bg, "stale")
	stale, _ := q.Read(bg, "dead", 1, 10*time.Millisecond)
	mr.SetTime(t0.Add(50 * time.Second))
	_ = q.Enqueue(bg, "fresh")
	fresh, _ := q.Read(bg, "dead", 1, 10*time.Millisecond)
	mr.SetTime(t0.Add(time.Minute))

	// Act
	reclaimed, err := q.Reclaim(bg, "rt", 30*time.Second)
	again, _ := q.Reclaim(bg, "rt", 30*time.Second)

	// Assert
	if err != nil || len(stale) != 1 || len(fresh) != 1 {
		t.Fatalf("setup: %v %v %v", stale, fresh, err)
	}
	if len(reclaimed) != 1 || reclaimed[0].RunID != "stale" || reclaimed[0].StreamID != stale[0].StreamID {
		t.Fatalf("reclaimed %v", reclaimed)
	}
	if len(again) != 0 {
		t.Fatalf("an entry just claimed is not idle: %v", again)
	}
	if err := q.Ack(bg, reclaimed[0].StreamID); err != nil || pendingCount(t, rdb) != 1 {
		t.Fatalf("acking the reclaimed entry must leave only the fresh one pending (%v)", err)
	}
}

func TestReclaimPagesThroughManyPendingEntries(t *testing.T) {
	// More pending entries than one XAUTOCLAIM page (100) are all reclaimed in a single call.
	// Arrange
	mr, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	_ = q.EnsureGroup(bg)
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	mr.SetTime(t0)
	for i := 0; i < 250; i++ {
		_ = q.Enqueue(bg, fmt.Sprintf("run-%03d", i))
	}
	read, _ := q.Read(bg, "dead", 250, 10*time.Millisecond)
	mr.SetTime(t0.Add(time.Hour))

	// Act
	reclaimed, err := q.Reclaim(bg, "rt", time.Minute)

	// Assert
	ids := map[string]bool{}
	for _, m := range reclaimed {
		ids[m.RunID] = true
	}
	if err != nil || len(read) != 250 || len(reclaimed) != 250 || len(ids) != 250 {
		t.Fatalf("read %d reclaimed %d distinct %d err %v", len(read), len(reclaimed), len(ids), err)
	}
}

func TestReclaimWithoutAGroupOrStreamIsEmpty(t *testing.T) {
	// Before any runtime created the group there is nothing to reclaim, and that is not an error.
	// Arrange
	_, rdb := newRedis(t)

	// Act
	got, err := redisx.NewQueue(rdb).Reclaim(bg, "rt", time.Second)

	// Assert
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v err %v", got, err)
	}
}

// touch records consumer activity at the mini-redis clock (XCLAIM of an unknown id refreshes the consumer's idle time).
func touch(t *testing.T, rdb redis.UniversalClient, consumer string) {
	t.Helper()
	if err := rdb.XClaim(bg, &redis.XClaimArgs{Stream: queue.Stream, Group: queue.Group, Consumer: consumer, Messages: []string{"999999-0"}}).Err(); err != nil {
		t.Fatal(err)
	}
}

func consumerNames(t *testing.T, rdb redis.UniversalClient) string {
	t.Helper()
	infos, err := rdb.XInfoConsumers(bg, queue.Stream, queue.Group).Result()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range infos {
		names = append(names, c.Name)
	}
	sort.Strings(names)
	return fmt.Sprint(names)
}

func TestPruneConsumersRemovesOnlyIdleConsumersWithoutPendingWork(t *testing.T) {
	// Stopped runtimes' consumers are deleted once idle with nothing pending; the caller's own consumer, ones with pending entries and recently active ones stay.
	// Arrange
	mr, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	_ = q.EnsureGroup(bg)
	t0 := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	mr.SetTime(t0)
	_ = q.Enqueue(bg, "a")
	busy, _ := q.Read(bg, "busy", 1, 10*time.Millisecond)
	for _, consumer := range []string{"stopped", "busy", "me"} {
		touch(t, rdb, consumer)
	}
	mr.SetTime(t0.Add(10 * time.Minute))
	touch(t, rdb, "recent")

	// Act
	n, err := q.PruneConsumers(bg, "me", 5*time.Minute)

	// Assert
	if len(busy) != 1 || err != nil || n != 1 || consumerNames(t, rdb) != "[busy me recent]" {
		t.Fatalf("pruned %d (%v), consumers left %s", n, err, consumerNames(t, rdb))
	}
}

func TestPruneConsumersTreatsAnUnknownIdleTimeAsIdle(t *testing.T) {
	// Some Redis-compatible servers report no idle time: a consumer without pending entries is then still pruned, one with pending entries never.
	// Arrange
	_, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	_ = q.EnsureGroup(bg)
	_ = q.Enqueue(bg, "a")
	_, _ = q.Read(bg, "busy", 1, 10*time.Millisecond)
	_ = q.Enqueue(bg, "b")
	got, _ := q.Read(bg, "done", 1, 10*time.Millisecond)
	_ = q.Ack(bg, got[0].StreamID)

	// Act
	n, err := q.PruneConsumers(bg, "me", time.Hour)

	// Assert
	if err != nil || n != 1 || consumerNames(t, rdb) != "[busy]" {
		t.Fatalf("pruned %d (%v), consumers left %s", n, err, consumerNames(t, rdb))
	}
}

func TestPruneConsumersWithoutAStreamPrunesNothing(t *testing.T) {
	// A missing stream or group (fresh Redis) is not an error for the housekeeping call.
	// Arrange
	_, rdb := newRedis(t)

	// Act
	n, err := redisx.NewQueue(rdb).PruneConsumers(bg, "me", time.Minute)

	// Assert
	if n != 0 || err != nil {
		t.Fatalf("pruned %d err %v", n, err)
	}
}

func TestCancelReachesOnlySubscribersOfTheRunUntilTheyUnsubscribe(t *testing.T) {
	// A published cancel calls the callback of every subscriber of that run, not of other runs, and not after unsubscribing.
	// Arrange
	_, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	got := make(chan string, 8)
	unsubA, err := q.SubscribeCancel(bg, "a", func() { got <- "a" })
	if err != nil {
		t.Fatal(err)
	}
	unsubOther, _ := q.SubscribeCancel(bg, "b", func() { got <- "b" })
	defer unsubOther()

	// Act
	_ = q.PublishCancel(bg, "a")
	first := <-got
	unsubA()
	_ = q.PublishCancel(bg, "a")
	_ = q.PublishCancel(bg, "b")
	second := <-got

	// Assert
	if first != "a" || second != "b" {
		t.Fatalf("callbacks %q then %q", first, second)
	}
}

func TestSteersKeepTheirOrderAndAreCappedPerRun(t *testing.T) {
	// A run holds at most MaxSteers pending steers (the next is refused); DrainSteers returns them in order, empties the list and removes the run from the index.
	// Arrange
	mr, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	for i := 0; i < queue.MaxSteers; i++ {
		if err := q.PushSteer(bg, "r", queue.Steer{ID: fmt.Sprint(i), Text: "t", Author: "alice"}); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	overflow := q.PushSteer(bg, "r", queue.Steer{ID: "x", Text: "t"})
	runs, _ := q.SteerRuns(bg)
	drained, err := q.DrainSteers(bg, "r")
	runsAfter, _ := q.SteerRuns(bg)
	empty, emptyErr := q.DrainSteers(bg, "r")

	// Assert
	if !errors.Is(overflow, queue.ErrTooManySteers) || len(runs) != 1 || runs[0] != "r" {
		t.Fatalf("overflow %v runs %v", overflow, runs)
	}
	if err != nil || len(drained) != queue.MaxSteers || drained[0].ID != "0" || drained[len(drained)-1].ID != fmt.Sprint(queue.MaxSteers-1) || drained[3].Author != "alice" {
		t.Fatalf("drained %v (%v)", drained, err)
	}
	if len(runsAfter) != 0 || emptyErr != nil || len(empty) != 0 || mr.Exists("work:run:r:steers") {
		t.Fatalf("after drain: runs %v, empty %v %v", runsAfter, empty, emptyErr)
	}
}

func TestPendingSteersExpireWithTheirList(t *testing.T) {
	// The steer list of a run that never finishes does not live forever: it expires after a day.
	// Arrange
	mr, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)

	// Act
	_ = q.PushSteer(bg, "r", queue.Steer{ID: "1", Text: "t"})

	// Assert
	if ttl := mr.TTL("work:run:r:steers"); ttl != 24*time.Hour {
		t.Fatalf("ttl %v", ttl)
	}
}

func TestFinalizeStopsAcceptingSteersAndReturnsThePendingOnes(t *testing.T) {
	// FinalizeSteers returns and removes the pending steers and refuses later ones; ReopenSteers accepts again.
	// Arrange
	_, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	_ = q.PushSteer(bg, "r", queue.Steer{ID: "1", Text: "one"})
	_ = q.PushSteer(bg, "r", queue.Steer{ID: "2", Text: "two"})

	// Act
	left, err := q.FinalizeSteers(bg, "r")
	refused := q.PushSteer(bg, "r", queue.Steer{ID: "3", Text: "late"})
	runs, _ := q.SteerRuns(bg)
	_ = q.ReopenSteers(bg, "r")
	reopened := q.PushSteer(bg, "r", queue.Steer{ID: "4", Text: "again"})

	// Assert
	if err != nil || len(left) != 2 || left[0].ID != "1" || left[1].ID != "2" {
		t.Fatalf("left %v (%v)", left, err)
	}
	if !errors.Is(refused, queue.ErrSteersClosed) || len(runs) != 0 || reopened != nil {
		t.Fatalf("refused %v, runs %v, reopened %v", refused, runs, reopened)
	}
}

func TestCloseKeepsSteersListedUntilTheyAreAcknowledged(t *testing.T) {
	// CloseSteers refuses further steers but leaves the pending ones (and the run in the index) until AckSteers removes them, a prefix at a time.
	// Arrange
	_, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	for _, id := range []string{"1", "2", "3"} {
		_ = q.PushSteer(bg, "r", queue.Steer{ID: id, Text: "t"})
	}

	// Act
	pending, err := q.CloseSteers(bg, "r")
	refused := q.PushSteer(bg, "r", queue.Steer{ID: "4", Text: "t"})
	samePending, _ := q.CloseSteers(bg, "r")
	_ = q.AckSteers(bg, "r", 2)
	rest, _ := q.CloseSteers(bg, "r")
	runsMid, _ := q.SteerRuns(bg)
	ackErr := q.AckSteers(bg, "r", 10)
	runsAfter, _ := q.SteerRuns(bg)

	// Assert
	if err != nil || len(pending) != 3 || len(samePending) != 3 || !errors.Is(refused, queue.ErrSteersClosed) {
		t.Fatalf("pending %v same %v refused %v (%v)", pending, samePending, refused, err)
	}
	if len(rest) != 1 || rest[0].ID != "3" || len(runsMid) != 1 {
		t.Fatalf("rest %v runs %v", rest, runsMid)
	}
	if ackErr != nil || len(runsAfter) != 0 {
		t.Fatalf("a fully acknowledged run leaves the index: %v %v", ackErr, runsAfter)
	}
}

func TestMalformedStoredSteersAreSkippedNotFatal(t *testing.T) {
	// A corrupt or textless entry in the steer list is dropped; valid steers around it are still delivered.
	// Arrange
	_, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	rdb.RPush(bg, "work:run:r:steers", `{"id":"1","text":"good"}`, `not json`, `{"id":"2","text":""}`, `{"id":"3","text":"also good"}`)

	// Act
	got, err := q.DrainSteers(bg, "r")

	// Assert
	if err != nil || len(got) != 2 || got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestEveryOperationReportsAnUnavailableRedis(t *testing.T) {
	// With Redis gone, no operation pretends to succeed: each returns the error (the callers retry or keep the work pending).
	// Arrange
	mr, rdb := newRedis(t)
	q := redisx.NewQueue(rdb)
	mr.SetError("ERR LOADING Redis is loading the dataset in memory")
	ops := map[string]func() error{
		"Enqueue":         func() error { return q.Enqueue(bg, "r") },
		"EnsureGroup":     func() error { return q.EnsureGroup(bg) },
		"Read":            func() error { _, err := q.Read(bg, "c", 1, time.Millisecond); return err },
		"Reclaim":         func() error { _, err := q.Reclaim(bg, "c", time.Second); return err },
		"PruneConsumers":  func() error { _, err := q.PruneConsumers(bg, "c", time.Second); return err },
		"Ack":             func() error { return q.Ack(bg, "1-1") },
		"PublishCancel":   func() error { return q.PublishCancel(bg, "r") },
		"SubscribeCancel": func() error { _, err := q.SubscribeCancel(bg, "r", func() {}); return err },
		"PushSteer":       func() error { return q.PushSteer(bg, "r", queue.Steer{ID: "1", Text: "t"}) },
		"DrainSteers":     func() error { _, err := q.DrainSteers(bg, "r"); return err },
		"FinalizeSteers":  func() error { _, err := q.FinalizeSteers(bg, "r"); return err },
		"CloseSteers":     func() error { _, err := q.CloseSteers(bg, "r"); return err },
		"ReopenSteers":    func() error { return q.ReopenSteers(bg, "r") },
		"AckSteers":       func() error { return q.AckSteers(bg, "r", 1) },
		"SteerRuns":       func() error { _, err := q.SteerRuns(bg); return err },
	}

	// Act / Assert
	for name, op := range ops {
		if err := op(); err == nil {
			t.Errorf("%s succeeded against a stopped Redis", name)
		}
	}
}
