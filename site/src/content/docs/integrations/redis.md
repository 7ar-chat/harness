---
title: Redis
description: "Share the run queue, event log, locks and live feed between web and worker processes with the redisx adapters and redisinfra."
---

Redis is what lets several harness processes cooperate: web replicas call `Send`, workers consume the queue, and any replica streams a run's events. Without it the harness is single-process and keeps all of that in memory. Redis support lives in two packages: `adapters/redisx` (the adapters) and `redisinfra` (the wiring), neither of which is linked unless you import it. For the idea, see [Infrastructure](/docs/concepts/infra/); for a full multi-process walkthrough, [Postgres and Redis for several processes](/docs/guides/postgres-redis/).

<div class="callout breaking">

**v0.3.x: Redis moved to `redisinfra`.** `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` were removed. Build the client with `redisinfra.NewClient(cfg)` and pass `redisinfra.Infra(rdb, store)` as `Options.Infra`:

`github.com/7ar-chat/harness/redisinfra`

Package `harness` no longer links go-redis, so a program that does not use Redis does not carry it. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

## Wiring example

```go
package main

import (
	"context"
	"log"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/redisinfra"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load() // POSTGRES_*, REDIS_*, USE_SENTINEL, LLM_*
	if err != nil {
		log.Fatal(err)
	}

	pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil {
		log.Fatal(err)
	}
	store := pgstore.NewPG(pool)

	rdb := redisinfra.NewClient(cfg) // Sentinel failover client or standalone
	defer rdb.Close()
	infra := redisinfra.Infra(rdb, store) // queue + bus, event log, coordinator, jobs, skill installs

	h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
	if err != nil {
		log.Fatal(err)
	}
	go h.Run(ctx) // web-only replicas skip Run and only call Send
	select {}
}
```

Use the same `Store` and `Infra` on the web and worker sides. The event log is built over the store, so pass the store you give to `harness.New`.

## What `redisinfra.Infra` builds

`harness.Infra` has five optional fields; a missing one stays in process. `redisinfra.Infra(rdb, store)` fills all five:

| `Infra` field | Adapter | Shared state | In-process default |
|---|---|---|---|
| `Broker` | `redisx.NewQueue(rdb)` | Run queue (stream `work:runs`, consumer group `runtime`), cancel on a per-run pub/sub channel, steer messages (at most 20 pending per run) | `queue.NewMemory` |
| `Log` | `redisx.NewEventLog(rdb, store)` | A Redis stream per run for live tailing (`work:run:<id>:events`, kept 24 h after the run ends), with Postgres as the durable copy and a repair set for runs whose final flush failed | `eventlog.NewMemory` over the store |
| `Coordinator` | `redisx.NewCoordinator(rdb)` | Sandbox creation locks and health streaks, sweeper and job locks, run liveness marks | `sandbox.LocalCoordinator` inside the manager |
| `Jobs` | `redisx.NewJobs(rdb)` | Running background jobs and their leases (`work:sbx:jobs`), so a restarted process resumes their keep-alives | memory only |
| `Installs` | `redisx.NewSkillInstalls(rdb)` | Installed skill revisions per sandbox (`work:skills:installed:<sandbox>`) and install locks | per process |

A sixth adapter, `redisx.NewFeedBus(rdb)`, is not part of `Infra`: it carries the team product's live feed between web replicas on the channel `work:feed` (pub/sub, best effort; a reconnecting client refetches `/api/overview`). `harness-web` builds it when `REDIS_HOST` is set in team mode. In code, hand it to `product.Options.Bus`.

With these in place, cancel, steer and approve work across processes, and a crashed runtime's runs are finished as `interrupted` by another.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `REDIS_HOST` | `redis` | Standalone server host. |
| `REDIS_PORT` | `6379` | Standalone server port. |
| `REDIS_PASSWORD` | empty | Password; also used for the Sentinels. |
| `REDIS_DB` | `0` | Database number. |
| `REDIS_POOL_SIZE` | `256` | Connection pool. It must exceed the concurrent SSE tailers: each holds a connection during `XREAD BLOCK`. |
| `USE_SENTINEL` | `false` | Use a Sentinel failover client instead of a standalone one. |
| `REDIS_SENTINEL_HOSTS` | empty | Comma-separated Sentinel addresses (`host:port,host:port`). |
| `REDIS_MASTER_NAME` | `mymaster` | Sentinel master name. |

```bash
# standalone
REDIS_HOST=redis REDIS_PORT=6379

# sentinel
USE_SENTINEL=true REDIS_SENTINEL_HOSTS=s1:26379,s2:26379,s3:26379 REDIS_MASTER_NAME=mymaster REDIS_PASSWORD=...
```

These are environment-only settings: they stay out of `config.yml`. `PING_DB_AT_STARTUP` (default `true`) makes `harness-web` and `harness-worker` ping Redis at start and fail with `connect to Redis: ...` if it is unreachable.

<div class="callout note">

**Presence of `REDIS_HOST` matters to the stock binaries.** `harness-web` enables the shared feed only when `REDIS_HOST` is set (and team mode is on). `harness-worker` always connects to Redis unless started with `-standalone`, and without `POSTGRES_HOST` it refuses to start with `no shared store: set POSTGRES_HOST (and the other POSTGRES_* settings), or pass -standalone`.

</div>

## What a multi-process deployment shares

| Concern | Shared through |
|---|---|
| Runs, messages, threads, approvals | the store (Postgres) |
| Which process executes a run, cancel and steer | the Redis queue and bus |
| A run's events, for any replica to stream and replay | the Redis stream, with Postgres as the durable copy |
| One sandbox per thread, quotas, health | the coordinator locks |
| Background jobs and skill installs | `Jobs` and `Installs` |
| Team live feed | `FeedBus` |

Not shared: the sandboxes themselves (reachable by `Connect` from any process through their id), and anything you keep in your own tools.

Custom tools and features must be compiled into the worker too, because the agent runs there. Build your own worker from `hosts/worker` with the same `Features` as the web tier. See [harness-worker](/docs/reference/cli-harness-worker/) and [Choose a deployment shape](/docs/guides/deployment-shapes/).

## Other transports

The engine knows only small interfaces (`harness.Broker`, `eventlog.Log`, `harness.Coordinator`, `sandboxtools.JobList`, `skills.InstallState`). To run on NATS or SQS, implement them and pass them in `Infra`; nothing else changes.
