package redisx_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/eventlog"
)

func TestAppendNumbersEventsAndTracksTheTerminalOne(t *testing.T) {
	// Append returns consecutive seqs after the writer's start, stamps a timestamp, and Terminal reports the last event only once it ends the run.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 4)
	_, emptyTerminal := f.log.Writer(f.run.ID, 0).Terminal()

	// Act
	s1, _ := w.Append(bg, step(1))
	_, runningTerminal := w.Terminal()
	s2, _ := w.Append(bg, agui.RunFinishedEvent("t", f.run.ID, "success", nil))
	end, endTerminal := w.Terminal()

	// Assert
	if s1 != 5 || s2 != 6 || w.LastSeq() != 6 {
		t.Fatalf("seqs %d %d last %d", s1, s2, w.LastSeq())
	}
	if emptyTerminal || runningTerminal || !endTerminal || end.Seq != 6 || end.Type != agui.RunFinished {
		t.Fatalf("terminal: empty %v running %v end %+v %v", emptyTerminal, runningTerminal, end, endTerminal)
	}
	entries, _ := f.mr.Stream(redisx.EventKey(f.run.ID))
	if len(entries) != 2 || entries[0].ID != "0-5" || !strings.Contains(entries[0].Values[1], `"timestamp"`) {
		t.Fatalf("stream %+v", entries)
	}
}

func TestAppendRefusesToOverwriteAnotherWritersEvent(t *testing.T) {
	// When another writer already wrote this seq (ownership lost) Append fails with ErrConflict and records nothing locally.
	// Arrange
	f := newFixture(t)
	first := f.log.Writer(f.run.ID, 0)
	_, _ = first.Append(bg, step(1))
	rival := f.log.Writer(f.run.ID, 0)

	// Act
	seq, err := rival.Append(bg, step(99))

	// Assert
	if !errors.Is(err, eventlog.ErrConflict) || seq != 0 || rival.LastSeq() != 0 {
		t.Fatalf("seq %d err %v last %d", seq, err, rival.LastSeq())
	}
	_ = rival.Flush(bg)
	if got := seqs(t, f.st); len(got) != 0 {
		t.Fatalf("the losing writer persisted %v", got)
	}
}

func TestAppendTreatsAnIdenticalExistingEntryAsItsOwnRetry(t *testing.T) {
	// A retried XADD (the first reply was lost) finds its own bytes already at that seq: Append succeeds instead of reporting a conflict, and the stream holds the event once.
	// Arrange
	f := newFixture(t)
	ev := func() agui.Event {
		return agui.New(agui.StepStarted, map[string]any{"stepName": "x", "timestamp": int64(1700000000000)})
	}
	_, _ = f.log.Writer(f.run.ID, 0).Append(bg, ev())

	// Act
	seq, err := f.log.Writer(f.run.ID, 0).Append(bg, ev())

	// Assert
	entries, _ := f.mr.Stream(redisx.EventKey(f.run.ID))
	if err != nil || seq != 1 || len(entries) != 1 {
		t.Fatalf("seq %d err %v stream %d entries", seq, err, len(entries))
	}
}

func TestAppendRejectsEventsThatCannotBeEncodedWithoutConsumingASeq(t *testing.T) {
	// An unencodable event fails and the next valid event takes the same seq: the log has no hole.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 0)

	// Act
	_, err := w.Append(bg, agui.New(agui.StepStarted, map[string]any{"stepName": make(chan int)}))
	seq, goodErr := w.Append(bg, step(1))

	// Assert
	if err == nil || goodErr != nil || seq != 1 {
		t.Fatalf("bad %v good %d %v", err, seq, goodErr)
	}
}

