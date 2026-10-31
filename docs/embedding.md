# Embedding the harness in a website

How a website that runs agentic tasks for many users builds on the harness. A
runnable ~250-line example is in [`examples/website`](../examples/website)
(`go test ./examples/website`); this page explains the choices behind it.

## 0. Add it to your application

```sh
go get github.com/7ar-chat/harness@latest
```

The root module is the engine: `harness.New`, the HTTP API, the features and the stores. It requires the smaller modules
(`ai`, `core`, `modelinfo`, `hosts/client`, `hosts/tui`, `adapters/sandbox`), all of which are released with it; you can also
use those on their own. `make check-embed` (and the `embed` CI job on every release tag) builds a small application in an
empty directory against the published versions and runs a conversation through it, so what is documented here is
what a stranger gets.

What a build links depends on what you import. Three parts that carry heavy SDKs are opt-in:

| You use | Import | Without it |
|---|---|---|
| Redis (several processes) | `github.com/7ar-chat/harness/redisinfra`: `redisinfra.Infra(rdb, store)` for `Options.Infra` | not linked |
| MCP servers (`MCP_SERVERS`, `.mcp.json`) | `_ "github.com/7ar-chat/harness/mcpfeature"` | `harness.New` fails and names the package, rather than ignoring the servers |
| `SANDBOX_BACKEND=opensandbox` | `_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"` | the unknown-backend error says to import it |

`TestTheFacadeLinksNoOptionalSDKs` keeps package `harness` free of go-redis, the MCP SDK and the OpenSandbox SDK. The product
and the commands are in the separate `app` module, so their dependencies (OIDC, JOSE, the terminal UI libraries) are not
part of an embedding either. Measured on the minimal embedding in `make check-embed`: __MEASURED__

## 1. Pick a shape

| | Library only | Library + `hosts/httpapi` | Web + workers |
|---|---|---|---|
| You write | endpoints, SSE relay | auth + mount the handler | the same, split in two processes |
| Runs execute in | your process (`h.Run`) | your process | `app/cmd/harness-worker` processes |
| State | memory or `filestore` | memory or `filestore` | Postgres + Redis (shared) |
| Scale | one node | one node | N web x M workers |
| Use when | you want your own API shape | you want the ready JSON+SSE API | production, long runs, sandboxes on another network |

All three use the same calls (`NewThread`, `Send`, `Events`, `Cancel`, `Steer`).
Start with the first column and move right without rewriting handlers.

### Library mode

```go
cfg, _ := config.Load()                      // LLM_*, SANDBOX_*, OPENSANDBOX_*, MAX_SANDBOXES_PER_USER...
h, err := harness.New(harness.Options{
    Config:   cfg,
    Store:    durableStore,                  // nil: memory (lost on restart); filestore.Open(dir) for one node
    Backend:  nil,                           // nil: chosen by SANDBOX_BACKEND / OPENSANDBOX_URL; none => no sandbox tools
    Features: append(harness.ProfileWork(), ordersFeature(db)), // your tools, prompt sections, hooks
})
go h.Run(ctx)                                // executes queued runs in this process

th, _ := h.NewThread(ctx, userID, "")
run, _ := h.Send(ctx, userID, th.ID, text, harness.RunOptions{Model: model, Sandbox: harness.SandboxOff})
for ev := range h.Events(ctx, run.ID, 0) { /* AG-UI events; ev.Seq is the resume point */ }
```

Every method takes the user id: it is the tenancy boundary (threads, runs,
sandboxes, memory, artifacts are all scoped by it, and a foreign id is
"not found", never "forbidden").

### `hosts/httpapi`: the ready-made API

```go
mux.Handle("/agent/", http.StripPrefix("/agent", httpapi.New(h, httpapi.Options{
    Authenticate: func(r *http.Request) (string, error) { return userFromSession(r) }, // your auth
    DefaultModel: cfg.LLMDefaultModel,
    NoUI:         true,   // the embedded UI uses absolute /api URLs; mount it only at the root
    AllowedOrigins: []string{"https://app.example.com"},  // only if the browser app is another origin
})))
```

