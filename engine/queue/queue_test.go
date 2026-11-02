package queue_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
)

// recorder is a MessageAppender that fails the first `failures` calls with err, then stores messages by id.
type recorder struct {
	mu       sync.Mutex
	failures int
	err      error
	stored   map[string]*store.Message
	calls    int
}

func newRecorder() *recorder { return &recorder{stored: map[string]*store.Message{}} }

func (r *recorder) AppendMessages(_ context.Context, msgs ...*store.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.failures > 0 {
		r.failures--
		return r.err
	}
	// Like the stores: a batch with a known id is refused as a whole, storing nothing.
	for _, m := range msgs {
		if _, dup := r.stored[m.ID]; dup {
			return store.ErrConflict
		}
	}
	for _, m := range msgs {
		r.stored[m.ID] = m
	}
	return nil
}

// flakyBus fails CloseSteers/AckSteers the first times they are called.
type flakyBus struct {
	*queue.Memory
	closeFailures, ackFailures int
	ackCalls                   int
}

func (b *flakyBus) CloseSteers(ctx context.Context, runID string) ([]queue.Steer, error) {
	if b.closeFailures > 0 {
		b.closeFailures--
		return nil, errors.New("redis: connection refused")
	}
	return b.Memory.CloseSteers(ctx, runID)
}

func (b *flakyBus) AckSteers(ctx context.Context, runID string, n int) error {
	b.ackCalls++
	if b.ackFailures > 0 {
		b.ackFailures--
		return errors.New("redis: connection refused")
	}
	return b.Memory.AckSteers(ctx, runID, n)
}

func pushAll(t *testing.T, bus queue.Bus, runID string, steers ...queue.Steer) {
	t.Helper()
	for _, s := range steers {
		if err := bus.PushSteer(context.Background(), runID, s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestKeepSteersStoresPendingSteersAsUndeliveredThreadMessages(t *testing.T) {
	// Pending steers of a finished run become undelivered user messages of the thread (authorship kept), are acknowledged, and later steers are refused.
	// Arrange
	ctx := context.Background()
	bus := queue.NewMemory()
	st := newRecorder()
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "first", Author: "alice", AuthorKind: "user"}, queue.Steer{ID: "s2", Text: "second", OnBehalfOf: "bob"})

	// Act
	kept, err := queue.KeepSteers(ctx, bus, st, "thread", "run", time.Now().Add(time.Minute))

	// Assert
	if err != nil || len(kept) != 2 || kept[0].ID != "s1" || kept[1].ID != "s2" {
		t.Fatalf("kept %v err %v", kept, err)
	}
	m := st.stored["s1"]
	if m == nil || m.ThreadID != "thread" || m.RunID != "run" || m.Role != store.RoleUser || m.Content != "first" ||
		m.AuthorLogin != "alice" || m.AuthorKind != "user" || m.Metadata["steer"] != true || m.Metadata["delivered"] != false {
		t.Fatalf("stored %+v", m)
	}
	if m2 := st.stored["s2"]; m2 == nil || m2.OnBehalfOf != "bob" {
		t.Fatalf("stored %+v", m2)
	}
	if runs, _ := bus.SteerRuns(ctx); len(runs) != 0 {
		t.Fatalf("stored steers must be acknowledged, still pending for %v", runs)
	}
	if err := bus.PushSteer(ctx, "run", queue.Steer{ID: "late", Text: "x"}); !errors.Is(err, queue.ErrSteersClosed) {
		t.Fatalf("late steer: %v", err)
	}
}

func TestKeepSteersWithNothingPendingStoresNothing(t *testing.T) {
	// A run without pending steers is closed and keeps nothing: no messages, no error.
	// Arrange
	ctx := context.Background()
	bus := queue.NewMemory()
	st := newRecorder()

	// Act
	kept, err := queue.KeepSteers(ctx, bus, st, "thread", "run", time.Now().Add(time.Minute))

	// Assert
	if err != nil || kept != nil || st.calls != 0 {
		t.Fatalf("kept %v err %v store calls %d", kept, err, st.calls)
	}
	if err := bus.PushSteer(ctx, "run", queue.Steer{ID: "late", Text: "x"}); !errors.Is(err, queue.ErrSteersClosed) {
		t.Fatalf("the list must be closed: %v", err)
	}
}

func TestKeepSteersRetriesTransientErrorsOfTheBusAndTheStore(t *testing.T) {
	// Transient failures of the bus (closing) and the store (saving) are retried before the deadline; the steers still end up stored once.
	// Arrange
	ctx := context.Background()
	bus := &flakyBus{Memory: queue.NewMemory(), closeFailures: 1}
	st := newRecorder()
	st.failures, st.err = 1, errors.New("connection reset")
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "hello"})

	// Act
	kept, err := queue.KeepSteers(ctx, bus, st, "thread", "run", time.Now().Add(time.Minute))

	// Assert
	if err != nil || len(kept) != 1 || len(st.stored) != 1 || st.calls != 2 {
		t.Fatalf("kept %v err %v stored %d store calls %d", kept, err, len(st.stored), st.calls)
	}
}

