---
title: "The Go facade"
description: "Package harness: Options, RunOptions, the Harness methods, features and profiles, the Builder, stores, sandbox helpers, Infra and the exported errors."
---

Package `github.com/7ar-chat/harness` is the facade: it wires the engine, the features, the store, the sandboxes and the shared infrastructure into one `*harness.Harness`, and a host (a web server, a terminal UI, a desktop app) drives it with a handful of calls. This page is the reference; for a first program see [Smallest embedding](/docs/quickstart/embed/), for the layering [Architecture](/docs/introduction/architecture/).

```go
h, err := harness.New(harness.Options{Config: cfg})
go h.Run(ctx)                                  // executes queued runs
th, _ := h.NewThread(ctx, "alice", "My thread")
run, _ := h.Send(ctx, "alice", th.ID, "Make a chart", harness.RunOptions{Model: "gpt-5"})
for ev := range h.Events(ctx, run.ID, 0) { /* AG-UI events; ev.Seq is the resume point */ }
```

Every method takes the user's login: it is the tenancy boundary. Threads, runs, sandboxes, memory and artifacts are scoped by it, and another user's id is `store.ErrNotFound`, never "forbidden".

<div class="callout breaking">

**v0.3 changes.** `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` were removed. Redis is the package `github.com/7ar-chat/harness/redisinfra`: `redisinfra.NewClient(cfg)` and `redisinfra.Infra(rdb, store)`, passed as `Options.Infra`. MCP is opt-in through a blank import of `github.com/7ar-chat/harness/mcpfeature`; the OpenSandbox backend through `github.com/7ar-chat/harness/adapters/sandbox/opensandbox`. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

## Options

`harness.New(o Options) (*Harness, error)`. Only `Config` is required (`config.Load()` reads it from the environment).

| Field | Type | Meaning |
|---|---|---|
| `Config` | `*config.Config` | The environment-driven configuration. Required: `harness: Options.Config is required`. |
| `Store` | `harness.Store` | Persistence for threads, runs, messages, artifacts, memory. Default: in memory. `OpenLocalStore` for one node on disk, `adapters/store/pgstore` for Postgres. See [The store](/docs/concepts/store/). |
| `Models` | `llm.Factory` | Builds the chat models. Default: `llm.New` over the `LLM_*` settings (OpenAI-compatible, Anthropic, Responses). Tests pass `llmtest.Factory`. |
| `Backend` | `sandbox.Backend` | Provides the sandboxes. Default: the backend `SANDBOX_BACKEND` names, else OpenSandbox when `OPENSANDBOX_URL` is set, else none (no sandbox tools). See [Sandboxes](/docs/concepts/sandboxes/). |
| `Blobs` | `artifacts.Blobs` | Artifact contents. Default: the Store. `adapters/blobs/s3blobs` for S3. |
| `Infra` | `*harness.Infra` | What several processes share. Every part optional; a missing one is in-process. See [Infrastructure](/docs/concepts/infra/). |
| `Features` | `[]harness.Feature` | Capabilities to install, in order. Default: the profile of `AGENT_PROFILE`. See [Features and profiles](/docs/concepts/features-profiles/). |
| `BuiltinSkills` | `fs.FS` | `<name>/SKILL.md` folders. Default: the built-in skills (`features/skills/skillsfs`). |
| `ExtraSkills` | `fs.FS` | More skill folders, added after the built-ins and before the user directory (`SKILLS_DIR`). |
| `Tools` | `[]tools.Provider` | Host-defined tools. See [Add a custom tool](/docs/guides/custom-tool/). |
| `Hooks` | `[]runtime.Hook` | Run at the end of every run, after the harness' own. |
| `Version` | `string` | Identifies the host to MCP servers. |
| `RuntimeID` | `string` | Names this process in run ownership. Default: hostname plus a random suffix. |
| `MemoryKey` | `func(ctx, login, threadID) string` | The memory a run reads and writes; `""` means the login's own. A team host returns `project:<id>` for project threads. |
| `MemoryUserKey` | `func(ctx, login, threadID) string` | The user's private layer inside a shared memory (`""` for none). |
| `MemoryShared` | `func(key string) bool` | Whether a key is a memory shared between people. |

## RunOptions

