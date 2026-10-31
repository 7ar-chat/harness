package redisx_test

import (
	"context"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/redisx"
)

func recv(t *testing.T, ch <-chan []byte) []byte {
	t.Helper()
	select {
	case p, ok := <-ch:
		if !ok {
			t.Fatal("subscription closed")
		}
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no payload received")
		return nil
	}
}

func TestFeedBusDeliversPayloadsToEverySubscriberInOrder(t *testing.T) {
	// Replicas subscribed through the same Redis each receive every payload published after they subscribed, in order; earlier payloads are not replayed.
	// Arrange
	_, rdb := newRedis(t)
	bus := redisx.NewFeedBus(rdb)
	_ = bus.Publish(bg, []byte("before"))
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	a, errA := bus.Subscribe(ctx)
	b, errB := bus.Subscribe(ctx)

	// Act
	_ = bus.Publish(bg, []byte("one"))
	_ = bus.Publish(bg, []byte("two"))

	// Assert
	if errA != nil || errB != nil {
		t.Fatalf("subscribe: %v %v", errA, errB)
	}
	for name, ch := range map[string]<-chan []byte{"a": a, "b": b} {
		if first, second := recv(t, ch), recv(t, ch); string(first) != "one" || string(second) != "two" {
			t.Errorf("%s got %q then %q", name, first, second)
		}
	}
}

func TestFeedBusSubscriptionEndsWithItsContext(t *testing.T) {
	// Cancelling the subscriber's context closes its channel and releases the Redis subscription.
	// Arrange
	mr, rdb := newRedis(t)
	bus := redisx.NewFeedBus(rdb)
	ctx, cancel := context.WithCancel(bg)
	ch, err := bus.Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Act
	cancel()

	// Assert
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("unexpected payload")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
	deadline := time.Now().Add(5 * time.Second)
	for mr.PubSubNumSub("work:feed")["work:feed"] != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Redis subscription not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFeedBusSlowSubscriberMissesEventsInsteadOfBlockingTheFeed(t *testing.T) {
	// A subscriber that does not read loses events beyond its buffer (best-effort feed) while another subscriber keeps receiving everything.
	// Arrange
	_, rdb := newRedis(t)
	bus := redisx.NewFeedBus(rdb)
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	slow, _ := bus.Subscribe(ctx)
	fast, _ := bus.Subscribe(ctx)
	const total = 600
	got := make(chan int, 1)
	go func() {
		n := 0
		for p := range fast {
			n++
			if string(p) == "last" {
				break
			}
		}
		got <- n
	}()

	// Act
	for i := 0; i < total-1; i++ {
		_ = bus.Publish(bg, []byte("x"))
	}
	_ = bus.Publish(bg, []byte("last"))

	// Assert
	select {
	case n := <-got:
		if n != total {
			t.Fatalf("the reading subscriber got %d of %d", n, total)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reading subscriber stalled behind the slow one")
	}
	if buffered := len(slow); buffered > 256 {
		t.Fatalf("slow subscriber buffered %d, above its 256 cap", buffered)
	}
}

func TestFeedBusClaimIsGrantedToOneCallerUntilItExpires(t *testing.T) {
	// A claim key is won by exactly one caller; others lose until its ttl passes, and claims on other keys are independent.
	// Arrange
	mr, rdb := newRedis(t)
	bus := redisx.NewFeedBus(rdb)

	// Act
	first, err := bus.Claim(bg, "digest:42", time.Minute)
	second, _ := bus.Claim(bg, "digest:42", time.Minute)
	other, _ := bus.Claim(bg, "digest:43", time.Minute)
	mr.FastForward(2 * time.Minute)
	afterExpiry, _ := bus.Claim(bg, "digest:42", time.Minute)

	// Assert
	if err != nil || !first || second || !other || !afterExpiry {
		t.Fatalf("first %v second %v other %v after expiry %v (%v)", first, second, other, afterExpiry, err)
	}
}

func TestFeedBusReportsAnUnavailableRedis(t *testing.T) {
	// Publish, Subscribe and Claim return the error when Redis is down; a Claim never reads as "won".
	// Arrange
	mr, rdb := newRedis(t)
	bus := redisx.NewFeedBus(rdb)
	mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	pubErr := bus.Publish(bg, []byte("x"))
	ch, subErr := bus.Subscribe(bg)
	won, claimErr := bus.Claim(bg, "k", time.Minute)

	// Assert
	if pubErr == nil || subErr == nil || ch != nil || claimErr == nil || won {
		t.Fatalf("publish %v subscribe %v/%v claim %v/%v", pubErr, ch, subErr, won, claimErr)
	}
}
