package product

import (
	"context"
	"sync"
	"time"
)

// FeedBus shares the live feed between the replicas of one deployment. Without
// one (Options.Bus nil) the feed, the watcher and the Telegram bridge live in
// the process: a run started through another replica reaches this one's feed
// only through the run's own stream. With one, every replica publishes the feed
// events it produces and delivers those of the others, so a subscriber sees
// everything its user may see whichever replica it is connected to.
//
// The payloads are opaque to the bus. Delivery is best effort and unordered
// across replicas, like the feed itself: a client refetches on reconnect.
// adapters/redisx.FeedBus is the Redis one; MemBus shares it between servers
// of one process.
type FeedBus interface {
	// Publish sends payload to every subscriber, including this replica's own.
	Publish(ctx context.Context, payload []byte) error
	// Subscribe returns the payloads published from now until ctx ends, when the channel closes.
	Subscribe(ctx context.Context) (<-chan []byte, error)
	// Claim reports whether the caller is the one replica to act on key for the next ttl
	// (a one-shot lock that expires by itself): the watcher uses it so that each
	// attention note is put to a user's assistant once, not once per replica.
	Claim(ctx context.Context, key string, ttl time.Duration) (bool, error)
}

// MemBus is a FeedBus in one process. Servers sharing one MemBus behave like
// replicas on a shared backend, which is how the tests exercise the feed.
type MemBus struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	claims map[string]time.Time
}

// NewMemBus returns an empty in-process bus.
func NewMemBus() *MemBus {
	return &MemBus{subs: map[chan []byte]struct{}{}, claims: map[string]time.Time{}}
}

var _ FeedBus = (*MemBus)(nil)

func (b *MemBus) Publish(_ context.Context, payload []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- payload:
		default:
		}
	}
	return nil
}

func (b *MemBus) Subscribe(ctx context.Context) (<-chan []byte, error) {
	ch := make(chan []byte, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		close(ch)
		b.mu.Unlock()
	}()
	return ch, nil
}

func (b *MemBus) Claim(_ context.Context, key string, ttl time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if until, ok := b.claims[key]; ok && now.Before(until) {
		return false, nil
	}
	b.claims[key] = now.Add(ttl)
	return true, nil
}