`Send(ctx, user, threadID, text, RunOptions)`.

| Field | Meaning |
|---|---|
| `Model` | Required: an id the Config allows. |
| `ReasoningEffort` | `low`, `medium`, `high`, ... or `""` for the model's default. |
| `Skills` | Skill names requested explicitly for this run. |
| `Interactive` | Risky tool calls wait for a decision (`Resume`). |
| `MessageID` | Makes `Send` idempotent for that user message. |
| `Sandbox` | `SandboxAuto` (default: sandbox tools when a backend exists, created on first use), `SandboxOff` (no sandbox tools, no-sandbox prompt), `SandboxOn` (like auto, but `Send` fails with `ErrInvalidRequest` when the harness has no sandbox). Kept in the run record, so a resumed run, a goal turn and a compaction keep it. |
| `Mode` | `ModeBuild` (default) or `ModePlan` (read-only: reads, searches, read-only MCP tools, no command execution). Kept in the run record, so a worker honours it. |
| `Attachments` | Images (`Attachment{Name, MIME, Data}`; `image/png`, `jpeg`, `gif`, `webp`), at most `MaxAttachments`, each at most `MaxAttachmentBytes` (5 MiB). Stored in the thread's workspace. |
| `Author`, `AuthorKind`, `OnBehalfOf` | Who typed the message when it is not the thread's owner (a project member, a supervisor). |

