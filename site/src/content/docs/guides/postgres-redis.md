---
title: "Postgres and Redis for several processes"
description: "Share one Postgres store and one Redis queue between a web tier and any number of harness workers; migrations, configuration, readiness and shutdown."
---

One process needs neither database. When a web tier must stay small while agent runs scale independently, put the state in Postgres and the queue, event log and locks in Redis. The web processes `Send` runs and stream events; workers execute the runs. This page is the wiring; for choosing between shapes read [Choose a deployment shape](/docs/guides/deployment-shapes/), for what each part of `Infra` does read [Infrastructure](/docs/concepts/infra/).

<div class="callout breaking">

**Redis moved out of the facade in v0.3.** `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` were removed. Redis is the package `github.com/7ar-chat/harness/redisinfra`: build the client with `redisinfra.NewClient(cfg)` and pass `redisinfra.Infra(rdb, store)` as `Options.Infra`. Programs that never import `redisinfra` do not link go-redis. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

## The wiring

Web tier and worker build the same `harness.Options`. The program below is one binary with two roles, chosen here by an environment variable for brevity (your web tier will serve `hosts/httpapi` or your own endpoints instead of waiting).

```go
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/pgstore"
	"github.com/7ar-chat/harness/adapters/store/pgstore/migrate"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/redisinfra"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load() // POSTGRES_*, REDIS_*, LLM_*, ...
	if err != nil {
		log.Fatal(err)
	}

	// Postgres: the pool, the embedded migrations, then the Store.
	pool, err := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, cfg.PingDBAtStartup)
	if err != nil {
		log.Fatalf("connect to Postgres: %v", err)
	}
	defer pool.Close()
	if err := migrate.Run(ctx, pool, cfg.PostgresSchema); err != nil {
		log.Fatalf("migrate Postgres: %v", err)
	}
	store := pgstore.NewPG(pool)

	// Redis: queue, control bus, event log, locks, background jobs, skill installs.
	rdb := redisinfra.NewClient(cfg)
	defer rdb.Close()
	infra := redisinfra.Infra(rdb, store)

	h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
	if err != nil {
		log.Fatal(err)
	}

	if os.Getenv("ROLE") == "worker" {
		// The agent runs here: consume the shared queue until SIGTERM, then drain
		// for RUNTIME_SHUTDOWN_GRACE_SECONDS.
		if err := h.Run(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	// The web tier only Sends (h.Send) and streams events; it never calls h.Run.
	_ = h
	<-ctx.Done()
}
```

This program builds and vets; it was not run against a live Postgres or Redis. Started without a database it stops at the first step with `connect to Postgres: pinging database: ... lookup postgres: no such host`, which is also what you will see when `POSTGRES_HOST` is wrong.

| Step | What it does |
|---|---|
| `pgstore.NewPool(ctx, dsn, maxConns, ping)` | opens a pgx pool; `ping=true` (`PING_DB_AT_STARTUP`, default `true`) fails the start when Postgres is unreachable, `false` connects lazily |
| `migrate.Run(ctx, pool, schema)` | creates the schema (`POSTGRES_SCHEMA`, default `work`) if missing and applies the embedded SQL migrations, each in its own transaction. A Postgres advisory lock serialises concurrent migrators, so every replica may call it at start |
| `pgstore.NewPG(pool)` | the `Store` over the pool |
| `redisinfra.NewClient(cfg)` | a standalone or Sentinel failover client from `REDIS_*` and `USE_SENTINEL` |
| `redisinfra.Infra(rdb, store)` | the `harness.Infra`: event log (Redis streams over the store), queue and control bus, coordinator (sandbox and sweeper locks, run liveness), background-job list, skill installs |

An `Infra` field you leave nil falls back to its in-process implementation, so a zero `Infra` is a self-contained harness. Cancel, steer and approvals cross processes through the shared bus, and a crashed worker's runs are finished as `interrupted` by another worker.

<div class="callout warning">

**Custom tools and features must be compiled into the worker too.** The agent runs there. Build your own worker `main` on `hosts/worker` or on `harness.New` with the same `Features` and `Tools` as the web tier, rather than running the stock `harness-worker` binary.

</div>

## Configuration

Web tier and workers use identical `POSTGRES_*`, `REDIS_*` and `LLM_*`. Workers add the sandbox and runtime settings.

| Variable | Default | Meaning |
|---|---|---|
| `POSTGRES_HOST` / `POSTGRES_PORT` | `postgres` / `5432` | server |
| `POSTGRES_DB` / `POSTGRES_USER` / `POSTGRES_PASSWORD` | `db` / `postgres` / empty | database and credentials |
| `POSTGRES_SCHEMA` | `work` | schema created and migrated; also the connection `search_path` |
| `POSTGRES_SSLMODE` | `require` | set `disable` for a local container |
| `DB_POOL_MAX_CONNS` | `20` | pgx pool size |
| `PING_DB_AT_STARTUP` | `true` | `false` starts without touching Postgres or Redis |
| `REDIS_HOST` / `REDIS_PORT` | `redis` / `6379` | server |
| `REDIS_PASSWORD` / `REDIS_DB` | empty / `0` | |
| `REDIS_POOL_SIZE` | `256` | must exceed the concurrent SSE tailers: each holds a connection during `XREAD BLOCK` |
| `USE_SENTINEL` | `false` | with `REDIS_SENTINEL_HOSTS` and `REDIS_MASTER_NAME` (default `mymaster`), use a Sentinel failover client |
| `RUNTIME_CONCURRENCY` | `16` | maximum concurrent runs per process (worker flag `-concurrency`) |
| `RUNTIME_SHUTDOWN_GRACE_SECONDS` | `15` | how long a stopping process lets runs finish |

