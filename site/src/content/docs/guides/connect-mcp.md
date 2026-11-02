---
title: "Connect MCP servers"
description: "Step by step: give an embedded harness or the CLI the tools of an MCP server, trust a project's own servers, check the result with -config-dump and add web search."
---

This guide connects MCP servers to the harness in the two usual situations: a Go program that embeds it, and the `harness` command line. For the concepts see [MCP](/docs/concepts/mcp/); for every field and variable see [MCP servers](/docs/integrations/mcp/).

## Embedded in a Go program

<div class="callout breaking">

**Add the blank import.** Since v0.3 the `harness` package does not link the MCP SDK. Import `github.com/7ar-chat/harness/mcpfeature` for its side effect, or `harness.New` fails as soon as a server is configured.

</div>

Declare the servers in `MCP_SERVERS` (a JSON array) and import the package. This program loads the configuration and calls `harness.New`; neither connects to the server (it is contacted when a run starts), so it runs offline:

```go
package main

import (
	"fmt"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"

	_ "github.com/7ar-chat/harness/mcpfeature" // opt-in: links the MCP SDK and installs the feature
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("MCP_SERVERS", `[{"name":"docs","url":"https://docs.example.com/mcp","headers":{"Authorization":"Bearer secret"},"tools":["search_docs"]}]`)

	cfg, err := config.Load()
	if err != nil {
		fmt.Println("config:", err)
		os.Exit(1)
	}
	for _, s := range cfg.MCPServers {
		fmt.Println("configured:", s.Name, "-", s.Describe(), "-", s.Origin)
	}
	h, err := harness.New(harness.Options{
		Config:  cfg,
		Models:  llmtest.Factory{M: llmtest.New()},
		Backend: sandboxtest.NewBackend(),
	})
	if err != nil {
		fmt.Println("harness.New:", err)
		os.Exit(1)
	}
	_ = h
	fmt.Println("harness.New: ok")
}
```

```text
configured: docs - http: https://docs.example.com/mcp - MCP_SERVERS
harness.New: ok
```

In a real program set `MCP_SERVERS` in the process environment, not with `os.Setenv`, and use a real model factory instead of `llmtest` (see [LLM providers](/docs/integrations/llm-providers/)). The tools appear as `docs__search_docs`. In production set `HARNESS_NO_USER_DIR=1` so that a server account's `~/.har/mcp.json` and project files are never read (see [Production checklist](/docs/operations/production/)).

<div class="callout warning">

**Do not put per-user secrets in `MCP_SERVERS`.** Its headers are shared by every user of the process, and `${VAR}` references are not expanded in `MCP_SERVERS`. For per-user credentials write a [tool provider](/docs/guides/custom-tool/) that fetches them per run.

</div>

### When mcpfeature is not imported

Delete the blank import from the program above and run it again. The harness refuses to ignore the servers:

```text
configured: docs - http: https://docs.example.com/mcp - MCP_SERVERS
harness.New: harness: feature "mcp": harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"
```

The cause is always the missing import (or the missing `harness.MCP()` feature when you pass your own `Options.Features`). A configuration with no servers installs nothing and does not need the import.

## From the command line

The `harness` binary already includes MCP support. Your own servers go in `~/.har/mcp.json` (or `$HAR_HOME/mcp.json`), in the format Claude Code and other tools share:

```json
{"mcpServers": {
  "search": {"type": "http", "url": "https://search.example.com/mcp", "headers": {"Authorization": "Bearer ${SEARCH_TOKEN}"}, "tools": ["web_search"]}
}}
```

`${SEARCH_TOKEN}` is read from the environment. If it is unset the command refuses to start and says which one:

```text
harness: /home/me/.har/mcp.json: mcp server "search": header Authorization: environment variable SEARCH_TOKEN is not set
```

