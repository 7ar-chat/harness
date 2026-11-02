package runtime_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7ar-chat/harness/ai/openai"

	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/engine/runtime"
)

// failingSteerStore fails the first n AppendMessages of steers (Postgres blip at finalize).
type failingSteerStore struct {
	*memstore.Store
	fails atomic.Int32
}

func (f *failingSteerStore) AppendMessages(ctx context.Context, msgs ...*store.Message) error {
	if len(msgs) > 0 && msgs[0].IsSteer() && f.fails.Add(-1) >= 0 {
		return errors.New("dial tcp: connection refused")
	}
	return f.Store.AppendMessages(ctx, msgs...)
}

// steerThenFail queues a run whose model call receives a steer and then fails, so the steer is a leftover;
// during gets called right after the steer was accepted.
func (r *rig) steerThenFail(during func()) (*store.Thread, *store.Run) {
	r.t.Helper()
	var runID string
	r.model = llmtest.New(llmtest.Turn{Err: &openai.APIError{Status: 400}, OnCall: func() {
		if err := r.q.PushSteer(context.Background(), runID, queue.Steer{ID: "s-1", Text: "also B"}); err != nil {
			r.t.Error(err)
		}
		during()
	}})
	r.agent.Models = llmtest.Factory{M: r.model}
	th, run := r.queued("q")
	runID = run.ID
	return th, run
}

func steerKept(msgs []*store.Message, id string) bool {
	for _, m := range msgs {
		if m.ID == id && m.IsSteer() && m.Metadata["delivered"] == false {
			return true
		}
	}
	return false
}

func TestALeftoverSteerSurvivesARedisErrorAtFinalize(t *testing.T) {
	// A Redis error when the run closes its steer list is retried: the accepted steer is stored in the thread and announced.
	// Arrange
	r := newRig(t)
	th, run := r.steerThenFail(func() {
		r.mr.SetError("LOADING Redis is loading the dataset in memory")
		time.AfterFunc(300*time.Millisecond, func() { r.mr.SetError("") })
	})

	// Act
	r.process(run)

	// Assert
	if !steerKept(r.messages(th.ID), "s-1") || len(steerEvents(t, r.events(run.ID))) != 1 {
		t.Fatalf("steer not kept: messages %d steers %v", len(r.messages(th.ID)), steerEvents(t, r.events(run.ID)))
	}
}

func TestALeftoverSteerIsOnlyAnnouncedOnceStored(t *testing.T) {
	// A Postgres error storing the leftover steer is retried; it is announced as kept only once it is in the thread.
	// Arrange
	r := newRig(t)
	st := &failingSteerStore{Store: r.st}
	st.fails.Store(1)
	th, run := r.steerThenFail(func() {})
	rt := runtime.New(r.opts, st, r.log, r.q, r.q, r.agent, redisx.NewCoordinator(r.rdb))

	// Act
	_ = rt.Process(context.Background(), run.ID)

	// Assert
	if !steerKept(r.messages(th.ID), "s-1") || len(steerEvents(t, r.events(run.ID))) != 1 {
		t.Fatalf("steer not kept: steers %v", steerEvents(t, r.events(run.ID)))
	}
}

func TestTheSweeperStoresSteersARedisOutageLeftInTheList(t *testing.T) {
	// When Redis stays down for the whole finalize budget, the next sweep stores the accepted steer of the finished run.
	// Arrange
	r := newRig(t)
	r.opts.FinishRetry = 200 * time.Millisecond
	th, run := r.steerThenFail(func() { r.mr.SetError("LOADING Redis is loading the dataset in memory") })
	r.process(run)
	r.mr.SetError("")

	// Act
	r.runtime().Sweep(context.Background())

	// Assert
	if got := r.reload(run); !store.IsTerminal(got.Status) || !steerKept(r.messages(th.ID), "s-1") {
		t.Fatalf("status %s, steer kept %v", got.Status, steerKept(r.messages(th.ID), "s-1"))
	}
}