func TestAppendStripsNULsSoRedisAndPostgresHoldTheSameBytes(t *testing.T) {
	// A NUL in the text (which Postgres jsonb rejects) is removed before the event is written anywhere, so the stream copy equals the persisted copy.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 0)

	// Act
	_, _ = w.Append(bg, agui.New(agui.StepStarted, map[string]any{"stepName": "a\x00b"}))
	flushErr := w.Flush(bg)

	// Assert
	entries, _ := f.mr.Stream(redisx.EventKey(f.run.ID))
	persisted, _ := f.st.ListEvents(bg, f.run.ID, 0, 0)
	if flushErr != nil || len(entries) != 1 || strings.Contains(entries[0].Values[1], `\u0000`) || len(persisted) != 1 || string(persisted[0].Data) != entries[0].Values[1] {
		t.Fatalf("flush %v stream %+v persisted %+v", flushErr, entries, persisted)
	}
}

func TestEventsKeepFlowingToPostgresWhileRedisIsDown(t *testing.T) {
	// A Redis outage does not stop the run: Append still numbers the event, Flush persists it, and replay serves it from Postgres.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 0)
	f.mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	s1, err1 := w.Append(bg, step(1))
	s2, err2 := w.Append(bg, agui.RunFinishedEvent("t", f.run.ID, "success", nil))
	flushErr := w.Flush(bg)
	got := collect(t, f.log.Replay(bg, f.run.ID, 0))

	// Assert
	if err1 != nil || err2 != nil || s1 != 1 || s2 != 2 || flushErr != nil || !equal(got, []int64{1, 2}) {
		t.Fatalf("append %d/%v %d/%v flush %v replay %v", s1, err1, s2, err2, flushErr, got)
	}
}

func TestCloseSchedulesRepairAndSetsTheExpiryOfTheLiveStream(t *testing.T) {
	// A clean Close persists everything and puts the stream on its ttl without scheduling repair; a Close that cannot persist schedules the run for repair.
	// Arrange
	st, l, _ := newFlaky(t)
	l.TTL = 90 * time.Minute
	healthy := l.Writer(runID, 0)
	_, _ = healthy.Append(bg, step(1))

	// Act
	cleanErr := healthy.Close(bg)
	scheduledClean, _ := l.Redis().SIsMember(bg, "work:eventlog:repair", runID).Result()
	ttl := l.Redis().TTL(bg, redisx.EventKey(runID)).Val()
	st.setDown(true)
	broken := l.Writer(runID, 1)
	_, _ = broken.Append(bg, step(2))
	brokenErr := broken.Close(bg)
	scheduled, _ := l.Redis().SIsMember(bg, "work:eventlog:repair", runID).Result()

	// Assert
	if cleanErr != nil || scheduledClean || ttl != 90*time.Minute {
		t.Fatalf("clean close: %v scheduled %v ttl %v", cleanErr, scheduledClean, ttl)
	}
	if brokenErr == nil || !scheduled {
		t.Fatalf("broken close: %v scheduled %v", brokenErr, scheduled)
	}
}

func TestCloseStillReportsThePersistenceErrorWhenRedisIsDownToo(t *testing.T) {
	// If both Postgres and Redis are unavailable at run end, Close returns the flush error (it cannot even schedule repair) instead of hiding the loss.
	// Arrange
	f := newFixture(t)
	st := &downStore{Full: f.st}
	l := redisx.NewEventLog(f.log.Redis(), st)
	l.CloseRetry = 20 * time.Millisecond
	w := l.Writer(f.run.ID, 0)
	_, _ = w.Append(bg, step(1))
	f.mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	err := w.Close(bg)

	// Assert
	if err == nil || !strings.Contains(err.Error(), "flush") {
		t.Fatalf("close: %v", err)
	}
}

// downStore fails every event write and, when getRunErr is set, run lookups.
type downStore struct {
	store.Full
	getRunErr error
}

func (d *downStore) AppendEvents(context.Context, string, []store.Event) error {
	return errors.New("dial tcp: connection refused")
}

func (d *downStore) GetRun(ctx context.Context, id string) (*store.Run, error) {
	if d.getRunErr != nil {
		return nil, d.getRunErr
	}
	return d.Full.GetRun(ctx, id)
}

