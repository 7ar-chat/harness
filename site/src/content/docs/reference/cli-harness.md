---
title: "harness (CLI and TUI)"
description: "Every flag, subcommand, exit code, key binding, slash command and tui.json setting of the harness terminal client."
---

`harness` is the terminal client: an interactive UI (Bubble Tea) over one conversation, or a print mode for scripts. By default it runs the engine in its own process; with `-remote` it attaches to a running [harness-web](/docs/reference/cli-harness-web/). The tutorial is [Run the CLI and TUI](/docs/quickstart/cli/); this page is the reference.

```bash
harness -model gpt-5                    # interactive UI (stdin is a terminal)
harness -p "summarise main.go"          # print mode: answer on stdout, tool activity on stderr
git diff | harness -p "review this"     # print mode: the prompt is -p plus stdin
echo "what is 2+2" | harness -json      # print mode: AG-UI events as JSON lines
harness -remote http://127.0.0.1:8080   # the same UI attached to a harness-web
```

The mode is chosen from the prompt: `-p` and/or piped stdin give a prompt and select print mode; otherwise the UI starts, which needs a terminal. With no prompt and no terminal the command exits 1 with `no prompt: pass -p, pipe one on stdin, or run in a terminal`.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-model` | `LLM_DEFAULT_MODEL` | Model id. Exits 1 with `no model: pass -model or set LLM_DEFAULT_MODEL` when neither is set. With `-remote`, the server's default model. |
| `-provider` | `LLM_PROVIDER` | A provider name defined under `providers:` in a `config.yml`, or `openai` / `anthropic`. |
| `-user` | `local` | Login the conversation belongs to. |
| `-p` | | Print mode: send this prompt (plus stdin, if piped), print the answer, exit. When both are present the prompt is `-p`, a blank line, then stdin. |
| `-json` | off | Print mode: emit the [AG-UI events](/docs/reference/events/) as JSON lines on stdout, nothing else. |
| `-auto` | off | Interactive UI: do not ask approval for risky tool calls. Print mode never asks. |
| `-continue` | off | Continue the most recent saved conversation. |
| `-resume ID` | | Continue the conversation with this id (see `-sessions`). |
| `-sessions` | off | List the saved conversations (up to 50, newest first) and exit. |
| `-undo` | off | Restore the workspace (a git repository) to the snapshot taken before the last run of the conversation (`-resume ID`, default the most recent), print the refs involved, and exit. The state it replaces is kept under `refs/harness/backups/`, so the undo can be undone. |
| `-ephemeral` | off | Keep nothing: do not save the conversation. Default: saved under `LOCAL_STATE_DIR` (else `~/.har/sessions`). `-continue`, `-resume`, `-sessions` and `-undo` need the store and fail with `-ephemeral`. |
| `-remote URL` | | Attach to the harness-web at URL instead of running an engine. The bearer token is `HARNESS_REMOTE_TOKEN`. No model or sandbox configuration is needed; `-undo` is refused (`-undo restores a local workspace: it does not work with -remote`). |
| `-config-dump` | off | Print the effective configuration with the source of each value (flag, env, project file, user file; secrets masked), then the entries of the project's `permissions.yml` that were ignored, and exit. |

With a local engine, `harness` needs an LLM key (`harness auth login <provider>`, `LLM_API_KEY` or the provider's own variable) and a sandbox to give the agent tools. Without one it prints `(no sandbox: set SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)` on stderr. See [Configuration files](/docs/reference/configuration/) and [Environment variables](/docs/reference/env-vars/).

## Print mode exit codes

| Code | Meaning |
|---|---|
| `0` | The run completed. |
| `1` | The run failed or was cancelled, could not start, or the event stream ended without a result. Setup errors (bad flags values, missing key, missing model) also exit 1. |
| `2` | The run ended waiting for an answer or an approval (the question goes to stderr: `harness: waiting for input (<reason>): <message>`). Print mode cannot answer it; continue with `-continue` in the UI. |

Flag parse errors exit 2. In print mode the answer is written to stdout; tool calls (`> name args`), their results, steers and sub-agent steps go to stderr. With `-json` stdout holds only the event lines.

## Subcommands

Subcommands are recognised when they are the first argument, before any flag.

### `harness auth`

Manages `auth.json` in the user directory (`~/.har`, or `HAR_HOME`), mode 0600. Keys never go in `config.yml`.

| Command | Effect |
|---|---|
| `harness auth login <provider> [-key-stdin]` | Store the API key of a provider. Prompts without echo on a terminal; reads one line from stdin with `-key-stdin` or when stdin is not a terminal. The provider name allows letters, digits, `.`, `_` and `-`. |
| `harness auth list` | Print the path of `auth.json` and the providers that have a key, masked (`****7890`). |
| `harness auth remove <provider>` | Delete the key of a provider; exits 1 when there was none. |

```bash
echo "$OPENAI_KEY" | harness auth login openai
harness auth list
```

Running `harness auth` with no argument prints the usage and exits 2.

### `harness config`

| Command | Effect |
|---|---|
| `harness config path` | Print the user directory and the project directory in use (`(none)` when there is no `.har` above the working directory). |
| `harness config init` | Write a commented starter `config.yml` in the user directory (mode 0600). An existing file is left as it is. |

### `harness trust` and `harness untrust`

A project file may only add permission allow rules and MCP servers when the project is trusted. Trust is recorded in `trust.json` in the user directory. See [Approvals and permissions](/docs/concepts/approvals-permissions/) and [Connect MCP servers](/docs/guides/connect-mcp/).

| Command | Effect |
|---|---|
| `harness trust [path]` | Trust a project (default: the project of the working directory, or of `LOCAL_WORKSPACE`). |
| `harness trust list` | Print `trust.json` and the trusted projects. |
| `harness trust mcp [name]` | Approve the exact command, arguments and environment of the project's stdio MCP servers (all pending ones without a name). The project must be trusted first, and stdio needs the local sandbox backend and `mcp.allow_stdio`. |
| `harness untrust [path]` | Stop trusting a project; exits 1 when it was not trusted. |
| `harness untrust mcp <name>` | Withdraw an MCP server approval. |

Changing a server's command or environment invalidates its approval: approve it again with `trust mcp`.

## Terminal UI

### Keys

| Key | Action |
|---|---|
| `enter` | Send the message; while a run is active, steer it. |
| `alt+enter`, `ctrl+j` | New line in the input. |
| `esc`, `ctrl+c` | Cancel the active run. `ctrl+c` again (or twice when idle) quits. |
| `ctrl+o` | Expand / collapse tool calls (arguments, longer results, diffs). |
| `pgup` / `pgdown`, `ctrl+u` / `ctrl+d`, mouse wheel | Scroll the transcript. `ctrl+g` / `home` go to the top, `end` to the bottom. |
| `alt+ctrl+y` / `alt+ctrl+e` | Scroll one line up / down. |
| `tab` / `shift+tab` | Toggle plan and build mode (after the slash completion took `tab`). |
| `ctrl+t` | Cycle the reasoning effort. |
| `f2` / `shift+f2` | Cycle recent models. |
| `ctrl+p` | Command palette over all slash commands. |
| `ctrl+b` | Show / hide the side panel. |
| `ctrl+z` | Suspend (not on Windows). |
| `alt+up` / `alt+down`, `alt+y` | Select a message (when the input is empty); copy the selection. |
| `y` / `a` / `n` (or `esc`) | Tool approval: approve once / approve for the rest of the run / deny. |
| `1`-`9`, or text and `enter` | Answer an `ask_user` question. |
| `ctrl+x` | The leader key: press it, then a letter from the table below. |

In the input: `@` fuzzy-picks a workspace file whose content is sent with the message (never outside the workspace, local sessions only). A message starting with `!` runs a shell command on your machine and shows the output; it never is a model tool call. `up` / `down` walk the input history. Pasting an image path attaches it. A message that only looks like a command (`/etc/hosts is wrong`) is sent as text; start with a space to send an unknown `/word` literally.

### Slash commands

Type `/` for the completion list; `tab` completes, `enter` runs. The leader column is the key after `ctrl+x`.

| Command | Leader | Action |
|---|---|---|
| `/model [id]` (`/models`) | `m` | Picker of the configured models plus the ones the provider reports; a typed id the list lacks is accepted. Applies to the next run. |
| `/sessions` (`/resume`, `/continue`) | `l` | Switch to a saved conversation. In the picker: `ctrl+r` rename, `ctrl+d` delete, `ctrl+o` pin. |
| `/new` (`/clear`) | `n` | Start an empty conversation. |
| `/undo` | `u` | Remove the last prompt and its answers and restore the workspace files (git workspaces). The prompt returns to the input. |
| `/redo` | `r` | Reapply what `/undo` removed. |
| `/compact` (`/summarize`) | `c` | Summarise the history now. |
| `/fork` | `f` | Continue in a copy of this conversation. |
| `/plan`, `/build` | | Read-only plan mode, enforced by the engine, or the full toolset. `tab` toggles. |
| `/effort [low\|medium\|high\|default]` | | Reasoning effort of the next runs. |
| `/connect [provider]` | | Pick a provider and enter its API key (stored in `auth.json`); applies after a restart. |
| `/init`, `/<name>` | | Custom commands (markdown templates in `~/.har/commands` and `<project>/.har/commands`) and skills; `/init` writes `AGENTS.md`. See [Titles, project docs, commands](/docs/concepts/titles-docs-commands/). |
| `/reload` | | Reload the custom commands. |
| `/themes [name]` (`/theme`) | | Pick a colour theme with live preview: `default`, `dark`, `light`, `high-contrast`, `solarized`, `nord`, `gruvbox`. Kept in `tui-state.json`. |
| `/thinking`, `/details`, `/timestamps` | | Show reasoning text, expand tool details, show message times. |
| `/sidebar` | `b` | Right panel: title, model and mode, token usage, plan, modified files, MCP servers. |
| `/editor`, `/export` | `e`, `x` | Compose the message in `$EDITOR`; export the conversation as Markdown and open it. |
| `/stash [pop\|list\|text]` | `s` | Park the input for later. |
| `/image [path]`, `/copy`, `/notify` | | Attach an image (no path: the clipboard); copy a message; desktop notifications when the terminal is in the background. |
| `/timeline` | `g` | Jump to one of your messages. |
| `/children` (`/subagents`) | `down` | Browse the sub-agent (explore) runs of the conversation. |
| `/diff`, `/mouse` | | Toggle split / stacked diffs; toggle mouse support. |
| `/tips` | `h` | Show / hide the tip on the welcome screen. |
| `/status` | `i` | Backend, model, workspace and the configuration in effect. |
| `/skills`, `/mcp` | | List the installed skills and the MCP servers. |
| `/cost`, `/context` | | Tokens and price of each run of the session; how full the context window is. |
| `/palette` | `p` | Search all commands. |
| `/help` (`/?`), `/tools`, `/quit` (`/exit`, `/q`) | `q` for quit | List commands and keys; expand tool calls; leave. |

The history commands (`/undo`, `/redo`, `/compact`, `/fork`, `/new`, `/sessions`) are refused while a run is active.

## tui.json

`~/.har/tui.json` (JSON with `//` and `/* */` comments and trailing commas allowed) configures the UI. It is read from the user directory only, never from a project, so a repository cannot rebind your keys. An invalid value falls back to its default and a notice is shown; an unknown field is ignored with a notice. With `HARNESS_NO_USER_DIR` set the defaults apply and no state is kept.

