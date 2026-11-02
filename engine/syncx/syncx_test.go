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

func TestLRUAddingAnExistingKeyReplacesItsValueAndRefreshesItsRecency(t *testing.T) {
	// Re-adding a key overwrites the value in place (no second entry) and makes it the most recently used, so the next eviction hits another key.
	// Arrange
	c := syncx.LRU[string, int]{Size: 2}
	c.Add("a", 1)
	c.Add("b", 2)

	// Act
	c.Add("a", 10)
	c.Add("c", 3)

	// Assert
	a, hasA := c.Get("a")
	_, hasB := c.Get("b")
	if !hasA || a != 10 || hasB || c.Len() != 2 {
		t.Fatalf("a %d/%v b %v len %d", a, hasA, hasB, c.Len())
	}
}

func TestLRUEmptyCacheMissesAndHoldsNothing(t *testing.T) {
	// A cache that was never written to misses every key and reports no entries.
	// Arrange
	var c syncx.LRU[string, int]

	// Act
	v, ok := c.Get("a")

	// Assert
	if ok || v != 0 || c.Len() != 0 {
		t.Fatalf("value %d found %v len %d", v, ok, c.Len())
	}
}

func TestLRUWithoutASizeIsBoundedByTheDefault(t *testing.T) {
	// An LRU whose Size is unset still evicts: it never grows beyond DefaultLRUSize, and the oldest key goes first.
	// Arrange
	var c syncx.LRU[int, int]

	// Act
	for i := 0; i <= syncx.DefaultLRUSize; i++ {
		c.Add(i, i)
	}

	// Assert
	_, hasOldest := c.Get(0)
	_, hasNewest := c.Get(syncx.DefaultLRUSize)
	if c.Len() != syncx.DefaultLRUSize || hasOldest || !hasNewest {
		t.Fatalf("len %d oldest %v newest %v", c.Len(), hasOldest, hasNewest)
	}
}

func TestKeyedMutexReleaseIsIdempotent(t *testing.T) {
	// Calling a release func twice must not unlock a lock someone else has since taken.
	// Arrange
	var k syncx.KeyedMutex
	release := k.Lock("t")
	release()
	second := k.Lock("t")

	// Act
	release()
	_, err := k.LockWait(context.Background(), "t", 20*time.Millisecond)

	// Assert
	if !errors.Is(err, syncx.ErrLockTimeout) {
		t.Fatalf("stale release freed the new holder's lock: %v", err)
	}
	second()
	if k.Len() != 0 {
		t.Fatalf("entries left %d", k.Len())
	}
}
