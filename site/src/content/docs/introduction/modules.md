---
title: "Module map"
description: "The repository is a root Go module plus seven modules in subdirectories; this page says what each is for, what it requires and when to import it on its own."
---

The harness is released as eight Go modules that live in one repository. Most programs import only the root module, `github.com/7ar-chat/harness`. The smaller modules exist so that a program that needs only a model client, the event vocabulary or the terminal UI does not pull in the engine and its dependencies.

## The modules

All modules declare `go 1.26.0`. The "Requires" column is read from each `go.mod` at tag `v0.3.1`; third-party requirements are summarised.

| Module | What it is for | Requires | Import it on its own when |
|---|---|---|---|
| `github.com/7ar-chat/harness` (root) | The engine: agent run loop, runtime, event log, queue, features and profiles, the `harness.New` facade, the HTTP API (`hosts/httpapi`), the in-process client (`hosts/inproc`), the worker (`hosts/worker`), the stores (`adapters/store/...`), `config` | `ai`, `core`, `modelinfo`, `hosts/client`, `adapters/sandbox`, plus third-party SDKs (Postgres, S3, Redis, MCP, Sentry) | You embed the harness in an application. This is the default; see [Smallest embedding](/docs/quickstart/embed/) |
| `.../ai` | Messages, tools, model clients (OpenAI-compatible, Anthropic, OpenAI Responses), the tool-calling loop, context compaction, a scripted fake model (`ai/llm/llmtest`) | One JSON-schema library (no other module of this repository) | You want to call models and run a tool loop without threads, stores or sandboxes: [Use the ai module on its own](/docs/guides/use-ai/) |
| `.../core` | The shared vocabulary: AG-UI events (`core/agui`), domain types and persistence ports (`core/store`), the sandbox contract (`core/sandbox`), blob port, folder names (`core/layout`) | `github.com/google/uuid` | You write a sandbox backend, a store or an event consumer: [Use core and modelinfo on their own](/docs/guides/use-core-modelinfo/) |
| `.../modelinfo` | Context windows, capabilities and per-token prices read from LiteLLM's public table | Nothing | You need model limits or cost estimates: [Use core and modelinfo on their own](/docs/guides/use-core-modelinfo/) |
| `.../hosts/client` | The `Client` contract a front end is written against, with the HTTP implementation (`client.HTTP`) | `core`, `modelinfo` | You write a front end or bot against a running harness server: [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/) |
| `.../hosts/tui` | The Bubble Tea terminal UI and print mode, over any `client.Client` | `core`, `hosts/client`, Bubble Tea and its libraries | You want the terminal UI in your own program: [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/) |
| `.../adapters/sandbox` | Sandbox backends for the `core/sandbox` contract: `local` (this machine), `opensandbox` (one container per conversation), and the `backends` registry that `SANDBOX_BACKEND` selects from | `core`, the OpenSandbox Go SDK, `uuid` | You want a ready sandbox backend without the engine, or you register your own: [Write a sandbox backend](/docs/guides/sandbox-backend/), [Sandbox backends](/docs/integrations/sandbox-backends/) |
| `.../app` | The product and the commands: `harness` (CLI and TUI), `harness-web`, `harness-worker`, `bench`, and `app/product` (OIDC login, projects, audit, connectors) | The root, every module above, OIDC and JOSE libraries, `golang.org/x/oauth2` | Never as a library: nothing imports it. You run its binaries ([Install](/docs/quickstart/install/)) |

<div class="callout note">

**The root does not require `hosts/tui`.** The terminal UI is linked only by `app/cmd/harness`. An embedding of the root module therefore carries no terminal libraries, and neither the OIDC and JOSE libraries of `app`.

</div>

The dependency direction is fixed: a module may require only modules below it in this order, and `layers_test.go` in the root fails when it does not.

```text
ai   core   modelinfo          no dependencies on each other
hosts/client   adapters/sandbox     require core (hosts/client also modelinfo)
hosts/tui                          requires core and hosts/client
root                               requires ai, core, modelinfo, hosts/client, adapters/sandbox
app                                requires the root and everything above
```

## Optional imports

Three parts of the root and `adapters/sandbox` carry heavy SDKs. They are separate packages that you import only when you use them, so a program that does not use them does not link them. Without the import the failure is an explicit error, not a silent ignore.

| You use | Import | Without it |
|---|---|---|
| Redis shared by several processes | `github.com/7ar-chat/harness/redisinfra`: `redisinfra.NewClient(cfg)` and `redisinfra.Infra(rdb, store)`, passed as `Options.Infra` | The Redis client is not linked; the harness is single-process |
| MCP servers (`MCP_SERVERS`, `.mcp.json`) | `_ "github.com/7ar-chat/harness/mcpfeature"` | `harness.New` fails: `harness: feature "mcp": harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"` |
| `SANDBOX_BACKEND=opensandbox` | `_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"` | `unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: local, none; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it` |

The first two packages are in the root module; `opensandbox` is a package of the `adapters/sandbox` module. The commands in `app` import the MCP and OpenSandbox packages already, which is why `harness` and `harness-web` need no extra step.

<div class="callout breaking">

**Removed in v0.3.x.** `harness.Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` no longer exist. Build the Redis parts with `redisinfra.NewClient(cfg)` and `redisinfra.Infra(rdb, store)` and pass the result as `Options.Infra`. MCP and the OpenSandbox backend became opt-in blank imports. See [Upgrade notes](/docs/operations/upgrade-notes/) and [Redis](/docs/integrations/redis/).

</div>

Further reading: [Connect MCP servers](/docs/guides/connect-mcp/), [Postgres and Redis for several processes](/docs/guides/postgres-redis/).

## go.work and tags

Inside a checkout, `go.work` joins the root and all seven modules (`.`, `./adapters/sandbox`, `./ai`, `./app`, `./core`, `./hosts/client`, `./hosts/tui`, `./modelinfo`), so `go build ./...` always uses the local source of a sibling module.

Outside the checkout there is no workspace. On `main`, each `go.mod` requires tagged versions of its siblings and has no `replace` lines, so a module resolves the same way for you and for someone importing it. Every module therefore needs its own git tag, with the module's directory as the tag prefix:

| Module | Tag |
|---|---|
| root | `vX.Y.Z` |
| `ai`, `core`, `modelinfo` | `ai/vX.Y.Z`, `core/vX.Y.Z`, `modelinfo/vX.Y.Z` |
| `hosts/client`, `hosts/tui` | `hosts/client/vX.Y.Z`, `hosts/tui/vX.Y.Z` |
| `adapters/sandbox` | `adapters/sandbox/vX.Y.Z` |
| `app` | `app/vX.Y.Z` |

Modules are released bottom-up, one layer at a time, and each is verified with the workspace off (`GOWORK=off`). `make test-modules` does this for every module. The full procedure, including how to retract a bad tag, is in [Releasing and versioning](/docs/guides/releasing/).
