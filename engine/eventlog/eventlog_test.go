package eventlog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/eventlog"
)

// rowStore fails per event seq: permanent (data error) rows are refused, transient rows return a connection error.
// A batch containing any failing row fails as a whole, like a database transaction.
type rowStore struct {
	*memstore.Store
	permanent map[int64]bool
	transient map[int64]bool
}

func (r *rowStore) AppendEvents(ctx context.Context, runID string, evs []store.Event) error {
	for _, e := range evs {
		if r.permanent[e.Seq] {
			return store.ErrInvalidData
		}
		if r.transient[e.Seq] {
			return errors.New("connection reset by peer")
		}
	}
	return r.Store.AppendEvents(ctx, runID, evs)
}

func persistedSeqs(t *testing.T, st store.Store) []int64 {
	t.Helper()
	evs, err := st.ListEvents(context.Background(), "run", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

func events(seqs ...int64) []store.Event {
	var out []store.Event
	for _, s := range seqs {
		out = append(out, store.Event{Seq: s, Type: "STEP_STARTED", Data: []byte(`{"type":"STEP_STARTED"}`)})
	}
	return out
}

func TestPersistWritesAWholeBatchInOneCall(t *testing.T) {
	// A healthy store takes the batch; nothing remains to write.
	// Arrange
	st := &rowStore{Store: memstore.New()}

	// Act
	left, err := eventlog.Persist(context.Background(), st, "run", events(1, 2, 3))

	// Assert
	if got := persistedSeqs(t, st); err != nil || left != nil || len(got) != 3 {
		t.Fatalf("left %v err %v persisted %v", left, err, got)
	}
}

func TestPersistKeepsEverythingDuringATransientFailure(t *testing.T) {
	// A transient error keeps the whole batch for a later retry and writes nothing.
	// Arrange
	st := &rowStore{Store: memstore.New(), transient: map[int64]bool{2: true}}

	// Act
	left, err := eventlog.Persist(context.Background(), st, "run", events(1, 2, 3))

	// Assert
	if err == nil || len(left) != 3 || len(persistedSeqs(t, st)) != 0 {
		t.Fatalf("left %v err %v persisted %v", left, err, persistedSeqs(t, st))
	}
}

func TestPersistDropsOnlyRowsTheStoreRefusesPermanently(t *testing.T) {
	// A permanently refused row is dropped; its neighbours are written.
	// Arrange
	st := &rowStore{Store: memstore.New(), permanent: map[int64]bool{2: true}}

	// Act
	left, err := eventlog.Persist(context.Background(), st, "run", events(1, 2, 3))

	// Assert
	got := persistedSeqs(t, st)
	if err != nil || left != nil || len(got) != 2 || got[0] != 1 || got[1] != 3 {
		t.Fatalf("left %v err %v persisted %v", left, err, got)
	}
}

func TestPersistReturnsTheUnwrittenTailWhenATransientErrorHitsAfterADrop(t *testing.T) {
	// After a data error the batch is written row by row; a transient failure on a later row returns that row and everything after it, keeping the earlier good rows written.
	// Arrange
	st := &rowStore{Store: memstore.New(), permanent: map[int64]bool{1: true}, transient: map[int64]bool{3: true}}

	// Act
	left, err := eventlog.Persist(context.Background(), st, "run", events(1, 2, 3, 4))

	// Assert
	got := persistedSeqs(t, st)
	if err == nil || len(left) != 2 || left[0].Seq != 3 || left[1].Seq != 4 || len(got) != 1 || got[0] != 2 {
		t.Fatalf("left %v err %v persisted %v", left, err, got)
	}
}
