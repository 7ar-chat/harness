package redisx_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	_ "github.com/GhaziBenDahmane/harness/adapters/store/pgstore" // registers the Postgres data-error classifier
	"github.com/GhaziBenDahmane/harness/core/agui"
	"github.com/GhaziBenDahmane/harness/core/store"
)

const runID = "11111111-1111-1111-1111-111111111111"

// flakyStore simulates Postgres: an outage (connection errors) and a row it always refuses (data error).
type flakyStore struct {
	*memstore.Store
	mu     sync.Mutex
	down   bool
	badSeq int64
	calls  atomic.Int32
}

func (f *flakyStore) setDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }

func (f *flakyStore) AppendEvents(ctx context.Context, id string, evs []store.Event) error {
	f.calls.Add(1)
	f.mu.Lock()
	down, bad := f.down, f.badSeq
	f.mu.Unlock()
	if down {
		return errors.New("dial tcp 10.0.0.5:5432: connect: connection refused")
	}
	for _, e := range evs {
		if e.Seq == bad {
			return &pgconn.PgError{Code: "22P05", Message: "unsupported Unicode escape sequence"}
		}
	}
	return f.Store.AppendEvents(ctx, id, evs)
}

func newFlaky(t *testing.T) (*flakyStore, *redisx.EventLog, *store.Run) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := &flakyStore{Store: memstore.New()}
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	run, _, _ := st.CreateRun(context.Background(), store.NewRun{RunID: runID, ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: "user", Content: "q"}})
	_, _ = st.ClaimRun(context.Background(), run.ID, "rt")
	l := redisx.NewEventLog(rdb, st)
	l.CloseRetry = 50 * time.Millisecond
	return st, l, run
}

func seqs(t *testing.T, st store.Full) []int64 {
	t.Helper()
	evs, _ := st.ListEvents(context.Background(), runID, 0, 0)
	out := []int64{}
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

func TestFlushDropsOnlyTheRowPostgresRefuses(t *testing.T) {
	// A data error (SQLSTATE class 22/23) drops only the offending event; the others are persisted.
	// Arrange
	st, l, _ := newFlaky(t)
	st.badSeq = 2
	ctx := context.Background()
	w := l.Writer(runID, 0)
	for i := 1; i <= 3; i++ {
		_, _ = w.Append(ctx, step(i))
	}

	// Act
	_ = w.Flush(ctx)
	err := w.Close(ctx)

	// Assert
	if got := seqs(t, st); !equal(got, []int64{1, 3}) || err != nil {
		t.Fatalf("persisted %v err %v", got, err)
	}
}

func TestFlushKeepsEventsDuringAnOutage(t *testing.T) {
	// During a Postgres outage nothing is dropped: the buffer is kept and flushed in order once Postgres is back.
	// Arrange
	st, l, run := newFlaky(t)
	ctx := context.Background()
	w := l.Writer(runID, 0)
	_, _ = w.Append(ctx, step(1))
	st.setDown(true)
	for i := 0; i < 5; i++ {
		_ = w.Flush(ctx)
	}
	_, _ = w.Append(ctx, step(2))
	st.setDown(false)

	// Act
	err := w.Flush(ctx)

	// Assert
	got, _ := st.GetRun(ctx, run.ID)
	if s := seqs(t, st); err != nil || !equal(s, []int64{1, 2}) || got.LastSeq != 2 {
		t.Fatalf("err %v persisted %v lastSeq %d", err, s, got.LastSeq)
	}
}

func TestRepairBackfillsEventsOfARunThatEndedDuringAnOutage(t *testing.T) {
	// If Postgres is down when a run ends, Close gives up after a bounded time and Repair later copies the events from Redis.
	// Arrange
	st, l, run := newFlaky(t)
	ctx := context.Background()
	w := l.Writer(runID, 0)
	_, _ = w.Append(ctx, step(1))
	_ = w.Flush(ctx)
	st.setDown(true)
	_, _ = w.Append(ctx, step(2))
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", runID, "success", nil))
	start := time.Now()
	closeErr := w.Close(ctx)
	elapsed := time.Since(start)
	st.setDown(false)
	_ = st.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", "")

	// Act
	repaired, err := l.Repair(ctx)
	again, _ := l.Repair(ctx)

	// Assert
	got, _ := st.GetRun(ctx, run.ID)
	if closeErr == nil || elapsed > 2*time.Second || err != nil || repaired != 1 || again != 0 {
		t.Fatalf("close %v after %v, repair %d %v, again %d", closeErr, elapsed, repaired, err, again)
	}
	if s := seqs(t, st); !equal(s, []int64{1, 2, 3}) || got.LastSeq != 3 {
		t.Fatalf("persisted %v lastSeq %d", s, got.LastSeq)
	}
}

func TestRepairKeepsRunWhenRedisCannotBeRead(t *testing.T) {
	// If the live stream cannot be read, Repair copies nothing and keeps the run scheduled for repair.
	// Arrange
	st, l, run := newFlaky(t)
	ctx := context.Background()
	w := l.Writer(runID, 0)
	_, _ = w.Append(ctx, step(1))
	st.setDown(true)
	_ = w.Close(ctx)
	st.setDown(false)
	_ = st.FinishRun(ctx, run.ID, "rt", store.StatusCompleted, "", "")
	l.Redis().Del(ctx, redisx.EventKey(runID))
	l.Redis().Set(ctx, redisx.EventKey(runID), "not a stream", 0) // XRANGE now fails (WRONGTYPE)

	// Act
	n, _ := l.Repair(ctx)

	// Assert
	still, _ := l.Redis().SIsMember(ctx, "work:eventlog:repair", runID).Result()
	if n != 0 || !still {
		t.Fatalf("repaired %d still scheduled %v", n, still)
	}
}
