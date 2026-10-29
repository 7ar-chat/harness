# Architecture

The module is one Go module arranged in layers. A package imports only packages of its own layer or a lower one,
and a few groups are further restricted. `layers_test.go` fails the build when a rule is broken, so this page
describes what the test enforces.

```
 front ends     hosts/tui   hosts/httpapi ◄── product      hosts/worker        cmd/*
                    │            │               │               │
                    ▼            ▼               ▼               ▼
                hosts/client (Client contract)   harness (facade: Options, Feature/Builder, profiles)
                                                   │
 capabilities                  features/*   coding/*   adapters/*
                                                   │
 engine                                        engine/*
                                                   │
 vocabulary                                     core/*  config  modelinfo
```

## The layers

| Layer | Packages | May import | Role |
|---|---|---|---|
| 0 foundation | `core/{agui,store,sandbox,blobs}`, `config`, `modelinfo`, `engine/syncx`, `engine/agent/prompts` | `core/` only (`core` never imports `config`) | Events, domain types and the ports: what the rest of the system agrees on |
| 2 engine | `engine/*` | layers 0, 2 | The agent loop, LLM clients, tool registry, run runtime, approvals, compaction, in-memory queue and event log |
| 3 plug-ins | `adapters/*`, `coding/*` | layers 0, 2; **not each other's group** | Implementations of the ports (Postgres, Redis, S3, OpenSandbox, local) and the coding toolset |
| 4 features | `features/*` | layers 0, 2, 3 (`coding`) | Optional capabilities (memory, skills, goals, permissions…). A feature never imports another feature |
| 5 hosts | `harness` (facade), `hosts/*` | everything below | The facade composes the engine and features; hosts drive it |
| 6 product | `product` | `hosts/httpapi`, `hosts/client`, the facade, `core/`, the engine's extension API | Teams, identity, connectors, assistant |
| 9 tooling | `cmd/*`, `bench`, `longtest`, `examples` | anything | Composition roots and tests |

Host rules on top of the layers:

* `hosts/tui` imports only `hosts/client` and `core/agui`, so it behaves the same against an in-process engine and a
  remote server.
* `hosts/httpapi` imports only the facade, `hosts/client` and `core/`. It is a transport over the client contract.
* `hosts/*` do not import each other; the facade imports no host.

## The seams

**Ports live in `core`.** `core/store` (persistence), `core/sandbox` (the `Sandbox`/`Backend`/`Manager` contract,
background-job registry), `core/blobs` (content store). Adapters implement them and may import nothing but core and the
engine; the engine and features depend on the port, never on a backend.

**`store.Store` is the small core** (threads, runs, messages, events, sandbox mapping). `store.Full` adds the stores of
the optional capabilities (artifacts, skills, goals and plans, approvals, memory, notifications). A package asks for
the smallest interface it uses; the memory, file and Postgres backends implement `Full`.

**Features plug in through `harness.Feature` / `Builder`**: tools, prompt sections, hooks and jobs. The built-in
features are listed in `features_core.go` and `features_state.go`; profiles (`coding`, `work`, `minimal`) are lists of
them.

**Sandboxes are selected by `SANDBOX_BACKEND`.** `adapters/sandbox/backends` is the registry; `opensandbox` and the
host's `local` backend register themselves, and your own adapter registers from an `init` (see
[sandbox-adapters.md](sandbox-adapters.md)). `local` runs commands on the host with no isolation: `harness-web`
refuses it in team mode or on a non-loopback address unless `HARNESS_ALLOW_LOCAL_SANDBOX=1`.

**The HTTP API is a core plus an extension seam.** `hosts/httpapi` serves threads, runs, approvals, files and the SSE
stream for a single user. A product plugs in through `httpapi.Extension` (a `Policy` that decides who may see a
thread, who is recorded as the author, what is audited, plus `Setup`/`Install` to add routes). `product` is the
team implementation: OIDC login, projects with roles, the audit log and live feed, the assistant (supervisor),
GitHub/Telegram connectors and the memory review pages. `cmd/harness-web` composes `product.New(h, api, prod)`.

**Front ends are written against `hosts/client.Client`**, with an in-process and an HTTP implementation checked by
the same conformance suite (`clienttest`). Operations a server may lack are `client.Extras`
(`ErrNotSupported`).

## The web UI

`web/` is a React app built by `make web` into `hosts/httpapi/ui`, which the Go binary embeds. The build output is
not committed (a `.gitkeep` keeps `go:embed` compiling); a binary built without it serves a notice at `/`.

## Known gaps

* `config` is imported by the engine, features and adapters (it is a leaf, so nothing cycles), but they read the whole
  `*config.Config`. They should take their own small option structs.
* `store/Full` still holds the types of every capability. Moving a capability's port out of core would make adapters
  import features, so the types stay in core; a feature cannot yet bring its own tables without a `core/store` change.
* `engine/runtime` (about 12k lines) and `engine/agent/verify.go` mix several concerns and should be split.
* The web app's TypeScript types are written by hand next to the Go DTOs of `hosts/client`; there is no generated
  schema. The memory and profile pages are served by `product` only, not exposed through `client.Client`, so the TUI
  does not have them.
* Comments and config files still mention "harness v2/v3" and review rounds; the code and file names no longer do
  (the SQL migration `0102_approvals_v3.sql` keeps its name because renaming an applied migration breaks tracking).
