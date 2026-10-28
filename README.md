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
* **Your own sandbox**: implement `sandbox.Backend` (Create/Connect/Kill) returning
  `sandbox.Instance` (Exec, ReadFile, WriteFile, MkdirAll, List, Stat, Healthy…).
  `sandbox/sandboxtest` is an in-memory fake for tests.
* **Your own tools**: `Options.Tools` takes `tools.Provider`s (eino tools, per-run).

## Sessions

Local hosts keep their conversations across restarts without Postgres or Redis:

```go
st, _ := harness.OpenLocalStore("")           // "" = $LOCAL_STATE_DIR, else $XDG_STATE_HOME/harness, else ~/.local/state/harness
defer st.Close()
h, _ := harness.New(harness.Options{Config: cfg, Store: st})

ss := st.Sessions()
list, _ := ss.List(ctx, "alice", 20)          // title, updated, last message preview, active run
fork, _ := ss.Fork(ctx, "alice", threadID, messageID) // new thread with the history up to that message
ss.Delete(ctx, "alice", threadID)
// resume = keep using the thread id: h.Send(ctx, "alice", threadID, "…")
// old runs replay: h.Events(ctx, oldRunID, 0)
```

`store/filestore` wraps the in-memory store and appends every change to
`<dir>/journal.jsonl` (JSON lines, upsert records, one `write(2)` per batch). Threads, messages,
runs, plans, summaries, compactions, goals, approvals and event logs persist; artifacts, blobs, the
skills catalog and sandbox ids stay in memory. A `kill -9` loses nothing and can only tear the last
line, which is cut at the next start; fsync happens every 500 ms and on `Close` (`SyncAlways` /
`SyncNever` via `filestore.Open(dir, filestore.Options{…})`). Runs that were in flight when the
process died are marked `interrupted` (with a terminal `RUN_ERROR`) at the next start. One process
per directory (flock). The journal is compacted on open when it holds many superseded records or
deleted threads.

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
| `llm` | eino model factory: OpenAI chat + Responses API, Studio LLM gateway transport, retry |
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
