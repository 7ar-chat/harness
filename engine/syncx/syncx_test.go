package syncx_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/engine/syncx"
)

func TestKeyedMutexSerializesPerKeyAndForgetsReleasedKeys(t *testing.T) {
	// Work on one key is serialized and a key's entry disappears once nobody holds or waits for it.
	// Arrange
	var k syncx.KeyedMutex
	var wg sync.WaitGroup
	counter := 0

	// Act
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			unlock := k.Lock("thread:" + string(rune('a'+i%5)))
			defer unlock()
			if i%5 == 0 {
				counter++
			}
		}(i)
	}
	wg.Wait()

	// Assert
	if counter != 10 || k.Len() != 0 {
		t.Fatalf("counter %d entries left %d", counter, k.Len())
	}
}

func TestLRUKeepsTheMostRecentlyUsedEntries(t *testing.T) {
	// Beyond its size the cache evicts the least recently used entry.
	// Arrange
	c := syncx.LRU[string, int]{Size: 2}
	c.Add("a", 1)
	c.Add("b", 2)
	_, _ = c.Get("a")

	// Act
	c.Add("c", 3)

	// Assert
	_, hasA := c.Get("a")
	_, hasB := c.Get("b")
	if v, ok := c.Get("c"); !hasA || hasB || !ok || v != 3 || c.Len() != 2 {
		t.Fatalf("a %v b %v c %v/%d len %d", hasA, hasB, ok, v, c.Len())
	}
}

func TestKeyedMutexWaitHonoursTheContextAndTheWait(t *testing.T) {
	// A waiter gives up when its context ends or its wait elapses, leaving no entry behind; the holder keeps the lock (C-23).
	// Arrange
	var k syncx.KeyedMutex
	release := k.Lock("t")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Act
	start := time.Now()
	_, ctxErr := k.LockWait(ctx, "t", 0)
	_, waitErr := k.LockWait(context.Background(), "t", 50*time.Millisecond)
	elapsed := time.Since(start)
	release()
	again, againErr := k.LockWait(context.Background(), "t", 50*time.Millisecond)

	// Assert
	if !errors.Is(ctxErr, context.DeadlineExceeded) || !errors.Is(waitErr, syncx.ErrLockTimeout) || elapsed > time.Second {
		t.Fatalf("ctx %v wait %v after %v", ctxErr, waitErr, elapsed)
	}
	if againErr != nil {
		t.Fatalf("free lock not taken: %v", againErr)
	}
	again()
	if k.Len() != 0 {
		t.Fatalf("entries left %d", k.Len())
	}
}
