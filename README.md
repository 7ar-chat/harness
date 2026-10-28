# harness

The agent harness behind Studio's **Work Chat**, extracted as a library: a ReAct loop
([eino](https://github.com/cloudwego/eino)) whose tools run in a **remote sandbox**
([OpenSandbox](https://github.com/alibaba/OpenSandbox)), streaming
[AG-UI](https://docs.ag-ui.com) events. It does not spawn Claude Code/Codex: the Go loop drives the
model, the sandbox only executes.

It is meant to sit behind any front end — a web server, a TUI, a desktop app.

```
        web UI ──► httpserver (net/http, SSE) ─┐
        TUI    ──► cmd/harness (reference)  ───┤
        your app ──────────────────────────────┤
                                               ▼
                                    harness.Harness  (Send / Events / Cancel / Steer)
                                               │
        ┌───────────── runtime (claim, heartbeat, hooks) ─────────────┐
        │  agent loop ── llm (OpenAI chat / Responses, gateway)       │
        │      │         compaction · steering · approvals · goals    │
        │      ▼                                                      │
        │  tools ── sandboxtools (exec, read, edit, search, view…)    │
        │           skilltools · memorytools · harnesstools · mcptools│
        └──────────────┬───────────────────────────┬──────────────────┘
                       ▼                           ▼
              sandbox.Manager ──► Backend    store.Store · eventlog.Log · queue
              (OpenSandbox | your own)       (memory | Postgres+Redis)
```

## Use it

```go
cfg, _ := config.Load()                       // env: LLM_*, OPENSANDBOX_*, …
h, _ := harness.New(harness.Options{Config: cfg})
go h.Run(ctx)                                 // consumes queued runs

th, _ := h.NewThread(ctx, "alice", "")
run, _ := h.Send(ctx, "alice", th.ID, "Make a chart of …", harness.RunOptions{Model: "gpt-5"})
for ev := range h.Events(ctx, run.ID, 0) {    // AG-UI events, ev.Seq is the resume point
    fmt.Println(ev.Type, string(ev.Data))
}
h.Steer(ctx, "alice", run.ID, "use blue instead")   // message to a running run
h.Cancel(ctx, "alice", run.ID)
```

* **Web**: `httpserver.New(h, httpserver.Options{User: …})` serves threads/runs as JSON and the
  run's events as SSE (`id` = seq, so `EventSource` reconnects resume without loss).
* **TUI**: `cmd/harness` is a 150-line terminal client; render the AG-UI events your way.
* **Single process** (default): store, event log and queue live in memory — nothing to run
  except the sandbox server. **Multi process**: pass `Options.Store` (`store/pgstore` +
  `store/pgstore/migrate`) and `Options.Redis`; replicas then share runs, logs, sandbox locks
  and skill installs, and a crashed runtime's runs are finished as `interrupted` by another.
* **Local, no container**: `SANDBOX_BACKEND=local` (optionally `LOCAL_WORKSPACE=dir`, default the current
  directory) runs the tools on this machine through `sandbox/local`; the agent still sees `/workspace`.
  There is no isolation beyond path confinement of the file API: use it on your own machine or inside a container.
* **Your own sandbox**: implement `sandbox.Backend` (Create/Connect/Kill) returning
  `sandbox.Instance` (Exec, ReadFile, WriteFile, MkdirAll, List, Stat, Healthy…).
  `sandbox/sandboxtest` is an in-memory fake for tests.
* **Your own tools**: `Options.Tools` takes `tools.Provider`s (eino tools, per-run).

## Coding profile

`AGENT_PROFILE=coding` switches the system prompt to a software-engineering one (orient with search, read
before edit, smallest change, run the project's own build/tests before finishing, no commits or
destructive commands unless asked) and puts the repository's `AGENTS.md` (else `CLAUDE.md`) into the
prompt. An explicit `LLM_PROMPT_VARIANTS` still wins per model. Typical terminal setup:

```
AGENT_PROFILE=coding SANDBOX_BACKEND=local LLM_PROVIDER=anthropic LLM_API_KEY=… \
LLM_MODELS=claude-sonnet-5-5 LLM_DEFAULT_MODEL=claude-sonnet-5-5 harness
```

## Providers that route by session

`LLM_SESSION_HEADER=x-opencode-session` sends the thread id in that header on every model call. OpenCode Go
requires it (it routes a conversation to one backend, which keeps the prompt cache warm: 86% cached on the
benchmark against 58–68% through a router without it):

```
LLM_PROVIDER=openai LLM_BASE_URL=https://opencode.ai/zen/go/v1 LLM_API_KEY=… \
LLM_SESSION_HEADER=x-opencode-session LLM_MODELS=deepseek-v4.1-flash LLM_DEFAULT_MODEL=deepseek-v4.1-flash
```

## Benchmark

`bench/` runs coding tasks through the real harness (local sandbox, coding profile) and reports pass/fail,
steps, tool calls, tokens, prompt-cache hits, wall time and memory. Each task has hidden tests restored
before verification and a reference solution; `go test ./bench` proves every task fails untouched and
passes with its solution, and exercises the runner with a scripted model (no network).

```
bench -list
bench -tasks go-fix-bugs,py-fix-bugs -n 3 -out run.json     # model from LLM_* env
bench -tier hard -n 5 -parallel 4 -out hard.json             # the hard tier, 5 repeats, 4 runs at once
bench -price-in 3 -price-cached 0.3 -price-out 15 ...        # USD per 1M tokens: adds an estimated cost
bench -compare before.json after.json                        # before/after a harness change
go test -tags live ./bench -run TestLive -v                  # same, as a test (BENCH_TASKS, BENCH_OUT)
```

Tasks have a tier: `easy` (six small tasks a small model solves in a few steps) and `hard` (multi-file features,
a bug far from its symptom, a data race checked with `go test -race`, a cross-package refactor, repository
conventions in AGENTS.md, a config format migration, a misleading README, a Node task; Python/Node/make tasks are
skipped when the program is missing). Use `-tier easy|hard` to filter and `bench -list` to see the tier.

Reports are meant for repeats (`-n`): the Markdown output has a per-task table with the pass rate (`pass 2/3`),
mean/median steps and tokens, and the JSON report keeps every run plus the per-task aggregates. Besides total
input tokens it reports uncached input per step, `(input - cached) / steps`, which is what prompt caching does
not absorb, and, when prices are given, an estimated cost; `-compare` shows both. `-parallel N` runs N tasks at
once (each run has its own working directory and state); wall time is per run, but peak RSS is process-wide, so
use `-parallel 1` for memory comparisons.

## Sandbox definition

`sandbox-image/` holds the sandbox image (`Dockerfile`: Python/Node/LibreOffice/pandoc/… toolchain,
build args for mirrors, optional CA certificates in `certs/`) and the OpenSandbox server config
(`opensandbox.toml`) with the isolation model (loopback-only ports, deny-by-default egress policy).
The Go side is `sandbox/` (`OpenSandbox` backend, `Manager`: one sandbox per thread, quota,
health, recreation notice, keep-alive, `Coordinator` for cross-replica locks).

## Packages

| Package | Role |
|---|---|
| `harness` (root) | Facade: wiring + `Send/Events/Cancel/Steer` |
| `httpserver` | `net/http` handlers + SSE for web front ends |
| `agent` | The loop: steps, streaming, retries, steering, tool hooks, approvals, spill, masking |
| `agui` | AG-UI event builders and order checker |
| `llm`, `llm/anthropic`, `llm/responses` | eino model factory: OpenAI chat, Responses API, native Anthropic Messages (prompt-cache breakpoints, `LLM_PROVIDER=anthropic`), Studio LLM gateway transport, retry |
| `compaction` | Context summarisation and token estimates |
| `sandbox`, `tools/sandboxtools` | Remote sandbox contract/manager and the file/exec/search/view tools |
| `tools/{skill,memory,harness,mcp}tools` | Skills, memory, plan/ask_user/goal and MCP tools |
| `skills`, `skills/skillsfs` | Skill packages, registry, lazy install; built-in skills (embedded) |
| `artifacts`, `workspace`, `renders` | Versioned deliverables, workspace git checkpoints, document renders |
| `approvals`, `goals`, `verify`, `titles`, `memory`, `ideas`, `websearch`, `classification` | Product features of Work Chat, usable as plugins |
| `runtime` | Run claiming, heartbeat, sweeper, end-of-run hooks |
| `store`, `store/memstore`, `store/pgstore` | Types + interface; in-memory and Postgres implementations |
| `eventlog`, `queue` | Per-run durable event log and run queue/bus (memory and Redis) |

## What stayed in Studio

The Hertz HTTP API (`httpapi`: APISIX/OIDC identity, CSRF rules, uploads, OpenAPI), the CLIs for
skill sync/import and the product's Helm/CI. Studio can later depend on this module instead of
its internal copy.

## Status / known gaps

* Lifted from `studio/work` (Go 1.26); behaviour and tests are unchanged except for the decoupling
  (`go test ./...`). `store/pgstore` tests need a Postgres (`WORK_TEST_DATABASE_URL`) and skip otherwise.
* `classification` still encodes Studio's MSIP confidentiality policy (labels `C4`, `C3-Amundi-Only`);
  swap it for your own policy before using the upload/derive paths outside Studio.
* The Anthropic skill packages and Studio's marketplace sync are not bundled (licensing /
  internal); `skills.Registry.LoadAnthropic(fs)` takes your own catalogue. Web search is not wired
  by `harness.New` yet (`websearch` package is there).
* The sandbox image was adapted for public base images and not built here.
