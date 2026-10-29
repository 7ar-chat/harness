package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GhaziBenDahmane/harness"
	"github.com/GhaziBenDahmane/harness/config"
	httpserver "github.com/GhaziBenDahmane/harness/hosts/web"
	"github.com/GhaziBenDahmane/harness/store"
	"github.com/GhaziBenDahmane/harness/store/pgstore"
	"github.com/GhaziBenDahmane/harness/store/pgstore/migrate"
	"github.com/GhaziBenDahmane/harness/store/teamstore"
)

// buildHarness is the only place this command constructs the harness; the rest of
// the command talks to the returned *harness.Harness. durable keeps threads, runs
// and event logs on disk (harness.OpenLocalStore); otherwise they live in memory.
// The returned closer releases the store.
func buildHarness(cfg *config.Config, durable bool) (*harness.Harness, io.Closer, error) {
	h, _, closer, err := buildWith(context.Background(), cfg, durable, false)
	return h, closer, err
}

// buildWith is buildHarness that can also build the team store: in team mode
// the main store is Postgres when POSTGRES_HOST is set (the team tables live
// beside the threads), else the durable local store with team.json next to it.
func buildWith(ctx context.Context, cfg *config.Config, durable, team bool) (*harness.Harness, store.TeamStore, io.Closer, error) {
	o := harness.Options{Config: cfg, Version: "web"}
	var closers multiCloser
	var ts store.TeamStore
	_, havePG := os.LookupEnv("POSTGRES_HOST")
	switch {
	case team && havePG:
		pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("connect to Postgres: %w", err)
		}
		closers = append(closers, poolCloser{pool.Close})
		if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil {
			_ = closers.Close()
			return nil, nil, nil, fmt.Errorf("migrate Postgres: %w", err)
		}
		o.Store, ts = pgstore.NewPG(pool), teamstore.NewPG(pool)
	case durable || team:
		st, err := harness.OpenLocalStore(cfg.LocalStateDir)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("open the session store: %w", err)
		}
		closers = append(closers, st)
		o.Store = st
		if team {
			dir := cfg.LocalStateDir
			if dir == "" {
				dir = filepath.Join(os.Getenv("HOME"), ".har", "sessions")
			}
			if ts, err = teamstore.Open(filepath.Join(dir, "team.json")); err != nil {
				_ = closers.Close()
				return nil, nil, nil, fmt.Errorf("open the team store: %w", err)
			}
		}
	}
	if ts != nil {
		o.MemoryKey = httpserver.MemoryKey(ts)
	}
	h, err := harness.New(o)
	if err != nil {
		_ = closers.Close()
		return nil, nil, nil, err
	}
	return h, ts, closers, nil
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
