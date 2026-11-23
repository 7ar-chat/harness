package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/blobs/s3blobs"
	"github.com/7ar-chat/harness/adapters/redisx"
	"github.com/7ar-chat/harness/adapters/store/capstore"
	"github.com/7ar-chat/harness/adapters/store/factorystore"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/adapters/store/reqstore"
	"github.com/7ar-chat/harness/adapters/store/rolestore"
	"github.com/7ar-chat/harness/adapters/store/schedstore"
	"github.com/7ar-chat/harness/adapters/store/teamstore"
	"github.com/7ar-chat/harness/adapters/store/usagestore"
	"github.com/7ar-chat/harness/app/factoryenv"
	"github.com/7ar-chat/harness/app/product"
	"github.com/7ar-chat/harness/app/usageenv"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/capabilities"
	"github.com/7ar-chat/harness/core/factory"
	"github.com/7ar-chat/harness/core/requirements"
	"github.com/7ar-chat/harness/core/roles"
	"github.com/7ar-chat/harness/core/schedule"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/core/usage"
	"github.com/7ar-chat/harness/features/artifacts"
	"github.com/7ar-chat/harness/features/schedules"
	"github.com/7ar-chat/harness/features/usageledger"
	"github.com/7ar-chat/harness/modelinfo"
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
	// preview runs the projects' apps in their sandboxes; the feature and the HTTP handler share it.
	preview *product.Preview
	// factory configures the product's factory routes (nil: the factory is off).
	factory *product.FactoryOptions
	// caps is the marketplace store (team mode only); the harness already reads its servers through it.
	caps capabilities.Store
	// reqs is the requirements store (team mode only).
	reqs requirements.Store
	// roles is the store of the roles of projects and users (team mode only); the harness resolves them through it.
	roles roles.Store
	// skills serves the connections of skills that need an account (nil without the marketplace).
	skills *product.SkillAPI
	// bus shares the live feed between replicas: set in team mode when REDIS_HOST is.
	bus product.FeedBus
	// usage is the ledger and the spend caps, sched the scheduled tasks (team mode only); locker shares the
	// scheduler's tick between replicas when there is Redis.
	usage  usage.Store
	sched  schedule.Store
	locker schedules.Locker
	// price prices runs (nil: tokens only).
	price  func(string) (modelinfo.Info, bool)
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
	var caps capabilities.Store
	var reqs requirements.Store
	var rs roles.Store
	var us usage.Store
	var ss schedule.Store
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
		caps = capstore.NewPG(pool, nil)
		reqs = reqstore.NewPG(pool, nil)
		rs = rolestore.NewPG(pool, nil)
		us, ss = usagestore.NewPG(pool, nil), schedstore.NewPG(pool, nil)
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
		if team {
			// Like the factory's, the marketplace keeps its state beside team.json when there is no Postgres.
			cf, err := capstore.Open(filepath.Join(stateDir(cfg), "capabilities.json"), nil)
			if err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the capabilities store: %w", err)
			}
			caps = cf
			rf, err := reqstore.Open(filepath.Join(stateDir(cfg), "requirements.json"), nil)
			if err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the requirements store: %w", err)
			}
			reqs = rf
			rlf, err := rolestore.Open(filepath.Join(stateDir(cfg), "roles.json"), nil)
			if err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the roles store: %w", err)
			}
			rs = rlf
			uf, err := usagestore.Open(filepath.Join(stateDir(cfg), "usage"))
			if err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the usage ledger: %w", err)
			}
			closers = append(closers, uf)
			sf, err := schedstore.Open(filepath.Join(stateDir(cfg), "schedules.json"))
			if err != nil {
				_ = closers.Close()
				return nil, fmt.Errorf("open the schedule store: %w", err)
			}
			us, ss = uf, sf
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
		b.locker = redisx.NewCoordinator(rdb)
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
		if us != nil {
			// Every finished run is booked (priced) and a run that would start over a monthly cap is refused. The hook
			// is the last one: it reads the usage of the title and goal hooks too.
			b.usage, b.sched, b.price = us, ss, usageenv.Price(cfg)
			o.Hooks = append(o.Hooks, usageledger.Hook(usageledger.Options{
				Store: us, Price: b.price, Resolve: product.UsageAttribution(ts, ss, fs),
			}))
			o.RunGate = usageledger.Gate(us, product.UsageGateResolver(ts), nil)
		}
		if err := installMarketplace(ctx, &o, ts, caps); err != nil {
			_ = closers.Close()
			return nil, err
		}
		b.caps, b.reqs = caps, reqs
		if rs != nil {
			b.roles = rs
			o.Roles = product.NewRoleRegistry(ts, rs)
		}
		if caps != nil {
			if b.skills, err = product.NewSkillAPI(ts, caps, product.SecretKeyFromEnv(os.Getenv)); err != nil {
				_ = closers.Close()
				return nil, err
			}
			o.ExtraSkills = product.Skills()
		}
		o.MemoryUserKey = product.MemoryUserKey(ts, o.Store.ListMessages)
		o.MemoryShared = product.MemoryShared
		b.sup = product.NewSupervisor(ts)
		o.Tools = append(o.Tools, b.sup.Tools())
		features := harness.ProfileNamed(cfg.AgentProfile)
		if b.skills != nil {
			features = append(features, b.skills.Feature()) // after the profile: it reads the skills the profile loads
		}
		b.preview = &product.Preview{}
		features = append(features, b.preview.Feature())
		features = append(features, product.RequirementsFeature(reqs))
		o.Features = append(features, b.sup.Feature(),
			(&product.RepoCheckout{Team: ts, GitHub: gh, Factory: fs, Brokered: cfg.OpenSandboxCredentialProxy && cfg.SandboxBackendName() == config.BackendOpenSandbox}).Feature())
		if fo, enabled, err := factoryenv.FromEnv(os.Getenv, cfg, fs, gh, ts); err != nil {
			_ = closers.Close()
			return nil, err
		} else if enabled {
			b.factory = &product.FactoryOptions{Escalator: fo.Escalator, Hooks: fo.Hooks}
			o.Features = append(o.Features, harness.FactoryFeature(fo.Options))
		}
	}
	if cfg.ArtifactsS3Bucket != "" {
		// Content (artifacts, attachments, the files of projects without a repository) goes to the S3-compatible store
		// (RustFS, AWS...). What was stored in the database before stays readable.
		s3, err := s3blobs.New(s3blobs.Options{Bucket: cfg.ArtifactsS3Bucket, Endpoint: cfg.ArtifactsS3Endpoint, Region: cfg.ArtifactsS3Region,
			AccessKey: cfg.AWSAccessKeyID, SecretKey: cfg.AWSSecretAccessKey, PathStyle: cfg.ArtifactsS3PathStyle, Prefix: cfg.ArtifactsS3Prefix})
		if err != nil {
			_ = closers.Close()
			return nil, fmt.Errorf("configure the S3 blob store: %w", err)
		}
		if cfg.ArtifactsS3CreateBucket {
			if err := s3.EnsureBucket(ctx); err != nil {
				_ = closers.Close()
				return nil, err
			}
		}
		o.Blobs = artifacts.FallbackBlobs{Primary: s3, Legacy: artifacts.PGBlobs{S: o.Store}}
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