func TestCloseGivesUpAtTheContextDeadline(t *testing.T) {
	// Shutdown (ctx with a short deadline) bounds the flush retries even when CloseRetry is long.
	// Arrange
	st, l, _ := newFlaky(t)
	l.CloseRetry = time.Hour
	st.setDown(true)
	w := l.Writer(runID, 0)
	_, _ = w.Append(bg, step(1))
	ctx, cancel := context.WithTimeout(bg, 200*time.Millisecond)
	defer cancel()

	// Act
	start := time.Now()
	err := w.Close(ctx)

	// Assert
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("close %v after %v", err, time.Since(start))
	}
}

func TestResumeBackfillsPostgresFromRedisAndContinuesAfterTheLastEvent(t *testing.T) {
	// A dead runtime's unflushed events are copied from Redis to Postgres, and the new writer continues after the highest seq with the log's terminal state known.
	// Arrange
	f := newFixture(t)
	dead := f.log.Writer(f.run.ID, 0)
	for i := 1; i <= 3; i++ {
		_, _ = dead.Append(bg, step(i))
	}
	_ = dead.Flush(bg)
	_, _ = dead.Append(bg, step(4))
	_, _ = dead.Append(bg, agui.RunErrorEvent("boom", "x"))

	// Act
	w, err := f.log.Resume(bg, f.run.ID, 3)
	end, terminal := w.Terminal()
	seq, _ := w.Append(bg, step(6))

	// Assert
	if err != nil || w.LastSeq() != 6 || seq != 6 || !terminal || end.Seq != 5 || end.Type != agui.RunError {
		t.Fatalf("err %v last %d seq %d terminal %v %+v", err, w.LastSeq(), seq, terminal, end)
	}
	if got := seqs(t, f.st); !equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("persisted %v", got)
	}
}

func TestResumeFindsTheEndOfALogThatOnlyPostgresStillHas(t *testing.T) {
	// When the live stream expired, Resume takes the last seq and terminal state from Postgres.
	// Arrange
	f := newFixture(t)
	w0 := f.log.Writer(f.run.ID, 0)
	_, _ = w0.Append(bg, step(1))
	_, _ = w0.Append(bg, agui.RunFinishedEvent("t", f.run.ID, "success", nil))
	_ = w0.Close(bg)
	f.mr.Del(redisx.EventKey(f.run.ID))

	// Act
	w, err := f.log.Resume(bg, f.run.ID, 2)
	_, terminal := w.Terminal()

	// Assert
	if err != nil || w.LastSeq() != 2 || !terminal {
		t.Fatalf("err %v last %d terminal %v", err, w.LastSeq(), terminal)
	}
}

func TestResumeOfARunWithoutEventsStartsAtTheCallersSeq(t *testing.T) {
	// With nothing in Redis or Postgres the writer starts after the caller's seq.
	// Arrange
	f := newFixture(t)

	// Act
	w, err := f.log.Resume(bg, f.run.ID, 0)
	seq, _ := w.Append(bg, step(1))

	// Assert
	if err != nil || seq != 1 {
		t.Fatalf("err %v seq %d", err, seq)
	}
}

func TestResumeReportsRedisAndPostgresFailures(t *testing.T) {
	// If the log's end cannot be read from Redis, or Postgres cannot be written/read, Resume fails rather than risk reusing a seq.
	// Arrange
	f := newFixture(t)
	w0 := f.log.Writer(f.run.ID, 0)
	_, _ = w0.Append(bg, step(1))
	broken := redisx.NewEventLog(f.log.Redis(), &downStore{Full: f.st})
	other := newFixture(t)

	// Act
	_, writeErr := broken.Resume(bg, f.run.ID, 0)
	other.mr.SetError("ERR LOADING Redis is loading the dataset in memory")
	_, redisErr := other.log.Resume(bg, other.run.ID, 0)

	// Assert
	if writeErr == nil || redisErr == nil {
		t.Fatalf("postgres failure %v, redis failure %v", writeErr, redisErr)
	}
}

