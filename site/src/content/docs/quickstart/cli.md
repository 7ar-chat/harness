---
title: "Run the CLI and TUI"
description: "Use harness as a terminal coding agent: store a key, start the interactive UI, script it with print mode, resume sessions and attach to a remote server."
---

`harness` (source: `app/cmd/harness`) is a terminal client over one conversation of an in-process engine, or of a running [`harness-web`](/docs/quickstart/web/). Use it as an interactive coding agent in a repository, or in scripts and pipelines through print mode. Build it with `make build` ([Install](/docs/quickstart/install/)) or `go install github.com/7ar-chat/harness/app/cmd/harness@latest`.

## First run

Store an API key for your provider, then start the UI in a project directory:

```bash
harness auth login anthropic                 # prompts without echo
# or: echo "$KEY" | harness auth login anthropic -key-stdin
harness auth list                            # providers with a key, masked, and the file path
harness auth remove anthropic
```

The key goes to `~/.har/auth.json` (mode 0600, in a 0700 directory), never into a repository; `HAR_HOME` relocates `~/.har`. Which provider and models are used comes from `~/.har/config.yml`, from the environment, or from `-provider` and `-model`. `harness config init` writes a commented starter `config.yml`, and `harness config path` prints the user and project directories in use.

A typical terminal setup for a coding agent on this machine, with everything in the environment:

```bash
AGENT_PROFILE=coding SANDBOX_BACKEND=local LLM_PROVIDER=anthropic \
LLM_MODELS=claude-sonnet-5-5 LLM_DEFAULT_MODEL=claude-sonnet-5-5 harness
```

Or the same as `~/.har/config.yml`:

```yaml
provider: anthropic
models: [claude-sonnet-5-5]
default_model: claude-sonnet-5-5
profile: coding
sandbox: {backend: local}
```

Credential lookup order, the `providers:` map and every key are in [Configuration files](/docs/reference/configuration/) and [config.yml keys](/docs/reference/config-yml/). If no key is found, the command stops with a message like the one below (the provider name is the selected one). A local endpoint (`localhost`, `127.0.0.1`) needs no key.

```text
harness: no API key for provider openai: run `harness auth login openai`, or export LLM_API_KEY, or export OPENAI_API_KEY
```

<div class="callout warning">

**The local sandbox is not isolation.** `SANDBOX_BACKEND=local` runs the agent's commands on your machine with your rights, confined to the workspace only for file paths. The coding profile adds a permission layer and git snapshots, but use the local backend on your own machine or inside a container. See [Sandboxes](/docs/concepts/sandboxes/) and [Approvals and permissions](/docs/concepts/approvals-permissions/).

</div>

Without a sandbox the CLI says so on stderr and the agent has no tools:

```text
(no sandbox: set SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)
```

`SANDBOX_BACKEND=local` works on Unix. With `LOCAL_WORKSPACE=dir` the tools act on that directory; the default is the current directory, which the agent sees as `/workspace`.

## Interactive and print mode

| Invocation | Mode |
|---|---|
| `harness` | Interactive terminal UI (stdin is a terminal) |
| `harness -p "summarise main.go"` | Print mode: the answer on stdout, tool activity on stderr, then exit |
| `git diff \| harness -p "review this"` | Print mode: the prompt is `-p` plus a blank line plus stdin |
| `echo "what is 2+2" \| harness` | Print mode: the prompt is read from stdin |
| `echo "what is 2+2" \| harness -json` | Print mode, with the AG-UI events as JSON lines on stdout |

Without a terminal and without a prompt the command fails with `harness: no prompt: pass -p, pipe one on stdin, or run in a terminal`.

Print mode exit codes: **0** success, **1** the run failed or was cancelled, **2** the run ended waiting for an answer or an approval (the question goes to stderr). In print mode nothing asks for approval of risky tool calls; the interactive UI does, unless you pass `-auto`.

Against a stub OpenAI-compatible server, `harness -p "say hello"` prints the answer on stdout and nothing else on stdout:

```text
Hello from the fake model.
```

and `-json` prints one event per line, in this order: `RUN_STARTED`, `STEP_STARTED`, `TEXT_MESSAGE_START`, one `TEXT_MESSAGE_CONTENT` per chunk, `TEXT_MESSAGE_END`, a `CUSTOM` event named `work.usage`, `STEP_FINISHED`, `RUN_FINISHED`. See [Event types](/docs/reference/events/).

## Flags

Verified with `harness -h`:

