package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/githoststore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/app/factoryenv"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/factory"
)

// factoryFeatures is worker.Options.Features when FACTORY=1, nil otherwise (the worker then builds the profile of
// AGENT_PROFILE as before).
//
// The end-of-run hooks, the open_pr and delegate tools, the policy and the budget guard act in the process that
// executes a run, so a worker that consumes factory runs must install the factory with the same FACTORY_*
// settings as the web tier. It reads them through factoryenv and keeps the tasks in the Postgres of the worker
// (the migrations that create the factory tables already ran).
//
// Unlike harness-web the worker does not run the dispatcher, the scheduler and the pull request loop unless
// FACTORY_RUN=1: its job is executing runs. The product-only pieces stay in the web tier: no team store (task
// threads are not linked to a project), escalations are only logged (no live feed, no Telegram).
//
// A standalone worker has no Postgres and keeps the factory in memory.
func factoryFeatures(getenv func(string) string, standalone bool) func(context.Context, *config.Config, harness.Store) ([]harness.Feature, func(), error) {
	if getenv("FACTORY") != "1" {
		return nil
	}
	return func(ctx context.Context, cfg *config.Config, _ harness.Store) ([]harness.Feature, func(), error) {
		gh, err := factoryenv.GitHubFromEnv(getenv)
		if err != nil {
			return nil, nil, err
		}
		var fs factory.Store
		var hosts *product.GitHosts // GitLab, Gitea and plain git hosts: only with Postgres and a secret key
		release := func() {}
		if !standalone {
			pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
			if err != nil {
				return nil, nil, fmt.Errorf("connect to Postgres for the factory: %w", err)
			}
			release = pool.Close
			fs = factorystore.NewPG(pool, nil)
			if key := product.SecretKeyFromEnv(getenv); len(key) > 0 {
				if hosts, err = product.NewGitHosts(githoststore.NewPG(pool, nil), teamstore.NewPG(pool), key); err != nil {
					release()
					return nil, nil, err
				}
			}
		}
		o, err := factoryOptionsWithHosts(getenv, cfg, fs, gh, hosts)
		if err != nil {
			release()
			return nil, nil, err
		}
		slog.Info("worker: agent factory installed", "dispatcher", o.Run, "github_app", gh != nil)
		return append(harness.ProfileNamed(cfg.AgentProfile), harness.FactoryFeature(o)), release, nil
	}
}

// factoryOptions is what a worker installs: the FACTORY_* settings of the web tier, except that the dispatcher,
// the scheduler and the pull request loop run only when FACTORY_RUN=1, and nothing of the product is attached.
func factoryOptions(getenv func(string) string, cfg *config.Config, fs factory.Store, gh *product.GitHubApp) (harness.FactoryOptions, error) {
	return factoryOptionsWithHosts(getenv, cfg, fs, gh, nil)
}

func factoryOptionsWithHosts(getenv func(string) string, cfg *config.Config, fs factory.Store, gh *product.GitHubApp, hosts *product.GitHosts) (harness.FactoryOptions, error) {
	w, _, err := factoryenv.FromEnvWithHosts(getenv, cfg, fs, gh, nil, hosts)
	if err != nil {
		return harness.FactoryOptions{}, err
	}
	w.Options.Run = getenv("FACTORY_RUN") == "1"
	return w.Options, nil
}
