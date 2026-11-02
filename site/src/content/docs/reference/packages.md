---
title: "Package index"
description: "One line per Go package of the repository and its eight modules, grouped by layer, with the module that ships each."
---

The repository is a root Go module plus seven standalone modules joined by `go.work`. This index lists the 77 packages that `go list ./...` reports in each of the eight modules (test-only helpers are included only where they are importable packages, such as `llmtest`, `sandboxtest` and `clienttest`). A package imports only packages of its own layer or a lower one; the enforced rules are in [Architecture](/docs/introduction/architecture/) and the choice of module in [Module map](/docs/introduction/modules/).

## Modules

| Module | Import path | Holds |
|---|---|---|
| root | `github.com/7ar-chat/harness` | The facade, engine, features, adapters, hosts (`httpapi`, `inproc`, `worker`, `clienttest`) and `config` |
| `ai` | `github.com/7ar-chat/harness/ai` | Model clients and the agent-loop vocabulary; imports nothing else of this repository |
| `core` | `github.com/7ar-chat/harness/core` | Events, domain types and ports (`agui`, `store`, `sandbox`, `blobs`, `layout`) |
| `modelinfo` | `github.com/7ar-chat/harness/modelinfo` | Model windows, capabilities and prices |
| `hosts/client` | `github.com/7ar-chat/harness/hosts/client` | The `Client` contract |
| `hosts/tui` | `github.com/7ar-chat/harness/hosts/tui` | The terminal UI; imports only `hosts/client` and `core/agui` |
| `adapters/sandbox` | `github.com/7ar-chat/harness/adapters/sandbox` | The sandbox backends (local, OpenSandbox) and their registry |
| `app` | `github.com/7ar-chat/harness/app` | The commands and the team product |

The smaller modules are released with the root module and can be used on their own; see [Use the ai module on its own](/docs/guides/use-ai/), [Use core and modelinfo on their own](/docs/guides/use-core-modelinfo/) and [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/).

## Packages

### Root module: facade and support

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness` | `root` | The facade: `Options`, `Harness`, `Feature`/`Builder`, profiles, `OpenLocalStore`, `NewSandboxBackend`. |
| `github.com/7ar-chat/harness/bench` | `root` | Runs coding tasks through the real harness against a model and reports pass/fail and cost. |
| `github.com/7ar-chat/harness/config` | `root` | Loads the configuration: environment, `config.yml` layers, `auth.json`, trust, model prices. |
| `github.com/7ar-chat/harness/longtest` | `root` | Drives the harness through long scripted sessions with a fake model (tests). |
| `github.com/7ar-chat/harness/mcpfeature` | `root` | Implementation of `harness.MCP()`; import it for its side effect to enable MCP servers. |
| `github.com/7ar-chat/harness/prompts` | `root` | The agent system prompts: per-model variants, optional sections, cache-stable assembly order. |
| `github.com/7ar-chat/harness/redisinfra` | `root` | Builds the `harness.Infra` of a Redis deployment (`NewClient`, `Infra`). |

### Foundation: core, ai, modelinfo

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/ai` | `ai` | Messages, tool descriptions, call options, streams, and helpers to build tools from Go functions. |
| `github.com/7ar-chat/harness/ai/agent` | `ai` | The tool-calling loop: ask the model, run its tools, feed results back until it answers. |
| `github.com/7ar-chat/harness/ai/compaction` | `ai` | Keeps an agent prompt inside the context window (summarising, masking). |
| `github.com/7ar-chat/harness/ai/llm` | `ai` | Builds chat models for a provider and classifies their failures (retry classes). |
| `github.com/7ar-chat/harness/ai/llm/anthropic` | `ai` | `ai.Model` over the Anthropic Messages API. |
| `github.com/7ar-chat/harness/ai/llm/llmtest` | `ai` | Scripted fake chat model for tests; runs offline. |
| `github.com/7ar-chat/harness/ai/llm/responses` | `ai` | `ai.Model` over the OpenAI Responses API, with encrypted reasoning items. |
| `github.com/7ar-chat/harness/ai/openai` | `ai` | `ai.Model` over the OpenAI Chat Completions API and its many compatible gateways. |
| `github.com/7ar-chat/harness/core/agui` | `core` | The event vocabulary of a run: AG-UI events, usage, error codes, order checks. |
| `github.com/7ar-chat/harness/core/blobs` | `core` | Port for storing content by key outside the database. |
| `github.com/7ar-chat/harness/core/layout` | `core` | Names the folders of a configuration directory (`<repo>/.har/`, `~/.har/`). |
| `github.com/7ar-chat/harness/core/sandbox` | `core` | The contract between an agent and the place its commands run. |
| `github.com/7ar-chat/harness/core/sandbox/sandboxtest` | `core` | In-memory sandbox backend for tests. |
| `github.com/7ar-chat/harness/core/store` | `core` | Domain types and the persistence interfaces (the ports). |
| `github.com/7ar-chat/harness/modelinfo` | `modelinfo` | Model context windows, limits, capabilities and per-token prices from LiteLLM's table. |

