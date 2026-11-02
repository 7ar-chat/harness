package eventlog_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/eventlog"
)

func TestMemoryLogTailsLiveAndReplaysAfterASeq(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := memstore.New()
	log := eventlog.NewMemory(st)
	w := log.Writer("run-1", 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent("t", "run-1"))
	_, _ = w.Append(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": "step-1"}))

	// Act
	ch := log.Replay(ctx, "run-1", 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run-1", "success", nil))
	}()
	var got []string
	for e := range ch {
		got = append(got, e.Type)
	}

	// Assert
	if len(got) != 2 || got[0] != agui.StepStarted || got[1] != agui.RunFinished {
		t.Fatalf("replayed %v", got)
	}
	if _, ok := w.Terminal(); !ok {
		t.Fatal("no terminal event")
	}
}

func TestMemoryLogPersistsOnFlushAndReplaysFromTheStoreInANewProcess(t *testing.T) {
	// Arrange
	ctx := context.Background()
	st := memstore.New()
	w := eventlog.NewMemory(st).Writer("run-1", 0)
	_, _ = w.Append(ctx, agui.RunStartedEvent("t", "run-1"))
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run-1", "success", nil))
	if err := w.Close(ctx); err != nil {
		t.Fatal(err)
	}

	// Act: a fresh log over the same store
	var got []string
	for e := range eventlog.NewMemory(st).Replay(ctx, "run-1", 0) {
		got = append(got, e.Type)
	}

	// Assert
	if len(got) != 2 || got[1] != agui.RunFinished {
		t.Fatalf("replayed %v", got)
	}
}

// failingStore wraps a memstore and fails the calls selected by its fields.
type failingStore struct {
	*memstore.Store
	appendErr  error
	lastErr    error
	listErr    error
	appendLogs [][]int64
}

func (f *failingStore) AppendEvents(ctx context.Context, runID string, evs []store.Event) error {
	var seqs []int64
	for _, e := range evs {
		seqs = append(seqs, e.Seq)
	}
	f.appendLogs = append(f.appendLogs, seqs)
	if f.appendErr != nil {
		return f.appendErr
	}
	return f.Store.AppendEvents(ctx, runID, evs)
}

func (f *failingStore) LastEvent(ctx context.Context, runID string) (store.Event, bool, error) {
	if f.lastErr != nil {
		return store.Event{}, false, f.lastErr
	}
	return f.Store.LastEvent(ctx, runID)
}

func (f *failingStore) ListEvents(ctx context.Context, runID string, after int64, limit int) ([]store.Event, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.Store.ListEvents(ctx, runID, after, limit)
}

func step(n int) agui.Event { return agui.New(agui.StepStarted, map[string]any{"stepName": n}) }

func TestMemoryLogRejectsEventsThatCannotBeEncoded(t *testing.T) {
	// An event that cannot be marshalled is refused without consuming a seq: the next valid event gets the next number.
	// Arrange
	ctx := context.Background()
	w := eventlog.NewMemory(nil).Writer("run", 0)

	// Act
	badSeq, err := w.Append(ctx, agui.New(agui.StepStarted, map[string]any{"stepName": make(chan int)}))
	goodSeq, goodErr := w.Append(ctx, step(1))

	// Assert
	if err == nil || badSeq != 0 || goodErr != nil || goodSeq != 1 || w.LastSeq() != 1 {
		t.Fatalf("bad %d %v good %d %v last %d", badSeq, err, goodSeq, goodErr, w.LastSeq())
	}
}

func TestMemoryLogWithoutAStoreFlushesNothingAndKeepsServingLiveEvents(t *testing.T) {
	// With no store Flush and Close succeed trivially, and events stay replayable from memory.
	// Arrange
	ctx := context.Background()
	log := eventlog.NewMemory(nil)
	w := log.Writer("run", 0)
	_, _ = w.Append(ctx, step(1))
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run", "success", nil))

	// Act
	flushErr := w.Flush(ctx)
	closeErr := w.Close(ctx)
	var seqs []int64
	for e := range log.Replay(ctx, "run", 0) {
		seqs = append(seqs, e.Seq)
	}

	// Assert
	if flushErr != nil || closeErr != nil || len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("flush %v close %v replayed %v", flushErr, closeErr, seqs)
	}
}