func TestKeepSteersGivesUpAtTheDeadlineAndLeavesSteersPending(t *testing.T) {
	// When the store keeps failing until the deadline, the error is returned, nothing is announced as kept and the steers stay pending for the sweeper.
	// Arrange
	ctx := context.Background()
	bus := queue.NewMemory()
	st := newRecorder()
	st.failures, st.err = 1000, errors.New("postgres down")
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "hello"})

	// Act
	kept, err := queue.KeepSteers(ctx, bus, st, "thread", "run", time.Now())

	// Assert
	if err == nil || err.Error() != "postgres down" || kept != nil {
		t.Fatalf("kept %v err %v", kept, err)
	}
	if runs, _ := bus.SteerRuns(ctx); len(runs) != 1 || runs[0] != "run" {
		t.Fatalf("steers must stay pending, SteerRuns %v", runs)
	}
}

func TestKeepSteersFailsWhenTheBusCannotCloseTheList(t *testing.T) {
	// If the steer list cannot be closed before the deadline nothing is stored and the error surfaces.
	// Arrange
	bus := &flakyBus{Memory: queue.NewMemory(), closeFailures: 1000}
	st := newRecorder()
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "hello"})

	// Act
	kept, err := queue.KeepSteers(context.Background(), bus, st, "thread", "run", time.Now())

	// Assert
	if err == nil || kept != nil || st.calls != 0 {
		t.Fatalf("kept %v err %v store calls %d", kept, err, st.calls)
	}
}

func TestKeepSteersTreatsAlreadyStoredSteersAsStored(t *testing.T) {
	// A sweeper that stored a steer first must not make the runtime fail: stored ids count as stored, and the rest of the batch is still saved.
	// Arrange
	ctx := context.Background()
	bus := queue.NewMemory()
	st := newRecorder()
	st.stored["s1"] = &store.Message{ID: "s1"}
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "one"}, queue.Steer{ID: "s2", Text: "two"})

	// Act
	kept, err := queue.KeepSteers(ctx, bus, st, "thread", "run", time.Now().Add(time.Minute))

	// Assert
	if err != nil || len(kept) != 2 || st.stored["s2"] == nil || st.stored["s2"].Content != "two" {
		t.Fatalf("kept %v err %v stored %v", kept, err, st.stored)
	}
	if runs, _ := bus.SteerRuns(ctx); len(runs) != 0 {
		t.Fatalf("steers must be acknowledged: %v", runs)
	}
}

func TestKeepSteersReportsAStoreErrorHitWhileRecoveringFromAConflict(t *testing.T) {
	// After a conflict the steers are saved one by one; a real error on one of them is returned, not swallowed as "already stored".
	// Arrange
	bus := queue.NewMemory()
	st := &conflictThenFail{}
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "one"})

	// Act
	kept, err := queue.KeepSteers(context.Background(), bus, st, "thread", "run", time.Now())

	// Assert
	if err == nil || err.Error() != "disk full" || kept != nil {
		t.Fatalf("kept %v err %v", kept, err)
	}
}

type conflictThenFail struct{ calls int }

func (c *conflictThenFail) AppendMessages(context.Context, ...*store.Message) error {
	c.calls++
	if c.calls == 1 {
		return store.ErrConflict
	}
	return errors.New("disk full")
}

func TestKeepSteersStillAnswersWhenAcknowledgingFails(t *testing.T) {
	// Once the steers are stored a failing acknowledgement does not turn into an error: the sweeper finds them stored already.
	// Arrange
	bus := &flakyBus{Memory: queue.NewMemory(), ackFailures: 1000}
	st := newRecorder()
	pushAll(t, bus, "run", queue.Steer{ID: "s1", Text: "hello"})

	// Act
	kept, err := queue.KeepSteers(context.Background(), bus, st, "thread", "run", time.Now())

	// Assert
	if err != nil || len(kept) != 1 || st.stored["s1"] == nil || bus.ackCalls == 0 {
		t.Fatalf("kept %v err %v stored %v acks %d", kept, err, st.stored, bus.ackCalls)
	}
}

func TestSteerMessagesBuildOneUserMessagePerSteerInOrder(t *testing.T) {
	// Each steer maps to a user message carrying its id and text, in order; no steers, no messages.
	// Arrange
	steers := []queue.Steer{{ID: "a", Text: "one"}, {ID: "b", Text: "two"}}

	// Act
	msgs := queue.SteerMessages("t", "r", steers)
	none := queue.SteerMessages("t", "r", nil)

	// Assert
	if len(msgs) != 2 || msgs[0].ID != "a" || msgs[0].Content != "one" || msgs[1].ID != "b" || msgs[1].Content != "two" || len(none) != 0 {
		t.Fatalf("messages %+v none %v", msgs, none)
	}
}
