package redisx_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/engine/queue"
)

func TestReadRecreatesALostGroupAtTheStreamEnd(t *testing.T) {
	// When the consumer group disappears (NOGROUP), it is re-created at "$": entries already handled are not delivered again.
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	q := redisx.NewQueue(rdb)
	ctx := context.Background()
	_ = q.EnsureGroup(ctx)
	for _, id := range []string{"a", "b", "c"} {
		_ = q.Enqueue(ctx, id)
	}
	old, _ := q.Read(ctx, "rt", 10, 10*time.Millisecond)
	for _, m := range old {
		_ = q.Ack(ctx, m.StreamID)
	}
	_ = rdb.XGroupDestroy(ctx, queue.Stream, queue.Group).Err()

	// Act
	_, nogroupErr := q.Read(ctx, "rt", 10, 10*time.Millisecond)
	again, err := q.Read(ctx, "rt", 10, 10*time.Millisecond)

	// Assert
	if len(old) != 3 || nogroupErr != nil || err != nil || len(again) != 0 {
		t.Fatalf("old %d, nogroup read %v, then %d entries (%v)", len(old), nogroupErr, len(again), err)
	}
}