| Key | Default | Values |
|---|---|---|
| `theme` | `default` | One of the theme names above. |
| `leader` | `ctrl+x` | A key. |
| `leader_timeout` | `2000` | Milliseconds the leader waits for the second key. |
| `scroll_speed` | `3` | Lines per wheel notch, 0.001 to 100. |
| `scroll_acceleration` | off | `{"enabled": true}`: the speed grows with the rate of wheel events. |
| `diff_style` | `auto` | `auto` (side by side from 120 columns) or `stacked`. |
| `cursor` | `block`, blinking | `{"style": "block"\|"underline", "blinking": true\|false}`. |
| `mouse` | `true` | Wheel scrolling. Off lets the terminal select text. |
| `attention` | | `{"enabled", "notifications", "sound", "volume" (0-1, the number of bells), "sound_command" (an argv array run without a shell, with `HARNESS_EVENT` and `HARNESS_VOLUME` in its environment), "sounds" ({"done","question","permission","error": true\|false})}`. |
| `keybinds` | | Action to key (or a list of keys, or a comma-separated string), merged over the defaults. `"none"` unbinds. |

Actions under `keybinds`: `leader`, `palette`, `send`, `cancel`, `quit`, `tools`, `page_up`, `page_down`, `half_page_up`, `half_page_down`, `first`, `last`, `messages_line_up`, `messages_line_down`, `messages_next`, `messages_previous`, `messages_last_user` (the last three are unbound by default), and `command.<name>` for any slash command. A command key is either `<leader>x` (after the leader) or a plain key (runs directly).

