package main

import (
	"context"
	"fmt"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/adapters/store/usagestore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/app/usageenv"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/features/usageledger"
)

type featuresFunc = func(ctx context.Context, cfg *config.Config, st harness.Store) ([]harness.Feature, func(), error)

// withUsage adds the usage ledger to the features of a worker that runs against Postgres: a run is finished by the
// process that executed it, so the worker, not the web tier, books its cost (attributed to the project, the person and
// the kind of work from the same tables the web tier reads). inner builds the rest (nil: the profile of AGENT_PROFILE).
// A standalone worker or one without POSTGRES_HOST has no ledger to write to.
func withUsage(inner featuresFunc, standalone bool) featuresFunc {
	if _, havePG := os.LookupEnv("POSTGRES_HOST"); standalone || !havePG {
		return inner
	}
	return func(ctx context.Context, cfg *config.Config, st harness.Store) ([]harness.Feature, func(), error) {
		var features []harness.Feature
		release := func() {}
		if inner != nil {
			var err error
			if features, release, err = inner(ctx, cfg, st); err != nil {
				return nil, nil, err
			}
		} else {
			features = harness.ProfileNamed(cfg.AgentProfile)
		}
		pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("connect to Postgres for the usage ledger: %w", err)
		}
		hook := usageledger.Hook(usageledger.Options{
			Store: usagestore.NewPG(pool, nil), Price: usageenv.Price(cfg),
			Resolve: product.UsageAttribution(teamstore.NewPG(pool), schedstore.NewPG(pool, nil), factorystore.NewPG(pool, nil)),
		})
		features = append(features, harness.FeatureFunc("usage-ledger", func(b *harness.Builder) error {
			b.EndOfRun(hook)
			return nil
		}))
		return features, func() { pool.Close(); release() }, nil
	}
}
