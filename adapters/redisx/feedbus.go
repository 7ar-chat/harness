package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// feedChannel is the pub/sub channel of the team product's live feed.
const feedChannel = "work:feed"

// FeedBus shares the live status feed (and one-shot claims) between the web
// replicas through Redis: the product's FeedBus. Pub/sub is enough because the
// feed is best effort (clients refetch on reconnect); go-redis resubscribes by
// itself when the connection drops.
type FeedBus struct{ rdb redis.UniversalClient }

// NewFeedBus returns the Redis FeedBus.
func NewFeedBus(rdb redis.UniversalClient) *FeedBus { return &FeedBus{rdb: rdb} }

// Publish sends payload to every subscribed replica.
func (b *FeedBus) Publish(ctx context.Context, payload []byte) error {
	return b.rdb.Publish(ctx, feedChannel, payload).Err()
}

// Subscribe returns the payloads published from now until ctx ends. It returns
// once the subscription is confirmed, so an event published afterwards is not missed.
func (b *FeedBus) Subscribe(ctx context.Context) (<-chan []byte, error) {
	sub := b.rdb.Subscribe(ctx, feedChannel)
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, err
	}
	out := make(chan []byte, 256)
	go func() {
		defer close(out)
		defer sub.Close()
		in := sub.Channel()
		for {
			select {
			case m, ok := <-in:
				if !ok {
					return
				}
				select {
				case out <- []byte(m.Payload):
				default: // a slow consumer misses events, like any feed subscriber
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Claim is SET NX with an expiry: true for the one caller that sets key.
func (b *FeedBus) Claim(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return b.rdb.SetNX(ctx, "work:feed:claim:"+key, "1", ttl).Result()
}