func TestResumeFailsWhenPostgresCannotReportItsLastEvent(t *testing.T) {
	// A store that cannot report the last event makes Resume fail (the seq to continue from is unknown).
	// Arrange
	f := newFixture(t)
	l := redisx.NewEventLog(f.log.Redis(), &lastEventFails{Full: f.st})

	// Act
	w, err := l.Resume(bg, f.run.ID, 0)

	// Assert
	if err == nil || w != nil {
		t.Fatalf("writer %v err %v", w, err)
	}
}

type lastEventFails struct{ store.Full }

func (lastEventFails) LastEvent(context.Context, string) (store.Event, bool, error) {
	return store.Event{}, false, errors.New("postgres down")
}

func TestRepairForgetsRunsThatNoLongerExistAndKeepsUnfinishedOnes(t *testing.T) {
	// A scheduled run that was deleted is dropped from the repair set; a run still running gets its events copied but stays scheduled until it ends.
	// Arrange
	st, l, run := newFlaky(t)
	w := l.Writer(runID, 0)
	_, _ = w.Append(bg, step(1))
	st.setDown(true)
	_ = w.Close(bg)
	st.setDown(false)
	l.Redis().SAdd(bg, "work:eventlog:repair", "99999999-9999-9999-9999-999999999999")

	// Act
	n, err := l.Repair(bg)
	members, _ := l.Redis().SMembers(bg, "work:eventlog:repair").Result()

	// Assert
	got, _ := st.GetRun(bg, run.ID)
	if err != nil || n != 0 || len(members) != 1 || members[0] != runID {
		t.Fatalf("repaired %d (%v), still scheduled %v", n, err, members)
	}
	if s := seqs(t, st); !equal(s, []int64{1}) || got.LastSeq != 1 {
		t.Fatalf("persisted %v lastSeq %d", s, got.LastSeq)
	}
}

func TestRepairStopsOnARunLookupErrorAndReportsRedisErrors(t *testing.T) {
	// An unexpected store error while looking up a scheduled run aborts the pass (nothing is dropped); an unreadable repair set is an error too.
	// Arrange
	f := newFixture(t)
	f.log.Redis().SAdd(bg, "work:eventlog:repair", f.run.ID)
	l := redisx.NewEventLog(f.log.Redis(), &downStore{Full: f.st, getRunErr: errors.New("postgres down")})

	// Act
	n, err := l.Repair(bg)
	still, _ := f.log.Redis().SIsMember(bg, "work:eventlog:repair", f.run.ID).Result()
	f.mr.SetError("ERR LOADING Redis is loading the dataset in memory")
	_, redisErr := f.log.Repair(bg)

	// Assert
	if err == nil || n != 0 || !still || redisErr == nil {
		t.Fatalf("lookup error %v (n=%d, still scheduled %v), redis error %v", err, n, still, redisErr)
	}
}

func TestReplayFallsBackToPostgresWhenRedisFails(t *testing.T) {
	// If Redis errors during replay, the persisted events are served from Postgres and the replay still ends at the terminal event.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(bg, step(1))
	_, _ = w.Append(bg, step(2))
	_, _ = w.Append(bg, agui.RunFinishedEvent("t", f.run.ID, "success", nil))
	_ = w.Close(bg)
	f.mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	got := collect(t, f.log.Replay(bg, f.run.ID, 1))

	// Assert
	if !equal(got, []int64{2, 3}) {
		t.Fatalf("replayed %v", got)
	}
}

func TestReplayOfALiveRunEndsWhenItsContextIsCancelled(t *testing.T) {
	// A replay of a live run ends (channel closed) when its context is cancelled, even while it waits on the stream.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(bg, step(1))
	ctx, cancel := context.WithCancel(bg)
	ch := f.log.Replay(ctx, f.run.ID, 0)
	first := <-ch

	// Act
	cancel()

	// Assert
	if first.Seq != 1 {
		t.Fatalf("first %+v", first)
	}
	if rest := collect(t, ch); len(rest) != 0 {
		t.Fatalf("events after cancel: %v", rest)
	}
}