You get `POST /agent/api/threads`, `GET /agent/api/threads`, `POST
/agent/api/threads/{id}/runs` (`{"message","model","sandbox":"auto|off|on",...}`),
`GET /agent/api/runs/{id}/events` (SSE), cancel, steer, resume (approvals,
`ask_user`), files and download. Behind your reverse proxy keep SSE unbuffered
(`X-Accel-Buffering: no` is already set; disable proxy buffering and idle timeouts under ~30 s,
the handler pings every 15 s).

Often you add a thin endpoint of your own in front of `Send` (the example's
`POST /ask`) to decide things the browser must not decide: which model, whether
the run gets a sandbox, quotas.

### Web + workers

The web tier calls `Send` and never `Run`; workers (`app/cmd/harness-worker`, see
README "Worker") consume the Redis queue, execute runs and write the event log,
which any web replica streams from. Configuration is only environment:

```
# web and workers, identical:
POSTGRES_*   REDIS_*   LLM_*
# workers only:
SANDBOX_BACKEND / OPENSANDBOX_URL   RUNTIME_CONCURRENCY   RUNTIME_SHUTDOWN_GRACE_SECONDS
```

In code, `harness.Options{Store: pgstore, Infra: &infra}` on both sides, with `infra := redisinfra.Infra(rdb, store)` (migrations:
`adapters/store/pgstore/migrate`). Custom tools and features must be compiled into the
**worker** too (the agent runs there): build your own worker `main` from
`hosts/worker` with the same `Features` as the web tier, rather than the stock
binary. Cancel/steer/approve work across processes through the shared bus. A
crashed worker's runs are finished as `interrupted` by another.

## 2. Sandbox per run

`RunOptions.Sandbox` (JSON `sandbox` on `POST .../runs`):

| Value | Tools offered | Prompt | `Backend.Create` | No backend configured |
|---|---|---|---|---|
| `auto` (default) | sandbox tools + yours | sandbox prompt | on the first command, never before | no sandbox tools, no-sandbox prompt |
| `off` | yours only | no-sandbox prompt | never | same as `auto` |
| `on` | like `auto` | like `auto` | like `auto` | `Send` fails: `ErrInvalidRequest` wrapping `ErrNoSandbox` (HTTP 400) |

The mode is stored on the run: resumed runs, goal turns and compactions of a
thread keep the mode of its latest run, so a conversation that started without
a sandbox cannot grow one by an approval round-trip. Features that need a sandbox
(command permissions, code checks, project docs, workspace checkpoints, artifact
snapshots, verification, explore) switch themselves off for `off` runs and for
harnesses with no backend: no errors, no prompt text about them.

A sensible policy: `off` for support, Q&A and API/MCP tools; `auto` when the
user asks for files, data work or code; `on` for a feature that cannot work
without one (so the failure is explicit instead of a model that apologises).
Decide it **server-side**, as the example does; do not forward a browser-supplied
`sandbox` to a user who has not paid for sandboxes.

## 3. Your own tools

A tool is a `tools.Provider` (`ai.InvokableTool`s, built per run; `ai.InferTool` derives the schema from a Go struct) registered by a Feature:

```go
func ordersFeature(db Orders) harness.Feature {
    return harness.FeatureFunc("orders", func(b *harness.Builder) error {
        b.AddTools(ordersTools{db})             // Tools(ctx, rc) is called per run; rc.Login is the user
        b.Section(func(ctx context.Context, in agent.Input) string { return "Orders: use lookup_order." })
        return nil
    })
}
```

* Build tools **per run** and close over `rc.Login`: the tool then cannot read
  another tenant's data, whatever the model or an injected document asks for.
* Return errors the model can act on with `tools.Errorf`; return the same
  answer for "missing" and "not yours".
