---
title: What is the harness
description: An open, embeddable agent harness written in Go - what it does, who it is for, and where to start.
---

The harness is a Go library and a set of programs built on it. It runs a ReAct loop in which a model calls tools, the tools run in a **sandbox** (a local directory or a remote [OpenSandbox](https://github.com/alibaba/OpenSandbox) container), and everything the run does is streamed as [AG-UI](https://docs.ag-ui.com) events. The Go loop drives the model (any OpenAI-compatible endpoint, the OpenAI Responses API, or Anthropic) and the sandbox only executes. Nothing spawns another agent CLI.

It ships the parts an agent product needs around that loop: tools (files, shell, search, document rendering), skills, memory, goals and plans, tool approvals and permissions, context compaction, and verification of the documents the agent produces.

## Who it is for

| You are | You use | Start at |
|---|---|---|
| A Go developer adding agents to a product or website | the root module as a library, optionally `hosts/httpapi` for a ready JSON + SSE API | [Smallest embedding](/docs/quickstart/embed/), [Embed in a website](/docs/guides/embed-website/) |
| A developer who wants a coding agent in the terminal | the `harness` binary (Bubble Tea UI and print mode) | [Run the CLI and TUI](/docs/quickstart/cli/) |
| A team that wants a self-hosted, multi-user agent product | `harness-web` with OIDC login, projects and workers | [Run the web product](/docs/quickstart/web/), [Run team mode](/docs/guides/team-mode/) |
| Someone who only needs a model client or a tool loop | the standalone `ai` module | [Use the ai module on its own](/docs/guides/use-ai/) |
| Someone writing a front end (Slack bot, desktop app) | `hosts/client`, one narrow interface with an in-process and an HTTP implementation | [Use hosts/client and hosts/tui](/docs/guides/use-client-tui/) |

## What you get

- **One loop, many hosts.** The same engine runs behind a terminal UI, an HTTP server, a headless worker and your own code. They all call the same facade: `NewThread`, `Send`, `Events`, `Steer`, `Cancel`, `Resume`.
- **Resumable event streams.** Every event has a sequence number. A consumer that disconnects resumes from the last one it saw, with no loss and no duplicates. See [Threads, runs and events](/docs/concepts/threads-runs-events/).
- **Features, not forks.** Memory, skills, goals, plan mode, MCP, permissions and the rest are optional `Feature`s that you compose into a profile (`work`, `coding`, `minimal`) or replace with your own. See [Features and profiles](/docs/concepts/features-profiles/).
- **Pluggable infrastructure.** One process needs nothing but the sandbox server. Several processes share a Postgres store and a Redis queue and event log, and a crashed process's runs are finished as `interrupted` by another. See [Infrastructure](/docs/concepts/infra/).
- **A safety model you can read.** A permission layer in front of shell and file writes, project files that can only tighten your settings, a trust step for MCP servers, and git snapshots with undo. See [Approvals and permissions](/docs/concepts/approvals-permissions/) and [Security model](/docs/operations/security/).
- **Small dependency footprint.** An application that embeds the harness links none of go-redis, the MCP SDK or the OpenSandbox SDK unless it imports `redisinfra`, `mcpfeature` or `adapters/sandbox/opensandbox`; a test pins this.

## What it is not

<div class="callout note">

The repository contains a terminal client, a `net/http` + SSE server package, a team product and a benchmark runner, but the **engine is a library**. Product-specific HTTP APIs (your authentication, your multi-tenant routing) belong to the embedding application. Web search is not built in: configure any search MCP server (see [Connect MCP servers](/docs/guides/connect-mcp/)).

</div>

The `local` sandbox backend runs commands on the host with only path confinement of the file API. Use it on your own machine or inside a container; use a remote backend where isolation matters. See [Sandboxes](/docs/concepts/sandboxes/).

## Where to go next

1. [Architecture](/docs/introduction/architecture/) explains the layers and the seams.
2. [Module map](/docs/introduction/modules/) tells you which Go module to import.
3. [Install](/docs/quickstart/install/) and [Smallest embedding](/docs/quickstart/embed/) get a conversation running in a few minutes.

The code is MIT licensed; the standalone modules (`ai`, `core`, `modelinfo`, `hosts/client`, `hosts/tui`, `adapters/sandbox`) carry the same license. The source is at [github.com/7ar-chat/harness](https://github.com/7ar-chat/harness).
