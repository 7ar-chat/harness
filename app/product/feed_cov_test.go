package product

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tmBus is a FeedBus whose operations can fail, to prove the hub degrades instead of breaking requests.
type tmBus struct {
	mu          sync.Mutex
	published   [][]byte
	publishErr  error
	claimErr    error
	subscribes  atomic.Int32
	subscribeFn func(ctx context.Context, n int) (<-chan []byte, error)
	gotPublish  chan struct{}
}

func (b *tmBus) Publish(_ context.Context, p []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.published = append(b.published, p)
	if b.gotPublish != nil {
		select {
		case b.gotPublish <- struct{}{}:
		default:
		}
	}
	return b.publishErr
}

func (b *tmBus) Subscribe(ctx context.Context) (<-chan []byte, error) {
	n := int(b.subscribes.Add(1))
	if b.subscribeFn != nil {
		return b.subscribeFn(ctx, n)
	}
	ch := make(chan []byte)
	return ch, nil
}

func (b *tmBus) Claim(context.Context, string, time.Duration) (bool, error) { return false, b.claimErr }

func TestTmClaimFailsOpenWhenTheBusIsDown(t *testing.T) {
	h := newHub()
	h.bus = &tmBus{claimErr: errors.New("redis down")}

	// A duplicate nudge is better than a lost one: the replica acts when it cannot ask.
	if !h.claim("stuck:r1", time.Hour) {
		t.Fatal("claim must succeed when the bus fails")
	}
	// A claim without a duration is not a lock at all.
	h.bus = &tmBus{}
	if !h.claim("k", 0) {
		t.Fatal("a zero ttl always wins")
	}
	if h.claim("k", time.Hour) {
		t.Fatal("the bus said another replica holds the key")
	}
}

