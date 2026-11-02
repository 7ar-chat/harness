---
title: "MCP servers"
description: "Reference for declaring MCP servers: file formats, sources and precedence, variable expansion, transports, the tools allow-list, environment variables and what is not read."
---

This page is the reference for configuring MCP servers. For what the feature does and why project servers are gated, read [MCP](/docs/concepts/mcp/); for a step-by-step setup, [Connect MCP servers](/docs/guides/connect-mcp/). Programs that embed the harness need the blank import `_ "github.com/7ar-chat/harness/mcpfeature"`; the `harness`, `harness-web` and worker binaries already have it.

## A minimal definition

<div class="tabs">

<div class="tab" data-label=".mcp.json">

```json
{"mcpServers": {
  "docs": {"type": "http", "url": "https://docs.example.com/mcp", "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
}}
```

</div>

<div class="tab" data-label="config.yml">

```yaml
mcp:
  servers:
    docs:
      type: http
      url: https://docs.example.com/mcp
      headers: {Authorization: "Bearer ${DOCS_TOKEN}"}
```

</div>

<div class="tab" data-label="MCP_SERVERS">

```bash
export MCP_SERVERS='[{"name":"docs","url":"https://docs.example.com/mcp","headers":{"Authorization":"Bearer secret"}}]'
```

</div>

</div>

## Definition fields

The same fields apply in every format. In `.mcp.json` and `config.yml` a server is a key (its name) mapped to a definition; in `MCP_SERVERS` it is an object with an extra `name`.

| Field | Meaning |
|---|---|
| `type` | `http` (streamable HTTP, the default for a `url`), `sse`, or `stdio` (the default when `command` is set). `streamable-http` and `streamable_http` are accepted as `http`. Any other value is an error. |
| `url` | Endpoint of an `http` or `sse` server. Required for those types. |
| `headers` | HTTP headers sent on every request to the server (`Authorization`, API keys). |
| `command`, `args`, `env` | A stdio server: the program, its arguments and extra environment variables. A stdio server needs a `command` and no `url`; a remote one needs a `url` and no `command`. |
| `tools` | Optional allow-list of the server's own tool names. Without it all tools are exposed; with it only the listed ones. This field is a harness addition to the shared format. |

In `.mcp.json` and `config.yml`, an unknown field is an error naming the file and the server (`mcp server "x": json: unknown field "disabled"`), and so is an unsupported type (`type must be http, sse or stdio, got "ws"`).

Tools reach the model as `{server}__{tool}` (invalid characters become `_`, at most 64 characters). Server names should therefore be short and use letters, digits, `_` and `-`.

## Sources and precedence

Servers are merged by name from these sources, lowest priority first. A later source replaces a server of the same name.

| # | Source | Trusted |
|---|---|---|
| 1 | `~/.har/config.yml`, key `mcp.servers` | always |
| 2 | `~/.har/mcp.json` | always |
| 3 | `<repo>/.mcp.json` | only after `harness trust` |
| 4 | `<repo>/.har/mcp.json` | only after `harness trust` |
| 5 | `<repo>/.har/config.yml`, key `mcp.servers` | only after `harness trust` |
| 6 | `MCP_SERVERS` | always |

`~` is the user directory (`HAR_HOME`, default `~/.har`; nothing is read with `HARNESS_NO_USER_DIR=1`). `<repo>` is the project root, the nearest directory with a `.git` entry going up from the working directory (or `LOCAL_WORKSPACE`). An untrusted project's servers are not started: `harness -config-dump` lists them under `mcp servers not started`. The effective result, with the file each server came from, is the `mcp servers:` block of `harness -config-dump`.

<div class="callout note">