func TestMemoryLogFlushPersistsOnlyWhatIsNewAndRetriesAfterAFailure(t *testing.T) {
	// A failed flush keeps the events pending; the next flush writes them (and only them) once the store is back.
	// Arrange
	ctx := context.Background()
	st := &failingStore{Store: memstore.New(), appendErr: errors.New("db down")}
	w := eventlog.NewMemory(st).Writer("run", 0)
	_, _ = w.Append(ctx, step(1))
	_, _ = w.Append(ctx, step(2))
	failed := w.Flush(ctx)
	st.appendErr = nil

	// Act
	retried := w.Flush(ctx)
	_, _ = w.Append(ctx, step(3))
	next := w.Flush(ctx)
	idle := w.Flush(ctx)

	// Assert
	if failed == nil || retried != nil || next != nil || idle != nil {
		t.Fatalf("failed %v retried %v next %v idle %v", failed, retried, next, idle)
	}
	want := [][]int64{{1, 2}, {1, 2}, {3}}
	if len(st.appendLogs) != len(want) {
		t.Fatalf("store writes %v", st.appendLogs)
	}
	for i := range want {
		if len(st.appendLogs[i]) != len(want[i]) || st.appendLogs[i][0] != want[i][0] {
			t.Fatalf("store writes %v", st.appendLogs)
		}
	}
	if got, _ := st.ListEvents(ctx, "run", 0, 0); len(got) != 3 {
		t.Fatalf("persisted %d events", len(got))
	}
}

func TestMemoryLogWriterStartsAfterTheGivenSeqAndSkipsFlushingIt(t *testing.T) {
	// A writer opened at lastSeq numbers from lastSeq+1 and treats events up to lastSeq as already persisted.
	// Arrange
	ctx := context.Background()
	st := &failingStore{Store: memstore.New()}
	w := eventlog.NewMemory(st).Writer("run", 7)

	// Act
	seq, _ := w.Append(ctx, step(1))
	_ = w.Flush(ctx)

	// Assert
	if seq != 8 || len(st.appendLogs) != 1 || st.appendLogs[0][0] != 8 {
		t.Fatalf("seq %d store writes %v", seq, st.appendLogs)
	}
}

func TestMemoryLogTerminalIsOnlyTheLastEventWhenItEndsTheRun(t *testing.T) {
	// Terminal reports nothing for an empty or still-running log, and the final event once it was appended.
	// Arrange
	ctx := context.Background()
	w := eventlog.NewMemory(nil).Writer("run", 0)
	_, emptyTerminal := w.Terminal()
	_, _ = w.Append(ctx, step(1))
	_, runningTerminal := w.Terminal()

	// Act
	_, _ = w.Append(ctx, agui.RunErrorEvent("boom", "failed"))
	end, endTerminal := w.Terminal()

	// Assert
	if emptyTerminal || runningTerminal || !endTerminal || end.Seq != 2 || end.Type != agui.RunError {
		t.Fatalf("empty %v running %v end %+v %v", emptyTerminal, runningTerminal, end, endTerminal)
	}
}

func TestMemoryLogResumeContinuesAfterTheHighestSeqKnownAnywhere(t *testing.T) {
	// Resume continues after the highest seq among the caller's database seq, the in-memory events and the store, and does not re-persist what is already known.
	// Arrange
	ctx := context.Background()
	st := &failingStore{Store: memstore.New()}
	log := eventlog.NewMemory(st)
	old := log.Writer("mem", 0)
	for i := 1; i <= 3; i++ {
		_, _ = old.Append(ctx, step(i))
	}
	_ = st.Store.AppendEvents(ctx, "stored", []store.Event{{Seq: 1}, {Seq: 2}, {Seq: 5, Type: agui.StepStarted}})

	// Act
	fromMemory, memErr := log.Resume(ctx, "mem", 1)
	fromStore, storeErr := log.Resume(ctx, "stored", 2)
	fromCaller, callerErr := log.Resume(ctx, "none", 9)
	memSeq, _ := fromMemory.Append(ctx, step(4))
	storeSeq, _ := fromStore.Append(ctx, step(6))
	callerSeq, _ := fromCaller.Append(ctx, step(10))

	// Assert
	if memErr != nil || storeErr != nil || callerErr != nil {
		t.Fatalf("errors %v %v %v", memErr, storeErr, callerErr)
	}
	if memSeq != 4 || storeSeq != 6 || callerSeq != 10 {
		t.Fatalf("resumed at %d %d %d", memSeq, storeSeq, callerSeq)
	}
	_ = fromMemory.Flush(ctx)
	if last := st.appendLogs[len(st.appendLogs)-1]; len(last) != 1 || last[0] != 4 {
		t.Fatalf("resumed writer flushed %v, want only its own event", last)
	}
}

func TestMemoryLogResumeReportsStoreFailures(t *testing.T) {
	// If the store cannot say what was already written, Resume fails instead of risking duplicate seqs.
	// Arrange
	st := &failingStore{Store: memstore.New(), lastErr: errors.New("db down")}

	// Act
	w, err := eventlog.NewMemory(st).Resume(context.Background(), "run", 0)

	// Assert
	if err == nil || w != nil {
		t.Fatalf("writer %v err %v", w, err)
	}
}

