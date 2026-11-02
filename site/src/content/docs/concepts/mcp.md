---
title: "MCP"
description: "How the harness mounts the tools of Model Context Protocol servers into a run, the opt-in mcpfeature import, and the trust model that keeps a cloned repository from starting programs."
---

The Model Context Protocol (MCP) lets a server publish tools that any agent can call. The `mcp` feature connects to the servers in your configuration for each run and presents their tools to the model next to the built-in ones, which is how you give the agent web search, an issue tracker or an internal API without writing Go. This page explains the model; for the file formats see [MCP servers](/docs/integrations/mcp/), and for a walk-through see [Connect MCP servers](/docs/guides/connect-mcp/).

<div class="callout breaking">

**MCP is opt-in since v0.3.** The `harness` package no longer links the MCP SDK. A program that embeds the harness and configures MCP servers must add a blank import:

```go
import _ "github.com/7ar-chat/harness/mcpfeature"
```

Without it `harness.New` fails and names the package instead of ignoring the servers (the exact message is on [Connect MCP servers](/docs/guides/connect-mcp/#when-mcpfeature-is-not-imported)). The `harness`, `harness-web` and worker binaries already import it.

</div>

## What the agent gets

For every run, the provider connects to each configured server (HTTP, SSE or a local stdio process), lists its tools and wraps them as ordinary [tools](/docs/concepts/tools/):

- **Names.** A tool is exposed as `{server}__{tool}` with characters outside `[a-zA-Z0-9_-]` replaced by `_`, at most 64 characters. If two tools would collide after that, a short hash of the original name is appended.
- **Allow-list.** A server definition may carry `tools: [...]` (the server's own tool names). Only those are exposed.
- **Results.** A result is capped at 1 MiB of text, keeping the head and the last 2000 characters; larger output is handled by the agent's [output spill](/docs/concepts/agent-loop/).
- **Timeouts.** Connecting and listing tools is bounded by 15 seconds per server. One tool call is bounded by `MCP_CALL_TIMEOUT_SECONDS` (default 120; `mcp.call_timeout_seconds` in `config.yml`). A call that takes longer fails with `the MCP tool call timed out after 2m0s; try again later or with a smaller request`.
- **Failures are local.** A server that cannot be reached is logged (`mcp server unavailable, skipping`) and skipped; the run goes on without its tools.
- **Read-only tools.** A tool annotated `readOnlyHint` by its server is treated as read-only: it may run in parallel with others and it stays available in [plan mode](/docs/concepts/plan-and-goals/). All other MCP tools are removed from plan runs.
- **No sandbox needed.** MCP tools are remote calls; they work with `Sandbox: off` runs.

The connection is per run, so credentials in `headers` are used for every user of the process. Pass per-user credentials through a [custom tool provider](/docs/guides/custom-tool/) instead of the shared MCP configuration.

## Approvals

MCP tools pass through the same [approval gate](/docs/concepts/approvals-permissions/) as built-in ones. In an interactive run, an MCP tool whose name has a part starting with a mutating word (`create|send|update|delete|post|publish|write`, configurable with `APPROVAL_RISKY_TOOL_PATTERNS`), or starting with `delete`, `remove`, `move` or `rename`, waits for the user's approval. Tools of **stdio** servers (local programs) additionally ask once per run on first use when the permission layer is active (local sandbox backend or the coding profile); an `allow` rule such as `my-server__*(*)` removes the question and `deny` blocks them.

## The trust model

A repository you clone can contain a `.mcp.json`. If the harness started whatever it found, a hostile repository could run a program or send your data to a server of its choice. The rules:

| Source | Trusted by default | Notes |
|---|---|---|
| `~/.har/config.yml` (`mcp.servers`), `~/.har/mcp.json` | yes | your own files |
| `MCP_SERVERS` | yes | your process environment |
| `<repo>/.mcp.json`, `<repo>/.har/mcp.json`, `<repo>/.har/config.yml` (`mcp.servers`) | **no** | only used after `harness trust` for that project |

An untrusted project's servers are not started and are not even given environment variables: `${VAR}` references in its definitions are left as written. `harness -config-dump` lists them under `mcp servers not started` with the hint `run `harness trust` to enable`. `harness trust` records the project root in `~/.har/trust.json` (mode 0600). `HARNESS_TRUST_PROJECT=1` trusts the current project for one process, for CI. With `HARNESS_NO_USER_DIR=1`, `trust.json` is not read and only the environment variable can trust a project.

### Stdio servers

A stdio server (`command` instead of `url`) is a program started on your machine, so it has more rules:

1. **Opt-in and local only.** Stdio servers run only with `SANDBOX_BACKEND=local` and an explicit `mcp.allow_stdio: true` in `~/.har/config.yml` or `HARNESS_MCP_STDIO=1`. `mcp.allow_stdio` in a project `config.yml` is an error (`mcp.allow_stdio cannot be set in a project file (a repository must not enable running its own programs)`). Otherwise they are skipped with a warning and listed by `-config-dump`; remote servers still work.
2. **Approval of the exact command for project servers.** A stdio server a *trusted project* defines is also pending until you approve its exact command: `harness trust mcp <name>` (all pending ones without a name; `harness untrust mcp <name>` withdraws it). The approval is bound to the project root, the server name and a SHA-256 of the command, arguments and `env`, so changing any of them asks again. It is checked when the configuration loads and again right before the process starts.
3. **In-run approval.** In an *interactive* run (the terminal UI by default) a pending server is also offered in the run: the model receives a tool named `mcp_approve__{server}_{hash}`. Calling it pauses the run with a normal approval that names the project, server, command, arguments and the *names* of the `env` keys (never their values). Approving records the same entry as `harness trust mcp`. The server's tools appear from the **next** run, because the current run's tools were built before the decision. Denying leaves it pending and the tool is not offered again on that thread. In automatic mode or on a host with no approval gate the tool is refused and the server stays unstarted.
4. **Reduced child environment.** The process inherits only `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`, `TERM`, `TMPDIR` and `SHELL`, plus the `env` of its definition. API keys and database passwords of the harness are not passed on.

<div class="callout warning">

**A stdio server runs with your privileges, outside the sandbox.** The local sandbox backend is not isolation either (see [Sandboxes](/docs/concepts/sandboxes/)). Approve only commands you would run yourself, and keep `HARNESS_MCP_STDIO` unset on servers that serve other people.

</div>

## Where it is wired

| Piece | Package |
|---|---|
| Feature constructor `harness.MCP()` (in the `work` and `coding` profiles; `minimal` has none) | `harness` |
| Implementation, registered by the blank import | `github.com/7ar-chat/harness/mcpfeature` |
| Provider, bridge, approval tool | `github.com/7ar-chat/harness/features/mcp` |
| Definitions, merge and trust checks | `config/mcp.go`, `config/trust.go` |

`harness.MCP()` installs nothing when no servers are configured, and fails only when servers (or pending project servers) are configured while `mcpfeature` is not imported. If you compose your own `Options.Features`, include `harness.MCP()`; see [Features and profiles](/docs/concepts/features-profiles/).

In the terminal UI, `/mcp` lists the configured servers and the sidebar shows them.
