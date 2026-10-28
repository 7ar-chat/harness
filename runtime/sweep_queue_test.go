package runtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/queue"
)

func TestSweepReclaimsEntriesOfDeadConsumersAndDeletesThem(t *testing.T) {
	// A queue entry a stopped runtime read but never acked is reclaimed (its run re-enqueued, the entry acked) and the dead consumer is deleted.
	// Arrange
	r := newRig(t)
	ctx := context.Background()
	_, run := r.queued("q")
	_ = r.q.EnsureGroup(ctx)
	_ = r.q.Enqueue(ctx, run.ID)
	if msgs, err := r.q.Read(ctx, "dead-rt", 1, 0); err != nil || len(msgs) != 1 {
		t.Fatalf("read %v %v", msgs, err)
	}
	r.opts.QueuedAge, r.opts.ConsumerIdle = 10*time.Millisecond, 10*time.Millisecond
	time.Sleep(30 * time.Millisecond)

	// Act
	r.runtime().Sweep(ctx)

	// Assert
	pending, _ := r.rdb.XPending(ctx, queue.Stream, queue.Group).Result()
	consumers, _ := r.rdb.XInfoConsumers(ctx, queue.Stream, queue.Group).Result()
	for _, c := range consumers {
		if c.Name == "dead-rt" {
			t.Fatalf("dead consumer kept: %+v", consumers)
		}
	}
	if pending.Count != 0 {
		t.Fatalf("pending %+v", pending)
	}
	entries, _ := r.rdb.XRange(ctx, queue.Stream, "-", "+").Result()
	if last := entries[len(entries)-1]; last.Values["runId"] != run.ID {
		t.Fatalf("run not re-enqueued: %v", entries)
	}
}