**Which servers actually start.** After merging, a stdio server is dropped unless the sandbox backend is `local` and stdio is enabled (`mcp.allow_stdio: true` in the user `config.yml`, or `HARNESS_MCP_STDIO=1`). A stdio server defined by a project additionally waits for `harness trust mcp <name>`. See [the trust model](/docs/concepts/mcp/#the-trust-model).

</div>

## `${VAR}` expansion

In `url`, every `headers` value, every `env` value, `command` and every entry of `args`, `${VAR}` takes the value of the process environment variable and `${VAR:-default}` falls back to `default`. A variable that is unset or empty and has no default is an error that names the file, the server and the field:

```text
harness: /home/me/.har/mcp.json: mcp server "search": header Authorization: environment variable SEARCH_TOKEN is not set
```

For an untrusted project the definitions are read with `${VAR}` left as written, so a repository cannot read your environment.

<div class="callout warning">

**`MCP_SERVERS` is not expanded.** Values in `MCP_SERVERS` are used literally: `${TOKEN}` in its `url` or `headers` stays `${TOKEN}`. Put the real value in the variable, or define the server in a file where expansion applies. Never commit tokens to a project file; reference them with `${VAR}`.

</div>

`MCP_SERVERS` has its own checks: it must be a JSON array, every entry needs a `name` and a `url` (or `command`), names must be unique, and `url` must parse (`MCP_SERVERS: every server needs name and url (or command)`).

## Environment variables and keys

| Variable | `config.yml` key | Meaning |
|---|---|---|
| `MCP_SERVERS` | `mcp.servers` (merged) | JSON array of server definitions (source 6). |
| `HARNESS_MCP_STDIO` | `mcp.allow_stdio` | Allows stdio servers; local sandbox backend only. The environment wins over the user file (`0` turns the file's `true` off). The key is user-file only: in a project file it is an error. |
| `MCP_CALL_TIMEOUT_SECONDS` | `mcp.call_timeout_seconds` | Bound for one tool call. Default 120; a value of 0 or less means 120. |
| `HARNESS_TRUST_PROJECT` | none | `1` trusts the current project for this process (CI). |
| `HARNESS_NO_USER_DIR` | none | `1` reads nothing from the user directory, including `mcp.json` and `trust.json`. |
| `HAR_HOME` | none | Moves the user directory. |
| `APPROVAL_RISKY_TOOL_PATTERNS` | none | Words (`create\|send\|update\|delete\|post\|publish\|write` by default) that make an MCP tool wait for approval in an interactive run. |

Connecting and listing tools is bounded by 15 seconds per server; it is not configurable. See [Environment variables](/docs/reference/env-vars/) and [config.yml keys](/docs/reference/config-yml/) for the complete lists.

## Transports

| `type` | Transport | Notes |
|---|---|---|
| `http` | streamable HTTP | The default for a `url`. The standalone server-to-client stream is not opened. |
| `sse` | HTTP with server-sent events | For older servers. |
| `stdio` | child process on the machine of the harness | Reduced environment: `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`, `TERM`, `TMPDIR`, `SHELL` plus the definition's `env`. Never inherits LLM keys or database passwords. |

## Commands

| Command | Effect |
|---|---|
| `harness trust [path]` | trust a project: its `.mcp.json`, `.har/mcp.json` and `mcp.servers` now apply |
| `harness trust list` | list the trusted project roots |
| `harness untrust [path]` | stop trusting a project |
| `harness trust mcp [name]` | approve the exact command of a project's stdio server (all pending ones without a name) |
| `harness untrust mcp <name>` | withdraw that approval |
| `harness -config-dump` | print the effective servers, their origin and the ones not started |

`trust.json` (mode 0600) lists `{"projects": [...]}` and the approved stdio servers. See [harness (CLI and TUI)](/docs/reference/cli-harness/).

## Compatibility

| Format | Read | Not read |
|---|---|---|
| `.mcp.json` (Claude Code and others) | `mcpServers` with `command`, `args`, `env`; `type: http` or `sse` with `url`, `headers`; `${VAR}` and `${VAR:-default}` | `enabledMcpjsonServers` approvals, OAuth, `type: ws`, any field not listed above |
| Cursor | none | `.cursor/mcp.json` |
| Claude Code `settings.json` | none | MCP settings in it |

A server speaking a transport other than the three above, or needing OAuth, cannot be used directly; put a proxy that handles it in front.

Related: [Approvals and permissions](/docs/concepts/approvals-permissions/), [Security model](/docs/operations/security/).
