// Package worker is the headless host that executes agent runs queued by other processes (a web
// server, an API, a scheduler): it connects to the shared Redis queue and Postgres store, consumes
// runs, and exposes an admin HTTP server (/healthz, /readyz, /metrics). Several workers can run in
// parallel, each with its own sandboxes.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/core/sandbox"
	"github.com/GhaziBenDahmane/harness/engine/llm"
	"github.com/GhaziBenDahmane/harness/engine/runtime"
)

// runtimeHook shortens the option literal in build.go.
type runtimeHook = runtime.Hook

// DefaultAddr is the admin address when neither Options.Addr nor HARNESS_WORKER_ADDR is set.
const DefaultAddr = "127.0.0.1:9090"

// ErrPrivateQueue: the worker's run queue lives in its own memory, so it could never receive work.
var ErrPrivateQueue = errors.New("worker: the run queue is in memory, private to this process, so no other process can queue work for it (connect Redis, or pass Standalone)")

// Options configures Run. The zero value reads everything from the environment.
type Options struct {
	// Config is the environment configuration (default: config.Load()).
	Config *config.Config
	// Addr is the admin listen address (default: HARNESS_WORKER_ADDR, else 127.0.0.1:9090).
	Addr string
	// Standalone allows a private in-memory queue and store (a single process; mostly for tests and demos).
	Standalone bool
	// Once processes the queued runs until none is queued or running, then returns.
	Once bool
	// IdleFor is how long the queue must stay empty in Once mode (default 1 s).
	IdleFor time.Duration
	// RuntimeID names this worker in run ownership (default: hostname-random).
	RuntimeID string

	// Overrides, mostly for tests; nil builds them from Config.
	Store   harness.Store
	Redis   redis.UniversalClient
	Models  llm.Factory
	Backend sandbox.Backend

	// OnListening is called with the admin address once the server accepts connections.
	OnListening func(addr string)
}

// Run builds the harness, serves the admin endpoints and consumes runs until ctx is done (or, with
// Once, until the queue is drained). On ctx done it stops claiming, lets in-flight runs finish for
// RUNTIME_SHUTDOWN_GRACE_SECONDS (then the runtime interrupts them and records why) and returns nil.
func Run(ctx context.Context, o Options) error {
	cfg := o.Config
	if cfg == nil {
		var err error
		if cfg, err = config.Load(); err != nil {
			return err
		}
	}
	addr := o.Addr
	if addr == "" {
		addr = os.Getenv("HARNESS_WORKER_ADDR")
	}
	if addr == "" {
		addr = DefaultAddr
	}
	m := newMetrics()
	b, err := buildHarness(ctx, cfg, o, m)
	if err != nil {
		return err
	}
	defer b.close()
	if b.wiring.Queue == "memory" && !o.Standalone {
		return ErrPrivateQueue
	}
	slog.Info("worker starting", "store", b.wiring.Store, "queue", b.wiring.Queue, "sandbox", b.wiring.Sandbox,
		"models", cfg.LLMModels, "concurrency", cfg.RuntimeConcurrency, "shutdown_grace_s", cfg.RuntimeShutdownGrace,
		"standalone", o.Standalone, "once", o.Once, "admin", addr)
	if b.wiring.Store == "memory" && !o.Standalone {
		slog.Warn("worker: the store is in memory; runs created by other processes will not be found")
	}
	if b.wiring.Sandbox == "none" {
		slog.Warn("worker: no sandbox backend (set OPENSANDBOX_URL or SANDBOX_BACKEND=local): agents have no tools")
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("worker: admin listen: %w", err)
	}
	srv := &http.Server{Handler: admin(b.h.Runtime.Health, m, cfg.RuntimeConcurrency), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	if o.OnListening != nil {
		o.OnListening(ln.Addr().String())
	}

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan struct{})
	go func() { defer close(done); _ = b.h.Run(runCtx) }()
	if o.Once {
		go func() { waitIdle(runCtx, b.h, m, o.IdleFor); stop() }()
	}
	<-runCtx.Done()
	m.draining.Store(true)
	slog.Info("worker draining", "in_flight", m.InFlight())
	<-done // the runtime honours RUNTIME_SHUTDOWN_GRACE_SECONDS, then interrupts what is left
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	slog.Info("worker stopped")
	return nil
}

// waitIdle returns once the runtime consumes its queue and nothing has been queued or running for idleFor.
func waitIdle(ctx context.Context, h *harness.Harness, m *metrics, idleFor time.Duration) {
	if idleFor <= 0 {
		idleFor = time.Second
	}
	var idleSince time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		queued, err := h.Store.ListQueuedRunsOlderThan(ctx, 0)
		if h.Runtime.Health() != nil || err != nil || len(queued) > 0 || m.InFlight() > 0 {
			idleSince = time.Time{}
			continue
		}
		if idleSince.IsZero() {
			idleSince = time.Now()
		}
		if time.Since(idleSince) >= idleFor {
			return
		}
	}
}
