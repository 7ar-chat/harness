package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
)

// marketplaceServers is worker.Options.MCPServers: the runs a worker executes get the MCP servers of their project
// or user from the shared Postgres, exactly as in harness-web (the web tier decides what they are, the process that
// runs the agent connects to them). A standalone worker, or one without Postgres, has no marketplace: its state
// would be private to the process.
func marketplaceServers(getenv func(string) string, standalone bool) func(context.Context, *config.Config, harness.Store) (harness.MCPServersFunc, func(), error) {
	if standalone {
		return nil
	}
	return func(ctx context.Context, cfg *config.Config, _ harness.Store) (harness.MCPServersFunc, func(), error) {
		if _, ok := os.LookupEnv("POSTGRES_HOST"); !ok {
			return nil, nil, nil
		}
		pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to Postgres for the marketplace: %w", err)
		}
		caps := capstore.NewPG(pool, nil)
		if err := caps.SeedCatalog(ctx, product.Catalog()); err != nil {
			slog.Error("marketplace: seeding the catalog failed", "error", err)
		}
		src, err := product.NewMCPSource(teamstore.NewPG(pool), caps, nil, product.SecretKeyFromEnv(getenv))
		if err != nil {
			pool.Close()
			return nil, nil, err
		}
		return src, pool.Close, nil
	}
}