Defaults are from `config/config.go`; the complete list is [Environment variables](/docs/reference/env-vars/). Details of the two stores are on [Stores](/docs/integrations/stores/) and [Redis](/docs/integrations/redis/).

<div class="callout note">

**The stock `harness-web` is not the web tier of this shape.** It runs the runtime itself (`h.Run`). In team mode with `POSTGRES_HOST` it uses Postgres for the store, and with `REDIS_HOST` it uses Redis only to share the live feed between replicas; it does not pass `Options.Infra`, so its queue stays in-process. Use the library wiring above (or [`examples/website`](https://github.com/7ar-chat/harness/tree/main/examples/website) as a base) for a web tier that only sends.

</div>

## The worker

`harness-worker` (`app/cmd/harness-worker`, package `hosts/worker`) is the ready-made worker. It builds the pool, the migrations and `redisinfra` exactly as above, then consumes the queue.

```bash
REDIS_HOST=redis POSTGRES_HOST=db POSTGRES_PASSWORD=... LLM_API_KEY=... \
  OPENSANDBOX_URL=http://sandboxes:8090 harness-worker
```

| Flag or variable | Meaning |
|---|---|
| `-concurrency N` / `RUNTIME_CONCURRENCY` | maximum concurrent runs (default 16) |
| `-addr` / `HARNESS_WORKER_ADDR` | admin server address, default `127.0.0.1:9090` |
| `-once` | process queued runs until none is queued or running, then exit 0 (batch, CI) |
| `-standalone` | allow a private in-memory queue and store; for tests and demos |
| `-id` | worker id recorded as the run owner (default hostname plus a random suffix) |

Without `-standalone` the worker refuses to start unless Redis and Postgres are configured:

```text
harness-worker: no shared store: set POSTGRES_HOST (and the other POSTGRES_* settings), or pass -standalone
```

The sandbox backend of a worker is chosen by `SANDBOX_BACKEND` or `OPENSANDBOX_URL` ([Write a sandbox backend](/docs/guides/sandbox-backend/)); the stock worker links the `opensandbox` backend. A worker with no backend logs a warning and the agents have no tools. The startup log names the store, queue, sandbox, models, concurrency and shutdown grace.

### Readiness and metrics

The admin server answers three routes (verified against a running `-standalone` worker):

| Route | Answer |
|---|---|
| `GET /healthz` | `200 ok` while the process is up |
| `GET /readyz` | `200 ok` while the queue loop reads. `503 not ready: ...` when the consumer group does not exist yet, the loop is stuck (no queue read completed for a while with free slots; Redis down), or the worker is draining |
| `GET /metrics` | Prometheus text: `harness_worker_up`, `_ready`, `_draining`, `_uptime_seconds`, `_concurrency`, `_runs_in_flight`, `_runs_started_total`, `_runs_finished_total{status}` |

Use `/readyz` as the container health check and for load or replica decisions. The web tier built on `hosts/httpapi` has `GET /healthz` (`{"status":"ok"}`, no auth) but no `/readyz`.

### Shutdown grace

On SIGINT or SIGTERM the worker flips `harness_worker_draining` to 1, `/readyz` returns 503, it stops claiming runs and lets in-flight ones finish for `RUNTIME_SHUTDOWN_GRACE_SECONDS`. Runs still going after that end `interrupted` and are never lost. A second signal during the drain kills the process. Give the container a stop period longer than the grace.

## docker-compose

One `redis`, one `postgres`, your web service and the worker. The file is validated with `docker compose config`; the images named here are placeholders for yours (the repository's `Dockerfile` builds only `harness-web`).

```yaml
services:
  redis:
    image: redis:7
  postgres:
    image: postgres:16
    environment:
      POSTGRES_DB: harness
      POSTGRES_USER: harness
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U harness -d harness"]
      interval: 3s
      timeout: 3s
      retries: 20

  web:                                  # your program built on the library (the sending side)
    build: .
    depends_on:
      postgres: { condition: service_healthy }
    environment: &shared
      POSTGRES_HOST: postgres
      POSTGRES_DB: harness
      POSTGRES_USER: harness
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_SSLMODE: disable         # default is require
      REDIS_HOST: redis
      LLM_API_KEY: ${LLM_API_KEY}

  worker:
    image: your-registry/harness-worker:v0.3.1   # built from app/cmd/harness-worker, or your own worker main
    depends_on:
      postgres: { condition: service_healthy }
    environment:
      <<: *shared
      SANDBOX_BACKEND: opensandbox
      OPENSANDBOX_URL: ${OPENSANDBOX_URL}
      HARNESS_WORKER_ADDR: 0.0.0.0:9090
      RUNTIME_CONCURRENCY: "16"
      RUNTIME_SHUTDOWN_GRACE_SECONDS: "60"
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://127.0.0.1:9090/readyz"]
      interval: 10s
      timeout: 3s
      retries: 3
    stop_grace_period: 90s              # longer than RUNTIME_SHUTDOWN_GRACE_SECONDS
    deploy:
      replicas: 3

volumes:
  pgdata:
```

The health check needs `curl` in the worker image. `deploy.replicas` is the number of workers wanted; add workers for more agent capacity, web replicas for more connections.

## Checklist

- Both tiers run the same `harness` version and the same migrations; `migrate.Run` at start is safe from several replicas.
- `Options.Store` is the same Postgres on both; `Options.Infra` is built from the same Redis on both.
- Tools and features compiled into the worker.
- `POSTGRES_SSLMODE` is `require` unless you set it, so a local container needs `disable`.
- Worker stop period longer than `RUNTIME_SHUTDOWN_GRACE_SECONDS`; a readiness probe on `/readyz`.
- Production hardening is in the [Production checklist](/docs/operations/production/); metrics and logs in [Observability](/docs/operations/observability/).
