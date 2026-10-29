package product

import (
	"encoding/json"
	"strconv"
	"sync"
	"time"
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
}

type subscriber struct {
	ch    chan FeedEvent
	allow func(FeedEvent) bool
}

// hub fans feed events out to the subscribers allowed to see them.
type hub struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
	seq  int64
}

func newHub() *hub { return &hub{subs: map[*subscriber]struct{}{}} }

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