func TestMemoryLogRepairAndFinalizingAreHarmless(t *testing.T) {
	// There is nothing to repair in memory, and the finalizing mark can be set and cleared repeatedly.
	// Arrange
	log := eventlog.NewMemory(nil)

	// Act
	n, err := log.Repair(context.Background())
	clear := log.Finalizing(context.Background(), "run", time.Minute)
	clear()
	clear()

	// Assert
	if n != 0 || err != nil {
		t.Fatalf("repaired %d %v", n, err)
	}
}

func TestMemoryLogReplayWithoutEventsEndsWhenTheContextEnds(t *testing.T) {
	// Replaying a run that never starts waits for its first event and closes the channel when the context is cancelled.
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	ch := eventlog.NewMemory(memstore.New()).Replay(ctx, "never-started", 0)

	// Act
	cancel()
	select {
	case e, ok := <-ch:

		// Assert
		if ok {
			t.Fatalf("unexpected event %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("replay did not stop on context cancellation")
	}
}

func TestMemoryLogReplayStopsWhenTheReaderGoesAway(t *testing.T) {
	// A replay whose consumer cancels closes its channel even though events are still queued behind it.
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	log := eventlog.NewMemory(nil)
	w := log.Writer("run", 0)
	for i := 1; i <= 3; i++ {
		_, _ = w.Append(ctx, step(i))
	}
	ch := log.Replay(ctx, "run", 0)
	first := <-ch

	// Act
	cancel()
	closed := make(chan struct{})
	go func() {
		for range ch {
		}
		close(closed)
	}()

	// Assert
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("replay kept running after its context was cancelled")
	}
	if first.Seq != 1 {
		t.Fatalf("first event %d", first.Seq)
	}
}

func TestMemoryLogReplayFromTheStoreStopsAtTheTerminalEventAndHonoursTheCursor(t *testing.T) {
	// A run of a previous process replays from the store after the given seq, ending at its terminal event without waiting for more.
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := memstore.New()
	prev := eventlog.NewMemory(st).Writer("run", 0)
	_, _ = prev.Append(ctx, step(1))
	_, _ = prev.Append(ctx, step(2))
	_, _ = prev.Append(ctx, agui.RunFinishedEvent("t", "run", "success", nil))
	_ = prev.Close(ctx)

	// Act
	var seqs []int64
	for e := range eventlog.NewMemory(st).Replay(ctx, "run", 1) {
		seqs = append(seqs, e.Seq)
	}

	// Assert
	if len(seqs) != 2 || seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("replayed %v", seqs)
	}
}

func TestMemoryLogReplayWhenTheStoreFailsWaitsForLiveEvents(t *testing.T) {
	// If the store cannot list a previous run's events, replay still serves what is appended live rather than failing or hanging up.
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st := &failingStore{Store: memstore.New(), listErr: errors.New("db down")}
	log := eventlog.NewMemory(st)
	ch := log.Replay(ctx, "run", 0)
	w := log.Writer("run", 0)

	// Act
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run", "success", nil))
	e := <-ch

	// Assert
	if e.Type != agui.RunFinished || e.Seq != 1 {
		t.Fatalf("got %+v", e)
	}
}

func TestMemoryLogManyWritersAndReplayersStayConsistent(t *testing.T) {
	// Concurrent appends, flushes and several tailing replays: every tailer sees every event once, in order.
	// Arrange
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st := memstore.New()
	log := eventlog.NewMemory(st)
	w := log.Writer("run", 0)
	const total = 200
	var tailers sync.WaitGroup
	results := make([][]int64, 4)
	for i := range results {
		ch := log.Replay(ctx, "run", 0)
		tailers.Add(1)
		go func() {
			defer tailers.Done()
			for e := range ch {
				results[i] = append(results[i], e.Seq)
			}
		}()
	}

	// Act
	for i := 1; i < total; i++ {
		_, _ = w.Append(ctx, step(i))
		if i%25 == 0 {
			_ = w.Flush(ctx)
		}
	}
	_, _ = w.Append(ctx, agui.RunFinishedEvent("t", "run", "success", nil))
	closeErr := w.Close(ctx)
	tailers.Wait()

	// Assert
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	for i, seqs := range results {
		if len(seqs) != total {
			t.Fatalf("tailer %d saw %d events", i, len(seqs))
		}
		for j, s := range seqs {
			if s != int64(j+1) {
				t.Fatalf("tailer %d: position %d has seq %d", i, j, s)
			}
		}
	}
	if got, _ := st.ListEvents(ctx, "run", 0, 1000); len(got) != total {
		t.Fatalf("persisted %d", len(got))
	}
}
