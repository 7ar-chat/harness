# harness

An open, embeddable agent harness written in Go: a ReAct loop whose tools run in a **sandbox** (a local directory or a remote
[OpenSandbox](https://github.com/alibaba/OpenSandbox) container), streaming
[AG-UI](https://docs.ag-ui.com) events. The Go loop drives the model (OpenAI-compatible or Anthropic) and the
sandbox only executes; nothing spawns another agent CLI. It ships tools (files, shell, search, document
rendering), skills, memory, goals and plans, tool approvals and permissions, context compaction and
verification of produced documents.

It is meant to sit behind any front end: a web server, a TUI, a worker, a desktop app. The repository contains
a terminal client (`app/cmd/harness`), a `net/http` + SSE server package and a benchmark runner; product-specific
HTTP APIs (authentication, multi-tenant routing) are left to the embedding application.

```
        web UI ──► product ► hosts/httpapi (net/http, SSE) ──┐
        TUI    ──► app/cmd/harness (reference)  ───┤
        your app ──────────────────────────────┤
                                               ▼
                                    harness.Harness  (Send / Events / Cancel / Steer)
                                               │
        ┌───────────── runtime (claim, heartbeat, hooks) ─────────────┐
        │  agent loop ── llm (OpenAI chat / Responses, Anthropic)│
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
cfg, _ := config.Load()                       // ~/.har/config.yml + <repo>/.har/config.yml + env: LLM_*, SANDBOX_*, …
                                              // the API key: LLM_API_KEY, else ~/.har/auth.json (see Configuration files)
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

* **Credentials**: `harness auth login <provider>` stores an API key in `~/.har/auth.json` (mode 0600), never in a
  repository; `HAR_HOME` relocates `~/.har`, and servers set `HARNESS_NO_USER_DIR=1` and use the environment
  (see [Configuration files and standards](#configuration-files-and-standards)).
* **Web**: `httpapi.New(h, httpapi.Options{…})` (`hosts/httpapi`) serves threads, runs, approvals,
  files and the run's events as SSE (`id` = seq, so reconnects resume without loss), plus a built-in
  browser UI; `app/cmd/harness-web` runs it (see [Web host](#web-host)).
* **TUI**: `app/cmd/harness` is a Bubble Tea terminal client with a print mode (see
  [Terminal UI](#terminal-ui)); package `tui` has the model and renderers to embed. It runs on an in-process
  engine or attaches to a running `harness-web` (`-remote`).
* **Your own front end**: write it against `hosts/client`, one narrow interface with an in-process and an HTTP
  implementation (see [Front ends and the client contract](#front-ends-and-the-client-contract)).
* **Single process** (default): store, event log and queue live in memory — nothing to run
  except the sandbox server. **Multi process**: pass `Options.Store` (`adapters/store/pgstore` +
  `adapters/store/pgstore/migrate`) and `Options.Infra` (`redisinfra.Infra(rdb, store)` for Redis; see
  [Infrastructure](#infrastructure)); replicas then share runs, logs, sandbox locks
  and skill installs, and a crashed runtime's runs are finished as `interrupted` by another.
* **Local, no container**: `SANDBOX_BACKEND=local` (optionally `LOCAL_WORKSPACE=dir`, default the current
  directory) runs the tools on this machine through `adapters/sandbox/local`; the agent still sees `/workspace`.
  There is no isolation beyond path confinement of the file API: use it on your own machine or inside a container.
* **Your own sandbox**: implement `sandbox.Backend` (Create/Connect/Kill) returning
  `sandbox.Instance` (Exec, ReadFile, WriteFile, MkdirAll, List, Stat, Healthy…), prove it with
  `sandboxtest.RunConformance` and select it with `SANDBOX_BACKEND=<name>` (`backends.Register`); see
  [docs/sandbox-adapters.md](docs/sandbox-adapters.md). `core/sandbox/sandboxtest` is an in-memory fake for tests.
* **A website**: [docs/embedding.md](docs/embedding.md) and `examples/website` (own auth, per-user threads, a
  custom tool, a sandbox only for the requests that need one, `hosts/httpapi` mounted under `/agent/`).
* **Your own tools**: `Options.Tools` takes `tools.Provider`s (`ai.InvokableTool`s, per run).

## Coding profile

`AGENT_PROFILE=coding` switches the system prompt to a software-engineering one (orient with search, read
before edit, smallest change, run the project's own build/tests before finishing, no commits or
destructive commands unless asked) and puts the repository's instructions (`AGENTS.md`, else `CLAUDE.md`
or `GEMINI.md`, plus `.har/instructions.md` and your `~/.har/AGENTS.md`; see "Configuration files and
standards") into the prompt. An explicit `LLM_PROMPT_VARIANTS` still wins per model. Typical terminal setup:

```
AGENT_PROFILE=coding SANDBOX_BACKEND=local LLM_PROVIDER=anthropic LLM_API_KEY=… \
LLM_MODELS=claude-sonnet-5-5 LLM_DEFAULT_MODEL=claude-sonnet-5-5 harness
```

## LLM providers

`provider` (`LLM_PROVIDER`, flag `-provider NAME`) selects a provider: one of your named providers (see
[Named providers](#named-providers-and-api-keys)) or a built-in type. The built-in `openai` (the default: any OpenAI-compatible endpoint at `LLM_BASE_URL` with `LLM_API_KEY`;
Chat Completions, or `{LLM_BASE_URL}/responses` for the models in `LLM_RESPONSES_MODELS`) or `anthropic`.
Model ids (`LLM_MODELS`) are sent to the provider verbatim. A user is just a login string; the harness
forwards no identity to the model or to MCP servers.

`LLM_DISCOVER_MODELS=true` makes every host (`harness-web`, the worker, the TUI) ask the provider at startup which
models it serves (`GET {base}/models`; Anthropic `/v1/models`, 8 s timeout) and append them to `LLM_MODELS`, so
`GET /api/models` and the web model picker list them and runs may use them. The configured ids stay first and
the default is unchanged; a provider that cannot be asked logs a warning and the configured list stands. It is off
by default because `LLM_MODELS` is an allow-list and discovery widens it to everything the key can reach; a project
file cannot set it. The list is read once per process start.

## Providers that route by session

`LLM_SESSION_HEADER=x-opencode-session` sends the thread id in that header on every model call. OpenCode Go
requires it (it routes a conversation to one backend, which keeps the prompt cache warm: 86% cached on the
benchmark against 58–68% through a router without it):

```
LLM_PROVIDER=openai LLM_BASE_URL=https://opencode.ai/zen/go/v1 LLM_API_KEY=… \
LLM_SESSION_HEADER=x-opencode-session LLM_MODELS=deepseek-v4.1-flash LLM_DEFAULT_MODEL=deepseek-v4.1-flash
```

## CLI sessions

`harness` saves each conversation under `LOCAL_STATE_DIR` (default `~/.har/sessions`, i.e. `$HAR_HOME/sessions`):
`harness -continue` resumes the latest, `-resume <id>` a given one, `-sessions` lists them, `-ephemeral`
keeps nothing. In a git repository the coding profile snapshots the tree before every run;
`harness -undo` restores the snapshot (the replaced state is kept under `refs/harness/backups/`).
`harness.Resume` is the facade call that answers a tool approval or an `ask_user` question.

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

## Terminal UI

`app/cmd/harness` is a terminal client (Bubble Tea) over one thread of an in-process harness, or of a remote `harness-web`.

```
harness -model gpt-5                  # interactive UI (stdin is a terminal)
harness -p "summarise main.go"        # print mode: answer on stdout, tool activity on stderr
git diff | harness -p "review this"   # print mode, the prompt is -p plus stdin
echo "what is 2+2" | harness -json    # print mode, AG-UI events as JSON lines on stdout
harness -remote http://127.0.0.1:8080 # the same UI, attached to a running harness-web
harness -remote https://host -p "…"   # print mode against it; HARNESS_REMOTE_TOKEN is the bearer token
```

With `-remote` the process runs no engine and needs no model or sandbox configuration: the server owns
them, and `-model` defaults to the server's default. `-continue`, `-resume` and `-sessions` use the server's
saved conversations; `-undo` (a local workspace) is refused.

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

Slash commands (type `/` for the completion list, `tab` completes, `enter` runs the highlighted one; `ctrl+p`
opens a palette over all of them). Most also have a leader keybind: `ctrl+x` then a letter, with a which-key hint.

| Command (leader key) | Action |
|---|---|
| `/model [id]` (m) | filterable picker of the configured models plus the ones the provider reports (`GET {base}/models`; Anthropic `/v1/models`); `ctrl+f` favorites, `f2`/`shift+f2` cycle recent models; a typed id the list lacks is accepted. Applies to the next run |
| `/sessions` (l) | switch to a saved conversation (messages and tool calls are replayed); `ctrl+r` rename, `ctrl+d` delete, `ctrl+o` pin |
| `/new` (n) | start an empty conversation |
| `/undo` (u), `/redo` (r) | remove the last prompt and its answers and restore the workspace files (git workspaces); the prompt returns to the input |
| `/compact` (c), `/fork` (f) | summarise the history now; copy the conversation into a new one |
| `/plan`, `/build` (`tab` toggles) | plan mode is read-only, enforced by the engine: only read-only tools plus `update_plan` and `ask_user` |
| `/effort [low\|medium\|high\|default]` (`ctrl+t` cycles) | reasoning effort of the next runs |
| `/connect` | pick a provider and enter its API key (stored in `~/.har/auth.json`, 0600); applies after a restart |
| `/init`, `/<your command>` | custom commands: markdown prompt templates in `~/.har/commands` and `<project>/.har/commands` (`$ARGUMENTS`, `$1`..`$9`, nested folders as `dir:name`), plus installed skills; `/init` writes `AGENTS.md`. `/reload` rereads them |
| `/themes` | seven themes with live preview; the choice is kept in `~/.har/tui-state.json` |
| `/thinking`, `/details`, `/timestamps` | show reasoning text, tool details (`ctrl+o`), message times |
| `/sidebar` (b, `ctrl+b`) | right panel: title, model and mode, token usage, the agent's plan, modified files, MCP servers |
| `/editor` (e), `/export` (x) | compose in `$EDITOR`; export the conversation to Markdown and open it |
| `/stash [pop\|list]` (s), `/image <path>`, `/copy`, `/notify` | park the input; attach an image; copy a message (OSC 52); desktop notification when the terminal is in the background |
| `/help`, `/tools`, `/quit` (q) | list commands and keys, expand tool calls, leave |

In the input: `@` fuzzy-picks a workspace file whose content is sent with the message (never outside the workspace;
local sessions only); a message starting with `!` runs a shell command on your machine and shows its output (it
also becomes context of your next message and is never a model tool call); `up`/`down` walk the input history;
pasting an image path attaches it, and long pastes are shown as `[pasted N lines]`. `alt+up`/`alt+down` select a
message and `alt+y` copies it. A message that only looks like a command (`/etc/hosts is wrong`) is sent as text;
start with a space to send an unknown `/word` literally. The history commands are refused while a run is active.

`~/.har/tui.json` (JSONC allowed; never read from a project) sets `theme`, `leader`, `leader_timeout`,
`scroll_speed`, `scroll_acceleration`, `diff_style` (`auto` is side by side from 120 columns, or `stacked`), `cursor`,
`mouse`, `attention` (notifications, bell count, a `sound_command` argv run without a shell) and `keybinds` (merged
with the defaults; `"command.<name>": "<leader>x"` rebinds a command). More commands: `/timeline` (g) jumps to a
message, `/children` lists the sub-agent (explore) runs of the conversation and opens one, `/diff`, `/mouse`,
`/tips` (h), `/status` (i), `/skills`, `/mcp`, `ctrl+z` suspends. The explore tool shows its steps live under its
tool call (`work.subagent` events; expand with `ctrl+o`).

**Model info and cost.** Context windows and prices come from LiteLLM's public table
(`raw.githubusercontent.com/BerriAI/litellm/.../model_prices_and_context_window.json`), fetched in the background
with a plain GET (nothing about you is sent), cached for 24 hours in `~/.har/cache/` and never blocking startup.
The status line shows `ctx 12.3k/200k (6%)` and the session cost (`~` when some usage had no price), the sidebar
a usage bar and the model's price per million tokens, the model picker the window, price and vision/reasoning
badges; `/cost` gives a per-run breakdown and `/context` the window usage. `HARNESS_MODEL_PRICES` is a file path or
URL replacing the source, or `off` to disable it; `~/.har/model-prices.json` (same schema) overrides or adds models,
e.g. a private endpoint. With `-remote` the server's table is used. Cache-write tokens are not priced and sub-agent
tokens only join the total when the run ends.

The status line shows the model, state, step count, tokens in/out/cached (with the cache share)
and the run's elapsed time. `edit_file` calls show a diff of `oldText`/`newText`. The UI is a pure
`tui.Model` fed by `client.Event`s through a `tui.Backend` (`tui.ClientBackend` adapts any `client.Client`),
with one render function per event type and golden tests in `hosts/tui/testdata`
(`go test ./hosts/tui -update` rewrites them). The package imports only `hosts/client` and `core/agui`.

## Web host

`hosts/httpapi` (package `httpapi`) is a standalone HTTP server for any web front end, with a small browser UI
embedded. `app/cmd/harness-web` runs it:

```
harness-web -local                      # in a repository: coding agent on this directory, UI at http://127.0.0.1:8080/
HARNESS_WEB_ADDR=0.0.0.0:8080 HARNESS_WEB_TOKEN=secret OPENSANDBOX_URL=… harness-web   # remote sandbox, shared
```

`-local` sets `SANDBOX_BACKEND=local` and `AGENT_PROFILE=coding` (unless already set) and keeps sessions on disk
(`harness.OpenLocalStore`). Flags: `-addr` (default `HARNESS_WEB_ADDR`, else `127.0.0.1:8080`), `-model`, `-local`,
`-user` (the single user's login, default `local`). A non-loopback address is refused unless `HARNESS_WEB_TOKEN` is set.

**Routes** (all under `/api`, also served without the prefix; errors are `{"error":{"code","message"}}`; the full
table with bodies is the doc comment of `hosts/httpapi/httpserver.go`):

| Route | |
|---|---|
| `GET /healthz` | liveness, no auth |
| `GET /api/models` | allowed models and the default |
| `GET /api/sessions` | saved conversations (durable file store only, else 501) |
| `POST/GET /api/threads`, `GET/PATCH/DELETE /api/threads/{id}` | create, list, read with messages, rename, delete |
| `POST /api/threads/{id}/runs` | send a message, 202 with the run |
| `POST /api/threads/{id}/resume` | answer an approval (`decision` approve/deny, `always`) or an `ask_user` question (`text`) |
| `GET /api/threads/{id}/files?path=`, `.../files/download?path=` | list and download `/workspace` of the thread's sandbox (read-only, confined, `attachment`) |
| `GET /api/runs/{id}`, `GET /api/runs/{id}/events?after=` | run state; AG-UI events as SSE, resumed with `Last-Event-ID` or `after` |
| `POST /api/runs/{id}/cancel`, `/steer` | stop a run; add a message to a running one |

A foreign thread or run id is always a 404. **Auth**: `Options.Authenticate` (`func(*http.Request) (user string, err error)`)
decides who calls; the default is one local user, with an optional static bearer token (`HARNESS_WEB_TOKEN`).
`HARNESS_WEB_ORIGINS` is a comma-separated CORS allowlist; other cross-origin requests are refused (403), and without a
token the server only answers loopback `Host` names. Every response carries a strict CSP (`default-src 'self'`).

**UI** (`hosts/httpapi/ui`, `go:embed`, about 450 lines of plain HTML/CSS/JS, no build step, no CDN): conversation list,
streamed transcript with basic markdown, collapsible tool calls (edit diffs), status line (model, steps, tokens,
cache %), send / steer / cancel, approval and question prompts, a file panel with downloads, light and dark themes,
phone-width layout. With a token set the UI asks for it once and keeps it for the tab.

## Front ends and the client contract

The engine is a library; a front end should not be written against it. `hosts/client` is the narrow contract
between the two, and the only thing a terminal UI, a web UI, a Slack bot or a desktop app needs to import:

```
engine/ features/ store/ sandbox/            the core (a library, no HTTP, no UI)
        ▲
   harness.Harness                           the facade
        ▲                       ▲
  inproc.New(h, user)   hosts/httpapi      the HTTP API server (JSON + SSE, bearer auth)
        │                       ▲
        │                client.HTTP(url, token)     Last-Event-ID reconnect with backoff
        └───────────┬───────────┘
                client.Client                the contract
              ▲      ▲       ▲
         hosts/tui   web UI   your Slack bot, desktop app, …
```

`Client` acts for one user and has: `Models`; `CreateThread`, `ListThreads`, `GetThread`, `RenameThread`,
`DeleteThread`; `Send` (returns the `Run`), `GetRun`, `Events(ctx, runID, after)` (AG-UI events with an ordered `Seq`,
resumable from any seq), `Steer`, `Cancel`, `Resume` (answers a tool approval or an `ask_user` question);
`ListFiles`, `DownloadFile`; `Sessions`. Errors are `*client.Error` values matched with `errors.Is` against
`client.ErrNotFound` (also what another user's thread or run gives), `ErrInvalidRequest`, `ErrRunNotActive`,
`ErrNotSupported`... The DTOs (`Thread`, `Message`, `Run`, `File`, `Session`, `SendOptions`, `ResumeRequest`) are the
JSON that `hosts/httpapi` serves: the server uses these very types, so the wire format has a single definition. The
run-creation request also takes an optional `sandbox` string, which the server forwards once the engine supports a
per-run sandbox.

Both implementations pass the same suite, `clienttest.Run(t, newClient)`: create, send, stream to `RUN_FINISHED`,
resume from a seq, steer, cancel, approval and `ask_user` round trips, files, validation and ownership.
A new implementation (a gRPC client, a recorded replay…) is tested by calling it.

A complete front end, in about 25 lines (`integration/client/example_test.go` compiles and runs it):

```go
func ask(ctx context.Context, c client.Client, question string) error {
	th, err := c.CreateThread(ctx, "")
	if err != nil {
		return err
	}
	run, err := c.Send(ctx, th.ID, question, client.SendOptions{})
	if err != nil {
		return err
	}
	events, err := c.Events(ctx, run.ID, 0)
	if err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case agui.TextMessageContent:
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			fmt.Print(e.Delta)
		case agui.RunFinished, agui.RunError:
			fmt.Println()
		}
	}
	return nil
}

// in-process:  ask(ctx, inproc.New(h, "alice", inproc.WithModels(cfg.LLMModels, "gpt-5")), "Say hello")
// remote:      ask(ctx, client.HTTP("http://127.0.0.1:8080", os.Getenv("HARNESS_REMOTE_TOKEN")), "Say hello")
```

A run that ends with `RUN_FINISHED` outcome `interrupt` is waiting for the user: read the interrupt (`reason`,
`id`) from the event, or the thread's `PendingApproval` / `PendingQuestion`, and answer with `Resume`. A stream
whose connection cannot be recovered ends with a synthetic `RUN_ERROR` (code `stream_lost`), so a consumer that
waits for a terminal event never hangs.

**Dependency rules** (`layers_test.go`; see [docs/architecture.md](docs/architecture.md)): `core/` imports only `core/`;
`engine/` imports `core/` and `engine/`; features import `core/`, `engine/` and `coding/`, never another feature;
`adapters/` and `coding/` do not import each other; `hosts/tui` imports only `hosts/client` and `core/agui`;
`hosts/httpapi` imports only the facade, `hosts/client` and `core/`; `app/product/` builds on `hosts/httpapi`; the facade
imports no host.

## Sessions

Local hosts keep their conversations across restarts without Postgres or Redis:

```go
st, _ := harness.OpenLocalStore("")           // "" = $LOCAL_STATE_DIR, else ~/.har/sessions ($HAR_HOME/sessions)
defer st.Close()
h, _ := harness.New(harness.Options{Config: cfg, Store: st})

ss := st.Sessions()
list, _ := ss.List(ctx, "alice", 20)          // title, updated, last message preview, active run
fork, _ := ss.Fork(ctx, "alice", threadID, messageID) // new thread with the history up to that message
ss.Delete(ctx, "alice", threadID)
// resume = keep using the thread id: h.Send(ctx, "alice", threadID, "…")
// old runs replay: h.Events(ctx, oldRunID, 0)
```

`adapters/store/filestore` wraps the in-memory store and appends every change to
`<dir>/journal.jsonl` (JSON lines, upsert records, one `write(2)` per batch). Threads, messages,
runs, plans, summaries, compactions, goals, approvals and event logs persist; artifacts, blobs
and sandbox ids stay in memory. A `kill -9` loses nothing and can only tear the last
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

## Permissions and git safety (local backend, coding profile)

`SANDBOX_BACKEND=local` runs commands on your machine with your rights, so a permission layer
(`permissions/`) sits in front of `execute_command`, `write_file` and `edit_file` whenever the local
backend or `AGENT_PROFILE=coding` is on (the work profile on OpenSandbox is unchanged). Each call is
allowed, asked or denied; an *ask* pauses the run with the usual approval interrupt and resumes when
approved, a *deny* never runs and the model gets a clear tool error. Shell lines are split into their
simple commands (`; && || |`, subshells, `$(...)`, backticks, `bash -c`, `sudo`, `FOO=1 cmd`,
redirections) and the most severe verdict wins.

| Default | Examples |
|---|---|
| allow | read-only commands (`ls cat grep find git status/diff/log`), build/test/lint (`go test/build/vet`, `make`, `npm test`, `pytest`, `cargo test`), file work inside the workspace (`mkdir cp mv rm`, `git add/commit`), writes inside the workspace |
| ask | network (`git push/pull/fetch`, `curl`, `wget`, `ssh`), installs (`pip/npm install`, `apt`, `brew`), `sudo`, `docker rm/run`, `chmod -R`, `dd`, `rm -rf` or any write outside the workspace, `git reset --hard/clean/rebase`, unknown programs and scripts |
| deny | `rm -rf /` or `~`, fork bombs, `mkfs`/`dd` on devices, `git push --force` to main/master, writes to `.git/`, `.env`, `id_rsa`, `*.pem` (unless explicitly allowed) |

`AGENT_PERMISSIONS` picks the mode: `ask` (default on local), `auto-edit` (edits free everywhere,
commands asked), `yolo` (no prompts, deny rules still apply), `yolo-unsafe` (nothing is checked),
`readonly` (only read-only commands, no edits). Rules go in `.har/permissions.yml` (workspace) and
`~/.har/permissions.yml`, or in `AGENT_PERMISSIONS_ALLOW|ASK|DENY` (entries separated by
newlines or `;;`); deny beats ask beats allow, and a user allow overrides a built-in ask:
A project's permissions (`.har/permissions.yml` and `permissions.*` of its `config.yml`) can only **tighten**: its `ask` and
`deny` rules and a `mode` as strict as yours (or stricter) are honoured; its `allow` rules and a looser `mode` are
ignored unless the project is trusted (`harness trust`). The ignored items are logged once and listed by `-config-dump`.

```yaml
mode: ask
allow: ["git push origin feature/*", "make deploy-staging", "write_file(.env.local)"]
ask:   ["docker*"]
deny:  ["curl*|sh", "Bash(terraform apply*)", "write_file(migrations/**)"]
```

An entry is `execute_command(glob)` (alias `Bash`, or a bare glob such as `git push*`; `*` matches
anything, a `|` matches a pipeline), `write_file(path-glob)` (alias `Edit`, covers `edit_file`) or
`any_tool(glob on its JSON arguments)`. Globs on commands match text, so `rm -rf*` does not see
`rm -r -f`; the built-in checks do. Paths are lexical: symlinks out of the workspace are not followed.

`gitsafe/` snapshots the work tree at the start of every local run (`git add -A` into a private index
plus a commit object kept under `refs/harness/snapshots/<thread>/<n>`; no commit on your branch, no
index or stash change) and `gitsafe.Restore(ctx, dir, ref)` / `gitsafe.Undo(ctx, dir, thread)` put files,
HEAD and the staged state back, saving the current state under `refs/harness/backups/` first.
Nothing happens outside a git repository.

## Skills

A skill is a folder with a `SKILL.md` (frontmatter `name` and `description`, then the instructions)
and optional `scripts/`, `reference/` files. The model sees an index of them in its system prompt,
reads one with `load_skill`, and its folder appears read-only under `/skills/<name>/` in the sandbox
(installed lazily, only for the skills a run loads or names in a command).

Skills are discovered from, lowest to highest precedence (a later source replaces a skill of the same name):

| Source | Where | Notes |
| --- | --- | --- |
| `builtin` | embedded in the binary (`features/skills/skillsfs/builtin`) | `docx`, `pptx`, `xlsx`, `pdf`, `charts`; `Options.BuiltinSkills` replaces them |
| `extra` | `Options.ExtraSkills` (an `fs.FS`) | the embedding product's own skills |
| `user` | `SKILLS_DIR`, `~/.har/skills`, `~/.agents/skills`, `~/.claude/skills` (in that order) | one folder per skill; the last two only with the local backend |
| `project` | `<repo>/.har/skills`, `<repo>/.agents/skills`, `<repo>/.claude/skills` (in that order) | the host's repository with the local backend; with a remote backend the sandbox's own `/workspace/.har/skills`, `.agents/skills`, `.claude/skills`, read through the sandbox API per thread and merged over the global skills for that thread's runs. Fenced as untrusted text; symbolic links are not followed; at most 64 skills of 500 files / 8 MiB each |

```
~/.har/skills/
  release-notes/
    SKILL.md          # ---\nname: release-notes\ndescription: Draft release notes from merged PRs.\n---\n…
    scripts/collect.py
```

The folder name is the skill name. `RunOptions.Skills: []string{"release-notes"}` attaches a skill to a
run (its `SKILL.md` goes into the system prompt and its files are installed before the first sandbox
command); the built-ins are only listed and installed on demand. An optional `requires: [other-skill]`
frontmatter list installs dependencies with a skill. Packages are validated (entry count, depth, file
and total size, safe paths) when loaded; an invalid folder is logged and skipped.

## Features and profiles

`harness.New` builds a small core (store, models, sandboxes, infrastructure, the agent loop, the runtime) and
installs **features** on top of it. A feature is a set of tools, prompt sections, hooks and jobs that belong
together. A host chooses its set with `Options.Features`; left nil, the profile named by `AGENT_PROFILE` is used.

| Profile | Constructor | Features (in order) |
|---|---|---|
| work (default) | `harness.ProfileWork()` | Sandbox, Codecheck(false), Skills, MCP, Plan, ProjectDocs, Artifacts, Workspace, Titles, Verify, Goals, Memory, ToolProfile(false), Permissions(false) |
| coding | `harness.ProfileCoding()` | the same with Codecheck(true), Explore, ToolProfile(true), Permissions(true) |
| minimal | `harness.ProfileMinimal()` | Sandbox, Plan |

`AGENT_PROFILE=work|coding|minimal` picks the profile when `Options.Features` is nil; the tool list and prompt
of work and coding are byte-identical to what they were before features existed. Features install in list
order, and that order is the order of tools and prompt sections. A feature that builds on another says so
(`Workspace` needs `Artifacts`, `Explore` and `Codecheck` need `Sandbox`) and `New` fails with a clear error
if it is missing or listed twice.

Compose your own set from the constructors (`Sandbox()`, `Skills()`, `Memory()`, `Goals()`...), drop what you
do not want, add your own:

```go
features := append(harness.ProfileMinimal(), harness.Memory(), harness.Titles(), myAudit)
h, err := harness.New(harness.Options{Config: cfg, Features: features})
```

### Writing a feature

```go
type Feature interface {
	Name() string
	Install(*harness.Builder) error
}
```

`harness.FeatureFunc(name, func(b *harness.Builder) error)` turns a function into one. `Install` reads the
environment from the builder (`b.Config`, `b.Store`, `b.Models`, `b.Sandboxes`, `b.Infra`, `b.Options`) and
contributes:

| Builder method | Contributes |
|---|---|
| `AddTools(providers...)`, `WrapTools(f)` | tool providers; a wrapper over the whole registry (filters, auditing) |
| `Section(f)`, `ContextSection(f)`, `TailSection(f)` | static prompt section; dynamic section in the final context block; per-run reminder at the end of each request |
| `BeforeTool(h)`, `ToolHook(h)`, `RunStart(h)`, `StepNote(f)` | gate tool calls; observe tool results; run before a run's first step; add a `<system-reminder>` note to the next model call when news appears mid-run (not persisted) |
| `SetAnswers`, `SetImages`, `SetChecker`, `Configure(f)` | final-answer review, image attachment, deliverable checker (one each); a free-form mutator of `agent.Harness` |
| `EndOfRun(h)`, `Job(j)`, `SetPurger(p)`, `Background(f)` | end-of-run `runtime.Hook`; periodic job (run by one runtime at a time); retention job; goroutine started once `New` succeeded |
| `SandboxTools(f)`, `PrepareSandbox(f)`, `OnSandboxCreate(f)` | chain onto the sandbox tools' options (`OnAcquire`, `BeforeCommand`, `CheckWrite`...); run when a sandbox is created |
| `RequireSandbox()`, `Require(names...)`, `StoreAs[T](b)` | fail with a clear error when no backend is configured, a feature is missing, or the store lacks an extension |
| `Provide[T](b, v)`, `Lookup[T](b)`, `b.Has(name)` | publish a service for later features; look one up |
| `After(f)` | run once every feature is installed |

The built-in features publish their services on the builder (`b.Artifacts`, `b.Workspace`, `b.Skills`,
`b.Installer`, `b.Memory`, `b.Verifier`; nil until installed). The wiring of the built-ins lives in
`features_core.go` and `features_state.go`; they use the same `Builder` a host's feature does. Example, a tool
plus a prompt section plus a hook (this is `TestACustomFeatureAddsAToolAPromptSectionAndAHook`):

```go
ping := harness.FeatureFunc("ping", func(b *harness.Builder) error {
	b.AddTools(longtest.ExtraTool("ping", func(context.Context, string) (string, error) { return "pong", nil }))
	b.Section(func(context.Context, agent.Input) string { return "Use ping to check the line." })
	b.ToolHook(auditHook) // agent.ToolHook: AfterTool(ctx, run, toolCallID, toolName)
	return nil
})
h, err := harness.New(harness.Options{Config: cfg, Features: append(harness.ProfileMinimal(), ping)})
```

## Sandbox per run

The sandbox is optional for each run: `RunOptions.Sandbox` (JSON `sandbox` on `POST /api/threads/{id}/runs`) is
`auto` (default: the sandbox tools are offered when a backend is configured and the sandbox is created at the
first command), `off` (no sandbox tools, the no-sandbox prompt, `Backend.Create` is never called) or `on`
(like `auto`, but `Send` fails with `ErrInvalidRequest` when the harness has no sandbox). The choice is kept in
the run record and inherited by the thread's resumed runs. `SANDBOX_BACKEND` names the backend: `local`, `opensandbox`,
`none` or one registered with `backends.Register`. See [docs/embedding.md](docs/embedding.md).

## Infrastructure

The engine, sandbox and feature packages do not know Redis (or any broker). They define the small interfaces
they need, with in-process implementations, and `harness.Infra` bundles the implementations a deployment
injects (`Options.Infra`; every field optional, a missing one stays in process):

| Field | Interface | In process | Redis (`redisx`) |
|---|---|---|---|
| `Broker` | `queue.Queue` + `queue.Bus` + `runtime.RunQueue`: run queue, cancel, steers | `queue.NewMemory` | `redisx.NewQueue` |
| `Log` | `eventlog.Log`: per-run event log with replay | `eventlog.NewMemory` | `redisx.NewEventLog` |
| `Coordinator` | `sandbox.Coordinator` + `runtime.Locks`: creation locks, health streaks, sweeper and job locks, run liveness | none (`sandbox.LocalCoordinator` inside the manager) | `redisx.NewCoordinator` |
| `Jobs` | `sandboxtools.JobList`: running background jobs and their leases | memory only | `redisx.NewJobs` |
| `Installs` | `skills.InstallState`: installed skill revisions and install locks | per process | `redisx.NewSkillInstalls` |

Package `redisinfra` builds all five over a Redis client:

```go
rdb := redisinfra.NewClient(cfg)              // REDIS_*, USE_SENTINEL
infra := redisinfra.Infra(rdb, store)
h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
```

It is its own package so that a program that does not use Redis does not link go-redis. To run on NATS or SQS,
implement the interfaces above and pass them in `Infra`; nothing else changes.

## Layout

The repository is a root Go module plus seven standalone modules (listed in `go.work`), in strict layers; a lower
layer never imports a higher one (`layers_test.go` enforces it, and that each standalone module requires only the
modules it declares; the reasoning is in [docs/architecture.md](docs/architecture.md)).

| Module | Holds | Needs |
|---|---|---|
| [`ai`](ai) | messages, tools, model contract, the OpenAI / Anthropic / Responses clients, a tool-calling loop (`ai/agent`), context compaction. A library on its own: see its [README](ai/README.md) | the standard library, a JSON-schema reflector |
| `core` | AG-UI events, domain types and ports (`store`, `sandbox`, `blobs`) | uuid |
| `modelinfo` | the model price and context-window table | nothing |
| `hosts/client` | the `Client` contract front ends are written against, with its HTTP implementation | `core`, `modelinfo` |
| `hosts/tui` | the terminal UI | `hosts/client`, `core`, charm libraries |
| `adapters/sandbox` | local and OpenSandbox backends, the `SANDBOX_BACKEND` registry | `core`, the OpenSandbox SDK |
| root | the engine, features, facade, HTTP API, stores: what you embed | all of the above |
| `app` | the team product (`app/product`) and the commands (`app/cmd`); an application, nothing imports it | the root and all of the above |

`make test-modules` builds and tests each one alone (`GOWORK=off`).

```
ai/                            module ai: how the agent talks to models and tools
  (root)      Message, ToolInfo, Model, options, streams, InferTool, ConcatMessages
  agent/      the one agent loop (Loop + Policy; Run = a policy over a slice) + Caller (stream, watchdog, retry), Schedule, Parallel   openai/  Chat Completions client
  llm/        provider factory, OpenAI Responses + Anthropic Messages clients, retry, test doubles (llm/llmtest)
  compaction/ context estimation and summarisation
core/                          module core: the vocabulary, no dependencies on the rest
  agui/       AG-UI events           store/     domain types + persistence ports (Store, Full, TeamStore…)
  sandbox/    sandbox contract, manager, background-job port, sandboxtest (fake)   blobs/   content-store port
config/ modelinfo/ engine/syncx prompts    settings, model table (module modelinfo), small helpers, prompt text
engine/                        the reusable intelligence
  agent/      the policy of a persisted run over ai/agent.Loop: steering, hooks, spill, masking, guards, approvals, persistence
  tools/      tool interfaces and the registry
  runtime/    run claiming, heartbeat, sweeper, end-of-run hooks
  eventlog/ queue/ approvals/   (interfaces and in-memory implementations only)
coding/                        the coding toolset: sandboxtools (read/write/edit/search/exec/jobs), gitsafe, codecheck
adapters/                      implementations of the ports
  store/{memstore,filestore,pgstore,teamstore}   memory, durable local files, Postgres
  redisx/                      Redis: queue + bus, event log, coordinator, job list, skill installs
  sandbox/{local,opensandbox,backends}   your machine (trusted), OpenSandbox (remote), the SANDBOX_BACKEND registry
  blobs/s3blobs/               S3-compatible blob store
features/                      optional capabilities a host switches on (never import each other)
  permissions/ projectdocs/ explore/ plan/ mcp/ memory/ skills/ goals/ titles/ verify/ artifacts/ (+ artifacts/workspace)
harness.go feature*.go profiles.go infra.go   the facade: New / Send / Events / Steer / Cancel / Resume, Feature + Builder, profiles, Infra
hosts/                         ways to drive the engine, and the contract front ends are written against
  client/ (the Client contract + HTTP implementation)   inproc/ (Client over a harness in this process)   clienttest/ (conformance suite)
  tui/ (terminal)   httpapi/ (HTTP + SSE + embedded UI; a Policy/Extension seam)   worker/ (headless queue consumer)
app/product/                   the team product on top of hosts/httpapi: OIDC, projects, audit, feed, assistant, connectors, memory review
app/cmd/harness  app/cmd/harness-web  app/cmd/harness-worker  app/cmd/bench   binaries (module app, with app/product);  app/cmd/gen-ts = the TypeScript type generator;  bench/ longtest/ = benchmark and long-session tests
web/                           the React app; `make web` builds it into hosts/httpapi/ui (not committed); its API types
                               (web/src/api/generated.ts) are generated from the Go DTOs: `make gen`
```

## Worker

`app/cmd/harness-worker` (package `hosts/worker`) is a headless process that executes runs queued by other
processes: a web server or API calls `Send` (it never calls `Run`), and any number of workers consume the
shared Redis queue and Postgres store, each with its own sandboxes. Intelligence and sandboxes scale
independently of the front end.

| Setting | Meaning |
|---|---|
| `REDIS_*`, `POSTGRES_*` | the shared queue/event log and store (the same values as the sending process; migrations are applied at start) |
| `LLM_*`, `OPENSANDBOX_URL` / `SANDBOX_BACKEND` | models and sandbox backend of this worker |
| `RUNTIME_CONCURRENCY` or `-concurrency N` | maximum concurrent runs (default 16) |
| `RUNTIME_SHUTDOWN_GRACE_SECONDS` | on SIGINT/SIGTERM the worker stops claiming and lets runs finish this long; the rest end `interrupted` (never lost) |
| `HARNESS_WORKER_ADDR` or `-addr` | admin server, default `127.0.0.1:9090` |
| `-once` | process queued runs until none is queued or running, then exit 0 (batch, CI) |
| `-standalone` | allow a private in-memory queue/store; without it the worker refuses to start unless Redis and Postgres are configured |
| `-id` | worker id recorded as the run owner (default hostname-random) |

Admin endpoints: `GET /healthz` (process is up), `GET /readyz` (200 while the queue loop reads; 503 when the
consumer group cannot be created, the loop is stuck, or the worker is draining), `GET /metrics` (Prometheus
text: `harness_worker_up`, `_ready`, `_draining`, `_uptime_seconds`, `_concurrency`, `_runs_in_flight`,
`_runs_started_total`, `_runs_finished_total{status}`). The startup log names the store, queue and sandbox
backend and the model list.

A docker-compose deployment is one `redis` and one `postgres` service, a web service built on the library
(environment: the same `REDIS_HOST`/`POSTGRES_HOST`), and a `worker` service running `harness-worker` with
those variables plus `LLM_API_KEY` and `OPENSANDBOX_URL`, `HARNESS_WORKER_ADDR=0.0.0.0:9090`, a health check
on `/readyz`, `stop_grace_period` longer than `RUNTIME_SHUTDOWN_GRACE_SECONDS`, and `deploy.replicas` set to
the number of workers wanted.

## Configuration files and standards

The harness follows the conventions of the other coding-agent tools: a project directory `.har/` next to the
code (and `~/.har/` for the user), and the de-facto standards for agent instructions (`AGENTS.md`), skills
(`SKILL.md` folders) and MCP servers (`.mcp.json`). The directory name is one constant, `config.DirName`.

```
<repo>/
  AGENTS.md                  agent instructions (https://agents.md); else CLAUDE.md, else GEMINI.md
  .mcp.json                  MCP servers, the format Claude Code and others share
  .har/
    config.yml               project settings: selections and tuning only (see below); no secrets
    permissions.yml          allow/ask/deny rules (see Permissions)
    instructions.md          extra instructions, always loaded, highest priority
    mcp.json                 MCP servers, same format as .mcp.json
    skills/<name>/SKILL.md   project skills (also .agents/skills and .claude/skills)
    memory/                  logical prefix of memory paths
```

The user directory is the harness's home, like `~/.claude` or `~/.codex`: general settings and credentials live
there and never in a repository.

```
~/.har/                      $HAR_HOME when set
  config.yml                 your settings: endpoints, providers, model catalog, sandbox (full schema); no secrets
  trust.json                 the projects you trust, mode 0600 (written by `harness trust`)
  auth.json                  API keys by provider, mode 0600 (written by `harness auth login`)
  AGENTS.md                  your own instructions, loaded for every project (local backend)
  permissions.yml            allow/ask/deny rules for every project
  mcp.json                   your MCP servers
  skills/<name>/SKILL.md     your skills
  sessions/                  the saved conversations (LOCAL_STATE_DIR overrides)
  memory/                    memory files
```

`HAR_HOME` points the harness at another directory (tests, containers). `HARNESS_NO_USER_DIR=1` makes the
harness read nothing from it (no `auth.json`, `config.yml`, `AGENTS.md`, skills, MCP, permissions; sessions fall back to
`$XDG_STATE_HOME/harness`): set it on servers and CI that must be hermetic and give the key through the environment.
`harness config path` prints the user directory and the project directory in use; `harness config init` writes a commented
starter `config.yml` (0600, in a 0700 directory) when there is none.

The project directory is the nearest `.har/` found walking up from the working directory (or `LOCAL_WORKSPACE`) to the git
root. `harness -config-dump` prints the effective configuration with the source of every value that was set
(`flag`, `env`, `project file`, `user file`; secrets are masked) and the files that were read.

### Precedence

| Layer | Source | Wins over |
|---|---|---|
| 1 | command-line flags (`-model`, ...) | everything |
| 2 | process environment (`LLM_*`, `SANDBOX_*`, ...) | files and defaults |
| 3 | project `<repo>/.har/config.yml` | user file and defaults |
| 4 | user `~/.har/config.yml` | defaults |
| 5 | built-in defaults | |

Scalars and lists are replaced by the higher layer as a whole. Exceptions: `permissions.allow|ask|deny` add up across the two
files (a user `deny` is never lost; a project's `allow` only counts when it is trusted) unless the matching `AGENT_PERMISSIONS_*`
variable is set, and MCP servers merge by name (see below). Unknown keys are an error naming the key and the file (`.har/config.yml: unknown key "sandbox.bakend"`), a
plain `api_key` is refused, and a value of the wrong type is an error.

### What a project file may and may not do

The **user** (`~/.har/config.yml`, the environment, the flags) owns every endpoint, key and model catalog. A **project**
(`<repo>/.har/config.yml`) may only *select* among what you configured and *tune* behaviour. A key in the right-hand column in a
project file is an error naming the file, the key and where it belongs (`~/.har/config.yml`).

| A project file may (selection and tuning) | Only you may (user file, environment, flags) |
|---|---|
| `provider: <name>`: one you defined, or `openai` / `anthropic` | `providers` (any definition), `base_url`, `api_key_env`, `api_key` |
| `default_model`, `explore_model`, `limits.compaction_model` and the `agents:` section, each checked against **your** model list | `models`, `presets`, `default_preset` (which models exist) |
| `profile`, `tools`, `code_checks` | `session_header`, `cache_ttl`, `timeout_seconds` |
| `limits.*`, `skills.*`, `memory.*`, `context_windows`, `prompt_variants`, ... | the whole `sandbox:` section (`backend`, `url`, `image`, `api_key_env`, `workspace`, `egress_*`, limits ...) and `env:` |
| `permissions`: `ask`, `deny` and a `mode` as strict as yours or stricter | `mcp.allow_stdio` |
| instruction files, skills, nested `AGENTS.md` (data, as before) | `permissions.allow` and looser modes (unless trusted), MCP servers (unless trusted) |

A selected model that is not in your list is an error: `model X (project .har/config.yml agents.explore.model) is not configured by you:
add it to providers.<p>.models in ~/.har/config.yml`. A project cannot switch the provider of a role: roles use models of the
selected provider.

**Trust.** `~/.har/trust.json` (`{"projects": ["/abs/path"]}`, mode 0600, written atomically) lists the project roots you trust. A
trusted project may also add permission `allow` rules and loosen the mode, and define MCP servers (`<repo>/.mcp.json`,
`<repo>/.har/mcp.json`, `mcp.servers` of its `config.yml`; `http`/`sse` freely, `stdio` still needs your `mcp.allow_stdio` and the
local backend). An untrusted project's MCP servers are not started; `-config-dump` lists them with the hint to run `harness trust`.
Your own `~/.har/mcp.json`, `mcp.servers` of your file and `MCP_SERVERS` are always trusted.

```
harness trust            # trust the project of the current directory (its git root / .har parent)
harness trust ~/src/app  # or a given directory
harness trust list       # the trusted projects
harness untrust          # stop trusting (a path may be given)
HARNESS_TRUST_PROJECT=1 harness -p "..."   # trust the current project for this process only (CI)
```

A project's **stdio** MCP server (a program to run) needs one more step: after `harness trust`, approve its exact command with
`harness trust mcp [name]` (all pending servers without a name; `harness untrust mcp <name>` withdraws it). The approval is
recorded in `trust.json` for (project root, server name, SHA-256 of command, arguments and `env`); changing any of them, or
`harness untrust` of the project, drops it and the server is listed as not started until approved again. The check runs when
the config is loaded and again right before the process is spawned. In an **interactive** run (the TUI by default; `-auto`,
`-p` and headless hosts are not) a pending server is also offered for approval in the run itself: the model gets the tool
`mcp_approve__<server>_<hash>`, and calling it pauses the run with a normal approval card naming the project, the server, the
exact command, the arguments and the names (never the values) of its `env`. Approving records the same `trust.json` entry as
`harness trust mcp` and the server's tools are available from the **next** run (the current run's tools were built before the
decision; the tool's result says so). Denying leaves it pending and it is not asked again on that thread; a changed
command is a new tool and asks again. Where nobody can answer approvals (automatic mode, hosts without an approval gate) the
tool is refused and the server stays unstarted; `harness trust mcp` remains the way to approve it. Your own servers (`~/.har/mcp.json`, `MCP_SERVERS`) need no approval.

Trust is by project root; `HARNESS_NO_USER_DIR=1` ignores `trust.json` (only the environment variable can trust then).

#### The `agents:` section

Maps roles to the model (and profile) they use; it is allowed in both files, and a project's choices are validated against your catalog.

| Key | Sets | Notes |
|---|---|---|
| `agents.default.model` | `LLM_DEFAULT_MODEL` | the model of new runs |
| `agents.default.profile` | `AGENT_PROFILE` | `work`, `coding` or `minimal` |
| `agents.explore.model` | `EXPLORE_MODEL` | the read-only explore sub-agent |
| `agents.compaction.model` | `COMPACTION_MODEL` | summarising old context |
| `agents.title`, `agents.goal_judge`, `agents.default.reasoning_effort` | | not available yet (the title and goal-judge calls use the model of the run; the effort comes from `default_preset`): rejected |

```yaml
# <repo>/.har/config.yml
provider: opencode            # must be defined in ~/.har/config.yml
agents:
  default:    {model: deepseek-v4.1-pro, profile: coding}
  explore:    {model: deepseek-v4.1-flash}
  compaction: {model: deepseek-v4.1-flash}
permissions:
  mode: readonly              # only counts when as strict as yours
  deny: ["Bash(terraform apply*)"]
```

### config.yml

This table is the schema of the **user file**; a project file accepts the subset allowed above. Keys are translated to the environment variables of the "Configuration" section, so every existing setting works;
`env:` is the escape hatch for the ones without a nice key (`env: {HARNESS_MEMORY_MAX_ENTRIES: 500}`; secret-looking names such as
`*_KEY`, `*_PASSWORD`, `*_TOKEN` are refused).

| Key | Environment variable | Notes |
|---|---|---|
| `provider` | `LLM_PROVIDER` | `openai`, `anthropic`, or the name of an entry of `providers` |
| `providers` | (see below) | named providers: `type`, `base_url`, `session_header`, `cache_ttl`, `models`, `default_model`, `api_key_env` |
| `base_url` | `LLM_BASE_URL` | |
| `api_key_env` | `LLM_API_KEY` | the NAME of the environment variable that holds the key; unset is an error |
| `models` | `LLM_MODELS` | list |
| `default_model` | `LLM_DEFAULT_MODEL` | |
| `discover_models` | `LLM_DISCOVER_MODELS` | `true`: at startup add the provider's `GET {base}/models` to `models` (user file only) |
| `explore_model` | `EXPLORE_MODEL` | |
| `session_header` | `LLM_SESSION_HEADER` | |
| `cache_ttl` | `LLM_CACHE_TTL` | `5m` or `1h` |
| `timeout_seconds` | `LLM_TIMEOUT_SECONDS` | |
| `profile` | `AGENT_PROFILE` | `work`, `coding` or `minimal` |
| `tools` | `AGENT_TOOLS` | list (`+name`, `-name`) |
| `code_checks` | `CODE_CHECKS` | |
| `context_windows` | `LLM_CONTEXT_WINDOWS` | map `model: tokens` |
| `reasoning_models`, `vision_models` | `LLM_REASONING_MODELS`, `LLM_VISION_MODELS` | lists |
| `model_labels`, `prompt_variants` | `LLM_MODEL_LABELS`, `LLM_PROMPT_VARIANTS` | maps |
| `presets`, `default_preset` | `LLM_PRESETS`, `LLM_DEFAULT_PRESET` | |
| `limits.max_steps` | `AGENT_MAX_STEPS` | |
| `limits.max_tool_output_chars`, `limits.tool_output_spill` | `AGENT_MAX_TOOL_OUTPUT_CHARS`, `AGENT_TOOL_OUTPUT_SPILL_CHARS` | |
| `limits.step_retries`, `limits.stream_idle_seconds`, `limits.call_max_seconds`, `limits.max_output_tokens` | `LLM_STEP_RETRIES`, `LLM_STREAM_IDLE_SECONDS`, `LLM_CALL_MAX_SECONDS`, `LLM_MAX_OUTPUT_TOKENS` | |
| `limits.compaction_threshold`, `limits.compaction_model`, `limits.keep_recent_tool_calls` | `COMPACTION_THRESHOLD`, `COMPACTION_MODEL`, `COMPACTION_KEEP_RECENT_TOOL_RESULTS` | |
| `sandbox.backend` | `SANDBOX_BACKEND` | `local`, `opensandbox` or `none` (`none` ignores `sandbox.url`) |
| `sandbox.url`, `sandbox.image`, `sandbox.api_key_env` | `OPENSANDBOX_URL`, `OPENSANDBOX_IMAGE`, `OPENSANDBOX_API_KEY` | `api_key_env` is a variable name |
| `sandbox.use_server_proxy`, `sandbox.credential_proxy`, `sandbox.timeout_seconds`, `sandbox.command_timeout`, `sandbox.cpu`, `sandbox.memory` | `OPENSANDBOX_USE_SERVER_PROXY`, `OPENSANDBOX_CREDENTIAL_PROXY`, `SANDBOX_TIMEOUT_SECONDS`, `SANDBOX_COMMAND_TIMEOUT_SECONDS`, `SANDBOX_CPU`, `SANDBOX_MEMORY` | `credential_proxy` keeps the project's GitHub token outside the sandbox, see [docs/sandbox-adapters.md](docs/sandbox-adapters.md) |
| `sandbox.egress_allowlist`, `sandbox.egress_unrestricted`, `sandbox.max_sandboxes_per_user` | `SANDBOX_EGRESS_ALLOWLIST`, `SANDBOX_EGRESS_UNRESTRICTED`, `MAX_SANDBOXES_PER_USER` | |
| `sandbox.workspace`, `sandbox.state_dir` | `LOCAL_WORKSPACE`, `LOCAL_STATE_DIR` | local backend |
| `permissions.mode` | `AGENT_PERMISSIONS` | `ask`, `auto-edit`, `yolo`, `yolo-unsafe`, `readonly` |
| `permissions.allow`, `.ask`, `.deny` | `AGENT_PERMISSIONS_ALLOW`, `_ASK`, `_DENY` | lists of rules, merged across the files |
| `skills.dir`, `skills.in_coding` | `SKILLS_DIR`, `SKILLS_IN_CODING` | |
| `memory.write_mode`, `.delete_mode`, `.default_scope`, `.embedding_backend`, `.recall_injection`, `.max_entries`, `.max_content_bytes`, `.consolidation_hours`, `.deleted_retention_days`, `.in_coding` | `HARNESS_MEMORY_*`, `MEMORY_IN_CODING` | |
| `mcp.servers` | `MCP_SERVERS` (merged) | map `name: {command,args,env}` or `{type,url,headers,tools}` |
| `mcp.allow_stdio` | `HARNESS_MCP_STDIO` | `true`; **user file only** (a project file that sets it is an error) |
| `agents.<role>.<key>` | see above | roles `default`, `explore`, `compaction` |
| `mcp.call_timeout_seconds` | `MCP_CALL_TIMEOUT_SECONDS` | |
| `env` | any setting | `ENV_NAME: value` |

Server and infrastructure settings (`POSTGRES_*`, `REDIS_*`, `ARTIFACTS_*`, `PORT`) stay in the environment.

```yaml
# ~/.har/config.yml  (the user file: the full schema)
provider: anthropic
api_key_env: ANTHROPIC_API_KEY
models: [claude-sonnet-5-5]
default_model: claude-sonnet-5-5
profile: coding
sandbox: {backend: local}
permissions:
  mode: ask
  allow: ["make test*"]
  deny:  ["Bash(terraform apply*)"]
mcp:
  servers:
    docs: {type: http, url: "https://docs.example.com/mcp", headers: {Authorization: "Bearer ${DOCS_TOKEN}"}}
```

### Named providers and API keys

`providers:` (user file only; a project file that has it is an error) defines providers you
select with `provider: <name>` (a project file may select one of yours) or `harness -provider <name>`. `type` is `openai` (any OpenAI-compatible endpoint) or
`anthropic`; a bare `provider: openai|anthropic` stays the built-in type with the `LLM_BASE_URL` default, and the flat
keys (`base_url`, `api_key_env`, ...) and every environment variable keep working. The values of the selected provider
sit above the flat keys of the files and below the environment (`LLM_BASE_URL` still wins).

```yaml
# ~/.har/config.yml
provider: opencode
providers:
  opencode:
    type: openai
    base_url: https://opencode.ai/zen/go/v1
    session_header: x-opencode-session
    models: [deepseek-v4.1-flash]
    default_model: deepseek-v4.1-flash
    api_key_env: OPENCODE_KEY      # optional
  anthropic:
    type: anthropic
```

**Key resolution**, first match wins, for the selected provider:

1. `LLM_API_KEY` in the environment (also a flat `api_key_env`, which fills it);
2. the environment variable named by the provider's `api_key_env`;
3. `~/.har/auth.json`, the entry named like the provider;
4. the conventional variable of its type: `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` (not for a named provider that sets
   its own `base_url`: an OpenAI key is not sent to another host).

With none, the command stops with a message that says how to set one (`harness auth login <provider>`); a local
endpoint (`localhost`, `127.0.0.1`) needs no key. A plain `api_key` is refused in every `config.yml`; keys go in `auth.json`:

```json
{"providers": {"opencode": {"api_key": "sk-..."}, "anthropic": {"api_key": "sk-ant-..."}}}
```

`auth.json` must not be readable by the group or others (`mode & 0o077 == 0`); otherwise it is refused with an error naming
the file and `chmod 600`. The CLI creates it 0600 in a 0700 directory and rewrites it atomically. Keys are never logged;
`-config-dump` shows `LLM_API_KEY  ****  (auth.json)` (or the environment variable) and never the key.

```
harness auth login opencode            # prompts without echo; or: echo "$KEY" | harness auth login opencode -key-stdin
harness auth list                      # providers with a key, last 4 characters, and the file path
harness auth remove opencode
harness -provider opencode -p "hello"
```

### Instruction files

Read for the coding profile, lowest priority first (a later file wins where they disagree; they are all shown to the model):

| # | File | When |
|---|---|---|
| 1 | `~/.har/AGENTS.md` | local backend, or `HARNESS_USER_INSTRUCTIONS=1`; trusted |
| 2 | `.github/copilot-instructions.md`, `.cursorrules`, `.cursor/rules/*.mdc` with `alwaysApply: true` | only if the repo root has no `AGENTS.md` |
| 3 | `AGENTS.md`, else `CLAUDE.md`, else `GEMINI.md` (repo root) | the first that exists; the others are not also loaded |
| 4 | `.har/instructions.md` | always, last |

These go into the system prompt, computed once per run so the prompt stays byte-identical across steps (prompt cache). Each file
is cut at 16 KiB and the total at 48 KiB, with a `[…cut: read_file <path> for the rest]` or `size limit` marker. Text from the
repository is data: the run's context is reported untrusted (memory writes are only proposed) and the block cannot close itself;
only `~/.har/AGENTS.md` is trusted. Everything is read through the sandbox API, so remote sandboxes work; the user's file is the
only one read from the host, and only with the local backend or the explicit `HARNESS_USER_INSTRUCTIONS=1`.

* **`@path/to/file` imports** (Claude Code style) inside an instruction file are appended as `[imported from <path>]`; the path is
  relative to the file, at most 3 levels deep, each file once, 16 KiB each, never outside the workspace (symbolic links are
  resolved and refused if they leave it; in `~/.har/AGENTS.md` imports stay inside `~/.har`). `@alice`, e-mail addresses and
  anything inside code spans or fenced blocks are not imports.
* **Nested files**: an `AGENTS.md` (else `CLAUDE.md`) in a subdirectory applies to the files under it, the closest one wins. They are
  not in the prompt. The first time `read_file`, `write_file` or `edit_file` touches a path under such a directory in a run, the
  note is appended once to that tool's result as `[Instructions for pkg/sub (pkg/sub/AGENTS.md, repository notes for files under this
  directory): ...]` (8 KiB max). Cursor rules with `globs` work the same way, when a matching file is touched.
  Implemented by `sandboxtools.Options.AfterPath`, a generic hook that appends text to those three results.

### Compatibility

| Standard | Read | Not read |
|---|---|---|
| `AGENTS.md` | repo root, nested directories, `~/.har/AGENTS.md` | `AGENTS.override.md` (Codex), `~/.codex/` |
| `CLAUDE.md` | repo root when there is no `AGENTS.md`; nested directories; `@imports` | `~/.claude/CLAUDE.md`, `CLAUDE.local.md`, `.claude/settings.json`, hooks, slash commands |
| `GEMINI.md` | repo root when there is neither of the above | `~/.gemini/`, `@` imports of `.gemini` settings |
| Cursor | `.cursorrules`, `.cursor/rules/*.mdc` (`alwaysApply`, `globs`) | "agent requested" rules (description only), `.cursor/mcp.json` |
| Copilot | `.github/copilot-instructions.md` | `.github/instructions/*.instructions.md` |
| `.mcp.json` | `mcpServers`: `command/args/env`, `type: http\|sse` + `url/headers`; `${VAR}` and `${VAR:-default}` in values | `enabledMcpjsonServers` approvals, OAuth, `type: ws` |
| Agent Skills | `SKILL.md` folders in `.har`, `.agents`, `.claude` (project and user) | `allowed-tools`, plugin marketplaces |
| `.har/permissions.yml` | harness format | Claude Code `settings.json` permission rules |

### MCP servers

Sources, lowest to highest (a later one replaces a server of the same name): `~/.har/config.yml` `mcp.servers`, `~/.har/mcp.json`,
`<repo>/.mcp.json`, `<repo>/.har/mcp.json`, `<repo>/.har/config.yml` `mcp.servers`, `MCP_SERVERS`. Files use the shared format:

```json
{"mcpServers": {
  "docs": {"type": "http", "url": "https://docs.example.com/mcp", "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}},
  "legacy": {"type": "sse", "url": "https://old.example.com/sse"},
  "fs": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."], "env": {"DEBUG": "0"}}
}}
```

`${VAR}` (or `${VAR:-default}`) in `url`, `headers`, `env`, `command` and `args` comes from the process environment; an unset one
without a default is an error naming the server and the variable. The harness adds an optional `tools` allow-list.

The three project sources (`<repo>/.mcp.json`, `<repo>/.har/mcp.json`, `mcp.servers` of the project `config.yml`) are only started
when the project is **trusted** (`harness trust`, see above); otherwise they are listed by `-config-dump`. Your own `~/.har/mcp.json`,
`mcp.servers` in `~/.har/config.yml` and `MCP_SERVERS` are always trusted.

Stdio servers (`command`) start a child process, so they are the dangerous kind (a cloned repository can bring a `.mcp.json`).
The rules:

0. A stdio server that a project file defines also needs your approval of its exact command: `harness trust mcp <name>`, or the in-run prompt of an interactive run (see Trust).
1. They run only with `SANDBOX_BACKEND=local` and an explicit opt-in: `mcp.allow_stdio: true` in **`~/.har/config.yml`** or
   `HARNESS_MCP_STDIO=1`. A project `config.yml` that sets it is an error, so a repository cannot enable them for itself
   (`HARNESS_MCP_STDIO=0` in the environment turns the user file's setting off).
2. Otherwise they are skipped with a warning (listed by `-config-dump`), and the remote ones still work.
3. The tools of a stdio server go through the permission layer as **ask on first use**: one approval per tool and run (the usual
   "approve for the run"); an `allow` rule (`my-server__*(*)` style, see Permissions) stops the question, `deny` blocks them, and
   `yolo` mode, as for all asks, does not prompt.
4. The child gets a reduced environment (`PATH`, `HOME`, `USER`, `LANG`, `TERM`, `TMPDIR`, ...) plus the server's own `env`; the
   harness's API keys and database passwords are not inherited.

## Configuration

`config.Load()` reads the environment, on top of the optional `config.yml` files (see "Configuration files and
standards" above; every variable below can also be set there). The harness-wide settings below use the `HARNESS_` prefix (no other
names are read); the model, sandbox and storage settings keep their descriptive names (`LLM_*`,
`SANDBOX_*`, `OPENSANDBOX_*`, `POSTGRES_*`, `REDIS_*`, `ARTIFACTS_*`, `AGENT_*`, see `config/`).

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_PRODUCTION` | unset | Any non-empty value turns production mode on (stricter startup checks); unset is debug mode |
| `HARNESS_WORKSPACE_CHECKPOINTS` | `true` | Local Git checkpoint of `/workspace` before every turn, with per-file history, diff and restore |
| `HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES` | `20971520` | Files larger than this are kept out of the checkpoints (`<= 0`: no limit) |
| `HARNESS_GIT_ARCHIVE_MAX_BYTES` | `52428800` | Largest `.git` archive stored after a run (a bigger one is not archived) |
| `HARNESS_MAX_UPLOAD_BYTES` | `104857600` | Largest accepted attachment |
| `HARNESS_MAX_CONCURRENT_UPLOADS` | `4` | Concurrent uploads per process |
| `HARNESS_MAX_CONCURRENT_UPLOADS_PER_USER` | `4` | Concurrent uploads per user and process |
| `HARNESS_PURGE_AFTER_DAYS` | `30` | Deleted threads are hard-deleted this many days later (`<= 0`: never) |
| `HARNESS_BLOB_GC_GRACE_HOURS` | `24` | A content blob is collected only after this many hours without a referencing version |
| `HARNESS_MEMORY_WRITE_MODE` | `warn` | `warn` records writes without a valid trigger, `strict` rejects them |
| `HARNESS_MEMORY_DELETE_MODE` | `soft` | `soft` keeps a deleted entry until the retention, `hard` removes it |
| `HARNESS_MEMORY_DEFAULT_SCOPE` | `personal` | Scope of new entries (`repository`, `workspace`, `personal`) |
| `HARNESS_MEMORY_EMBEDDING_BACKEND` | `simple` | Ranking backend (`simple`; `disabled`, `none`, `keyword` only change the reported capabilities) |
| `HARNESS_MEMORY_RECALL_INJECTION` | `true` | Put the recalled memories in the system prompt context |
| `HARNESS_MEMORY_MAX_ENTRIES` | `1000` | Entries per user (`<= 0`: no limit) |
| `HARNESS_MEMORY_MAX_CONTENT_BYTES` | `200000` | Size of one entry |
| `HARNESS_MEMORY_CONSOLIDATION_HOURS` | `168` | Period of the dedup and purge job (`<= 0`: disabled) |
| `HARNESS_MEMORY_DELETED_RETENTION_DAYS` | `30` | Soft-deleted entries are purged after this many days |
| `HARNESS_MCP_STDIO` | unset | `1` allows MCP servers that are local programs (stdio); local backend only. Wins over the user file's `mcp.allow_stdio` |
| `HARNESS_USER_INSTRUCTIONS` | unset | `1` reads the host user's `~/.har/AGENTS.md` also with a remote sandbox backend (always read with the local one) |
| `HARNESS_TRUST_PROJECT` | unset | `1` trusts the current project for this process (its `allow` permission rules and MCP servers apply); see `harness trust` |
| `HARNESS_TEST_DATABASE_URL` | unset | Postgres used by the `adapters/store/pgstore` tests (skipped when unset) |
| `HARNESS_TEST_S3_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY` | unset | S3-compatible endpoint for the artifact blob tests (skipped when unset) |
| `SENTRY_DSN` | unset | `harness-web` only. Reports every error the process logs (failed runs, agent panics, storage and sweeper failures, HTTP 500s, handler panics) to a Sentry-protocol tracker such as Rustrak, Sentry or GlitchTip. Events are grouped by log message, carry the run and thread id, and contain no request bodies, headers or user identity. Warnings and cancellations are not sent; an unreachable tracker never blocks a request |
| `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE` | `production` under `HARNESS_PRODUCTION`, else `development`; unset | Tags on every event |
| `SENTRY_BROWSER_DSN` | unset | `harness-web` only. Errors raised in the browser UI (uncaught errors, unhandled rejections, a crashed screen) go to this Sentry-protocol project. Use a project of its own, so UI and server errors do not mix. The UI reads it from `GET /api/client-config` at startup and loads the error SDK only then. Nothing the user typed is collected: no cookies, headers, request bodies, query strings, local variables or identity |
| `UMAMI_URL`, `UMAMI_WEBSITE_ID` | unset | `harness-web` only. Usage statistics with a self-hosted [Umami](https://umami.is): `UMAMI_URL` is its origin (`https://umami.example.com`), `UMAMI_WEBSITE_ID` the website's id. Both are needed. Page views follow the UI's hash routes; a signed-in person is identified by their login and nothing else (no name or email). The browser's do-not-track setting is honoured |

With either browser setting, the UI's Content-Security-Policy widens to exactly those origins (`script-src` for Umami, `connect-src` for both) and no further; with neither it stays `'self'`. A malformed value stops `harness-web` at startup.

Memory entries live in the store; their logical paths (`memories/semantic/facts.md`, ...) may be written with
the `.har/memory/` prefix, which is stripped. The workspace checkpoints leave `.har/tmp/` and
`.har/runtime/` out of the history.

## Building

`make build` produces stripped binaries in `bin/` (`app/cmd/harness` is about 30 MB; a plain `go build` is 44 MB
because it keeps the symbol table and DWARF). Idle resident memory is about 20 MB. An application that embeds
the harness links none of go-redis, the MCP SDK or the OpenSandbox SDK unless it imports `redisinfra`, `mcpfeature` or
`adapters/sandbox/opensandbox` (a test pins this); the engine, sandbox, features and store packages do not import go-redis.

CI (`.github/workflows/ci.yml`, every push and pull request) runs `gofmt`, `go vet` and `go test ./...` against a
Postgres service, the web checks (`typecheck`, `test`, `build`) and a Docker build plus a `docker-compose.prod.yml`
validation. `make ci` runs the Go and web steps locally. After changing a DTO of `hosts/client` or a view of `product`, run
`make gen`: `go test ./cmd/gen-ts` (run in `app/`) fails while `web/src/api/generated.ts` is stale.

## Status / known gaps

* Requires Go 1.26 (`go test ./...`). `adapters/store/pgstore` tests need a Postgres (`HARNESS_TEST_DATABASE_URL`) and skip otherwise.
* A remote sandbox's project skills are read once per run, when the run first uses its sandbox (or at prompt time if
  one is already running), so a skill added to `/workspace` mid-run is seen by the next run. When the run itself
  starts the sandbox, the project skills it holds (fenced as untrusted, like the prompt's) are announced to the model
  in a note before its next model call, once per run, because the prompt never starts a sandbox just to list them.
* Web search is not built in: configure any web-search MCP server in `MCP_SERVERS`.
* The sandbox image (`sandbox-image/Dockerfile`) was built and smoke-tested on 2026-10-30 for linux/arm64 and linux/amd64 (see `sandbox-image/README.md`); it has not yet been run under a live OpenSandbox lifecycle server.

## License

MIT, see [LICENSE](LICENSE). The standalone modules (`ai`, `core`, `modelinfo`, `hosts/client`, `hosts/tui`, `adapters/sandbox`) carry the same license.
