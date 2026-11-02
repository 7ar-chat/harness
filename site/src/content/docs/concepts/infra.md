---
title: Infrastructure
description: "What harness.Infra shares between processes (run queue, event log, locks, job list, skill installs), the in-process defaults, the Redis implementation, crash recovery and the web/worker split."
---

`harness.Infra` bundles everything the harness shares between processes: the run queue and control bus, the per-run event log, locks, the list of background jobs and the installed skill revisions. Every field is optional and a missing one stays in the process, so a zero `Infra` is a complete single-process harness. You only touch it when you run **several processes**, for example web replicas plus workers; then you pass `Options.Infra` built over Redis.

<div class="callout breaking">

**Breaking in v0.3.** `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` were **removed** from package `harness`, so that a program without Redis does not link go-redis. Redis now lives in its own package, `github.com/7ar-chat/harness/redisinfra`: build the client with `redisinfra.NewClient(cfg)` and pass `redisinfra.Infra(rdb, store)` as `Options.Infra`. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

```go
rdb := redisinfra.NewClient(cfg)              // REDIS_*, USE_SENTINEL
infra := redisinfra.Infra(rdb, store)         // store: the Postgres store both processes share
h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
```

## The five fields

| Field | Interface | In process (default) | Redis (`redisinfra.Infra`) |
|---|---|---|---|
| `Broker` | `Broker`: `queue.Queue` + `queue.Bus` + `runtime.RunQueue`. The run queue, and the per-run control bus for cancel and steers. | `queue.NewMemory()` | `redisx.NewQueue` |
| `Log` | `eventlog.Log`. The per-run event log with replay, so any process can stream any run. | `eventlog.NewMemory(store)` | `redisx.NewEventLog(rdb, store)`: Redis streams for live tailing, the store for durability |
| `Coordinator` | `Coordinator`: `sandbox.Coordinator` + `runtime.Locks`. Sandbox creation locks and health streaks, the sweeper and job locks, run liveness marks. | none: the sandbox manager keeps its locks in the process (`sandbox.LocalCoordinator`) | `redisx.NewCoordinator` |
| `Jobs` | `sandboxtools.JobList`. Running background jobs and their leases, so a restarted process resumes their keep-alives. | memory only | `redisx.NewJobs` |
| `Installs` | `skills.InstallState`. Which skill revisions each sandbox holds, and the install locks. | per process | `redisx.NewSkillInstalls` |

`harness.New` fills `Broker` and `Log` with the in-process versions when they are nil (the log over the store you passed), and the other three stay nil with the behaviour in the table. You can replace any subset: for NATS or SQS, implement the same interfaces and pass them in `Infra`; nothing else changes. The engine, sandbox and feature packages do not know Redis.

<div class="callout warning">

**Pass the same store.** `redisinfra.Infra(rdb, store)` makes the event log durable in `store`. If you build it over a different store than `Options.Store`, events and runs diverge. With the in-process defaults the log is over `Options.Store` automatically.

</div>

## Single process

Nothing to configure. Run queue, event log and locks are in memory, the store is whatever you chose ([the store](/docs/concepts/store/)), and `h.Run(ctx)` executes runs in the same process that calls `Send`. This is what the [smallest embedding](/docs/quickstart/embed/), the TUI and `harness-web` without Redis do. It cannot survive the process, and cancel and steer only reach runs of this process.

## The worker split

With a shared store and Redis, the work is split by role:

- **Web tier.** Calls `NewThread`, `Send`, `Events`, `Cancel`, `Steer`, `Resume`; **never calls `Run`**. `Send` stores the run and enqueues its id on the Redis queue.
- **Workers.** Call `Run`: they claim queued runs, execute the [agent loop](/docs/concepts/agent-loop/), write events to the log, and hold the sandboxes. The stock one is `harness-worker` (package `hosts/worker`, see [harness-worker](/docs/reference/cli-harness-worker/)).
- **Cancel and steer** go through the shared bus, so they reach a run on another process. Events are streamed from the shared log by whichever web replica holds the client's connection.

This program runs both roles in one process over an in-memory Redis (`miniredis`), so you can see the split offline. In production `rdb` is `redisinfra.NewClient(cfg)` and `st` is a `pgstore` store:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/redisinfra"
)

