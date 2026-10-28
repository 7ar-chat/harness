package redisx_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness/engine/agui"
	"github.com/GhaziBenDahmane/harness/redisx"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/memstore"
)

type fixture struct {
	mr  *miniredis.Miniredis
	st  *memstore.Store
	log *redisx.EventLog
	run *store.Run
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	th, _ := st.CreateThread(context.Background(), "alice", "t")
	run, _, _ := st.CreateRun(context.Background(), store.NewRun{RunID: "11111111-1111-1111-1111-111111111111", ThreadID: th.ID, UserLogin: "alice", Model: "m", Message: store.Message{Role: "user", Content: "q"}})
	_, _ = st.ClaimRun(context.Background(), run.ID, "rt")
	l := redisx.NewEventLog(rdb, st)
	l.TailBlock, l.Poll, l.TerminalWait = 50*time.Millisecond, 10*time.Millisecond, 100*time.Millisecond
	return &fixture{mr: mr, st: st, log: l, run: run}
}

func step(n int) agui.Event { return agui.New(agui.StepStarted, map[string]any{"stepName": n}) }

func collect(t *testing.T, ch <-chan store.Event) []int64 {
	t.Helper()
	var seqs []int64
	timeout := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return seqs
			}
			seqs = append(seqs, e.Seq)
		case <-timeout:
			t.Fatalf("replay did not finish; got %v", seqs)
		}
	}
}

func equal(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestReplayStitchesPostgresAndRecreatedStream(t *testing.T) {
	// If Redis lost the stream mid-run, replay serves the flushed part from Postgres then continues with Redis, gap- and duplicate-free.
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	w := f.log.Writer(f.run.ID, 0)
	for i := 1; i <= 3; i++ {
		_, _ = w.Append(ctx, step(i))
	}
	_ = w.Flush(ctx)
	f.mr.Del(redisx.EventKey(f.run.ID))
	_, _ = w.Append(ctx, step(4))
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", f.run.ID, "success", nil))

	// Act
	seqs := collect(t, f.log.Replay(ctx, f.run.ID, 0))

	// Assert
	if !equal(seqs, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("seqs %v", seqs)
	}
}

func TestReplayWaitsForMissingEventsInsteadOfSkipping(t *testing.T) {
	// When neither Redis nor Postgres has the next seq yet, replay waits for the flush rather than skipping ahead.
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(ctx, step(1))
	_, _ = w.Append(ctx, step(2))
	f.mr.Del(redisx.EventKey(f.run.ID))
	_, _ = w.Append(ctx, step(3))
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = w.Flush(ctx)
		_, _ = w.Append(ctx, agui.RunFinishedEvent("t", f.run.ID, "success", nil))
	}()

	// Act
	seqs := collect(t, f.log.Replay(ctx, f.run.ID, 0))

	// Assert
	if !equal(seqs, []int64{1, 2, 3, 4}) {
		t.Fatalf("seqs %v", seqs)
	}
}

func TestReplaySynthesizesTerminalEventFromStoredErrorCode(t *testing.T) {
	// A failed run whose log lacks a terminal event ends with a synthetic RUN_ERROR carrying the run's stored code.
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(ctx, step(1))
	_ = w.Close(ctx)
	_ = f.st.FinishRun(ctx, f.run.ID, "rt", store.StatusFailed, "max_steps", "")

	// Act
	var evs []store.Event
	for e := range f.log.Replay(ctx, f.run.ID, 0) {
		evs = append(evs, e)
	}

	// Assert
	if len(evs) != 2 || evs[1].Seq != 2 || evs[1].Type != "RUN_ERROR" || !strings.Contains(string(evs[1].Data), `"code":"max_steps"`) {
		t.Fatalf("events %+v", evs)
	}
}

func TestReplayWaitsForALateTerminalEvent(t *testing.T) {
	// When the run row is terminal before its RUN_FINISHED is written, replay waits for the real event instead of inventing one.
	// Arrange
	f := newFixture(t)
	f.log.TerminalWait = 2 * time.Second
	ctx := context.Background()
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(ctx, step(1))
	_ = f.st.FinishRun(ctx, f.run.ID, "rt", store.StatusCompleted, "", "")
	go func() {
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Append(ctx, agui.RunFinishedEvent("t", f.run.ID, "success", []agui.Usage{{Model: "real"}}))
	}()

	// Act
	var evs []store.Event
	for e := range f.log.Replay(ctx, f.run.ID, 0) {
		evs = append(evs, e)
	}

	// Assert
	if len(evs) != 2 || evs[1].Type != "RUN_FINISHED" || !strings.Contains(string(evs[1].Data), `"model":"real"`) {
		t.Fatalf("events %+v", evs)
	}
}

func TestReplayEndsCancelledRunWithRunFinished(t *testing.T) {
	// A cancelled run with an empty log ends with RUN_STARTED then RUN_FINISHED{outcome:cancelled} (strict AG-UI order).
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	_ = f.st.FinishRun(ctx, f.run.ID, "rt", store.StatusCancelled, "", "")

	// Act
	var evs []store.Event
	for e := range f.log.Replay(ctx, f.run.ID, 0) {
		evs = append(evs, e)
	}

	// Assert
	if len(evs) != 2 || evs[0].Type != "RUN_STARTED" || evs[1].Type != "RUN_FINISHED" || evs[1].Seq != 2 || !strings.Contains(string(evs[1].Data), `"cancelled"`) {
		t.Fatalf("events %+v", evs)
	}
}

func TestReplaySynthesizesRunStartedBeforeRunErrorOnAnEmptyLog(t *testing.T) {
	// A failed run whose runtime died before its first event replays as RUN_STARTED, RUN_ERROR (never RUN_ERROR first).
	// Arrange
	f := newFixture(t)
	ctx := context.Background()
	_ = f.st.FinishRun(ctx, f.run.ID, "rt", store.StatusInterrupted, "", "")

	// Act
	var raw []json.RawMessage
	for e := range f.log.Replay(ctx, f.run.ID, 0) {
		raw = append(raw, e.Data)
	}

	// Assert
	if err := agui.CheckOrderJSON(raw); err != nil || len(raw) != 2 {
		t.Fatalf("order %v events %d", err, len(raw))
	}
}

func TestReplayAfterTheTerminalEventEndsWithoutInventingAnother(t *testing.T) {
	// Reconnecting with Last-Event-ID at (or past) the terminal seq closes the stream at once: no second, invented terminal.
	// Arrange
	f := newFixture(t)
	f.log.TerminalWait = 50 * time.Millisecond
	ctx := context.Background()
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent("t", f.run.ID))
	_, _ = w.Append(ctx, agui.RunErrorEvent("model_error", "boom"))
	_ = w.Close(ctx)
	_ = f.st.FinishRun(ctx, f.run.ID, "rt", store.StatusFailed, "model_error", "boom")

	// Act
	atTerminal := collect(t, f.log.Replay(ctx, f.run.ID, 2))
	pastEnd := collect(t, f.log.Replay(ctx, f.run.ID, 50))

	// Assert
	if len(atTerminal) != 0 || len(pastEnd) != 0 {
		t.Fatalf("after=2 got %v, after=50 got %v", atTerminal, pastEnd)
	}
}
