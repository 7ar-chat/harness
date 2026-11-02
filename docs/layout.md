# Layout

The repository is a root Go module plus seven standalone modules (listed in `go.work`), in strict layers; a lower
layer never imports a higher one (`layers_test.go` enforces it, and that each standalone module requires only the
modules it declares; the reasoning is in [docs/architecture.md](architecture.md)).

| Module | Holds | Needs |
|---|---|---|
| [`ai`](../ai) | messages, tools, model contract, the OpenAI / Anthropic / Responses clients, a tool-calling loop (`ai/agent`), context compaction. A library on its own: see its [README](../ai/README.md) | the standard library, a JSON-schema reflector |
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
  agui/       AG-UI events           store/     domain types + persistence ports (Store, Full, TeamStore…)   factory/   factory tasks, triggers and ports
  sandbox/    sandbox contract, manager, background-job port, sandboxtest (fake)   blobs/   content-store port
config/ modelinfo/ engine/syncx prompts    settings, model table (module modelinfo), small helpers, prompt text
engine/                        the reusable intelligence
  agent/      the policy of a persisted run over ai/agent.Loop: steering, hooks, spill, masking, guards, approvals, persistence
  tools/      tool interfaces and the registry
  runtime/    run claiming, heartbeat, sweeper, end-of-run hooks
  eventlog/ queue/ approvals/   (interfaces and in-memory implementations only)
coding/                        the coding toolset: sandboxtools (read/write/edit/search/exec/jobs), gitsafe, codecheck
adapters/                      implementations of the ports
  store/{memstore,filestore,pgstore,teamstore,factorystore}   memory, durable local files, Postgres
  redisx/                      Redis: queue + bus, event log, coordinator, job list, skill installs
  sandbox/{local,opensandbox,backends}   your machine (trusted), OpenSandbox (remote), the SANDBOX_BACKEND registry
  blobs/s3blobs/               S3-compatible blob store
features/                      optional capabilities a host switches on (never import each other)
  permissions/ projectdocs/ explore/ plan/ mcp/ memory/ skills/ goals/ titles/ verify/ artifacts/ (+ artifacts/workspace)
  factory/{tasks,triggers,delegate,forge,review,budget,policy,lessons}   the agent factory (one unit; see docs/factory.md)
harness.go feature*.go profiles.go infra.go   the facade: New / Send / Events / Steer / Cancel / Resume, Feature + Builder, profiles, Infra
hosts/                         ways to drive the engine, and the contract front ends are written against
  client/ (the Client contract + HTTP implementation)   inproc/ (Client over a harness in this process)   clienttest/ (conformance suite)
  tui/ (terminal)   httpapi/ (HTTP + SSE + embedded UI; a Policy/Extension seam)   worker/ (headless queue consumer)
app/product/                   the team product on top of hosts/httpapi: OIDC, projects, audit, feed, assistant, connectors, memory review
app/cmd/harness  app/cmd/harness-web  app/cmd/harness-worker  app/cmd/bench   binaries (module app, with app/product);  app/cmd/gen-ts = the TypeScript type generator;  bench/ longtest/ = benchmark and long-session tests
web/                           the React app; `make web` builds it into hosts/httpapi/ui (not committed); its API types
                               (web/src/api/generated.ts) are generated from the Go DTOs: `make gen`
```

