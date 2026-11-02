package product

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// FeedEvent is one entry of the live status feed (GET /api/events): what
// happened to a thread or project that the subscriber may see. Clients refetch
// the thread (or the overview) when it concerns something they display.
type FeedEvent struct {
	// Type: thread.created, thread.updated, thread.deleted, run.started,
	// run.finished, message.sent, member.changed, project.changed.
	Type      string `json:"type"`
	ProjectID string `json:"projectId,omitempty"`
	ThreadID  string `json:"threadId,omitempty"`
	RunID     string `json:"runId,omitempty"`
	Actor     string `json:"actor,omitempty"`
	// Status is, for run.finished, completed, failed, cancelled, interrupted,
	// waiting_approval or waiting_input.
	Status string    `json:"status,omitempty"`
	Title  string    `json:"title,omitempty"`
	At     time.Time `json:"at"`

	// owner is the login a personal thread is stored under; not sent.
	owner string
	// affects, when set, is the one login a project.changed concerns.
	affects string
	// remote is set on events that came from another replica through the FeedBus. The live feed shows
	// them; the watcher and the Telegram bridge act on local events only, so each event is handled once.
	remote bool
}

// localOnly keeps the events this process produced.
func localOnly(ev FeedEvent) bool { return !ev.remote }

type subscriber struct {
	ch    chan FeedEvent
	allow func(FeedEvent) bool
}

// hub fans feed events out to the subscribers allowed to see them. With a
// FeedBus (see feedbus.go) it also hands the events it produces to the other
// replicas and delivers the ones they produce.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	seq  int64

	id    string        // this replica, to ignore its own events coming back from the bus
	bus   FeedBus       // nil: single process
	out   chan []byte   // events waiting to go to the bus
	retry time.Duration // wait before subscribing to the bus again
}

func newHub() *hub {
	return &hub{subs: map[*subscriber]struct{}{}, id: uuid.NewString(), retry: 5 * time.Second}
}

// attach connects the hub to a bus: its events are published there and the
// bus's events from other replicas are delivered to the local subscribers.
func (h *hub) attach(ctx context.Context, bus FeedBus) {
	h.bus = bus
	h.out = make(chan []byte, 256)
	go h.send(ctx)
	go h.relay(ctx)
}

// send publishes the queued events in order, so a slow bus never blocks a request.
func (h *hub) send(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-h.out:
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := h.bus.Publish(pctx, b); err != nil {
				slog.Warn("feed: publishing to the bus failed", "error", err)
			}
			cancel()
		}
	}
}

// relay delivers the events of the other replicas, resubscribing when the bus drops.
func (h *hub) relay(ctx context.Context) {
	for ctx.Err() == nil {
		ch, err := h.bus.Subscribe(ctx)
		if err != nil {
			slog.Warn("feed: subscribing to the bus failed", "error", err)
		} else {
			for raw := range ch {
				h.receive(raw)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(h.retry):
		}
	}
}

// wireEvent is a feed event on the bus: the unexported routing fields travel too.
type wireEvent struct {
	Origin  string    `json:"origin"`
	Owner   string    `json:"owner,omitempty"`
	Affects string    `json:"affects,omitempty"`
	Event   FeedEvent `json:"event"`
}

func (h *hub) receive(raw []byte) {
	var w wireEvent
	if err := json.Unmarshal(raw, &w); err != nil || w.Origin == h.id {
		return
	}
	w.Event.owner, w.Event.affects, w.Event.remote = w.Owner, w.Affects, true
	h.deliver(w.Event)
}

// claim reports whether this replica wins key for ttl (always, without a bus).
func (h *hub) claim(key string, ttl time.Duration) bool {
	if h.bus == nil || ttl <= 0 {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := h.bus.Claim(ctx, key, ttl)
	if err != nil {
		slog.Warn("feed: claiming on the bus failed", "key", key, "error", err)
		return true // a duplicate nudge is better than a lost one
	}
	return ok
}

func (h *hub) subscribe(allow func(FeedEvent) bool) (*subscriber, func()) {
	s := &subscriber{ch: make(chan FeedEvent, 128), allow: allow}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s, func() {
		h.mu.Lock()
		delete(h.subs, s)
		h.mu.Unlock()
	}
}

// publish never blocks: a subscriber that cannot keep up misses events (its
// client refetches on reconnect).
func (h *hub) publish(ev FeedEvent) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	h.deliver(ev)
	if h.bus == nil {
		return
	}
	b, err := json.Marshal(wireEvent{Origin: h.id, Owner: ev.owner, Affects: ev.affects, Event: ev})
	if err != nil {
		return
	}
	select {
	case h.out <- b:
	default: // the bus cannot keep up: other replicas miss it, like a slow subscriber
	}
}

// deliver hands ev to the local subscribers allowed to see it.
func (h *hub) deliver(ev FeedEvent) {
	h.mu.Lock()
	subs := make([]*subscriber, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		if s.allow != nil && !s.allow(ev) {
			continue
		}
		select {
		case s.ch <- ev:
		default:
		}
	}
}

func (h *hub) nextID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	return strconv.FormatInt(h.seq, 10)
}

func (ev FeedEvent) sse(id string) []byte {
	b, _ := json.Marshal(ev)
	return []byte("id: " + id + "\nevent: " + ev.Type + "\ndata: " + string(b) + "\n\n")
}
