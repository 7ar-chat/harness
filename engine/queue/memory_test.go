package queue_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/7ar-chat/harness/engine/queue"
)

func TestMemoryQueueDeliversRunsInOrder(t *testing.T) {
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	_ = q.Enqueue(ctx, "a")
	_ = q.Enqueue(ctx, "b")

	// Act
	got, err := q.Read(ctx, "c", 1, time.Second)
	rest, _ := q.Read(ctx, "c", 5, time.Second)
	empty, _ := q.Read(ctx, "c", 5, 10*time.Millisecond)

	// Assert
	if err != nil || len(got) != 1 || got[0].RunID != "a" || len(rest) != 1 || rest[0].RunID != "b" || len(empty) != 0 {
		t.Fatalf("got %v %v %v", got, rest, empty)
	}
}

func TestMemoryBusSteersAndCancel(t *testing.T) {
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	cancelled := make(chan struct{}, 1)
	unsub, _ := q.SubscribeCancel(ctx, "r", func() { cancelled <- struct{}{} })

	// Act
	_ = q.PublishCancel(ctx, "r")
	_ = q.PushSteer(ctx, "r", queue.Steer{ID: "1", Text: "hi"})
	left, _ := q.FinalizeSteers(ctx, "r")
	closedErr := q.PushSteer(ctx, "r", queue.Steer{ID: "2", Text: "late"})
	unsub()

	// Assert
	select {
	case <-cancelled:
	default:
		t.Fatal("cancel not delivered")
	}
	if len(left) != 1 || left[0].Text != "hi" || !errors.Is(closedErr, queue.ErrSteersClosed) {
		t.Fatalf("left %v err %v", left, closedErr)
	}
}

func TestMemoryQueueReadHonoursTheContext(t *testing.T) {
	// A reader blocked on an empty queue returns the context's error as soon as it ends, not after the block.
	// Arrange
	q := queue.NewMemory()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	got, err := q.Read(ctx, "c", 1, time.Minute)

	// Assert
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("got %v err %v", got, err)
	}
}

func TestMemoryQueueWakesEveryBlockedReaderForQueuedRuns(t *testing.T) {
	// Two readers asking for one run each both get one when two runs are queued: leftover runs wake the next reader instead of waiting for another enqueue.
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	results := make(chan []queue.Message, 2)
	for range 2 {
		go func() {
			got, _ := q.Read(ctx, "c", 1, 10*time.Second)
			results <- got
		}()
	}

	// Act
	_ = q.Enqueue(ctx, "a")
	_ = q.Enqueue(ctx, "b")
	first, second := <-results, <-results

	// Assert
	if len(first) != 1 || len(second) != 1 || first[0].RunID == second[0].RunID {
		t.Fatalf("readers got %v and %v", first, second)
	}
	if first[0].StreamID == second[0].StreamID {
		t.Fatalf("stream ids must be unique: %v %v", first, second)
	}
}

func TestMemoryQueueDeliversAtMostOnce(t *testing.T) {
	// A run handed out by Read is gone: it is not reclaimable, there is no consumer to prune, and acking it is harmless.
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	_ = q.EnsureGroup(ctx)
	_ = q.Enqueue(ctx, "a")
	got, _ := q.Read(ctx, "c", 1, time.Second)

	// Act
	ackErr := q.Ack(ctx, got[0].StreamID)
	reclaimed, reclaimErr := q.Reclaim(ctx, "other", 0)
	pruned, pruneErr := q.PruneConsumers(ctx, "other", 0)
	again, _ := q.Read(ctx, "other", 1, 10*time.Millisecond)

	// Assert
	if ackErr != nil || reclaimErr != nil || pruneErr != nil || len(reclaimed) != 0 || pruned != 0 || len(again) != 0 {
		t.Fatalf("ack %v reclaim %v %v prune %d %v again %v", ackErr, reclaimed, reclaimErr, pruned, pruneErr, again)
	}
}