* Remote tools: `MCP_SERVERS` (the `mcp` feature) mounts MCP servers' tools. They need no sandbox.
  Pass per-user credentials in a provider of your own rather than in the shared MCP config.
* Risky tools: name them so `APPROVAL_RISKY_TOOL_PATTERNS` (default `create|send|update|delete|post|publish|write`) matches, and run with
  `Interactive: true` so the user approves (`resume` API).
* Prompt injection: tool results are data. Keep your
  tools from returning secrets or other users' content in the first place.

## 4. Multi-tenancy checklist

- [ ] **Identity**: one stable user id per account (not an email that can be changed or reused). The `Authenticator` is the only place that trusts a credential; verify signature/session there, never a bare header from the browser.
- [ ] **Ownership**: use the harness calls with the user id: `Send`, `Resume`, `Cancel`, `Steer` and the store's `GetThread` / `GetUserRun` all check ownership. `Harness.Events` takes only a run id, so resolve the run with `Store.GetUserRun(ctx, user, runID)` first (hosts/client and hosts/httpapi do). If you add endpoints, resolve ids through `h.Store.GetThread(ctx, user, id)` first; answer 404 for foreign ids (no existence oracle). Test it (see `TestUsersSeeOnlyTheirOwnThreadsAndOrders`).
- [ ] **Sandbox quota**: `MAX_SANDBOXES_PER_USER` (default 5; one sandbox per thread, so it caps concurrent code-running conversations per user). Over it, tools return a message asking to delete an old conversation. Deleting a thread (`DELETE .../threads/{id}`) kills its sandbox: expose that in your UI. Also bound idle cost with `SANDBOX_TIMEOUT_SECONDS`.
- [ ] **Egress**: set `SANDBOX_EGRESS_ALLOWLIST` (domains the agent may reach: package index, your API). Production refuses an empty list unless `SANDBOX_EGRESS_UNRESTRICTED=true`. Never let sandboxes reach your internal network, metadata or each other (`SANDBOX_EGRESS_DENY_CIDRS`).
- [ ] **Secrets**: LLM keys, OpenSandbox keys and MCP tokens live in the process environment or your secret manager, never in a prompt, a thread, a sandbox env or a tool result. Per-user third-party tokens go into your tool providers (fetched per run), not into the conversation.
- [ ] **User directory**: set `HARNESS_NO_USER_DIR=1` on servers, workers and containers. `config.Load` then reads nothing from `~/.har` (no `auth.json`, `config.yml`, `AGENTS.md`, `skills/`, `mcp.json`), so a server account's home never leaks into tenants' runs and the process is hermetic. Supply the LLM key through the environment (`LLM_API_KEY`, or the variable a provider's `api_key_env` names) or your secret manager. `HAR_HOME` relocates the directory instead of disabling it (a container volume). A server should set `HARNESS_NO_USER_DIR=1` and supply endpoints and keys through the environment (`LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODELS`, `SANDBOX_*`, `OPENSANDBOX_*`): a `<repo>/.har/config.yml` can never change them (it cannot set providers, addresses, keys, model catalogs, the sandbox or `env:`), it can only select among what the environment allows and tighten permissions. Leave `HARNESS_TRUST_PROJECT` unset: a repository's `.mcp.json`, `.har/mcp.json` and `mcp.servers` are only started for a trusted project, and `MCP_SERVERS` is how a server declares its own.
- [ ] **Rate limits and concurrency**: limit `POST .../runs` per user and per IP in your layer (the harness does not): one active run per thread is enforced (`409 conflict`), `RUNTIME_CONCURRENCY` caps runs per process, `AGENT_MAX_STEPS` and `SANDBOX_COMMAND_TIMEOUT_SECONDS` cap one run. Cap message and upload sizes at your edge.
- [ ] **Models**: the allowed list is `LLM_MODELS`; choose the model server-side per plan.
- [ ] **Cost and usage**: see section 6.
- [ ] **Data retention**: threads, messages and artifacts persist until deleted; `HARNESS_PURGE_AFTER_DAYS` (default 30) governs the purge job for deleted threads. Deleting a user must delete their threads (`DeleteThread`), which also kills sandboxes.
- [ ] **Untrusted content**: documents, web pages and tool results are data; keep `Interactive` approvals for tools with side effects (send email, refunds).
- [ ] **Shutdown**: on SIGTERM stop accepting requests, `srv.Shutdown`, then wait for `h.Run` to return (it drains in-flight runs for `RUNTIME_SHUTDOWN_GRACE_SECONDS`, the rest end `interrupted` and can be resumed by the user).
- [ ] **Observability**: structured logs (`slog`) carry run, thread and sandbox ids, never message text; the worker exposes `/metrics`.

## 5. Streaming events to a browser

`GET /agent/api/runs/{id}/events` is Server-Sent Events:

```
: connected

id: 1
data: {"type":"RUN_STARTED","threadId":"…","runId":"…","protocolVersion":"1.0","timestamp":…}

id: 2
data: {"type":"TEXT_MESSAGE_CONTENT","messageId":"…","delta":"Hello"}
…
id: 57
data: {"type":"RUN_FINISHED","outcome":{"type":"success"},"usage":[{"model":"…","inputTokens":…}]}
```

* There is one JSON event per `data:` line; `type` is an
  [AG-UI](https://docs.ag-ui.com) event (`RUN_STARTED`, `TEXT_MESSAGE_*`,
  `TOOL_CALL_*`, `CUSTOM` with names `work.tool_output`, `work.steer`,
  `work.usage`, `RUN_FINISHED`, `RUN_ERROR`). The stream ends after `RUN_FINISHED`
  or `RUN_ERROR`.
* `id:` is the event's sequence number in the run's durable log. Resume without
  loss or duplicates with the standard `Last-Event-ID` header (what
  `EventSource` sends on reconnect automatically), or `?after=<seq>` when you
  build the request yourself (`fetch` streams; `EventSource` cannot set headers).
  Replay starts from the log, then follows live, so opening the stream after the
  run finished returns everything.
* `RUN_FINISHED.outcome.type` is `success`, `cancelled` or `interrupt` (the run
  waits for an approval or an `ask_user` answer: `POST .../resume` with the
  interrupt id, then stream the new run it returns).
* Browser sketch:

```js
const r = await fetch("/agent/api/threads/" + id + "/runs", {method:"POST", credentials:"include",
  body: JSON.stringify({message, sandbox: "off"})});
const {id: runId} = await r.json();
const es = new EventSource("/agent/api/runs/" + runId + "/events", {withCredentials: true});
es.onmessage = (m) => { const ev = JSON.parse(m.data); render(ev); if (ev.type === "RUN_FINISHED" || ev.type === "RUN_ERROR") es.close(); };
```

If you relay the stream through your own endpoint, copy `id:` lines and
forward `Last-Event-ID` upstream, disable response buffering, and flush after
each event. In library mode `h.Events(ctx, runID, after)` gives the same events
as a channel of `store.Event{Seq, Type, Data}`.

## 6. Usage and cost

`RUN_FINISHED.usage` lists tokens per model: `inputTokens`, `outputTokens`,
`totalTokens`, `reasoningTokens`, `cachedInputTokens`. Meter from there:

* in library mode, read the event in your `Events` loop and write a usage row
  keyed by `runId` (idempotent: a stream may be replayed);
* server side regardless of any browser, use `Options.Hooks` (a `runtime.Hook`
  runs when a run ends on the process that executed it, with `Finish.Result.Usage`)
  or a `Feature` calling `b.EndOfRun`, and write to your billing store there. In a
  web + worker split, that code runs on the workers;
* sandbox time is separate: count sandbox creations with your adapter (it is
  the one place that sees `Create`), or read `h.Store.UserSandboxes` for live counts.

Run states are also in the store (`Run.Status`, `Error`, `ErrorCode`) for a
dashboard.
