package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/factoryenv"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/redisinfra"
)

// buildHarness is the only place this command constructs the harness; the rest of
// the command talks to the returned *harness.Harness. durable keeps threads, runs
// and event logs on disk (harness.OpenLocalStore); otherwise they live in memory.
// The returned closer releases the store.
func buildHarness(cfg *config.Config, durable bool) (*harness.Harness, io.Closer, error) {
	b, err := buildWith(context.Background(), cfg, durable, false)
	if err != nil {
		return nil, nil, err
	}
	return b.h, b.closer, nil
}

// built is what buildWith returns.
type built struct {
	h      *harness.Harness
	team   store.TeamStore
	sup    *product.Supervisor
	github *product.GitHubApp
	// factory configures the product's factory routes (nil: the factory is off).
	factory *product.FactoryOptions
	// bus shares the live feed between replicas: set in team mode when REDIS_HOST is.
	bus    product.FeedBus
	closer io.Closer
}

// buildWith is buildHarness that can also build the team store: in team mode
// the main store is Postgres when POSTGRES_HOST is set (the team tables live
// beside the threads), else the durable local store with team.json next to it.
func buildWith(ctx context.Context, cfg *config.Config, durable, team bool) (*built, error) {
	o := harness.Options{Config: cfg, Version: "web"}
	var closers multiCloser
	var ts store.TeamStore
	var fs factory.Store
	_, havePG := os.LookupEnv("POSTGRES_HOST")
	switch {
	case team && havePG:
		pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
		if err != nil {
			return nil, fmt.Errorf("connect to Postgres: %w", err)
		}
		closers = append(closers, poolCloser{pool.Close})
		if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil {
			_ = closers.Close()
			return nil, fmt.Errorf("migrate Postgres: %w", err)
		}
		o.Store, ts = pgstore.NewPG(pool), teamstore.NewPG(pool)
		fs = factorystore.NewPG(pool, nil)
	case durable || team:
		st, err := harness.OpenLocalStore(cfg.LocalStateDir)
		if err != nil {
			return nil, fmt.Errorf("open the session store: %w", err)
		}
		closers = append(closers, st)
		o.Store = st
		if team {
			if ts, err = teamstore.Open(filepath.Join(stateDir(cfg), "team.json")); err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the team store: %w", err)
			}
		}
	}
	b := &built{team: ts, closer: closers}
	if _, ok := os.LookupEnv("REDIS_HOST"); ok && ts != nil {
		rdb := redisinfra.NewClient(cfg)
		closers = append(closers, rdb)
		b.closer = closers
		if cfg.PingDBAtStartup {
			if err := rdb.Ping(ctx).Err(); err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("connect to Redis: %w", err)
			}
		}
		b.bus = redisx.NewFeedBus(rdb)
	}
	if ts != nil && cfg.LocalSandbox() {
		be, err := teamLocalBackend(cfg)
		if err != nil {
			_ = closers.Close()
			return nil, err
		}
		if be != nil {
			o.Backend = be
		}
	}
	if ts != nil {
		if fs == nil && os.Getenv("FACTORY") == "1" {
			// One store for the factory and for the checkout that must leave its threads alone: without
			// Postgres the factory keeps its state in factory.json beside team.json, so tasks, triggers and
			// thread links survive a restart while their pull requests stay open. Both have to look at the
			// same instance, and closing it on shutdown flushes the events not yet written.
			ff, err := factorystore.Open(filepath.Join(stateDir(cfg), "factory.json"), nil)
			if err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the factory store: %w", err)
			}
			closers = append(closers, ff)
			b.closer = closers
			fs = ff
		}
		gh, err := factoryenv.GitHubFromEnv(os.Getenv)
		if err != nil {
			_ = closers.Close()
			return nil, err
		}
		b.github = gh
		o.MemoryKey = product.MemoryKey(ts)
		o.MemoryUserKey = product.MemoryUserKey(ts, o.Store.ListMessages)
		o.MemoryShared = product.MemoryShared
		b.sup = product.NewSupervisor(ts)
		o.Tools = append(o.Tools, b.sup.Tools())
		o.Features = append(harness.ProfileNamed(cfg.AgentProfile), b.sup.Feature(),
			(&product.RepoCheckout{Team: ts, GitHub: gh, Factory: fs, Brokered: cfg.OpenSandboxCredentialProxy && cfg.SandboxBackendName() == config.BackendOpenSandbox}).Feature())
		if fo, enabled, err := factoryenv.FromEnv(os.Getenv, cfg, fs, gh, ts); err != nil {
			_ = closers.Close()
			return nil, err
		} else if enabled {
			b.factory = &product.FactoryOptions{Escalator: fo.Escalator, Hooks: fo.Hooks}
			o.Features = append(o.Features, harness.FactoryFeature(fo.Options))
		}
	}
	h, err := harness.New(o)
	if err != nil {
		_ = closers.Close()
		return nil, err
	}
	b.h = h
	return b, nil
}

// stateDir is where the durable local state lives: the session store and, in team mode, team.json and
// factory.json.
func stateDir(cfg *config.Config) string {
	if cfg.LocalStateDir != "" {
		return cfg.LocalStateDir
	}
	return filepath.Join(os.Getenv("HOME"), ".har", "sessions")
}

type multiCloser []io.Closer

func (m multiCloser) Close() error {
	var first error
	for _, c := range m {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type poolCloser struct{ f func() }

func (p poolCloser) Close() error { p.f(); return nil }
