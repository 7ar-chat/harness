---
title: "Observability"
description: "What the harness logs, reports and exposes at v0.3.1: slog logs, Sentry-protocol error reporting, Umami, worker metrics, run state, the team audit log and token usage."
---

The harness is observable through **structured logs**, **optional error reporting** to any Sentry-protocol tracker, **Prometheus metrics on the worker**, **run state and events** in the store, and, in team mode, an **audit log and live feed**. This page lists what each one contains, taken from the code at v0.3.1.

<div class="callout note">

**There is no OpenTelemetry and no distributed tracing.** The repository has no OpenTelemetry dependency and no trace spans. Metrics are limited to the worker's hand-written Prometheus endpoint; `harness-web` exposes no `/metrics`.

</div>

## Logs

All logging uses Go's `log/slog` with a text handler on stderr (`time=... level=... msg=... key=value`). The level is fixed in each command; there is no environment variable for it.

| Command | Level | Format |
|---|---|---|
| `harness-web` | `WARN` and above | text |
| `harness-worker` | `INFO` and above | text |
| `harness` (CLI/TUI) | `WARN` and above | text |

An embedding application chooses its own handler with `slog.SetDefault`; the library logs through the default logger. Records carry ids as attributes (`run_id`, `thread_id`, `runtime_id`, `sandbox_id`), not message text. Representative records:

```text
level=INFO msg="worker starting" store=postgres queue=redis sandbox=opensandbox models=[...] concurrency=16 shutdown_grace_s=15 standalone=false once=false admin=127.0.0.1:9090
level=INFO msg="runtime started" runtime_id=host-1a2b3c4d concurrency=16
level=WARN msg="run failed" run_id=... code=model_error detail="..."
level=ERROR msg="agent panic" run_id=... panic=...
level=WARN msg="sweeper: run interrupted (runtime stopped heartbeating)" run_id=... runtime_id=...
level=INFO msg="worker draining" in_flight=2
```

The worker's startup record names the store, queue and sandbox backend it is wired to, which is the quickest check that it shares Postgres and Redis with the web process.

<div class="callout tip">

**`harness-web` hides `INFO`.** Permission asks (`permissions: asking`), runtime start and sweeper repairs are `INFO` and never appear from the web binary. Run the worker, or embed the library with your own handler, when you need them.

</div>

## Server error reporting: `SENTRY_DSN`

`harness-web` only. With `SENTRY_DSN` set, every `slog.Error` of the process is also sent to a Sentry-protocol tracker (Sentry, GlitchTip, Rustrak). Failed runs, agent panics, storage and sweeper failures, HTTP 500s and handler panics are all logged at `ERROR`, so they all arrive.

| Item | Behaviour |
|---|---|
| Sent | Records of level `ERROR` and above. Warnings, info and context cancellations are not sent. |
| Grouping | By the log message with digit runs replaced by `#` (`run failed` is one issue however many runs fail). The dynamic error text stays in each event. |
| Content | The message, the `error` (or `panic`) value, a stack trace, the other log attributes as the event's `log` context, and the tags `run_id` and `thread_id`. `SendDefaultPII` is off; no request body, header or user identity is attached. |
| Tags | `environment`: `SENTRY_ENVIRONMENT`, else `production` under `HARNESS_PRODUCTION`, else `development`. `release`: `SENTRY_RELEASE`. |
| Delivery | Queued and sent in the background; an unreachable tracker never blocks a request. The process flushes for up to 2 s at exit. |
| Bad DSN | Startup fails: `errtrack: [Sentry] DsnParseError: ...`. |

<div class="callout warning">