func TestReplayStopsMidBatchWhenTheConsumerCancels(t *testing.T) {
	// A consumer that stops reading and cancels releases the replay goroutine even when more events are waiting to be sent.
	// Arrange
	f := newFixture(t)
	w := f.log.Writer(f.run.ID, 0)
	for i := 1; i <= 200; i++ {
		_, _ = w.Append(bg, step(i))
	}
	ctx, cancel := context.WithCancel(bg)
	ch := f.log.Replay(ctx, f.run.ID, 0)
	<-ch

	// Act
	cancel()
	done := make(chan struct{})
	go func() {
		for range ch {
		}
		close(done)
	}()

	// Assert
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("replay goroutine did not stop after the context was cancelled")
	}
}

func TestFinalizingKeepsReplayWaitingForTheRealTerminalEvent(t *testing.T) {
	// While a run is marked finalizing, replay of its (already terminal) row does not invent a terminal event, however long the continuations take; the real one is delivered when written. Clearing the mark removes the key.
	// Arrange
	f := newFixture(t)
	f.log.TerminalWait = 50 * time.Millisecond
	w := f.log.Writer(f.run.ID, 0)
	_, _ = w.Append(bg, step(1))
	_ = f.st.FinishRun(bg, f.run.ID, "rt", store.StatusCompleted, "", "")
	done := f.log.Finalizing(bg, f.run.ID, time.Hour)
	markTTL := f.mr.TTL("work:run:" + f.run.ID + ":finalizing")
	ch := f.log.Replay(bg, f.run.ID, 0)
	first := <-ch

	// Act
	var early *store.Event
	select {
	case e := <-ch:
		early = &e
	case <-time.After(10 * f.log.TerminalWait):
	}
	_, _ = w.Append(bg, agui.RunFinishedEvent("t", f.run.ID, "success", []agui.Usage{{Model: "real"}}))
	last := <-ch
	done()
	done()

	// Assert
	if first.Seq != 1 || early != nil {
		t.Fatalf("first %+v, early event while finalizing %+v", first, early)
	}
	if last.Seq != 2 || !strings.Contains(string(last.Data), `"model":"real"`) {
		t.Fatalf("last %+v", last)
	}
	if markTTL != time.Hour || f.mr.Exists("work:run:"+f.run.ID+":finalizing") {
		t.Fatalf("mark ttl %v, still set after clearing: %v", markTTL, f.mr.Exists("work:run:"+f.run.ID+":finalizing"))
	}
}

func TestFinalizingIsBestEffortWhenRedisIsDown(t *testing.T) {
	// A Redis failure while marking or clearing must not panic or block: Finalizing returns a usable clear func.
	// Arrange
	f := newFixture(t)
	f.mr.SetError("ERR LOADING Redis is loading the dataset in memory")

	// Act
	done := f.log.Finalizing(bg, f.run.ID, time.Minute)
	done()

	// Assert
	if done == nil {
		t.Fatal("no clear func")
	}
}

func TestReplayOfARunInProgressTailsLiveEventsUntilItsContextEnds(t *testing.T) {
	// A replay of a run that is still running waits on the live stream: events appended later are delivered in order and the channel closes only when the context ends.
	// Arrange
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(bg, 400*time.Millisecond)
	defer cancel()
	w := f.log.Writer(f.run.ID, 0)
	ch := f.log.Replay(ctx, f.run.ID, 0)

	// Act
	_, _ = w.Append(bg, step(1))
	_, _ = w.Append(bg, step(2))
	got := collect(t, ch)

	// Assert
	if !equal(got, []int64{1, 2}) {
		t.Fatalf("replayed %v", got)
	}
	if ctx.Err() == nil {
		t.Fatal("the replay ended before its context did, although the run never finished")
	}
}
