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