`ResumeRequest` answers an interrupt: `InterruptID`, `Reason`, `Decision` (`store.DecisionApprove`, `DecisionDeny`, `DecisionRespond`), `Always`, `Text`, `Model`, `Interactive`, `ReasoningEffort`, `Mode` (empty keeps the thread's previous mode).

## The Harness

`Harness` exposes its parts as fields: `Store`, `Log` (the event log), `Queue`, `Bus`, `Sandboxes`, `Artifacts`, `Skills`, `Workspace`, `Runtime`, `Agent`, and `Memory` (nil without the Memory feature).

| Method | Purpose |
|---|---|
| `Run(ctx) error` | Consume queued runs until `ctx` is done, then let in-flight runs finish (or interrupt them after the shutdown grace). |
| `NewThread(ctx, user, title) (*store.Thread, error)` | Create a conversation. |
| `Send(ctx, user, threadID, text, RunOptions) (*store.Run, error)` | Add a user message and queue a run answering it. |
| `Events(ctx, runID, after int64) <-chan store.Event` | The run's [events](/docs/reference/events/) with `seq > after`, then live until the terminal event. It takes no user: the caller must have checked that the run belongs to the requester (`Store.GetUserRun`), as `hosts/client` and `hosts/httpapi` do. |
| `Steer(ctx, user, runID, text) error`, `SteerAs(..., Author)` | Add a message to a running run; the model sees it at its next step. |
| `Cancel(ctx, user, runID) error` | A queued run is cancelled at once; a running one ends with a `cancelled` outcome. |
| `Resume(ctx, user, threadID, ResumeRequest) (*store.Run, error)` | Answer an approval or an `ask_user` question and queue the continuing run. |
| `Compact(ctx, user, threadID, model) (*CompactResult, error)` | Summarise the history now with a run of kind compact, and wait. `store.ErrRunActive` while a run is in progress. |
| `Undo`, `Redo(ctx, user, threadID) (*HistoryStep, error)` | Remove the last user message with everything after it (and restore the files in a git workspace); reapply it. The thread must be idle. |
| `Fork(ctx, user, threadID, messageID) (*store.Thread, error)` | Copy the thread up to and including `messageID` (`""`: all) into a new thread titled `<old> (fork)`. |
| `HasSandbox() bool` | Whether runs can use a sandbox (a backend is configured and the Sandbox feature is installed). |

`CompactResult` has `Compacted`, `TokensBefore`, `TokensAfter`; `HistoryStep` has `Prompt`, `Files`, `CanUndo`, `CanRedo`. Front ends should prefer the [Client contract](/docs/reference/client-contract/) to calling these directly.

## Features and profiles

A `Feature` is `Name() string` plus `Install(*Builder) error`: a set of tools, prompt sections, hooks and background jobs that belong together. `Install` runs once, in the order of `Options.Features`, while the harness is built, and returns an error to abort `New`. `FeatureFunc(name, install)` adapts a function. See [Add a custom feature](/docs/guides/custom-feature/).

| Constructor | Name constant | Contributes |
|---|---|---|
| `Sandbox()` | `FeatureSandbox` | `execute_command`, `job_output`, `job_kill`, `read_file`, `view_file`, `search_files`, `write_file`, `edit_file`, `list_files`. Nothing without a backend. |
| `Codecheck(coding)` | `FeatureCodecheck` | Syntax check appended to `write_file` / `edit_file` results. Requires Sandbox. |
| `Explore()` | `FeatureExplore` | The read-only `explore` sub-agent (`LLM_EXPLORE_MODEL`). Requires Sandbox. |
| `Skills()` | `FeatureSkills` | `load_skill`, the skills index, installation into sandboxes. Publishes `Builder.Skills`, `Builder.Installer`. |
| `MCP()` | `FeatureMCP` | Tools of the configured MCP servers. Needs `import _ ".../mcpfeature"`. |
| `Plan()` | `FeaturePlan` | `update_plan` and `ask_user`. |
| `ProjectDocs()` | `FeatureProjectDocs` | `AGENTS.md` and the files other agent tools read, in the prompt. |
| `Artifacts()` | `FeatureArtifacts` | Snapshots of produced files, previews, retention purge. Publishes `Builder.Artifacts`. |
| `Workspace()` | `FeatureWorkspace` | Git checkpoints of `/workspace` before every turn. Requires Artifacts. Publishes `Builder.Workspace`. |
| `Titles()` | `FeatureTitles` | Names new threads from their first exchange. |
| `Verify()` | `FeatureVerify` | Deliverable checkers. Requires Artifacts. Publishes `Builder.Verifier`. |
| `Goals()` | `FeatureGoals` | A goal judged after every run; continues while unmet. |
| `Memory()` | `FeatureMemory` | Memory tools, recall and consolidation. Publishes `Builder.Memory`. |
| `ToolProfile(coding)` | `FeatureToolProfile` | The tool allow-list and `AGENT_TOOLS` overrides. |
| `Permissions(coding)` | `FeaturePermissions` | Allow / ask / deny rules and modes in front of the tools; git snapshots on the local backend. |

Profiles are ready-made lists: `ProfileWork()` (everything; `AGENT_PROFILE=work` or unset), `ProfileCoding()` (`AGENT_PROFILE=coding`: work plus explore, code checks and permissions, with the lean tool allow-list) and `ProfileMinimal()` (sandbox and plan tools only). `ProfileNamed(name)` maps an `AGENT_PROFILE` value: `coding`, `minimal`, anything else is work.

```go
feats := append(harness.ProfileMinimal(), harness.FeatureFunc("orders", func(b *harness.Builder) error {
	b.AddTools(myTools{})
	return nil
}))
h, err := harness.New(harness.Options{Config: cfg, Features: feats})
```

### Builder

A feature sees the environment through the `Builder`: `Config`, `Store`, `Models`, `Sandboxes`, `Blobs`, `Infra`, `Options`, and the services earlier features published (`Artifacts`, `Workspace`, `Skills`, `Installer`, `Memory`, `Verifier`; nil until installed, so install the provider earlier in the list). It contributes through these methods:

| Method | Adds |
|---|---|
| `AddTools(...tools.Provider)` | Tool providers, before `Options.Tools`. |
| `Section(s)`, `TailSection(s)`, `ContextSection(s)` | A static system-prompt section; a section in the reminder at the end of every request; a dynamic section of the final context block. |
| `StepNote(n)` | A note source asked before every model call. |
| `BeforeTool(h)`, `ToolHook(h)`, `WrapTools(w)` | A gate before tool calls; an observer of results; a wrapper of the whole registry. |
| `RunStart(h)`, `EndOfRun(h)` | A hook before the first step; a hook at the end of every run. |
| `Job(j)`, `Background(f)` | A periodic maintenance job; a goroutine started once `New` returns. |
| `SandboxTools(f)`, `OnSandboxCreate(f)`, `PrepareSandbox(f)` | Options of the sandbox tools; functions run when a sandbox is created. |
| `SetChecker`, `SetAnswers`, `SetImages`, `SetPurger` | The single-owner slots (an error when a second feature sets one). |
| `Configure(f)` | Change the agent's configuration after every feature contributed. |
| `After(f)` | Run once every feature is installed. |
| `Has(name)`, `Require(names...)`, `RequireSandbox()` | Test or demand other features. |
| `Provide[T](b, v)`, `Lookup[T](b)`, `StoreAs[T](b)` | Publish a service by type; find one; get the store as a capability type. |

## Stores

```go
type Store interface {
	store.Full
	sandbox.Store
	store.WorkspaceGitStore
}
```

| Function | Purpose |
|---|---|
| `OpenLocalStore(dir) (*filestore.Store, error)` | The durable single-process store for local hosts: threads, messages, runs, plans, summaries, approvals and event logs survive restarts, with no Postgres or Redis. Close it on exit. `""` uses `DefaultLocalStoreDir()`. Runs in progress when the previous process died are marked interrupted. `Store.Sessions()` lists, forks and deletes threads. |
| `DefaultLocalStoreDir() string` | `$LOCAL_STATE_DIR`, else `<user dir>/sessions` (`$HAR_HOME` or `~/.har`), else `$XDG_STATE_HOME/harness`, else `~/.local/state/harness`. |

## Sandbox helpers

| Function | Purpose |
|---|---|
| `NewSandboxBackend(cfg) (sandbox.Backend, error)` | Builds the backend the configuration selects: the registered factory of `SANDBOX_BACKEND`, or `opensandbox` when only `OPENSANDBOX_URL` is set, or none (a nil backend, nil error). Applies the production refusal of unrestricted egress first. Every process that can create a sandbox goes through it. |
| `SandboxSettings(cfg) backends.Settings` | Maps `SANDBOX_BACKEND`, `OPENSANDBOX_*`, `SANDBOX_*`, `LOCAL_*` to the settings the registered backends are built from. |

An unregistered `opensandbox` fails with an error that ends `; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it`. See [Sandbox backends](/docs/integrations/sandbox-backends/) and [Write a sandbox backend](/docs/guides/sandbox-backend/).

## Infra

Everything the harness shares between processes. Every field is optional; a zero `Infra` is a self-contained single-process harness.

| Field | Default | Role |
|---|---|---|
| `Log` | `eventlog.NewMemory` over the Store | The per-run event log. |
| `Broker` | `queue.NewMemory` | Run queue and per-run control bus (`queue.Queue`, `queue.Bus`, `runtime.RunQueue`). Any transport (NATS, SQS) implements the same three. |
| `Coordinator` | none, in-process | Sandbox creation locks and health streaks, sweeper and job locks, run liveness marks. |
| `Jobs` | memory only | Persists running background jobs so a restarted process resumes their keep-alives. |
| `Installs` | in-process | Which skill revisions each sandbox holds, and the install locks. |

```go
rdb := redisinfra.NewClient(cfg)
infra := redisinfra.Infra(rdb, store)
h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
```

See [Postgres and Redis for several processes](/docs/guides/postgres-redis/) and [Redis](/docs/integrations/redis/).

## Errors and limits

| Name | Meaning |
|---|---|
| `ErrInvalidRequest` | `invalid request`: wraps the errors caused by the caller's input. |
| `ErrNoSandbox` | `this run requires a sandbox but the harness has none: set Options.Backend or SANDBOX_BACKEND`. Returned by `Send` (wrapped in `ErrInvalidRequest`) for `SandboxOn`. |
| `ErrNothingToUndo`, `ErrNothingToRedo` | `nothing to undo`; `nothing to redo` (none was undone, or a message was sent since). |
| `ErrHistoryUnsupported` | `the store cannot edit a thread's history`. |
| `MaxAttachments`, `MaxAttachmentBytes` | Limits of `RunOptions.Attachments`. |

The MCP error when the feature is configured but not linked reads `harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"`; `harness.New` wraps it as `harness: feature "mcp": ...`. A configuration naming no MCP servers installs nothing and needs no import. `RegisterMCP` is what `mcpfeature` calls from its `init`.

Store errors such as `store.ErrNotFound` and `store.ErrRunActive` come from `core/store`; `hosts/client` maps them to its [error sentinels](/docs/reference/client-contract/#errors).