| Flag | Meaning |
|---|---|
| `-model ID` | Model id (default `LLM_DEFAULT_MODEL`) |
| `-provider NAME` | A name under `providers:` in a `config.yml`, or `openai` / `anthropic` (default `LLM_PROVIDER`) |
| `-user LOGIN` | User login (default `local`) |
| `-p PROMPT` | Print mode: send this prompt (and stdin, if piped), print the answer, exit |
| `-json` | Print mode: emit the AG-UI events as JSON lines on stdout |
| `-auto` | Interactive UI: do not ask for approval of risky tool calls |
| `-continue` | Continue the most recent conversation |
| `-resume ID` | Continue the conversation with this id (see `-sessions`) |
| `-sessions` | List the saved conversations and exit |
| `-undo` | Restore the workspace files to the last snapshot taken before a run of the conversation, then exit |
| `-ephemeral` | Keep nothing: do not save the conversation |
| `-remote URL` | Attach to the `harness-web` server at this URL instead of running an engine in process |
| `-config-dump` | Print the effective configuration with the source of each value (secrets masked), then exit |

The other subcommands are `harness auth login|list|remove`, `harness config path|init` and `harness trust [path] | trust list | untrust [path]` (let a project add allow rules and MCP servers). The full reference is [harness (CLI and TUI)](/docs/reference/cli-harness/).

## Sessions

Each conversation is saved under `LOCAL_STATE_DIR` (default `~/.har/sessions`, i.e. `$HAR_HOME/sessions`).

```bash
harness -sessions       # id, time, message count, title, preview
harness -continue       # latest conversation
harness -resume <id>    # a given one
harness -ephemeral      # save nothing
```

`-continue` and `-resume` fail with `harness: -continue and -resume need the session store (drop -ephemeral)` when combined with `-ephemeral`.

**Undo.** In a git repository the coding profile snapshots the tree before every run. `harness -undo` restores the snapshot of the most recent conversation (or of `-resume <id>`), and keeps the state it replaced under `refs/harness/backups/`, so the undo can itself be undone. Inside the UI, `/undo` and `/redo` do this for the last prompt and its answers.

## Attach to a server

```bash
export HARNESS_REMOTE_TOKEN=secret
harness -remote http://127.0.0.1:8080                  # the same UI, attached to harness-web
harness -remote https://host -p "summarise the repo"   # print mode against it
```

With `-remote` the process runs no engine and needs no model or sandbox configuration: the server owns them, and `-model` defaults to the server's default model. `HARNESS_REMOTE_TOKEN` is sent as the bearer token (set `HARNESS_WEB_TOKEN` on the server, see [Run the web product](/docs/quickstart/web/)). `-continue`, `-resume` and `-sessions` use the server's saved conversations. `-undo` is refused with `harness: -undo restores a local workspace: it does not work with -remote`.

## In the UI

Type `/` for the command list (tab completes, enter runs; `ctrl+p` opens a palette). The commands registered in the terminal UI are:

| Area | Commands |
|---|---|
| Conversations | `/sessions` (aliases `/resume`, `/continue`), `/new` (`/clear`), `/fork`, `/compact`, `/undo`, `/redo`, `/export`, `/timeline`, `/children` |
| Model | `/model [id]`, `/effort`, `/connect` (store a provider key in `auth.json`), `/cost`, `/context` |
| Modes | `/plan` and `/build` (plan mode is read-only; `tab` toggles) |
| Display | `/sidebar`, `/themes`, `/thinking`, `/details`, `/timestamps`, `/tools`, `/diff`, `/mouse`, `/tips` |
| Input | `/editor`, `/stash`, `/image <path>`, `/copy`, `/notify`, `/palette` |
| Project | `/status`, `/skills`, `/mcp`, `/reload`; your own markdown commands and installed skills also appear as `/<name>` |
| Help | `/help`, `/quit` |

In the input line, `@` picks a workspace file whose content is sent with the message, a message starting with `!` runs a shell command on your machine (never as a model tool call), `enter` sends or, while a run is active, steers it, and `esc` or `ctrl+c` cancels the run. A tool approval is answered with `y` (once), `a` (for this run) or `n`. The key list, leader keys and `~/.har/tui.json` settings are in [harness (CLI and TUI)](/docs/reference/cli-harness/). To embed the same UI in your own program see [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/).

<div class="callout tip">

**See where a setting came from.** `harness -config-dump` lists every effective value with its source (`flag`, `env`, `project file`, `user file`) and masks keys. Use it first when the wrong model or sandbox is selected.

</div>