Servers in your own files and in `MCP_SERVERS` are trusted. You can also declare them in `~/.har/config.yml` under `mcp.servers` (same fields); the YAML form of the example is on [MCP servers](/docs/integrations/mcp/#a-minimal-definition).

### Check what will start

`harness -config-dump` prints the effective configuration without contacting any server. The `mcp servers:` block lists what will start and the file each server came from:

```text
mcp file:     /home/me/.har/mcp.json

mcp servers:
  search               http: https://search.example.com/mcp (/home/me/.har/mcp.json)
```

Header and environment values are never printed. Anything that was skipped is listed under `mcp servers not started` with the reason. In the terminal UI, `/mcp` lists the servers of the running session.

### Use a project's own servers

A repository may carry `.mcp.json` (or `.har/mcp.json`, or `mcp.servers` in `.har/config.yml`). They are ignored until you trust the project. Given this `.mcp.json` in the repository:

```json
{"mcpServers": {
  "tracker": {"type": "http", "url": "https://tracker.example.com/mcp"},
  "fs": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]}
}}
```

Before trusting, `harness -config-dump` shows:

```text
trust:        project not trusted (run `harness trust` to let it add allow rules and MCP servers)
...
mcp servers not started:
  fs (/path/to/proj/.mcp.json): stdio: npx -y @modelcontextprotocol/server-filesystem .: the project is not trusted, run `harness trust` to enable
  tracker (/path/to/proj/.mcp.json): http: https://tracker.example.com/mcp: the project is not trusted, run `harness trust` to enable
```

Read the file first, then trust it:

```bash
harness trust            # trust the project of the current directory
harness trust list       # show the trusted project roots
harness untrust          # undo
```

After `harness trust`, the `tracker` HTTP server starts. The stdio server `fs` still does not:

```text
mcp servers not started:
  fs (/path/to/proj/.mcp.json): stdio servers need the local backend and an opt-in
```

For CI, `HARNESS_TRUST_PROJECT=1 harness -p "..."` trusts the project for that process only.

### Allow and approve a stdio server

A stdio server runs a program on your machine, so it needs three things:

1. The local sandbox backend and your opt-in: `mcp.allow_stdio: true` in `~/.har/config.yml`, or `HARNESS_MCP_STDIO=1` in the environment. A project file cannot set it.
2. The project trusted (above), if the server comes from the project.
3. Your approval of its exact command, if it comes from the project:

```bash
export HARNESS_MCP_STDIO=1 SANDBOX_BACKEND=local
harness trust mcp fs
# approved MCP server fs (stdio: npx -y @modelcontextprotocol/server-filesystem .)
harness untrust mcp fs
# withdrew the approval of MCP server fs
```

Without the opt-in, `harness trust mcp fs` fails with `no project stdio MCP server is waiting for approval (it may already be approved, or not allowed: stdio needs the local backend and mcp.allow_stdio)`. With the opt-in but before approving, `-config-dump` says `a project's stdio server needs your approval of this exact command, run `harness trust mcp fs``. Changing the command, an argument or the `env` of the server revokes the approval.

You can also approve in the run itself: in an interactive session the model is offered a tool `mcp_approve__fs_<hash>`, which pauses the run with an approval naming the command. The server's tools then exist from your next message on. See [the trust model](/docs/concepts/mcp/#stdio-servers).

## Add web search

The harness has no built-in web search; use any search server that speaks MCP. Declare it like the `search` entry above (your server's URL and credential), and restrict it to the one tool the agent needs with `tools`:

```json
{"mcpServers": {
  "search": {"type": "http", "url": "https://search.example.com/mcp",
             "headers": {"Authorization": "Bearer ${SEARCH_TOKEN}"}, "tools": ["web_search"]}
}}
```

The tool name in `tools` is the name the server itself publishes; check its documentation. The model sees it as `search__web_search`. If the tool name contains a mutating word such as `post` or `send`, an interactive run asks for approval before each first call; add an `allow` rule in `.har/permissions.yml` to stop the question ([Approvals and permissions](/docs/concepts/approvals-permissions/)). In an embedded program set the same definition in `MCP_SERVERS`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `harness.New` fails and names `mcpfeature` | add the blank import (see above) |
| A server is missing from the list | `-config-dump` shows it under `mcp servers not started` with the reason: untrusted project, stdio not allowed, or approval pending |
| `environment variable X is not set` | export `X`, or give the reference a default: `${X:-fallback}` |
| `unknown field "..."` or `type must be http, sse or stdio` | only the fields on [MCP servers](/docs/integrations/mcp/) are read; `ws` is not supported |
| The tools are missing in one run only | the server was unreachable at that run's start; the log says `mcp server unavailable, skipping` |
| A call fails with `the MCP tool call timed out` | raise `MCP_CALL_TIMEOUT_SECONDS` (default 120) |

More in [Troubleshooting](/docs/operations/troubleshooting/).
