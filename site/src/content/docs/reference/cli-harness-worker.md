---
title: "harness-worker"
description: "Flags, admin endpoints, metrics and shutdown behaviour of the headless worker that executes queued runs."
---

`harness-worker` executes agent runs that other processes queued: a `harness-web`, your own API, a scheduler. It shares the Redis queue and the Postgres store with them and exposes `/healthz`, `/readyz` and `/metrics` on an admin port. Run several workers to scale; each creates its own sandboxes. For the topology see [Postgres and Redis for several processes](/docs/guides/postgres-redis/) and [Choose a deployment shape](/docs/guides/deployment-shapes/).

```bash
REDIS_HOST=redis POSTGRES_HOST=db LLM_API_KEY=sk-... LLM_MODELS=gpt-5 LLM_DEFAULT_MODEL=gpt-5 \
OPENSANDBOX_URL=http://sandboxes:8090 harness-worker
```

Without `-standalone` the worker connects to Redis (`REDIS_*`) and requires Postgres (`POSTGRES_HOST` must be set). It runs the database migrations at start. Logs go to stderr.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `HARNESS_WORKER_ADDR`, else `127.0.0.1:9090` | Admin listen address. |
| `-concurrency` | `RUNTIME_CONCURRENCY` (16) | Maximum concurrent runs. Sets `RUNTIME_CONCURRENCY` for this process; values below 1 are raised to 1. |
| `-id` | hostname plus a random suffix | Worker id recorded in run ownership. |
| `-once` | off | Process the queued runs until none is queued or running (for 1 s), then drain and exit. For jobs and tests. |
| `-standalone` | off | Allow a private in-memory queue and store. No other process can send work to it; use for demos and tests. |

Exit codes: `0` after a clean stop, `1` for a startup failure (`harness-worker: <reason>` on stderr), `2` for a flag parse error.

Startup refusals worth knowing:

| Message | Cause |
|---|---|
| `harness-worker: no shared store: set POSTGRES_HOST (and the other POSTGRES_* settings), or pass -standalone` | Not standalone and no `POSTGRES_HOST`. |
| `harness-worker: connect to Redis: ...` | Redis unreachable (when `PING_DB_AT_STARTUP` is on). |
| `worker: the run queue is in memory, private to this process, so no other process can queue work for it (connect Redis, or pass Standalone)` | The queue is not shared. |

The worker's sandbox comes from the same settings as the web server: `OPENSANDBOX_URL` or `SANDBOX_BACKEND`. Without one it logs `worker: no sandbox backend (set OPENSANDBOX_URL or SANDBOX_BACKEND=local): agents have no tools`. MCP is linked in (the worker blank-imports `mcpfeature`).

## Admin endpoints

All three are `GET` and need no authentication: keep the port on a private network.

| Endpoint | Answer |
|---|---|
| `/healthz` | `200 ok` while the process runs. |
| `/readyz` | `200 ok` while the worker consumes the run queue; `503 not ready: <reason>` while draining (`draining: no longer claiming runs`) or when the queue loop is unhealthy. |
| `/metrics` | Prometheus text format (`text/plain; version=0.0.4`). |

### Metrics

| Metric | Type | Meaning |
|---|---|---|
| `harness_worker_up` | gauge | Always 1 while the process runs. |
| `harness_worker_ready` | gauge | 1 when `/readyz` would answer 200. |
| `harness_worker_draining` | gauge | 1 once shutdown has begun. |
| `harness_worker_uptime_seconds` | gauge | Seconds since start. |
| `harness_worker_concurrency` | gauge | Configured maximum of concurrent runs. |
| `harness_worker_runs_in_flight` | gauge | Runs executing on this worker. |
| `harness_worker_runs_started_total` | counter | Runs this worker started (compaction runs excluded). |
| `harness_worker_runs_finished_total{status}` | counter | Runs finished, by terminal status (`completed`, `failed`, `cancelled`, `interrupted`). Series appear after the first run of a status ends. |

A fresh worker answers:

```text
harness_worker_up 1
harness_worker_ready 1
harness_worker_draining 0
harness_worker_uptime_seconds 2
harness_worker_concurrency 16
harness_worker_runs_in_flight 0
harness_worker_runs_started_total 0
```

For dashboards and alerts see [Observability](/docs/operations/observability/).

## Shutdown

`SIGINT` or `SIGTERM` makes the worker stop claiming runs (`/readyz` turns 503, `harness_worker_draining` becomes 1), let in-flight runs finish for `RUNTIME_SHUTDOWN_GRACE_SECONDS`, then interrupt what is left and record why (the run ends `interrupted` with the message "The run was interrupted (the runtime stopped). Send your message again to continue."). A second signal during the drain kills the process.

## Runtime settings

| Variable | Default | Meaning |
|---|---|---|
| `RUNTIME_CONCURRENCY` | `16` | Maximum concurrent runs (`-concurrency` overrides). |
| `RUNTIME_SHUTDOWN_GRACE_SECONDS` | `15` | How long in-flight runs may finish after a stop signal. |
| `RUNTIME_TERMINATION_GRACE_SECONDS` | `30` | Your orchestrator's termination grace (a pod's `terminationGracePeriodSeconds`). The whole shutdown must fit it. |
| `HARNESS_WORKER_ADDR` | `127.0.0.1:9090` | Default of `-addr`. |

<div class="callout warning">

**The grace periods are validated.** `RUNTIME_SHUTDOWN_GRACE_SECONDS` plus 12 seconds (for interrupting runs, background archives and flushing the error tracker) must be at most `RUNTIME_TERMINATION_GRACE_SECONDS`, or the configuration fails to load: `RUNTIME_SHUTDOWN_GRACE_SECONDS (n) + 12 s must fit RUNTIME_TERMINATION_GRACE_SECONDS (m), the pod's termination grace`. Set your orchestrator's grace to the same value as `RUNTIME_TERMINATION_GRACE_SECONDS`.

</div>

The remaining variables (`LLM_*`, `POSTGRES_*`, `REDIS_*`, `SANDBOX_*`) are in [Environment variables](/docs/reference/env-vars/).

## Try it offline

```bash
LLM_PROVIDER=openai LLM_API_KEY=x LLM_MODELS=demo LLM_DEFAULT_MODEL=demo LLM_DISCOVER_MODELS=false \
  harness-worker -standalone -addr 127.0.0.1:9192 &
curl -s 127.0.0.1:9192/readyz        # ok
curl -s 127.0.0.1:9192/metrics
```

A standalone worker only proves the admin surface; real work arrives through the shared Redis queue (see [Postgres and Redis](/docs/guides/postgres-redis/)).
