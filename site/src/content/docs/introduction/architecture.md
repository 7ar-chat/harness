---
title: Architecture
description: The layers of the repository, what each may import, the seams that keep them apart, and how one run flows through them.
---

The harness is a root Go module plus seven standalone modules, arranged in layers. A package imports only packages of its own layer or a lower one, and `layers_test.go` fails the build when a rule is broken, so this page describes what a test enforces rather than an intention. Read it before you decide where your own code plugs in.

## The layers

<svg class="diagram" viewBox="0 0 760 376" role="img" aria-label="Layers of the repository, from front ends at the top to core and ai at the bottom; each layer imports only the layers below it">
<text class="s" x="0" y="40">front ends</text><text class="s" x="0" y="116">contract and facade</text><text class="s" x="0" y="192">capabilities</text><text class="s" x="0" y="268">engine</text><text class="s" x="0" y="344">vocabulary</text><rect class="box" x="140" y="10" width="114" height="52" rx="8"/><text class="b m" x="197" y="32">hosts/tui</text><text class="s m" x="197" y="50">terminal UI</text><rect class="box" x="264" y="10" width="114" height="52" rx="8"/><text class="b m" x="321" y="32">hosts/httpapi</text><text class="s m" x="321" y="50">JSON + SSE</text><rect class="box" x="388" y="10" width="114" height="52" rx="8"/><text class="b m" x="445" y="32">app/product</text><text class="s m" x="445" y="50">teams, OIDC</text><rect class="box" x="512" y="10" width="114" height="52" rx="8"/><text class="b m" x="569" y="32">hosts/worker</text><text class="s m" x="569" y="50">queue consumer</text><rect class="box" x="636" y="10" width="114" height="52" rx="8"/><text class="b m" x="693" y="32">app/cmd/*</text><text class="s m" x="693" y="50">binaries</text><rect class="box hot" x="140" y="86" width="290" height="52" rx="8"/><text class="b m" x="285" y="108">hosts/client</text><text class="s m" x="285" y="126">the Client contract</text><rect class="box hot" x="460" y="86" width="290" height="52" rx="8"/><text class="b m" x="605" y="108">harness (facade)</text><text class="s m" x="605" y="126">Options, Send, Events, profiles</text><rect class="box" x="140" y="162" width="190" height="52" rx="8"/><text class="b m" x="235" y="184">features/*</text><text class="s m" x="235" y="202">memory, skills, goals…</text><rect class="box" x="350" y="162" width="190" height="52" rx="8"/><text class="b m" x="445" y="184">coding/*</text><text class="s m" x="445" y="202">sandbox tools, gitsafe</text><rect class="box" x="560" y="162" width="190" height="52" rx="8"/><text class="b m" x="655" y="184">adapters/*</text><text class="s m" x="655" y="202">stores, Redis, sandboxes</text><rect class="box" x="140" y="238" width="610" height="52" rx="8"/><text class="b m" x="445" y="260">engine/*</text><text class="s m" x="445" y="278">run loop policy, tool registry, runtime, queue and event log (in memory)</text><rect class="box soft" x="140" y="314" width="145" height="52" rx="8"/><text class="b m" x="212.5" y="336">core/*</text><text class="s m" x="212.5" y="354">agui, store, sandbox</text><rect class="box soft" x="295" y="314" width="145" height="52" rx="8"/><text class="b m" x="367.5" y="336">ai/*</text><text class="s m" x="367.5" y="354">models, agent loop</text><rect class="box soft" x="450" y="314" width="145" height="52" rx="8"/><text class="b m" x="522.5" y="336">config</text><text class="s m" x="522.5" y="354">settings</text><rect class="box soft" x="605" y="314" width="145" height="52" rx="8"/><text class="b m" x="677.5" y="336">modelinfo</text><text class="s m" x="677.5" y="354">prices, context windows</text><path class="arrow" d="M260 62 V80"/><path class="head" d="M256 79 L260 86 L264 79Z"/><path class="arrow" d="M640 62 V80"/><path class="head" d="M636 79 L640 86 L644 79Z"/><path class="arrow" d="M260 138 V156"/><path class="head" d="M256 155 L260 162 L264 155Z"/><path class="arrow" d="M640 138 V156"/><path class="head" d="M636 155 L640 162 L644 155Z"/><path class="arrow" d="M260 214 V232"/><path class="head" d="M256 231 L260 238 L264 231Z"/><path class="arrow" d="M640 214 V232"/><path class="head" d="M636 231 L640 238 L644 231Z"/><path class="arrow" d="M260 290 V308"/><path class="head" d="M256 307 L260 314 L264 307Z"/><path class="arrow" d="M640 290 V308"/><path class="head" d="M636 307 L640 314 L644 307Z"/>
</svg>

| Layer | Packages | May import | Role |
|---|---|---|---|
| 0 foundation | `ai/*`, `core/{agui,store,sandbox,blobs,layout}`, `config`, `modelinfo`, `engine/syncx`, `prompts` | `core/` only (`core` never imports `config`); `ai/*` imports nothing of this repository outside `ai/` | Messages and tool vocabulary, model clients, events, domain types and the ports |
| 2 engine | `engine/*` | layers 0, 2 | The run loop (persistence, approvals, steering), tool registry, run runtime, in-memory queue and event log |
| 3 plug-ins | `adapters/*`, `coding/*` | layers 0, 2; **not each other's group** | Implementations of the ports (Postgres, Redis, S3, OpenSandbox, local) and the coding toolset |
| 4 features | `features/*` | layers 0, 2, 3 (`coding`) | Optional capabilities (memory, skills, goals…). A feature never imports another feature |
| 5 hosts | `harness` (facade), `hosts/*`, `redisinfra`, `mcpfeature` | everything below | The facade composes the engine and the features; hosts drive it |
| 6 product | `app/product` | `hosts/httpapi`, `hosts/client`, the facade, `core/`, the engine's extension API | Teams, identity, connectors, assistant |
| 9 tooling | `app/cmd/*`, `bench`, `longtest`, `examples` | anything | Composition roots and tests |

Host rules on top of the layers:

- `hosts/tui` imports only `hosts/client` and `core/agui`, so it behaves the same against an in-process engine and a remote server.
- `hosts/httpapi` imports only the facade, `hosts/client` and `core/`. It is a transport over the client contract.
- `hosts/*` do not import each other, and the facade imports no host.
- `engine/*`, `adapters/*` and `features/*` do not import `config`, except `features/mcp` and `features/permissions` (a known gap, listed in `mayImportConfig`). The facade and the hosts translate `*config.Config` into each package's own options.

## The seams

**Ports live in `core`.** `core/store` (persistence), `core/sandbox` (the `Sandbox`/`Backend`/`Manager` contract and the background-job registry) and `core/blobs` (content store). Adapters implement them and import nothing but core and the engine; the engine and the features depend on the port, never on a backend.

**`store.Store` is the small core, `store.Full` adds the rest.** `Store` holds threads, runs, messages, events and the sandbox mapping. `Full` adds the stores of the optional capabilities (artifacts, skills, goals and plans, approvals, memory, notifications). A package asks for the smallest interface it uses; the memory, file and Postgres backends implement `Full`. See [the store](/docs/concepts/store/).

**Features plug in through `harness.Feature` and `Builder`.** A feature contributes tools, prompt sections, hooks and jobs. The built-in features are listed in `features_core.go` and `features_state.go`; profiles (`coding`, `work`, `minimal`) are lists of them. See [features and profiles](/docs/concepts/features-profiles/).

**Sandboxes are selected by `SANDBOX_BACKEND`.** `adapters/sandbox/backends` is the registry. The `local` backend registers itself, `opensandbox` registers when you import it, and your own adapter registers from an `init`. See [sandboxes](/docs/concepts/sandboxes/).

**The HTTP API is a core plus an extension seam.** `hosts/httpapi` serves threads, runs, approvals, files and the SSE stream for a single user. A product plugs in through `httpapi.Extension`: a `Policy` that decides who may see a thread, who is recorded as the author and what is audited, plus `Setup` and `Install` to add routes. `app/product` is the team implementation. See [the HTTP API](/docs/reference/http-api/).

**Front ends are written against `hosts/client.Client`**, which has an in-process implementation (`hosts/inproc`) and an HTTP one, checked by the same conformance suite (`clienttest`). Operations a server may lack are `client.Extras` and return `ErrNotSupported`. See [the Client contract](/docs/reference/client-contract/).

## One run, end to end

<svg class="diagram" viewBox="0 0 760 340" role="img" aria-label="One run: the host calls the facade, the runtime claims the run, the agent loop calls the model and the tools, tools use the sandbox, everything is persisted in the store and event log">
<rect class="box" x="20" y="10" width="200" height="52" rx="8"/><text class="b m" x="120" y="32">your code or a host</text><text class="s m" x="120" y="50">web, TUI, worker</text><rect class="box hot" x="280" y="10" width="200" height="52" rx="8"/><text class="b m" x="380" y="32">harness.Harness</text><text class="s m" x="380" y="50">Send, Events, Steer, Cancel, Resume</text><rect class="box" x="540" y="10" width="200" height="52" rx="8"/><text class="b m" x="640" y="32">runtime</text><text class="s m" x="640" y="50">claim, heartbeat, end-of-run hooks</text><path class="arrow" d="M220 36 H274"/><path class="head" d="M273 32 L280 36 L273 40Z"/><path class="arrow" d="M480 36 H534"/><path class="head" d="M533 32 L540 36 L533 40Z"/><rect class="box" x="280" y="100" width="460" height="52" rx="8"/><text class="b m" x="510" y="122">agent loop (ai/agent.Loop + the run policy)</text><text class="s m" x="510" y="140">steps, steering, approvals, compaction, guards, persistence</text><path class="arrow" d="M640 62 V94"/><path class="head" d="M636 93 L640 100 L644 93Z"/><rect class="box soft" x="20" y="190" width="200" height="52" rx="8"/><text class="b m" x="120" y="212">model</text><text class="s m" x="120" y="230">OpenAI, Responses, Anthropic</text><rect class="box" x="280" y="190" width="200" height="52" rx="8"/><text class="b m" x="380" y="212">tools</text><text class="s m" x="380" y="230">sandbox, skills, memory, MCP, yours</text><rect class="box soft" x="540" y="190" width="200" height="52" rx="8"/><text class="b m" x="640" y="212">store + event log + queue</text><text class="s m" x="640" y="230">memory or Postgres + Redis</text><path class="arrow" d="M330 152 V190"/><path class="head" d="M326 184 L330 190 L334 184Z"/><path class="arrow" d="M280 126 H120 V184"/><path class="head" d="M116 184 L120 190 L124 184Z"/><path class="arrow" d="M640 152 V190"/><path class="head" d="M636 184 L640 190 L644 184Z"/><rect class="box" x="280" y="278" width="200" height="52" rx="8"/><text class="b m" x="380" y="300">sandbox.Manager</text><text class="s m" x="380" y="318">one sandbox per thread</text><rect class="box soft" x="540" y="278" width="200" height="52" rx="8"/><text class="b m" x="640" y="300">sandbox backend</text><text class="s m" x="640" y="318">local, OpenSandbox, yours</text><path class="arrow" d="M380 242 V278"/><path class="head" d="M376 272 L380 278 L384 272Z"/><path class="arrow" d="M480 304 H534"/><path class="head" d="M533 300 L540 304 L533 308Z"/>
</svg>

1. The host calls `Send`, which stores the user message and queues a run.
2. A runtime (in this process through `h.Run`, or in a `harness-worker`) claims the run and keeps a heartbeat. If the runtime dies, another one finishes the run as `interrupted`.
3. The agent loop, `ai/agent.Loop`, asks the model for a step, schedules the tool calls (read-only calls in parallel, the rest alone) and feeds the results back, until the model answers or the budget is spent.
4. Tools run in the thread's sandbox when there is one. Every message, tool result and AG-UI event is persisted, so `Events(ctx, runID, after)` can replay from any sequence number.

There is exactly one agent loop. `engine/agent` is the policy of a persisted run (step events, steering, compaction, guards, verification, approvals and storing each answer); `features/explore` runs the same loop through `ai/agent.Run`, whose policy keeps the conversation in a slice.

## The web UI

`web/` is a React app that `make web` builds into `hosts/httpapi/ui`, which the Go binary embeds. The build output is not committed, so a binary built without it serves a notice at `/`. The TypeScript types of the wire format are generated from the Go DTOs with `make gen`, and a test fails while the committed file is stale.

<div class="callout note">

The memory and profile pages of the team product are served by `app/product` only. They are not part of `client.Client`, so the terminal UI does not have them.

</div>
