package queue

import (
	"context"
	"strconv"
	"sync"
	"time"
)

// Memory is an in-process run queue and control bus: it implements Queue, the
// runtime's RunQueue and Bus without Redis, for a host that runs the agent in
// its own process (TUI, desktop app, tests).
type Memory struct {
	mu      sync.Mutex
	pending []Message
	next    int
	notify  chan struct{}

	cancels map[string]map[int]func()
	subSeq  int
	steers  map[string][]Steer
	closed  map[string]bool
}

// NewMemory returns an empty in-memory queue and bus.
func NewMemory() *Memory {
	return &Memory{notify: make(chan struct{}, 1), cancels: map[string]map[int]func(){},
		steers: map[string][]Steer{}, closed: map[string]bool{}}
}

var (
	_ Queue = (*Memory)(nil)
	_ Bus   = (*Memory)(nil)
)

func (q *Memory) Enqueue(_ context.Context, runID string) error {
	q.mu.Lock()
	q.next++
	q.pending = append(q.pending, Message{StreamID: "mem-" + strconv.Itoa(q.next), RunID: runID})
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return nil
}

// EnsureGroup is a no-op.
func (q *Memory) EnsureGroup(context.Context) error { return nil }

// Read waits up to block for queued runs and hands out at most count of them.
// Delivery is at most once: a process that dies loses its queue anyway.
func (q *Memory) Read(ctx context.Context, _ string, count int, block time.Duration) ([]Message, error) {
	deadline := time.NewTimer(block)
	defer deadline.Stop()
	for {
		q.mu.Lock()
		if n := min(count, len(q.pending)); n > 0 {
			out := append([]Message(nil), q.pending[:n]...)
			q.pending = q.pending[n:]
			if len(q.pending) > 0 {
				select {
				case q.notify <- struct{}{}:
				default:
				}
			}
			q.mu.Unlock()
			return out, nil
		}
		q.mu.Unlock()
		select {
		case <-q.notify:
		case <-deadline.C:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (q *Memory) Ack(context.Context, string) error { return nil }

// Reclaim returns nothing: entries are not kept after Read.
func (q *Memory) Reclaim(context.Context, string, time.Duration) ([]Message, error) {
	return nil, nil
}

func (q *Memory) PruneConsumers(context.Context, string, time.Duration) (int, error) { return 0, nil }

func (q *Memory) PublishCancel(_ context.Context, runID string) error {
	q.mu.Lock()
	var fns []func()
	for _, fn := range q.cancels[runID] {
		fns = append(fns, fn)
	}
	q.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	return nil
}

func (q *Memory) SubscribeCancel(_ context.Context, runID string, fn func()) (func(), error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.subSeq++
	id := q.subSeq
	if q.cancels[runID] == nil {
		q.cancels[runID] = map[int]func(){}
	}
	q.cancels[runID][id] = fn
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		delete(q.cancels[runID], id)
		if len(q.cancels[runID]) == 0 {
			delete(q.cancels, runID)
		}
	}, nil
}

func (q *Memory) PushSteer(_ context.Context, runID string, s Steer) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed[runID] {
		return ErrSteersClosed
	}
	if len(q.steers[runID]) >= MaxSteers {
		return ErrTooManySteers
	}
	q.steers[runID] = append(q.steers[runID], s)
	return nil
}

func (q *Memory) DrainSteers(_ context.Context, runID string) ([]Steer, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.steers[runID]
	delete(q.steers, runID)
	return out, nil
}

func (q *Memory) FinalizeSteers(_ context.Context, runID string) ([]Steer, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed[runID] = true
	out := q.steers[runID]
	delete(q.steers, runID)
	return out, nil
}

func (q *Memory) ReopenSteers(_ context.Context, runID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.closed, runID)
	return nil
}

func (q *Memory) CloseSteers(_ context.Context, runID string) ([]Steer, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed[runID] = true
	return append([]Steer(nil), q.steers[runID]...), nil
}

func (q *Memory) AckSteers(_ context.Context, runID string, n int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	cur := q.steers[runID]
	if n >= len(cur) {
		delete(q.steers, runID)
		return nil
	}
	q.steers[runID] = cur[n:]
	return nil
}

func (q *Memory) SteerRuns(context.Context) ([]string, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []string
	for id, s := range q.steers {
		if len(s) > 0 {
			out = append(out, id)
		}
	}
	return out, nil
}