**Event content is your log attributes.** The `error` text of a failing call can include whatever the dependency put in it (a provider's message, a path). The harness sends provider details to clients as a fixed message (`The AI model could not answer. Please try again in a moment.`) but logs the detail, so it reaches the tracker.

</div>

## Browser reporting and usage: `SENTRY_BROWSER_DSN`, `UMAMI_*`

| Variable | Effect |
|---|---|
| `SENTRY_BROWSER_DSN` | The UI reports uncaught errors, unhandled rejections and a crashed screen to this project (use a different project from `SENTRY_DSN`). Collected: stack, route (the hash, never the query string), release. Cookies, headers, request bodies, query strings, local variables and user identity are switched off. A DSN must look like `https://<key>@<host>/<project>`; anything else stops `harness-web` at startup. |
| `UMAMI_URL` + `UMAMI_WEBSITE_ID` | Page views to your Umami server; a signed-in person is identified by login only, and the browser's do-not-track setting is honoured. Both or neither, or startup fails with `client analytics: the Umami URL and the website id go together`. `UMAMI_URL` is an origin without a path. |
| `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE` | Tags for the server and the browser. |

With either browser setting, the Content-Security-Policy widens to exactly the origins named, see [Security model](/docs/operations/security/#web-host-csp-cors-authentication).

### `GET /api/client-config`

The UI reads its telemetry settings at startup from this public endpoint (no authentication, `Cache-Control: no-cache`). Verified output with both settings on:

```json
{"sentry":{"dsn":"https://k@sentry.example.com/1","environment":"development"},"umami":{"scriptUrl":"https://u.example.com/script.js","websiteId":"abc"}}
```

A key is omitted when its feature is off; with nothing configured the answer is `{}`. It holds a DSN, which is a write-only key.

## Worker metrics

`harness-worker` serves `GET /healthz`, `GET /readyz` and `GET /metrics` (Prometheus text format `0.0.4`) on `HARNESS_WORKER_ADDR` (default `127.0.0.1:9090`).

| Metric | Type | Meaning |
|---|---|---|
| `harness_worker_up` | gauge | Always `1` while the process serves. |
| `harness_worker_ready` | gauge | `1` when `/readyz` would answer 200. |
| `harness_worker_draining` | gauge | `1` after a stop signal. |
| `harness_worker_uptime_seconds` | gauge | Seconds since start. |
| `harness_worker_concurrency` | gauge | Configured maximum of concurrent runs. |
| `harness_worker_runs_in_flight` | gauge | Runs executing now. |
| `harness_worker_runs_started_total` | counter | Runs started (compaction runs excluded). |
| `harness_worker_runs_finished_total{status="..."}` | counter | Runs finished, by terminal status. One series per status seen. |

Sample from a standalone worker (`curl -s :19191/metrics`, comments removed):

```text
harness_worker_up 1
harness_worker_ready 1
harness_worker_draining 0
harness_worker_uptime_seconds 2
harness_worker_concurrency 16
harness_worker_runs_in_flight 0
harness_worker_runs_started_total 0
```

Counters are per process and reset at restart. Alert on `harness_worker_ready == 0`, on `rate(harness_worker_runs_finished_total{status="failed"}[5m])`, and on `harness_worker_runs_in_flight` pinned at `harness_worker_concurrency`.

## Run status and events

Every run has a status in the store: `queued`, `running`, `completed`, `failed`, `cancelled` or `interrupted`, plus an `error` and `errorCode` for the last two failure kinds (`internal`, `interrupted`, `model_error`, ...). A run that waits for a human ends its event stream with `RUN_FINISHED` outcome `interrupt`. The per-run event log (AG-UI events, `GET /api/runs/{id}/events`) is the detailed record: tool calls, results, steers, errors. See [Threads, runs and events](/docs/concepts/threads-runs-events/) and [Event types](/docs/reference/events/).

`GET /api/overview` and the feed (below) expose the derived thread status: `idle`, `queued`, `running`, `waiting_approval`, `waiting_input`, `failed`.

## Token usage and cost

`RUN_FINISHED` carries `usage`, one entry per model used in the run (sub-agents and the compaction model under their own names):

```json
{"model":"gpt-5.6-terra","inputTokens":1200,"outputTokens":300,"totalTokens":1500,"reasoningTokens":0,"cachedInputTokens":800}
```

To meter on the server, register a `runtime.Hook` (`Options.Hooks`), which runs when a run ends on the process that executed it and sees `Finish.Result.Usage`; that code runs on workers in a web-plus-worker split. To meter in the browser or TUI, read the event.

Money: the `modelinfo` module loads a price table (LiteLLM data) in the background; `harness-web` passes it to the model list, so `GET /api/models` returns per-model prices and context windows when known. `client.ModelInfo.Cost(in, out, cached, prompt)` turns a usage entry into dollars (it reports `ok=false` for an unpriced model, and uses the tier selected by the largest single prompt). The terminal UI shows context and cost in its status line and `/cost`. The harness does not store a cost per run. See [Use core and modelinfo on their own](/docs/guides/use-core-modelinfo/).

## Audit log and live feed

In team mode (`harness-web -team`), actions of people and of the supervisor are written to an audit log and published to a live feed.

- `GET /api/projects/{id}/audit?limit=` (members; default 100) lists entries `{id, projectId, threadId, actor, onBehalfOf, action, detail, at}`. `onBehalfOf` is set when the supervisor acted for a user.
- Actions recorded include: `thread.create`, `thread.fork`, `thread.delete`, `run.send`, `run.resume`, `run.cancel`, `run.steer`, `project.create`, `project.update`, `member.set`, `member.remove`, `member.invite`, `github.connect`, `project.import`, `memory.create`, `memory.update`, `memory.delete`, `memory.confirm`, `memory.reject`, `approval.telegram` and `supervisor.send`, `.steer`, `.approval`, `.answer`, `.cancel`, `.spawn`. A resume entry names the tool that was approved.
- A failed audit write is logged as `httpserver: audit failed` at `WARN` and does not fail the request.
- `GET /api/events` is a Server-Sent Events stream of feed events `{type, projectId, threadId, runId, actor, status, title, at}`. Types: `thread.created`, `thread.updated`, `thread.deleted`, `run.started`, `run.finished` (status `completed`, `failed`, `cancelled`, `interrupted`, `waiting_approval` or `waiting_input`), `message.sent`, `member.changed`, `project.changed` (and `memory.changed`). Each subscriber receives only what it may see. Delivery is best effort with no replay: a reconnecting client refetches `/api/overview`.
- With `REDIS_HOST` set, replicas share the feed on the Redis channel `work:feed`; if publishing or subscribing fails it is logged (`feed: publishing to the bus failed`, `feed: subscribing to the bus failed`).

See [Run team mode](/docs/guides/team-mode/).

## `harness -config-dump`

Prints the effective configuration, the source of each value that was set (`flag`, `env`, `project file`, `user file`), the files read, whether the project is trusted, the MCP servers that start and those that do not (with the reason), and permission rules the project asked for and was not allowed. Secrets are masked (`****` plus the last four characters, or `****` when short). It is the first thing to run when behaviour differs between machines.

```text
project root: /work/proj
project file: (none)
user file:    (none)
auth file:    /home/me/.har/auth.json
trust:        project not trusted (run `harness trust` to let it add allow rules and MCP servers)
provider:     openai (type openai)
precedence:   flag > env > project file > user file > default

LLM_API_KEY                              ****                           (auth.json)

mcp servers not started:
  fs (/work/proj/.mcp.json): stdio: echo x: the project is not trusted, run `harness trust` to enable
```

## What to alert on

| Signal | Source |
|---|---|
| Errors you did not expect | `SENTRY_DSN` issues, `level=ERROR` lines |
| Runs failing or being interrupted | `harness_worker_runs_finished_total{status="failed"}` or `"interrupted"`; `sweeper: run interrupted` warnings |
| Worker cannot read the queue | `/readyz` 503; log `runtime: cannot create consumer group` |
| Browser breakage after a release | `SENTRY_BROWSER_DSN` project, tagged with `SENTRY_RELEASE` |
| Who approved what | the project audit log |
