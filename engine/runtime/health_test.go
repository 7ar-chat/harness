package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GhaziBenDahmane/harness/engine/queue"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
	"github.com/GhaziBenDahmane/harness/redisx"
)

// brokenQueue fails EnsureGroup or blocks Read past its block time (a stuck connection).
type brokenQueue struct {
	*redisx.Queue
	groupErr  error
	stuckRead chan struct{}
}

func (b brokenQueue) EnsureGroup(ctx context.Context) error {
	if b.groupErr != nil {
		return b.groupErr
	}
	return b.Queue.EnsureGroup(ctx)
}

func (b brokenQueue) Read(ctx context.Context, consumer string, count int, block time.Duration) ([]queue.Message, error) {
	if b.stuckRead != nil {
		select {
		case <-b.stuckRead:
		case <-ctx.Done(): // shutdown only
			return nil, ctx.Err()
		}
	}
	return b.Queue.Read(ctx, consumer, count, block)
}

// healthAfter starts the runtime loop with q and returns its health after wait.
func (r *rig) healthAfter(q runtime.RunQueue, wait time.Duration) error {
	r.t.Helper()
	r.opts.StuckAfter = 100 * time.Millisecond
	rt := runtime.New(r.opts, r.st, r.log, q, r.q, r.agent, redisx.NewCoordinator(r.rdb))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rt.Run(ctx); close(done) }()
	r.t.Cleanup(func() { cancel(); <-done })
	time.Sleep(wait)
	return rt.Health()
}

func TestHealthIsOKWhileTheQueueLoopReads(t *testing.T) {
	// A runtime whose consumer loop keeps reading the queue is healthy.
	// Arrange
	r := newRig(t)

	// Act
	err := r.healthAfter(r.q, 300*time.Millisecond)

	// Assert
	if err != nil {
		t.Fatal(err)
	}
}

func TestHealthFailsWhenTheConsumerGroupCannotBeCreated(t *testing.T) {
	// A runtime that cannot create its consumer group (so never consumes) reports unhealthy.
	// Arrange
	r := newRig(t)

	// Act
	err := r.healthAfter(brokenQueue{Queue: r.q, groupErr: errors.New("NOPERM")}, 50*time.Millisecond)

	// Assert
	if err == nil {
		t.Fatal("healthy without a consumer group")
	}
}

func TestHealthFailsWhenTheQueueLoopIsStuck(t *testing.T) {
	// A runtime whose queue read has not returned for longer than StuckAfter reports unhealthy.
	// Arrange
	r := newRig(t)
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })

	// Act
	err := r.healthAfter(brokenQueue{Queue: r.q, stuckRead: stuck}, 300*time.Millisecond)

	// Assert
	if err == nil {
		t.Fatal("healthy with a stuck queue loop")
	}
}
