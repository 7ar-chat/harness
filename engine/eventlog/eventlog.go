// Package eventlog is the per-run AG-UI event log contract and its in-process
// implementation (MemoryLog). Shared implementations (Redis streams for live
// tailing, the store for durability) live in the adapter packages, e.g. redisx.
package eventlog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

// Writer appends events of one run. There is a single writer per run.
type Writer interface {
	// Append assigns the next seq, publishes the event live and buffers it for the store.
	Append(ctx context.Context, ev agui.Event) (int64, error)
	// Flush persists buffered events to the store (and work_runs.last_seq).
	Flush(ctx context.Context) error
	// Close flushes and schedules expiry of the live stream.
	Close(ctx context.Context) error
	LastSeq() int64
	// Terminal returns the log's terminal event (RUN_FINISHED/RUN_ERROR) if it has one.
	Terminal() (store.Event, bool)
}

// Log creates writers and replays runs.
type Log interface {
	Writer(runID string, lastSeq int64) Writer
	// Resume positions a writer after the highest seq already written (live
	// stream or store), backfilling the store first. Used to finish runs of a
	// dead runtime.
	Resume(ctx context.Context, runID string, dbLastSeq int64) (Writer, error)
	// Repair copies, from the live stream to the store, the events of runs whose final flush
	// failed (store outage at run end). Idempotent; returns runs completed.
	Repair(ctx context.Context) (int, error)
	// Replay streams events with seq > afterSeq, gap- and duplicate-free, then
	// tails until a terminal event. The channel closes at the end or on ctx done.
	Replay(ctx context.Context, runID string, afterSeq int64) <-chan store.Event
	// Finalizing marks a run whose row is (about to be) terminal while its end-of-run
	// continuations still emit events before the terminal one: replay keeps waiting
	// for the real terminal event instead of synthesizing one. The returned func
	// clears the mark (it also expires after ttl).
	Finalizing(ctx context.Context, runID string, ttl time.Duration) func()
}

// ErrConflict means another writer already wrote this seq (ownership lost).
var ErrConflict = errors.New("eventlog: seq already written by another writer")

// Persist writes events to the store; it returns the events still to write
// (transient error) and drops only rows the store refuses permanently
// (data/constraint errors). Adapters use it to flush their buffers.
func Persist(ctx context.Context, st store.Store, runID string, evs []store.Event) ([]store.Event, error) {
	err := st.AppendEvents(ctx, runID, evs)
	if err == nil {
		return nil, nil
	}
	if !store.IsDataError(err) {
		return evs, fmt.Errorf("eventlog: flush: %w", err)
	}
	var dropped []int64
	defer func() {
		if len(dropped) > 0 {
			slog.Error("eventlog: dropping events the store refuses", "run_id", runID, "seqs", dropped, "error", err)
		}
	}()
	for i, e := range evs {
		perr := st.AppendEvents(ctx, runID, []store.Event{e})
		switch {
		case perr == nil:
		case store.IsDataError(perr):
			dropped = append(dropped, e.Seq)
		default:
			return evs[i:], fmt.Errorf("eventlog: flush: %w", perr)
		}
	}
	return nil, nil
}
