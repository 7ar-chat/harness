package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
)

// MemoryLog is a Log that needs no Redis: the live events of a run are held in
// the process (tailing is a broadcast) and Flush persists them to the Store.
// It is the log of a single-process host (TUI, desktop app, tests); runs of a
// previous process replay from the Store.
type MemoryLog struct {
	st store.Store

	mu   sync.Mutex
	runs map[string]*memRun
}

type memRun struct {
	events []store.Event
	// changed is closed and replaced on every append: tailers wait on it.
	changed chan struct{}
}

// NewMemory returns a MemoryLog persisting to st (nil: nothing is persisted).
func NewMemory(st store.Store) *MemoryLog {
	return &MemoryLog{st: st, runs: map[string]*memRun{}}
}

var _ Log = (*MemoryLog)(nil)

func (l *MemoryLog) run(runID string) *memRun {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.runs[runID]
	if r == nil {
		r = &memRun{changed: make(chan struct{})}
		l.runs[runID] = r
	}
	return r
}

type memWriter struct {
	l     *MemoryLog
	runID string
	r     *memRun
	mu    sync.Mutex
	seq   int64
	// persisted is the highest seq already written to the store.
	persisted int64
}

func (l *MemoryLog) Writer(runID string, lastSeq int64) Writer {
	return &memWriter{l: l, runID: runID, r: l.run(runID), seq: lastSeq, persisted: lastSeq}
}

func (w *memWriter) Append(_ context.Context, ev agui.Event) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := ev["timestamp"]; !ok {
		ev["timestamp"] = time.Now().UnixMilli()
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("eventlog: marshal: %w", err)
	}
	data = store.CleanJSON(data)
	w.seq++
	e := store.Event{Seq: w.seq, Type: ev.Type(), Data: data}
	w.l.mu.Lock()
	w.r.events = append(w.r.events, e)
	close(w.r.changed)
	w.r.changed = make(chan struct{})
	w.l.mu.Unlock()
	return w.seq, nil
}

func (w *memWriter) Flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.l.st == nil {
		return nil
	}
	w.l.mu.Lock()
	var pending []store.Event
	for _, e := range w.r.events {
		if e.Seq > w.persisted {
			pending = append(pending, e)
		}
	}
	w.l.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	if err := w.l.st.AppendEvents(ctx, w.runID, pending); err != nil {
		return fmt.Errorf("eventlog: flush: %w", err)
	}
	w.persisted = pending[len(pending)-1].Seq
	return nil
}

func (w *memWriter) Close(ctx context.Context) error { return w.Flush(ctx) }

func (w *memWriter) LastSeq() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seq
}

func (w *memWriter) Terminal() (store.Event, bool) {
	w.l.mu.Lock()
	defer w.l.mu.Unlock()
	if n := len(w.r.events); n > 0 && agui.IsTerminal(w.r.events[n-1].Type) {
		return w.r.events[n-1], true
	}
	return store.Event{}, false
}

// Resume positions a writer after the highest seq written, in memory or in the store.
func (l *MemoryLog) Resume(ctx context.Context, runID string, dbLastSeq int64) (Writer, error) {
	last := dbLastSeq
	l.mu.Lock()
	if r := l.runs[runID]; r != nil && len(r.events) > 0 {
		last = max(last, r.events[len(r.events)-1].Seq)
	}
	l.mu.Unlock()
	if l.st != nil {
		if e, ok, err := l.st.LastEvent(ctx, runID); err != nil {
			return nil, err
		} else if ok {
			last = max(last, e.Seq)
		}
	}
	w := l.Writer(runID, last).(*memWriter)
	w.persisted = last
	return w, nil
}

// Repair is a no-op: nothing is copied between stores.
func (l *MemoryLog) Repair(context.Context) (int, error) { return 0, nil }

// Finalizing is a no-op: replay never synthesizes a terminal event.
func (l *MemoryLog) Finalizing(context.Context, string, time.Duration) func() { return func() {} }

// Replay streams events with seq > afterSeq, then tails until a terminal event.
func (l *MemoryLog) Replay(ctx context.Context, runID string, afterSeq int64) <-chan store.Event {
	out := make(chan store.Event)
	go func() {
		defer close(out)
		cursor := afterSeq
		send := func(e store.Event) bool {
			select {
			case out <- e:
				cursor = e.Seq
				return !agui.IsTerminal(e.Type)
			case <-ctx.Done():
				return false
			}
		}
		r := l.run(runID) // may not have started yet: wait for its first event
		l.mu.Lock()
		inMemory := len(r.events) > 0
		l.mu.Unlock()
		if !inMemory && l.st != nil { // a run of a previous process
			for {
				evs, err := l.st.ListEvents(ctx, runID, cursor, 500)
				if err != nil || len(evs) == 0 {
					break
				}
				for _, e := range evs {
					if !send(e) {
						return
					}
				}
			}
		}
		if !inMemory && l.st != nil { // reconnecting after the end of a run of a previous process: nothing is left
			if end, ok, err := l.st.LastEvent(ctx, runID); err == nil && ok && agui.IsTerminal(end.Type) && end.Seq <= cursor {
				return
			}
		}
		for {
			l.mu.Lock()
			var batch []store.Event
			for _, e := range r.events {
				if e.Seq > cursor {
					batch = append(batch, e)
				}
			}
			ended := len(batch) == 0 && len(r.events) > 0 && agui.IsTerminal(r.events[len(r.events)-1].Type) // reconnecting after the end
			changed := r.changed
			l.mu.Unlock()
			if ended {
				return
			}
			for _, e := range batch {
				if !send(e) {
					return
				}
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}
