package worker

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/adapters/store/memstore"
	"github.com/GhaziBenDahmane/harness/adapters/store/pgstore"
	"github.com/GhaziBenDahmane/harness/adapters/store/pgstore/migrate"
	"github.com/GhaziBenDahmane/harness/config"
	"github.com/GhaziBenDahmane/harness/engine/queue"
)

// Wiring says what the harness of a worker is connected to.
type Wiring struct {
	Store   string // postgres | memory | the Go type of a custom store
	Queue   string // redis | memory
	Sandbox string // opensandbox | local | custom | none
}

// built is a harness and what to release when the worker stops.
type built struct {
	h      *harness.Harness
	wiring Wiring
	close  func()
}

// buildHarness is the only place where the worker constructs a harness: everything that depends on
// the harness package (Options, hook registration, how the queue kind is detected) lives in this
// file, so a change of the facade is adapted here alone.
//
// Not standalone: Redis (REDIS_*) is always connected, and Postgres (POSTGRES_*) must be configured
// (POSTGRES_HOST set) unless the caller supplies a Store; the worker then shares its queue, event log
// and store with the processes that Send. Standalone keeps whatever is not supplied in memory.
func buildHarness(ctx context.Context, cfg *config.Config, o Options, m *metrics) (*built, error) {
	ho := harness.Options{
		Config: cfg, Store: o.Store, Models: o.Models, Backend: o.Backend, Redis: o.Redis,
		RuntimeID: o.RuntimeID, Version: "worker",
		Hooks: []runtimeHook{m},
	}
	var cleanups []func()
	cleanup := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*built, error) { cleanup(); return nil, err }

	if !o.Standalone {
		if ho.Redis == nil {
			rdb := harness.NewRedisClient(cfg)
			cleanups = append(cleanups, func() { _ = rdb.Close() })
			if cfg.PingDBAtStartup {
				if err := rdb.Ping(ctx).Err(); err != nil {
					return fail(fmt.Errorf("connect to Redis: %w", err))
				}
			}
			ho.Redis = rdb
		}
		if ho.Store == nil {
			if _, ok := os.LookupEnv("POSTGRES_HOST"); !ok {
				return fail(errors.New("no shared store: set POSTGRES_HOST (and the other POSTGRES_* settings), or pass -standalone"))
			}
			pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
			if err != nil {
				return fail(fmt.Errorf("connect to Postgres: %w", err))
			}
			cleanups = append(cleanups, pool.Close)
			if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil {
				return fail(fmt.Errorf("migrate Postgres: %w", err))
			}
			ho.Store = pgstore.NewPG(pool)
		}
	}
	h, err := harness.New(ho)
	if err != nil {
		return fail(err)
	}
	// "A run started" has no runtime hook: the agent's run-start hooks are public on the agent.
	h.Agent.Harness.RunStart = append(h.Agent.Harness.RunStart, m)
	return &built{h: h, wiring: describe(h, cfg, o), close: cleanup}, nil
}

// describe names what h is wired to.
func describe(h *harness.Harness, cfg *config.Config, o Options) Wiring {
	w := Wiring{Store: fmt.Sprintf("%T", h.Store), Queue: "redis", Sandbox: "none"}
	switch h.Store.(type) {
	case *memstore.Store:
		w.Store = "memory"
	case *pgstore.PG:
		w.Store = "postgres"
	}
	if _, ok := h.Queue.(*queue.Memory); ok {
		w.Queue = "memory"
	}
	switch {
	case o.Backend != nil:
		w.Sandbox = "custom"
	case cfg.LocalSandbox():
		w.Sandbox = "local"
	case cfg.SandboxEnabled():
		w.Sandbox = cfg.SandboxBackendName()
	}
	return w
}