func TestMemoryBusCancelReachesOnlyActiveSubscribersOfTheRun(t *testing.T) {
	// A cancel reaches every subscriber of its run, not other runs' and not ones that unsubscribed.
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	var one, two, other, gone int
	_, _ = q.SubscribeCancel(ctx, "r", func() { one++ })
	_, _ = q.SubscribeCancel(ctx, "r", func() { two++ })
	_, _ = q.SubscribeCancel(ctx, "other", func() { other++ })
	unsub, _ := q.SubscribeCancel(ctx, "r", func() { gone++ })
	unsub()

	// Act
	err := q.PublishCancel(ctx, "r")
	noOne := q.PublishCancel(ctx, "nobody")

	// Assert
	if err != nil || noOne != nil || one != 1 || two != 1 || other != 0 || gone != 0 {
		t.Fatalf("one %d two %d other %d gone %d (%v %v)", one, two, other, gone, err, noOne)
	}
}

func TestMemoryBusCapsPendingSteersAndDrainsThem(t *testing.T) {
	// A run holds at most MaxSteers pending steers (the next is refused, nothing is lost); draining returns them in order and empties the list.
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	for i := 0; i < queue.MaxSteers; i++ {
		if err := q.PushSteer(ctx, "r", queue.Steer{ID: string(rune('a' + i)), Text: "t"}); err != nil {
			t.Fatal(err)
		}
	}

	// Act
	overflow := q.PushSteer(ctx, "r", queue.Steer{ID: "z", Text: "t"})
	runs, _ := q.SteerRuns(ctx)
	drained, _ := q.DrainSteers(ctx, "r")
	runsAfter, _ := q.SteerRuns(ctx)
	accepts := q.PushSteer(ctx, "r", queue.Steer{ID: "again", Text: "t"})

	// Assert
	if !errors.Is(overflow, queue.ErrTooManySteers) || len(runs) != 1 || runs[0] != "r" {
		t.Fatalf("overflow %v runs %v", overflow, runs)
	}
	if len(drained) != queue.MaxSteers || drained[0].ID != "a" || drained[queue.MaxSteers-1].ID != string(rune('a'+queue.MaxSteers-1)) {
		t.Fatalf("drained %v", drained)
	}
	if len(runsAfter) != 0 || accepts != nil {
		t.Fatalf("runs after drain %v, push after drain %v", runsAfter, accepts)
	}
}

func TestMemoryBusCloseKeepsSteersUntilAcknowledged(t *testing.T) {
	// CloseSteers refuses new steers but leaves the pending ones listed until AckSteers removes them (a prefix at a time); Reopen accepts steers again.
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	for _, id := range []string{"1", "2", "3"} {
		_ = q.PushSteer(ctx, "r", queue.Steer{ID: id, Text: "t"})
	}

	// Act
	pending, _ := q.CloseSteers(ctx, "r")
	refused := q.PushSteer(ctx, "r", queue.Steer{ID: "4", Text: "t"})
	_ = q.AckSteers(ctx, "r", 2)
	rest, _ := q.CloseSteers(ctx, "r")
	runs, _ := q.SteerRuns(ctx)
	_ = q.AckSteers(ctx, "r", 5)
	runsAfter, _ := q.SteerRuns(ctx)
	_ = q.ReopenSteers(ctx, "r")
	reopened := q.PushSteer(ctx, "r", queue.Steer{ID: "5", Text: "t"})

	// Assert
	if len(pending) != 3 || !errors.Is(refused, queue.ErrSteersClosed) {
		t.Fatalf("pending %v refused %v", pending, refused)
	}
	if len(rest) != 1 || rest[0].ID != "3" || len(runs) != 1 || len(runsAfter) != 0 {
		t.Fatalf("rest %v runs %v after %v", rest, runs, runsAfter)
	}
	if reopened != nil {
		t.Fatalf("reopened push: %v", reopened)
	}
}

func TestMemoryBusCloseDoesNotExposeItsInternalList(t *testing.T) {
	// The steers returned by CloseSteers are a copy: changing them does not alter what AckSteers later removes.
	// Arrange
	ctx := context.Background()
	q := queue.NewMemory()
	_ = q.PushSteer(ctx, "r", queue.Steer{ID: "1", Text: "original"})

	// Act
	pending, _ := q.CloseSteers(ctx, "r")
	pending[0].Text = "tampered"
	again, _ := q.CloseSteers(ctx, "r")

	// Assert
	if again[0].Text != "original" {
		t.Fatalf("pending list was mutated through the returned slice: %v", again)
	}
}
