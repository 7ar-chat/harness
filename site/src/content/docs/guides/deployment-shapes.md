---
title: Choose a deployment shape
description: "Library only, library plus the HTTP handler, or a web tier with separate workers: what each runs, the exact calls, and when to move to the next."
---

The harness runs in three shapes. They use the same calls (`NewThread`, `Send`, `Events`, `Cancel`, `Steer`), so you can start with the first and move right without rewriting your handlers.

| | Library only | Library + `hosts/httpapi` | Web + workers |
|---|---|---|---|
| You write | endpoints, SSE relay | auth + mount the handler | the same, split in two processes |
| Runs execute in | your process (`h.Run`) | your process | worker processes |
| State | memory or `filestore` | memory or `filestore` | Postgres + Redis (shared) |
| Scale | one node | one node | N web x M workers |
| Use when | you want your own API shape | you want the ready JSON + SSE API | production, long runs, sandboxes on another network |

<svg class="diagram" viewBox="0 0 760 150" role="img" aria-label="Single process versus web tier and workers sharing Postgres and Redis">
<rect class="lane" x="10" y="10" width="240" height="130"/>
<text x="20" y="30" class="b">One process</text>
<rect class="box" x="30" y="45" width="200" height="30"/>
<text x="130" y="65" class="m">your handlers / httpapi</text>
<rect class="box hot" x="30" y="95" width="200" height="30"/>
<text x="130" y="115" class="m">harness: Send + Run</text>
<path class="arrow" d="M130 75 L130 93"/>
<path class="head" d="M126 90 L134 90 L130 96 Z"/>
<rect class="lane" x="280" y="10" width="470" height="130"/>
<text x="290" y="30" class="b">Web tier and workers</text>
<rect class="box" x="300" y="45" width="140" height="30"/>
<text x="370" y="65" class="m">web: Send, Events</text>
<rect class="box hot" x="300" y="95" width="140" height="30"/>
<text x="370" y="115" class="m">worker: Run</text>
<rect class="box soft" x="560" y="45" width="170" height="30"/>
<text x="645" y="65" class="m">Redis: queue, events</text>
<rect class="box soft" x="560" y="95" width="170" height="30"/>
<text x="645" y="115" class="m">Postgres: store</text>
<path class="arrow" d="M440 60 L558 60"/>
<path class="arrow" d="M440 110 L558 110"/>
<path class="arrow" d="M440 70 L558 100"/>
</svg>

## Library only

The web tier is yours: you write the endpoints and relay the events (see [Embed in a website](/docs/guides/embed-website/)). One node, one process.

```go
cfg, _ := config.Load() // LLM_*, SANDBOX_*, OPENSANDBOX_*, MAX_SANDBOXES_PER_USER...
h, err := harness.New(harness.Options{
	Config:   cfg,
	Store:    durableStore, // nil: memory, lost on restart; filestore.Open(dir) for one node
	Features: append(harness.ProfileWork(), ordersFeature(db)),
})
go h.Run(ctx) // executes queued runs in this process

th, _ := h.NewThread(ctx, userID, "")
run, _ := h.Send(ctx, userID, th.ID, text, harness.RunOptions{Model: model, Sandbox: harness.SandboxOff})
for ev := range h.Events(ctx, run.ID, 0) {
	// AG-UI events; ev.Seq is the resume point
}
```

`filestore.Open(dir)` (import `github.com/7ar-chat/harness/adapters/store/filestore`) keeps threads, runs and event logs across restarts without a database; close it on exit. Backend `nil` lets `SANDBOX_BACKEND` / `OPENSANDBOX_URL` choose; with neither, runs have no sandbox tools. Details on stores: [The store](/docs/concepts/store/) and [Stores](/docs/integrations/stores/).

## Library + `hosts/httpapi`

Same process, plus the ready-made API under a prefix. You supply only authentication.

```go
api := httpapi.New(h, httpapi.Options{
	Authenticate: userFromSession,          // func(*http.Request) (string, error)
	DefaultModel: cfg.LLMDefaultModel,
	NoUI:         true,                     // the embedded UI uses absolute /api URLs
	// AllowedOrigins: []string{"https://app.example.com"}, // only if the browser app is another origin
})
mux.Handle("/agent/", http.StripPrefix("/agent", api))
```