### Engine

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/engine/agent` | `root` | The agent loop: streams AG-UI events, checkpoints messages, steering, approvals, compaction. |
| `github.com/7ar-chat/harness/engine/approvals` | `root` | Interactive mode: risky tool calls pause until the user decides. |
| `github.com/7ar-chat/harness/engine/eventlog` | `root` | The per-run event log contract and its in-process implementation. |
| `github.com/7ar-chat/harness/engine/queue` | `root` | Run queue, per-run control bus (cancel) and steer list; in-memory implementation. |
| `github.com/7ar-chat/harness/engine/runtime` | `root` | Claims queued runs, drives the agent, finishes runs; sweeper and jobs. |
| `github.com/7ar-chat/harness/engine/syncx` | `root` | Keyed mutex and bounded LRU cache. |
| `github.com/7ar-chat/harness/engine/tools` | `root` | Tool providers assembled per run. |

### Adapters

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/adapters/blobs/s3blobs` | `root` | S3-compatible artifact blob store (AWS, RustFS, Dell ECS). |
| `github.com/7ar-chat/harness/adapters/errtrack` | `root` | Sends logged errors to a Sentry-protocol tracker. |
| `github.com/7ar-chat/harness/adapters/redisx` | `root` | Redis client (Sentinel or standalone), coordinator, feed bus, Redis event log and queue. |
| `github.com/7ar-chat/harness/adapters/sandbox/backends` | `adapters/sandbox` | Registry of backends selectable by `SANDBOX_BACKEND`. |
| `github.com/7ar-chat/harness/adapters/sandbox/local` | `adapters/sandbox` | Sandbox that runs on the host: no container, no server. |
| `github.com/7ar-chat/harness/adapters/sandbox/opensandbox` | `adapters/sandbox` | OpenSandbox backend; a blank import registers `SANDBOX_BACKEND=opensandbox`. |
| `github.com/7ar-chat/harness/adapters/store/filestore` | `root` | Durable single-process store over an append-only journal (`OpenLocalStore`). |
| `github.com/7ar-chat/harness/adapters/store/memstore` | `root` | In-memory store (tests and the default). |
| `github.com/7ar-chat/harness/adapters/store/pgstore` | `root` | PostgreSQL store (`NewPool`, `NewPG`). |
| `github.com/7ar-chat/harness/adapters/store/pgstore/migrate` | `root` | Applies the embedded SQL migrations. |
| `github.com/7ar-chat/harness/adapters/store/teamstore` | `root` | `store.TeamStore`: in memory with a JSON file, or PostgreSQL. |