func TestTmPublishKeepsDeliveringLocallyWhenTheBusFailsOrIsFull(t *testing.T) {
	// Arrange: a bus that rejects every publish
	bus := &tmBus{publishErr: errors.New("boom"), gotPublish: make(chan struct{}, 8)}
	h := newHub()
	h.attach(context.Background(), bus)
	sub, done := h.subscribe(nil)
	defer done()

	// Act
	h.publish(FeedEvent{Type: "thread.created", ThreadID: "t1"})
	h.publish(FeedEvent{Type: "thread.created", ThreadID: "t2"})

	// Assert: local subscribers got both, in order, and the failing bus was still offered both
	for _, want := range []string{"t1", "t2"} {
		select {
		case ev := <-sub.ch:
			if ev.ThreadID != want || ev.At.IsZero() {
				t.Fatalf("event = %+v, want thread %s with a timestamp", ev, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no event for %s", want)
		}
	}
	for range 2 {
		select {
		case <-bus.gotPublish:
		case <-time.After(5 * time.Second):
			t.Fatal("the bus was not offered the events")
		}
	}

	// A backed-up queue drops for the bus instead of blocking the request.
	full := newHub()
	full.bus, full.out = &tmBus{}, make(chan []byte, 1)
	full.publish(FeedEvent{Type: "a"})
	finished := make(chan struct{})
	go func() { full.publish(FeedEvent{Type: "b"}); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("publish blocked on a full bus queue")
	}
	if len(full.out) != 1 {
		t.Fatalf("queue length %d", len(full.out))
	}
}

func TestTmRelaySubscribesAgainAfterTheBusDropsAndStopsWithItsContext(t *testing.T) {
	// Arrange: the first subscription fails, the second delivers one event then closes, the third stays open
	deliver := make(chan []byte, 1)
	bus := &tmBus{}
	bus.subscribeFn = func(_ context.Context, n int) (<-chan []byte, error) {
		switch n {
		case 1:
			return nil, errors.New("not yet")
		case 2:
			ch := make(chan []byte, 1)
			ch <- []byte(`{"origin":"other","event":{"type":"thread.created","threadId":"remote-1"}}`)
			close(ch)
			return ch, nil
		}
		return deliver, nil
	}
	h := newHub()
	h.retry = time.Millisecond
	sub, done := h.subscribe(nil)
	defer done()
	ctx, cancel := context.WithCancel(context.Background())

	// Act
	h.attach(ctx, bus)

	// Assert: the event of the second subscription arrives, flagged as remote, and a third subscription is made
	select {
	case ev := <-sub.ch:
		if ev.ThreadID != "remote-1" || !ev.remote {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event after resubscribing")
	}
	deadline := time.After(5 * time.Second)
	for bus.subscribes.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("subscribed %d times", bus.subscribes.Load())
		case <-time.After(time.Millisecond):
		}
	}
	// events that are our own echo, or garbage, are ignored
	deliver <- []byte(`{"origin":"` + h.id + `","event":{"type":"thread.created","threadId":"echo"}}`)
	deliver <- []byte(`not json`)
	deliver <- []byte(`{"origin":"other","owner":"alice","affects":"bob","event":{"type":"project.changed","threadId":"routed"}}`)
	select {
	case ev := <-sub.ch:
		if ev.ThreadID != "routed" || ev.owner != "alice" || ev.affects != "bob" {
			t.Fatalf("routing fields lost across the bus: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no routed event")
	}
	cancel()
}

func TestTmSubscribersOnlyGetWhatTheyMaySeeAndSlowOnesDropEvents(t *testing.T) {
	h := newHub()
	onlyA, doneA := h.subscribe(func(ev FeedEvent) bool { return ev.Actor == "a" })
	defer doneA()
	slow, doneS := h.subscribe(nil)
	defer doneS()
	gone, doneG := h.subscribe(nil)
	doneG() // unsubscribed: gets nothing

	// Act: more events than a subscriber's buffer holds, none read
	for range cap(slow.ch) + 10 {
		h.publish(FeedEvent{Type: "message.sent", Actor: "b"})
	}
	h.publish(FeedEvent{Type: "message.sent", Actor: "a"})

	// Assert: publishing never blocked; the filtered subscriber got only its event; the full one holds a bounded backlog
	if len(onlyA.ch) != 1 || (<-onlyA.ch).Actor != "a" {
		t.Fatal("the filter did not apply")
	}
	if len(slow.ch) != cap(slow.ch) {
		t.Fatalf("slow subscriber holds %d events", len(slow.ch))
	}
	if len(gone.ch) != 0 {
		t.Fatal("an unsubscribed subscriber still receives events")
	}
	if a, b := h.nextID(), h.nextID(); a == b {
		t.Fatalf("ids repeat: %s %s", a, b)
	}
}

func TestTmFeedEventFormatsAsServerSentEvent(t *testing.T) {
	ev := FeedEvent{Type: "run.finished", ThreadID: "t", Status: "failed", owner: "secret-owner", affects: "secret-affects"}

	got := string(ev.sse("7"))

	want := "id: 7\nevent: run.finished\ndata: {\"type\":\"run.finished\",\"threadId\":\"t\",\"status\":\"failed\",\"at\":\"0001-01-01T00:00:00Z\"}\n\n"
	if got != want {
		t.Fatalf("sse = %q\nwant  %q (routing fields must never reach clients)", got, want)
	}
}

func TestTmMemBusDeliversToEverySubscriberAndStopsAtCancel(t *testing.T) {
	bus := NewMemBus()
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := bus.Subscribe(ctx)
	b, _ := bus.Subscribe(ctx)

	_ = bus.Publish(ctx, []byte("x"))

	if string(<-a) != "x" || string(<-b) != "x" {
		t.Fatal("every subscriber gets the payload, the publisher's own included")
	}
	cancel()
	if _, open := <-a; open {
		t.Fatal("the channel stays open after its context ended")
	}
	// a claim expires by itself
	if ok, _ := bus.Claim(context.Background(), "k", time.Millisecond); !ok {
		t.Fatal("first claim")
	}
	if ok, _ := bus.Claim(context.Background(), "k", time.Hour); ok {
		t.Fatal("second claim inside the ttl")
	}
	time.Sleep(5 * time.Millisecond)
	if ok, _ := bus.Claim(context.Background(), "k", time.Hour); !ok {
		t.Fatal("claim after the ttl")
	}
}
