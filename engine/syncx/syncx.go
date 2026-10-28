// Package syncx holds small concurrency helpers: a keyed mutex whose entries
// are freed on release and a bounded LRU cache.
package syncx

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// KeyedMutex serializes work per key; a key's entry exists only while it is
// held or waited for, so the map never grows with the number of keys seen.
type KeyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyed
}

type keyed struct {
	ch   chan struct{} // holds one token while the key is locked
	refs int
}

// ErrLockTimeout means LockWait gave up before the key was free.
var ErrLockTimeout = errors.New("timed out waiting for the lock")

// Lock takes the key's lock and returns its release func.
func (k *KeyedMutex) Lock(key string) func() {
	release, _ := k.LockWait(context.Background(), key, 0)
	return release
}

// LockWait takes the key's lock, waiting at most wait (≤ 0: no limit) and
// never past ctx: it returns ErrLockTimeout or ctx's error otherwise (C-23).
func (k *KeyedMutex) LockWait(ctx context.Context, key string, wait time.Duration) (func(), error) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyed{}
	}
	l := k.locks[key]
	if l == nil {
		l = &keyed{ch: make(chan struct{}, 1)}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()
	forget := func() {
		k.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case l.ch <- struct{}{}:
	case <-ctx.Done():
		forget()
		return nil, ctx.Err()
	case <-timeout:
		forget()
		return nil, ErrLockTimeout
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-l.ch
			forget()
		})
	}, nil
}

// Len is the number of keys currently held or waited for.
func (k *KeyedMutex) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.locks)
}

// DefaultLRUSize is the bound of an LRU whose Size is not set.
const DefaultLRUSize = 1024

// LRU is a goroutine-safe cache bounded to Size entries (least recently used
// evicted; DefaultLRUSize when Size ≤ 0). Size must not change once in use.
type LRU[K comparable, V any] struct {
	Size int

	mu    sync.Mutex
	order *list.List
	items map[K]*list.Element
}

type entry[K comparable, V any] struct {
	key K
	val V
}

// Get returns the cached value and marks it recently used.
func (c *LRU[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	if c.items == nil {
		return zero, false
	}
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*entry[K, V]).val, true
}

// Add caches a value, evicting the least recently used entries beyond Size.
func (c *LRU[K, V]) Add(key K, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items, c.order = map[K]*list.Element{}, list.New()
	}
	if el, ok := c.items[key]; ok {
		el.Value.(*entry[K, V]).val = val
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&entry[K, V]{key: key, val: val})
	size := c.Size
	if size <= 0 {
		size = DefaultLRUSize
	}
	for c.order.Len() > size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*entry[K, V]).key)
	}
}

// Len is the number of cached entries.
func (c *LRU[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.order == nil {
		return 0
	}
	return c.order.Len()
}
