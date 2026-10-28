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
* **TUI**: `cmd/harness` is a Bubble Tea terminal client with a print mode (see
  [Terminal UI](#terminal-ui)); package `tui` has the model and renderers to embed.
* **Single process** (default): store, event log and queue live in memory — nothing to run
  except the sandbox server. **Multi process**: pass `Options.Store` (`store/pgstore` +
  `store/pgstore/migrate`) and `Options.Redis`; replicas then share runs, logs, sandbox locks
  and skill installs, and a crashed runtime's runs are finished as `interrupted` by another.
* **Your own sandbox**: implement `sandbox.Backend` (Create/Connect/Kill) returning
  `sandbox.Instance` (Exec, ReadFile, WriteFile, MkdirAll, List, Stat, Healthy…).
  `sandbox/sandboxtest` is an in-memory fake for tests.
* **Your own tools**: `Options.Tools` takes `tools.Provider`s (eino tools, per-run).

## Terminal UI

`cmd/harness` is a terminal client (Bubble Tea) over one in-process harness and one thread.

```
harness -model gpt-5                  # interactive UI (stdin is a terminal)
harness -p "summarise main.go"        # print mode: answer on stdout, tool activity on stderr
git diff | harness -p "review this"   # print mode, the prompt is -p plus stdin
echo "what is 2+2" | harness -json    # print mode, AG-UI events as JSON lines on stdout
```

Flags: `-model` (default `LLM_DEFAULT_MODEL`), `-user`, `-p`, `-json`, `-auto` (UI: do not ask for
approval of risky tool calls). Print mode exits 0 on success, 1 when the run failed or was
cancelled, 2 when it ended waiting for an answer or approval (the question goes to stderr).

| Key | Action |
|---|---|
| `enter` | send the message; while a run is active, **steer** it |
| `alt+enter`, `ctrl+j` | new line in the input |
| `esc`, `ctrl+c` | cancel the active run; `ctrl+c` again (or twice when idle) quits |
| `ctrl+o` | expand / collapse tool calls (args, longer results, diffs) |
| `pgup`/`pgdn`, `ctrl+u`/`ctrl+d`, mouse wheel | scroll the transcript |
| `y` / `a` / `n` (or `esc`) | tool approval: approve once / approve for this run / deny |
| `1`-`9`, or text + `enter` | answer an `ask_user` question |

The status line shows the model, state, step count, tokens in/out/cached (with the cache share)
and the run's elapsed time. `edit_file` calls show a diff of `oldText`/`newText`. The UI is a pure
`tui.Model` fed by `store.Event`s through a `tui.Backend` (`tui.HarnessBackend` for a
`harness.Harness`), with one render function per event type and golden tests in `tui/testdata`
(`go test ./tui -update` rewrites them). The facade has no call to answer an interrupt, so
`HarnessBackend.Resume` creates the resume run through `h.Store` and `h.Queue`.

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
