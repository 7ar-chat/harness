package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/adapters/redisx"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/ai/llm/llmtest"
	"github.com/GhaziBenDahmane/harness/core/store"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

// outageStore fails event writes while down, like an unreachable Postgres.
type outageStore struct {
	*memstore.Store
	down atomic.Bool
}

func (o *outageStore) AppendEvents(ctx context.Context, id string, evs []store.Event) error {
	if o.down.Load() {
		return errors.New("dial tcp: connection refused")
	}
	return o.Store.AppendEvents(ctx, id, evs)
}

func TestSweepBackfillsEventsLostToAnOutageAtRunEnd(t *testing.T) {
	// Events a run could not persist because Postgres was down at its end are copied from Redis by the sweeper.
	// Arrange
	r := newRig(t)
	flaky := &outageStore{Store: r.st}
	r.log = redisx.NewEventLog(r.rdb, flaky)
	r.log.CloseRetry = 50 * time.Millisecond
	r.model = llmtest.New(llmtest.Turn{Chunks: llmtest.Text("answer"), OnCall: func() { flaky.down.Store(true) }})
	r.agent.Models = llmtest.Factory{M: r.model}
	rt := runtime.New(r.opts, r.st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	_, run := r.queued("q")
	_ = rt.Process(context.Background(), run.ID)
	lost := len(r.events(run.ID))
	flaky.down.Store(false)

	// Act
	rt.Sweep(context.Background())

	// Assert
	evs := r.events(run.ID)
	got := r.reload(run)
	if lost >= len(evs) || last(evs)["type"] != "RUN_FINISHED" || got.LastSeq != int64(len(evs)) || got.Status != store.StatusCompleted {
		t.Fatalf("before %d after %d types %v lastSeq %d", lost, len(evs), types(evs), got.LastSeq)
	}
}
