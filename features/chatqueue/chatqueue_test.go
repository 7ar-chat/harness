package chatqueue_test

import (
	"context"
	"sync"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/threadstate"
	"github.com/7ar-chat/harness/features/chatqueue"
)

func TestTheQueueKeepsOrderAndRefusesEmptyAndOverflow(t *testing.T) {
	ctx := context.Background()
	q := &chatqueue.Queue{St: threadstate.NewMem()}
	for _, text := range []string{"a", "b", "c"} {
		if _, err := q.Add(ctx, "t", text, chatqueue.Options{Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.Add(ctx, "t", "  ", chatqueue.Options{}); err != chatqueue.ErrEmpty {
		t.Fatalf("empty message: %v", err)
	}
	items, _ := q.List(ctx, "t")
	if len(items) != 3 || items[0].Text != "a" || items[2].Text != "c" || items[1].Options.Model != "m" {
		t.Fatalf("items = %+v", items)
	}
	head, ok, _ := q.Head(ctx, "t")
	if !ok || head.Text != "a" {
		t.Fatalf("head = %+v", head)
	}
	// a message put back goes to the front, once
	_, _, _ = q.Remove(ctx, "t", items[0].ID)
	_ = q.Prepend(ctx, "t", items[0])
	_ = q.Prepend(ctx, "t", items[0])
	items, _ = q.List(ctx, "t")
	if len(items) != 3 || items[0].Text != "a" || items[1].Text != "b" {
		t.Fatalf("after prepend = %+v", items)
	}
	for i := 0; i < chatqueue.MaxItems; i++ {
		_, _ = q.Add(ctx, "t", "x", chatqueue.Options{})
	}
	if _, err := q.Add(ctx, "t", "y", chatqueue.Options{}); err != chatqueue.ErrFull {
		t.Fatalf("overflow: %v", err)
	}
}

func TestOfSeveralCallersRemovingAMessageExactlyOneGetsIt(t *testing.T) {
	ctx := context.Background()
	q := &chatqueue.Queue{St: threadstate.NewMem()}
	it, _ := q.Add(ctx, "t", "claim me", chatqueue.Options{})
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok, _ := q.Remove(ctx, "t", it.ID); ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d callers got the message, want 1", wins)
	}
	if items, _ := q.List(ctx, "t"); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
}