You get `POST /agent/api/threads`, `GET /agent/api/threads`, `POST /agent/api/threads/{id}/runs`, `GET /agent/api/runs/{id}/events` (SSE), cancel, steer, resume, files and download. The full list is the [HTTP API](/docs/reference/http-api/) reference.

## Web + workers

The web tier calls `Send` and never `Run`. Workers consume the Redis queue, execute runs and write the event log, which any web replica streams from. Cancel, steer and approvals cross processes through the shared bus, and a crashed worker's runs are finished as `interrupted` by another.

Both sides use the same options. Redis is the `redisinfra` package (an explicit import, so single-process programs do not link go-redis):

```go
// shared by web and workers
pool, _ := pgstore.NewPool(ctx, cfg.PostgresDSN(), cfg.DBPoolMaxConns, true)
_ = migrate.Run(ctx, pool, cfg.PostgresSchema) // adapters/store/pgstore/migrate
store := pgstore.NewPG(pool)

rdb := redisinfra.NewClient(cfg)
infra := redisinfra.Infra(rdb, store)

h, err := harness.New(harness.Options{
	Config:   cfg,
	Store:    store,
	Infra:    &infra,
	Features: append(harness.ProfileWork(), ordersFeature(db)), // identical on both sides
})
```

- **Web process**: serve `httpapi.New(h, ...)` or your own endpoints, and do **not** call `h.Run`.
- **Worker process**: `h.Run(ctx)` and nothing else. It stops claiming runs when `ctx` ends and drains in-flight ones for `RUNTIME_SHUTDOWN_GRACE_SECONDS`.

Configuration is environment only: `POSTGRES_*`, `REDIS_*` and `LLM_*` identical on both; `SANDBOX_BACKEND` or `OPENSANDBOX_URL`, `RUNTIME_CONCURRENCY` and `RUNTIME_SHUTDOWN_GRACE_SECONDS` on the workers.

<div class="callout breaking">

**Moved in v0.3.** `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` no longer exist. Use `redisinfra.NewClient(cfg)` and pass `redisinfra.Infra(rdb, store)` as `Options.Infra`. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

<div class="callout warning">

**Custom tools and features must be compiled into the worker too**, because the agent runs there. A worker without your `ordersFeature` has no `lookup_order` tool. Build your own worker `main` with the same `Features` as the web tier rather than running the stock `harness-worker` binary. If you use MCP or OpenSandbox, add their blank imports (`mcpfeature`, `adapters/sandbox/opensandbox`) to the worker as well; MCP and sandboxes are created where the run executes.

</div>

The stock `harness-worker` binary (see [harness-worker](/docs/reference/cli-harness-worker/)) is the right choice when you add no custom tools: it applies migrations at start and serves `/healthz`, `/readyz` and `/metrics` on `HARNESS_WORKER_ADDR`. A worker you write yourself with `h.Run` has none of those admin endpoints; add them if your orchestrator needs health checks.

The Postgres and Redis setup (migrations, sentinel, sizing) is in [Postgres and Redis for several processes](/docs/guides/postgres-redis/); what each part of `Infra` does is in [Infrastructure](/docs/concepts/infra/).

## When to move

| Symptom | Move |
|---|---|
| You need the JSON + SSE API and do not want to write it | library → library + `httpapi` |
| State must survive restarts on one node | add `filestore.Open(dir)` as `Store` |
| Runs are long, or restarts of the web tier must not interrupt them | web + workers |
| More than one web replica | web + workers (the event log must be shared) |
| Sandboxes should run on another network than the web tier | web + workers, with `SANDBOX_BACKEND` only on the workers |

Because all three shapes use the same calls, the move is a change in `main`: the store, `Infra`, and whether this process calls `h.Run`. Your handlers, tools and features do not change.

<div class="callout tip">

**Check it compiles.** The snippets above were assembled into one program (single-process, web and worker functions over `pgstore` and `redisinfra`) and built with `go vet`. They were not run against a live Postgres or Redis.

</div>