```json
{
  // JSON with comments is accepted
  "theme": "nord",
  "leader_timeout": 1500,
  "diff_style": "stacked",
  "cursor": { "style": "underline", "blinking": false },
  "attention": { "enabled": true, "volume": 0.5, "sounds": { "error": false } },
  "keybinds": {
    "palette": "ctrl+k",
    "command.sessions": "<leader>l",
    "command.stash": "none"
  }
}
```

This file parses without notices with `tui.ParseTUIConfig`.

## Model info and prices

Context windows and prices come from LiteLLM's public table, fetched in the background with a plain GET (nothing about you is sent), cached for 24 hours in `~/.har/cache/` and never blocking startup. The status line shows `ctx 12.3k/200k (6%)` and the session cost (`~` when some usage had no price); the model picker shows window, price and vision/reasoning badges; `/cost` and `/context` give details. Cache-write tokens are not priced.

| Setting | Effect |
|---|---|
| `HARNESS_MODEL_PRICES` | A file path or an `http(s)` URL replacing the source; `off` disables the table. |
| `~/.har/model-prices.json` | Same schema; its models override or add to the table (a private endpoint, a self-hosted model). |

With `-remote`, the server's table is used.

<div class="callout note">

**The UI state is separate from the config.** Theme, favourite and recent models and the input history live in `tui-state.json` in the user directory; `tui.json` is only what you edit by hand.

</div>

See [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/) to embed the same UI in your program.
