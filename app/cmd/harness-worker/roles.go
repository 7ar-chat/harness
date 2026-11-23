package main

import (
	"context"
	"fmt"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/rolestore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/roles"
)

// rolesResolver is worker.Options.Roles: a run a worker executes in a role of its project or user applies it, from the
// shared Postgres, exactly as in harness-web. A standalone worker, or one without Postgres, has no roles: the
// store would be private to the process.
func rolesResolver(standalone bool) func(context.Context, *config.Config, harness.Store) (roles.Resolver, func(), error) {
	if standalone {
		return nil
	}
	return func(ctx context.Context, cfg *config.Config, _ harness.Store) (roles.Resolver, func(), error) {
		if _, ok := os.LookupEnv("POSTGRES_HOST"); !ok {
			return nil, nil, nil
		}
		pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
		if err != nil {
			return nil, nil, fmt.Errorf("connect to Postgres for the roles: %w", err)
		}
		return product.NewRoleRegistry(teamstore.NewPG(pool), rolestore.NewPG(pool, nil)), pool.Close, nil
	}
}
