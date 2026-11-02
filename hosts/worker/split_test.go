package worker_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/hosts/worker"
	"github.com/7ar-chat/harness/redisinfra"
)

// split is the two-process deployment inside one test binary: one shared store and one shared Redis,
// a "web" harness that only Sends (never Runs) and workers started through hosts/worker.
type split struct {
	t   *testing.T
	cfg *config.Config
	st  *memstore.Store
	rdb *redis.Client
	web *harness.Harness
}

func newSplit(t *testing.T) *split {
	t.Helper()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Setenv("LLM_PROVIDER", "openai")
	t.Setenv("LLM_MODELS", "gpt-test")
	t.Setenv("RUNTIME_SHUTDOWN_GRACE_SECONDS", "1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	st := memstore.New()
	infra := redisinfra.Infra(rdb, st)
	web, err := harness.New(harness.Options{Config: cfg, Store: st, Infra: &infra,
		Models: llmtest.Factory{M: llmtest.New()}, Backend: sandboxtest.NewBackend(), RuntimeID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	return &split{t: t, cfg: cfg, st: st, rdb: rdb, web: web}
}

// startWorker runs a worker on the shared store and Redis; stop cancels it and waits for Run to return.
func (s *split) startWorker(id string, turns ...llmtest.Turn) (stop func()) {
	s.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(ctx, worker.Options{
			Config: s.cfg, Addr: "127.0.0.1:0", RuntimeID: id, Store: s.st, Redis: s.rdb,
			Models: llmtest.Factory{M: llmtest.New(turns...)}, Backend: sandboxtest.NewBackend(),
			OnListening: func(a string) { addr <- a },
		})
	}()
	<-addr
	var once bool
	stop = func() {
		if once {
			return
		}
		once = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				s.t.Errorf("worker %s: %v", id, err)
			}
		case <-time.After(20 * time.Second):
			s.t.Errorf("worker %s did not stop", id)
		}
	}
	s.t.Cleanup(stop)
	return stop
}

func (s *split) send(text string) *store.Run {
	s.t.Helper()
	ctx := context.Background()
	th, err := s.web.NewThread(ctx, "alice", "")
	if err != nil {
		s.t.Fatal(err)
	}
	run, err := s.web.Send(ctx, "alice", th.ID, text, harness.RunOptions{Model: "gpt-test"})
	if err != nil {
		s.t.Fatal(err)
	}
	return run
}

// events collects the run's stream as seen by the web process, failing after 15 s.
func (s *split) events(run *store.Run) []store.Event {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var evs []store.Event
	for ev := range s.web.Events(ctx, run.ID, 0) {
		evs = append(evs, ev)
	}
	if ctx.Err() != nil {
		s.t.Fatalf("the event stream of %s did not finish: %d events", run.ID, len(evs))
	}
	return evs
}

func types(evs []store.Event) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}

func (s *split) status(run *store.Run) string {
	s.t.Helper()
	r, err := s.st.GetUserRun(context.Background(), "alice", run.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	return r.Status
}

func texts(n int, s string) []llmtest.Turn {
	out := make([]llmtest.Turn, n)
	for i := range out {
		out[i] = llmtest.Turn{Chunks: llmtest.Text(s)}
	}
	return out
}

func TestARunQueuedByTheWebProcessIsExecutedByTheWorkerAndStreamedBack(t *testing.T) {
	// Arrange: the web process only Sends; the worker is the only consumer.
	s := newSplit(t)
	s.startWorker("worker-1", texts(4, "Hello from the worker")...)

	// Act
	run := s.send("Say hello")
	evs := s.events(run)

	// Assert
	got := types(evs)
	if got[0] != agui.RunStarted || got[len(got)-1] != agui.RunFinished {
		t.Fatalf("events %v", got)
	}
	var text strings.Builder
	for _, e := range evs {
		if e.Type == agui.TextMessageContent {
			text.Write(e.Data)
		}
	}
	if !strings.Contains(text.String(), "Hello from the worker") {
		t.Fatalf("streamed text %q", text.String())
	}
	if got := s.status(run); got != store.StatusCompleted {
		t.Fatalf("status %s", got)
	}
	if owner := s.owner(run); owner != "worker-1" {
		t.Fatalf("the run was executed by %q", owner)
	}
}

func (s *split) owner(run *store.Run) string {
	r, err := s.st.GetUserRun(context.Background(), "alice", run.ID)
	if err != nil {
		s.t.Fatal(err)
	}
	return r.RuntimeID
}

func TestAWorkerKilledMidRunInterruptsTheRunAndALaterWorkerStillServesNewRuns(t *testing.T) {
	// Arrange: the model call of the first run hangs until the worker is cancelled.
	s := newSplit(t)
	called := make(chan struct{}, 1)
	stop := s.startWorker("worker-1", llmtest.Turn{Block: true, OnCall: func() { called <- struct{}{} }})
	run := s.send("Take your time")
	select {
	case <-called:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker never called the model")
	}

	// Act: the worker is stopped while the model call is in flight (grace 1 s, then interrupted).
	stop()

	// Assert: the run is not lost; its stream ended and its status says why.
	if got := s.status(run); got != store.StatusInterrupted {
		t.Fatalf("status after the kill = %s", got)
	}
	evs := s.events(run)
	if last := evs[len(evs)-1]; last.Type != agui.RunError || !strings.Contains(string(last.Data), "interrupted") {
		t.Fatalf("last event %s %s", last.Type, last.Data)
	}

	// Act: a second worker starts and a new run is sent.
	s.startWorker("worker-2", texts(4, "Back in business")...)
	next := s.send("Are you there?")
	nextEvs := s.events(next)

	// Assert
	if got := types(nextEvs); got[len(got)-1] != agui.RunFinished {
		t.Fatalf("events %v", got)
	}
	if got := s.status(next); got != store.StatusCompleted {
		t.Fatalf("status of the new run = %s", got)
	}
	if owner := s.owner(next); owner != "worker-2" {
		t.Fatalf("the new run was executed by %q", owner)
	}
}

func TestOnceDrainsTheQueuedRunsAndReturns(t *testing.T) {
	// Arrange: two runs wait in the queue before any worker exists.
	s := newSplit(t)
	a, b := s.send("first"), s.send("second")

	// Act
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(context.Background(), worker.Options{
			Config: s.cfg, Addr: "127.0.0.1:0", Once: true, IdleFor: 300 * time.Millisecond, RuntimeID: "batch",
			Store: s.st, Redis: s.rdb, Models: llmtest.Factory{M: llmtest.New(texts(8, "done")...)},
			Backend: sandboxtest.NewBackend(),
		})
	}()

	// Assert
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("-once did not return")
	}
	for _, r := range []*store.Run{a, b} {
		if got := s.status(r); got != store.StatusCompleted {
			t.Errorf("run %s: %s", r.ID, got)
		}
	}
}
