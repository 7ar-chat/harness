# harness

An open, embeddable agent harness written in Go: a ReAct loop
([eino](https://github.com/cloudwego/eino)) whose tools run in a **sandbox** (a local directory or a remote
[OpenSandbox](https://github.com/alibaba/OpenSandbox) container), streaming
[AG-UI](https://docs.ag-ui.com) events. The Go loop drives the model (OpenAI-compatible or Anthropic) and the
sandbox only executes; nothing spawns another agent CLI. It ships tools (files, shell, search, document
rendering), skills, memory, goals and plans, tool approvals and permissions, context compaction and
verification of produced documents.

It is meant to sit behind any front end: a web server, a TUI, a worker, a desktop app. The repository contains
a terminal client (`cmd/harness`), a `net/http` + SSE server package and a benchmark runner; product-specific
HTTP APIs (authentication, multi-tenant routing) are left to the embedding application.

```
        web UI ──► hosts/web (net/http, SSE) ──┐
        TUI    ──► cmd/harness (reference)  ───┤
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
cfg, _ := config.Load()                       // env: LLM_*, SANDBOX_*, OPENSANDBOX_*, HARNESS_*, …
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

* **Web**: `httpserver.New(h, httpserver.Options{…})` (`hosts/web`) serves threads, runs, approvals,
  files and the run's events as SSE (`id` = seq, so reconnects resume without loss), plus a built-in
  browser UI; `cmd/harness-web` runs it (see [Web host](#web-host)).
* **TUI**: `cmd/harness` is a Bubble Tea terminal client with a print mode (see
  [Terminal UI](#terminal-ui)); package `tui` has the model and renderers to embed.
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

## LLM providers

`LLM_PROVIDER` is `openai` (the default: any OpenAI-compatible endpoint at `LLM_BASE_URL` with `LLM_API_KEY`;
Chat Completions, or `{LLM_BASE_URL}/responses` for the models in `LLM_RESPONSES_MODELS`) or `anthropic`.
Model ids (`LLM_MODELS`) are sent to the provider verbatim. A user is just a login string; the harness
forwards no identity to the model or to MCP servers.

## Providers that route by session

`LLM_SESSION_HEADER=x-opencode-session` sends the thread id in that header on every model call. OpenCode Go
requires it (it routes a conversation to one backend, which keeps the prompt cache warm: 86% cached on the
benchmark against 58–68% through a router without it):

```
LLM_PROVIDER=openai LLM_BASE_URL=https://opencode.ai/zen/go/v1 LLM_API_KEY=… \
LLM_SESSION_HEADER=x-opencode-session LLM_MODELS=deepseek-v4.1-flash LLM_DEFAULT_MODEL=deepseek-v4.1-flash
```

## CLI sessions

`harness` saves each conversation under `LOCAL_STATE_DIR` (default `~/.local/state/harness`):
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

## Web host

`hosts/web` (package `httpserver`) is a standalone HTTP server for any web front end, with a small browser UI
embedded. `cmd/harness-web` runs it:

```
harness-web -local                      # in a repository: coding agent on this directory, UI at http://127.0.0.1:8080/
HARNESS_WEB_ADDR=0.0.0.0:8080 HARNESS_WEB_TOKEN=secret OPENSANDBOX_URL=… harness-web   # remote sandbox, shared
```

`-local` sets `SANDBOX_BACKEND=local` and `AGENT_PROFILE=coding` (unless already set) and keeps sessions on disk
(`harness.OpenLocalStore`). Flags: `-addr` (default `HARNESS_WEB_ADDR`, else `127.0.0.1:8080`), `-model`, `-local`,
`-user` (the single user's login, default `local`). A non-loopback address is refused unless `HARNESS_WEB_TOKEN` is set.

**Routes** (all under `/api`, also served without the prefix; errors are `{"error":{"code","message"}}`; the full
table with bodies is the doc comment of `hosts/web/httpserver.go`):

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

**UI** (`hosts/web/ui`, `go:embed`, about 450 lines of plain HTML/CSS/JS, no build step, no CDN): conversation list,
streamed transcript with basic markdown, collapsible tool calls (edit diffs), status line (model, steps, tokens,
cache %), send / steer / cancel, approval and question prompts, a file panel with downloads, light and dark themes,
phone-width layout. With a token set the UI asks for it once and keeps it for the tab.

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
`readonly` (only read-only commands, no edits). Rules go in `.harness/permissions.yml` (workspace) and
`~/.config/harness/permissions.yml`, or in `AGENT_PERMISSIONS_ALLOW|ASK|DENY` (entries separated by
newlines or `;;`); deny beats ask beats allow, and a user allow overrides a built-in ask:

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
| `user` | `SKILLS_DIR`, default `~/.config/harness/skills` | one folder per skill |
| `project` | `<workspace>/.harness/skills` | not wired yet (see Status); fenced as untrusted text |

```
~/.config/harness/skills/
  release-notes/
    SKILL.md          # ---\nname: release-notes\ndescription: Draft release notes from merged PRs.\n---\n…
    scripts/collect.py
```

The folder name is the skill name. `RunOptions.Skills: []string{"release-notes"}` attaches a skill to a
run (its `SKILL.md` goes into the system prompt and its files are installed before the first sandbox
command); the built-ins are only listed and installed on demand. An optional `requires: [other-skill]`
frontmatter list installs dependencies with a skill. Packages are validated (entry count, depth, file
and total size, safe paths) when loaded; an invalid folder is logged and skipped.

## Layout

The repository is one Go module in strict layers; a lower layer never imports a higher one
(`layers_test.go` enforces it).

```
config/ store/                 foundation: configuration, the persistence interfaces
engine/agui, syncx, prompts    foundation: AG-UI events, small helpers, prompt text
sandbox/                       where tools run: contract + manager; local (your machine), opensandbox (remote), sandboxtest (fake)
engine/                        the reusable intelligence
  agent/      the ReAct loop: steps, streaming, retries, steering, hooks, spill, masking
  llm/        OpenAI chat + Responses, Anthropic Messages, retry, test doubles (llm/llmtest)
  tools/      tool interfaces; tools/sandboxtools = read/write/edit/search/exec/jobs
  runtime/    run claiming, heartbeat, sweeper, end-of-run hooks
  eventlog/ queue/ compaction/ approvals/
store/{memstore,filestore,pgstore}   persistence adapters (memory, durable local files, Postgres)
features/                      optional capabilities a host switches on
  permissions/ gitsafe/ codecheck/ projectdocs/ explore/ plan/ mcp/
  memory/ skills/ goals/ titles/ verify/ artifacts/ workspace/
hosts/                         ways to drive the engine
  tui/ (terminal)   web/ (HTTP + SSE + embedded UI)
harness.go                     the facade: New / Send / Events / Steer / Cancel / Resume
cmd/harness  cmd/harness-web  cmd/bench   binaries;  bench/ longtest/ = benchmark and long-session tests
```

## Configuration

`config.Load()` reads the environment. The harness-wide settings below use the `HARNESS_` prefix (no other
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
| `HARNESS_TEST_DATABASE_URL` | unset | Postgres used by the `store/pgstore` tests (skipped when unset) |
| `HARNESS_TEST_S3_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY` | unset | S3-compatible endpoint for the artifact blob tests (skipped when unset) |

Memory entries live in the store; their logical paths (`memories/semantic/facts.md`, ...) may be written with
the `.harness/memory/` prefix, which is stripped. The workspace checkpoints leave `.harness/tmp/` and
`.harness/runtime/` out of the history.

## Building

`make build` produces stripped binaries in `bin/` (`cmd/harness` is about 30 MB; a plain `go build` is 44 MB
because it keeps the symbol table and DWARF). Idle resident memory is about 20 MB. go-redis remains linked;
the coding profile does not use it.

## Status / known gaps

* Requires Go 1.26 (`go test ./...`). `store/pgstore` tests need a Postgres (`HARNESS_TEST_DATABASE_URL`) and skip otherwise.
* Project skills (`<workspace>/.harness/skills`) are not discovered yet: the registry supports the
  `project` source (`Registry.AddFS` / `AddDir`, fenced as untrusted data) but nothing feeds it a
  workspace directory per run. Follow-up.
* Web search is not built in: configure any web-search MCP server in `MCP_SERVERS`.
* The sandbox image was adapted for public base images and not built here.
