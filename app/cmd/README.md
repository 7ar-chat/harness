# app/cmd

The binaries: `harness` (terminal client), `harness-web`, `harness-worker` and `bench`.

## Coding profile

`AGENT_PROFILE=coding` switches the system prompt to a software-engineering one (orient with search, read
before edit, smallest change, run the project's own build/tests before finishing, no commits or
destructive commands unless asked) and puts the repository's instructions (`AGENTS.md`, else `CLAUDE.md`
or `GEMINI.md`, plus `.har/instructions.md` and your `~/.har/AGENTS.md`; see [configuration](../../docs/configuration.md#configuration-files-and-standards)) into the prompt. An explicit `LLM_PROMPT_VARIANTS` still wins per model. Typical terminal setup:

```
AGENT_PROFILE=coding SANDBOX_BACKEND=local LLM_PROVIDER=anthropic LLM_API_KEY=… \
LLM_MODELS=claude-sonnet-5-5 LLM_DEFAULT_MODEL=claude-sonnet-5-5 harness
```

## CLI sessions

`harness` saves each conversation under `LOCAL_STATE_DIR` (default `~/.har/sessions`, i.e. `$HAR_HOME/sessions`):
`harness -continue` resumes the latest, `-resume <id>` a given one, `-sessions` lists them, `-ephemeral`
keeps nothing. In a git repository the coding profile snapshots the tree before every run;
`harness -undo` restores the snapshot (the replaced state is kept under `refs/harness/backups/`).
`harness.Resume` is the facade call that answers a tool approval or an `ask_user` question.

## Terminal UI

`app/cmd/harness` is a terminal client (Bubble Tea) over one thread of an in-process harness, or of a remote `harness-web`.

```
harness -model gpt-5                  # interactive UI (stdin is a terminal)
harness -p "summarise main.go"        # print mode: answer on stdout, tool activity on stderr
git diff | harness -p "review this"   # print mode, the prompt is -p plus stdin
echo "what is 2+2" | harness -json    # print mode, AG-UI events as JSON lines on stdout
harness -remote http://127.0.0.1:8080 # the same UI, attached to a running harness-web
harness -remote https://host -p "…"   # print mode against it; HARNESS_REMOTE_TOKEN is the bearer token
```

With `-remote` the process runs no engine and needs no model or sandbox configuration: the server owns
them, and `-model` defaults to the server's default. `-continue`, `-resume` and `-sessions` use the server's
saved conversations; `-undo` (a local workspace) is refused.

Flags: `-model` (default `LLM_DEFAULT_MODEL`), `-user`, `-p`, `-json`, `-auto` (UI: do not ask for
approval of risky tool calls). Print mode exits 0 on success, 1 when the run failed or was
cancelled, 2 when it ended waiting for an answer or approval (the question goes to stderr).

| Key | Action |
|---|---|
| `enter` | send the message; while a run is active, **steer** it |
| `alt+enter`, `ctrl+j` | new line in the input |
| `esc`, `ctrl+c` | cancel the active run; `ctrl+c` again (or twice when idle) quits |
| `ctrl+o` | expand / collapse tool calls (args, longer results, diffs) |
| `pgup`/`pgdn`, `ctrl+u`/`ctrl+d`, mouse wheel | scroll the transcript |
| `y` / `a` / `n` (or `esc`) | tool approval: approve once / approve for this run / deny |
| `1`-`9`, or text + `enter` | answer an `ask_user` question |

Slash commands (type `/` for the completion list, `tab` completes, `enter` runs the highlighted one; `ctrl+p`
opens a palette over all of them). Most also have a leader keybind: `ctrl+x` then a letter, with a which-key hint.

| Command (leader key) | Action |
|---|---|
| `/model [id]` (m) | filterable picker of the configured models plus the ones the provider reports (`GET {base}/models`; Anthropic `/v1/models`); `ctrl+f` favorites, `f2`/`shift+f2` cycle recent models; a typed id the list lacks is accepted. Applies to the next run |
| `/sessions` (l) | switch to a saved conversation (messages and tool calls are replayed); `ctrl+r` rename, `ctrl+d` delete, `ctrl+o` pin |
| `/new` (n) | start an empty conversation |
| `/undo` (u), `/redo` (r) | remove the last prompt and its answers and restore the workspace files (git workspaces); the prompt returns to the input |
| `/compact` (c), `/fork` (f) | summarise the history now; copy the conversation into a new one |
| `/plan`, `/build` (`tab` toggles) | plan mode is read-only, enforced by the engine: only read-only tools plus `update_plan` and `ask_user` |
| `/effort [low\|medium\|high\|default]` (`ctrl+t` cycles) | reasoning effort of the next runs |
| `/connect` | pick a provider and enter its API key (stored in `~/.har/auth.json`, 0600); applies after a restart |
| `/init`, `/<your command>` | custom commands: markdown prompt templates in `~/.har/commands` and `<project>/.har/commands` (`$ARGUMENTS`, `$1`..`$9`, nested folders as `dir:name`), plus installed skills; `/init` writes `AGENTS.md`. `/reload` rereads them |
| `/themes` | seven themes with live preview; the choice is kept in `~/.har/tui-state.json` |
| `/thinking`, `/details`, `/timestamps` | show reasoning text, tool details (`ctrl+o`), message times |
| `/sidebar` (b, `ctrl+b`) | right panel: title, model and mode, token usage, the agent's plan, modified files, MCP servers |
| `/editor` (e), `/export` (x) | compose in `$EDITOR`; export the conversation to Markdown and open it |
| `/stash [pop\|list]` (s), `/image <path>`, `/copy`, `/notify` | park the input; attach an image; copy a message (OSC 52); desktop notification when the terminal is in the background |
| `/help`, `/tools`, `/quit` (q) | list commands and keys, expand tool calls, leave |

In the input: `@` fuzzy-picks a workspace file whose content is sent with the message (never outside the workspace;
local sessions only); a message starting with `!` runs a shell command on your machine and shows its output (it
also becomes context of your next message and is never a model tool call); `up`/`down` walk the input history;
pasting an image path attaches it, and long pastes are shown as `[pasted N lines]`. `alt+up`/`alt+down` select a
message and `alt+y` copies it. A message that only looks like a command (`/etc/hosts is wrong`) is sent as text;
start with a space to send an unknown `/word` literally. The history commands are refused while a run is active.

`~/.har/tui.json` (JSONC allowed; never read from a project) sets `theme`, `leader`, `leader_timeout`,
`scroll_speed`, `scroll_acceleration`, `diff_style` (`auto` is side by side from 120 columns, or `stacked`), `cursor`,
`mouse`, `attention` (notifications, bell count, a `sound_command` argv run without a shell) and `keybinds` (merged
with the defaults; `"command.<name>": "<leader>x"` rebinds a command). More commands: `/timeline` (g) jumps to a
message, `/children` lists the sub-agent (explore) runs of the conversation and opens one, `/diff`, `/mouse`,
`/tips` (h), `/status` (i), `/skills`, `/mcp`, `ctrl+z` suspends. The explore tool shows its steps live under its
tool call (`work.subagent` events; expand with `ctrl+o`).

**Model info and cost.** Context windows and prices come from LiteLLM's public table
(`raw.githubusercontent.com/BerriAI/litellm/.../model_prices_and_context_window.json`), fetched in the background
with a plain GET (nothing about you is sent), cached for 24 hours in `~/.har/cache/` and never blocking startup.
The status line shows `ctx 12.3k/200k (6%)` and the session cost (`~` when some usage had no price), the sidebar
a usage bar and the model's price per million tokens, the model picker the window, price and vision/reasoning
badges; `/cost` gives a per-run breakdown and `/context` the window usage. `HARNESS_MODEL_PRICES` is a file path or
URL replacing the source, or `off` to disable it; `~/.har/model-prices.json` (same schema) overrides or adds models,
e.g. a private endpoint. With `-remote` the server's table is used. Cache-write tokens are not priced and sub-agent
tokens only join the total when the run ends.

The status line shows the model, state, step count, tokens in/out/cached (with the cache share)
and the run's elapsed time. `edit_file` calls show a diff of `oldText`/`newText`. The UI is a pure
`tui.Model` fed by `client.Event`s through a `tui.Backend` (`tui.ClientBackend` adapts any `client.Client`),
with one render function per event type and golden tests in `hosts/tui/testdata`
(`go test ./hosts/tui -update` rewrites them). The package imports only `hosts/client` and `core/agui`.

