package main

import (
	"context"
	"fmt"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/executorstore"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
)

// withExecutors adds the run_executor tool to a worker that runs against Postgres: the CLI runs in the process that
// executes the run, with the credentials the user sealed in the shared database (as in harness-web, which serves
// them). It is installed before the factory feature so that roles naming an executor run on it. A standalone worker or
// one without POSTGRES_HOST has no credentials to read.
func withExecutors(inner featuresFunc, standalone bool, getenv func(string) string) featuresFunc {
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
		if !cfg.SandboxEnabled() {
			return features, release, nil // the executors run in the sandbox
		}
		pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
		if err != nil {
			release()
			return nil, nil, fmt.Errorf("connect to Postgres for the executors: %w", err)
		}
		api, err := product.NewExecutorAPI(teamstore.NewPG(pool), capstore.NewPG(pool, nil), executorstore.NewPG(pool, nil), product.SecretKeyFromEnv(getenv))
		if err != nil {
			pool.Close()
			release()
			return nil, nil, err
		}
		api.AllowInstall = getenv("HARNESS_EXECUTORS_INSTALL") == "1"
		var taskStore harness.ExecutorTaskStore
		if getenv("FACTORY") == "1" {
			taskStore = factorystore.NewPG(pool, nil)
		}
		// Before the factory feature (last of what inner returned when FACTORY=1): its delegate tool looks the runner up.
		at := len(features)
		if getenv("FACTORY") == "1" && at > 0 {
			at--
		}
		features = append(features[:at:at], append([]harness.Feature{api.Feature(taskStore)}, features[at:]...)...)
		return features, func() { pool.Close(); release() }, nil
	}
}