### Coding toolset

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/coding/codecheck` | `root` | Syntax feedback appended to `write_file` / `edit_file` results. |
| `github.com/7ar-chat/harness/coding/gitsafe` | `root` | Restorable snapshots of a git working tree that do not touch branches, index or stash. |
| `github.com/7ar-chat/harness/coding/sandboxtools` | `root` | The sandbox tools: `execute_command`, `read_file`, `write_file`, `edit_file`, `search_files`, `list_files`... |
| `github.com/7ar-chat/harness/coding/sandboxtools/conformance` | `root` | Checks that a sandbox adapter runs the sandbox tools end to end. |

### Features

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/features/artifacts` | `root` | Durable, versioned files the agent produces under `/workspace`; previews; downloads. |
| `github.com/7ar-chat/harness/features/artifacts/workspace` | `root` | Git history of a thread's `/workspace`: checkpoints, diffs, restores. |
| `github.com/7ar-chat/harness/features/commands` | `root` | Custom commands: markdown prompt templates invoked by name. |
| `github.com/7ar-chat/harness/features/explore` | `root` | The `explore` tool: a read-only sub-agent. |
| `github.com/7ar-chat/harness/features/goals` | `root` | A goal per thread, judged after each run, continued while unmet. |
| `github.com/7ar-chat/harness/features/mcp` | `root` | Exposes the tools of configured MCP servers (behind `mcpfeature`). |
| `github.com/7ar-chat/harness/features/memory` | `root` | Long-term memory service: entries, search, write policy, recall, consolidation. |
| `github.com/7ar-chat/harness/features/memory/memorytools` | `root` | The `memory_*` tools backed by the memory service. |
| `github.com/7ar-chat/harness/features/permissions` | `root` | Allow / ask / deny rules and modes in front of tool calls. |
| `github.com/7ar-chat/harness/features/plan` | `root` | The agent tools `update_plan` and `ask_user`. |
| `github.com/7ar-chat/harness/features/projectdocs` | `root` | `AGENTS.md` and other agents' instruction files in the prompt. |
| `github.com/7ar-chat/harness/features/skills` | `root` | Skill registry: `SKILL.md` folders. |
| `github.com/7ar-chat/harness/features/skills/skillsfs` | `root` | The embedded built-in skills. |
| `github.com/7ar-chat/harness/features/skills/skilltools` | `root` | The `load_skill` tool. |
| `github.com/7ar-chat/harness/features/titles` | `root` | Short model-written thread titles. |
| `github.com/7ar-chat/harness/features/verify` | `root` | Runs the checkers of deliverables written under `/workspace/outputs/`. |

### Hosts

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/hosts/client` | `hosts/client` | The `Client` contract between a front end and the engine, its DTOs and `client.HTTP`. |
| `github.com/7ar-chat/harness/hosts/clienttest` | `root` | Conformance suite every `Client` implementation must pass. |
| `github.com/7ar-chat/harness/hosts/httpapi` | `root` | HTTP server: JSON API, SSE streams, embedded browser UI. |
| `github.com/7ar-chat/harness/hosts/inproc` | `root` | `Client` over a `Harness` in the same process. |
| `github.com/7ar-chat/harness/hosts/tui` | `hosts/tui` | Bubble Tea terminal UI, render functions and print mode, over any `Client`. |
| `github.com/7ar-chat/harness/hosts/worker` | `root` | Headless host that executes queued runs; admin endpoints. |

### Product and commands

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/app/cmd/bench` | `app` | Command: the coding benchmark. |
| `github.com/7ar-chat/harness/app/cmd/gen-ts` | `app` | Command: generates the TypeScript types of the wire format. |
| `github.com/7ar-chat/harness/app/cmd/harness` | `app` | Command: the terminal client ([reference](/docs/reference/cli-harness/)). |
| `github.com/7ar-chat/harness/app/cmd/harness-web` | `app` | Command: the web server ([reference](/docs/reference/cli-harness-web/)). |
| `github.com/7ar-chat/harness/app/cmd/harness-worker` | `app` | Command: the headless worker ([reference](/docs/reference/cli-harness-worker/)). |
| `github.com/7ar-chat/harness/app/product` | `app` | Team product: OIDC, projects, shared threads, audit, feed, supervisor, connectors, memory pages. |

### Examples and integration tests

| Package | Module | Role |
|---|---|---|
| `github.com/7ar-chat/harness/app/integration/tui` | `app` | Integration tests of print mode. |
| `github.com/7ar-chat/harness/examples/sandbox-adapter` | `root` | A worked sandbox adapter for a generic REST sandbox service. |
| `github.com/7ar-chat/harness/examples/website` | `root` | Command: a website embedding the harness as a library. |
| `github.com/7ar-chat/harness/integration/client` | `root` | Integration tests: the conformance suite over HTTP. |
| `github.com/7ar-chat/harness/integration/sandboxmanager` | `root` | Integration tests of the sandbox manager. |

## Opt-in packages

Three packages that carry heavy SDKs are not linked unless you import them:

| You use | Import |
|---|---|
| Redis shared between processes | `github.com/7ar-chat/harness/redisinfra` |
| MCP servers | `_ "github.com/7ar-chat/harness/mcpfeature"` |
| `SANDBOX_BACKEND=opensandbox` | `_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"` |

See [The Go facade](/docs/reference/harness-api/) for the API of the root package.