// node builds one harness process over the shared store and Redis.
func node(cfg *config.Config, st harness.Store, rdb redis.UniversalClient, id string) *harness.Harness {
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("answered by the worker")})
	infra := redisinfra.Infra(rdb, st)
	h, err := harness.New(harness.Options{
		Config: cfg, Store: st, Infra: &infra, RuntimeID: id,
		Models: llmtest.Factory{M: model}, Features: harness.ProfileMinimal(),
	})
	if err != nil {
		panic(err)
	}
	return h
}

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	mr, _ := miniredis.Run() // an in-process Redis, so this runs offline
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	st := memstore.New() // stands in for Postgres: shared by both nodes

	web := node(cfg, st, rdb, "web-1")       // calls Send, never Run
	worker := node(cfg, st, rdb, "worker-1") // calls Run, never Send
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = worker.Run(ctx) }()

	th, _ := web.NewThread(ctx, "alice", "")
	run, err := web.Send(ctx, "alice", th.ID, "hello", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
	if err != nil {
		panic(err)
	}
	for ev := range web.Events(ctx, run.ID, 0) { // streamed from the shared log
		if agui.IsTerminal(ev.Type) {
			fmt.Println("web saw:", ev.Type)
			break
		}
	}
	done, _ := st.GetRun(ctx, run.ID)
	fmt.Println("run executed by:", done.RuntimeID)
}
```

```text
web saw: RUN_FINISHED
run executed by: worker-1
```

<div class="callout note">

**Compile your tools into the worker.** The agent runs in the worker, so custom tools, features and `Options.Tools` must be part of the worker process too, with the same feature list as the web tier, and the MCP and OpenSandbox [opt-in imports](/docs/concepts/mcp/) if you use them. The stock `harness-worker` runs the default profile of `AGENT_PROFILE` and has none of your code: build your own worker `main` that calls `harness.New` as above and then `h.Run(ctx)`.

</div>

`hosts/worker.Run` refuses to start when its queue is in memory (`ErrPrivateQueue`: nothing could ever queue work for it) unless you pass `Standalone`. On shutdown a worker stops claiming and lets runs finish for `RUNTIME_SHUTDOWN_GRACE_SECONDS`; `RUNTIME_CONCURRENCY` (default 16) caps concurrent runs per process.

## Crashed runtimes and recovery

Every runtime heartbeats the runs it executes (in the store, and with a `Coordinator` also as a liveness key in Redis) and runs a **sweeper** every 30 seconds. With a shared `Coordinator` the sweeper holds a lock, so one runtime sweeps at a time. It:

- marks `running` runs whose heartbeat is older than 60 seconds as `interrupted` and appends a `RUN_ERROR` with code `interrupted` to their log, so streams end. It spares a run whose Redis liveness key is still present, so a Postgres outage does not interrupt live runs;
- re-enqueues runs that stayed `queued` too long (30 seconds), and reclaims queue entries left pending by stopped runtimes;
- finishes the work a crashed process left: repairs event logs whose final flush failed, stores steers that were accepted but never delivered, and runs the retention purge and periodic jobs (each once across runtimes).

A runtime that finds out it lost a run (its next heartbeat or log append shows the sweeper took over) stops that run without writing a second terminal event. The result is the status and terminal event described in [what interrupted means](/docs/concepts/agent-loop/#cancel-interrupted-and-crashes): the work done so far is kept, the user sends a message to continue. Background jobs (`execute_command` jobs that outlive a step) are tracked in `Infra.Jobs`; a restarted process resumes their keep-alives when that is shared, and forgets them when it is in memory.

Without shared infra the same marking happens at the next start of the [file store](/docs/concepts/store/#how-it-persists), because no other process exists to do it.

## Choosing

| You run | Store | Infra |
|---|---|---|
| One process, may lose state on restart | memory (default) | none |
| One process, keep conversations | `harness.OpenLocalStore` | none |
| Web replicas plus workers | `pgstore` + `migrate.Run` | `redisinfra.Infra(rdb, store)` |
| Web plus workers on NATS or SQS | `pgstore` | your own `Broker`, `Log` and `Coordinator` |

The step-by-step setup is [Postgres and Redis for several processes](/docs/guides/postgres-redis/); the Redis settings (`REDIS_HOST`, `REDIS_PORT`, `REDIS_PASSWORD`, `REDIS_DB`, `USE_SENTINEL`, `REDIS_SENTINEL_HOSTS`, `REDIS_MASTER_NAME`, `REDIS_POOL_SIZE`) are on [Redis](/docs/integrations/redis/).

## Related

- [The store](/docs/concepts/store/): what lives in Postgres or on disk.
- [Choose a deployment shape](/docs/guides/deployment-shapes/): single process, library plus API, web plus workers.
- [Production checklist](/docs/operations/production/).
