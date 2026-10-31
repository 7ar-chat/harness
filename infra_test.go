package harness_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/eventlog"
	"github.com/7ar-chat/harness/engine/queue"
	"github.com/7ar-chat/harness/redisinfra"
)

// countingBroker counts the runs enqueued through an injected Broker.
type countingBroker struct {
	*queue.Memory
	enqueued atomic.Int32
}

func (c *countingBroker) Enqueue(ctx context.Context, runID string) error {
	c.enqueued.Add(1)
	return c.Memory.Enqueue(ctx, runID)
}

// countingCoordinator counts the run heartbeats of an injected Coordinator.
type countingCoordinator struct {
	*sandbox.LocalCoordinator
	beats atomic.Int32
}

func (c *countingCoordinator) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	c.beats.Add(1)
	return c.LocalCoordinator.Set(ctx, key, value, ttl)
}

// finishARun sends one message through h and waits for the run's last event.
func finishARun(t *testing.T, h *harness.Harness) (runID, lastEvent string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	go func() { _ = h.Run(ctx) }()
	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		t.Fatal(err)
	}
	run, err := h.Send(ctx, "alice", th.ID, "Say hello", harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	for ev := range h.Events(ctx, run.ID, 0) {
		lastEvent = ev.Type
	}
	return run.ID, lastEvent
}

func TestAHarnessRunsOnInjectedInMemoryInfrastructure(t *testing.T) {
	// Arrange
	st := memstore.New()
	broker := &countingBroker{Memory: queue.NewMemory()}
	coord := &countingCoordinator{LocalCoordinator: sandbox.NewLocalCoordinator()}
	h, err := harness.New(harness.Options{
		Config: featureConfig(t, nil), Store: st, Backend: sandboxtest.NewBackend(),
		Models: llmtest.Factory{M: llmtest.New(
			llmtest.Turn{Chunks: llmtest.Text("Hello from injected infrastructure")},
			llmtest.Turn{Chunks: llmtest.Text("A title")})},
		Infra: &harness.Infra{Log: eventlog.NewMemory(st), Broker: broker, Coordinator: coord},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	_, last := finishARun(t, h)

	// Assert
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	if n := broker.enqueued.Load(); n < 1 {
		t.Fatalf("the injected broker enqueued %d runs, want at least 1", n)
	}
	if n := coord.beats.Load(); n < 1 {
		t.Fatalf("the injected coordinator recorded %d heartbeats, want at least 1", n)
	}
}

func TestAHarnessRunsOnTheRedisAdapterOverMiniredis(t *testing.T) {
	// Arrange
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	infra := redisinfra.Infra(rdb, st)
	h, err := harness.New(harness.Options{
		Config: featureConfig(t, nil), Store: st, Backend: sandboxtest.NewBackend(), Infra: &infra,
		Models: llmtest.Factory{M: llmtest.New(
			llmtest.Turn{Chunks: llmtest.Text("Hello through Redis")},
			llmtest.Turn{Chunks: llmtest.Text("A title")})},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Act
	runID, last := finishARun(t, h)

	// Assert
	if last != agui.RunFinished {
		t.Fatalf("last event %q", last)
	}
	if !mr.Exists(redisx.EventKey(runID)) {
		t.Fatal("the run's events are not in the Redis stream")
	}
	if !mr.Exists(queue.Stream) {
		t.Fatal("the run was not queued through the Redis stream")
	}
}
