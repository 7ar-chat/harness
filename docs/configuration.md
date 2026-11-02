# Configuration

Settings, providers, credentials, instruction files, MCP servers and environment variables. Overview: [README](../README.md).

## LLM providers

`provider` (`LLM_PROVIDER`, flag `-provider NAME`) selects a provider: one of your named providers (see
[Named providers](#named-providers-and-api-keys)) or a built-in type. The built-in `openai` (the default: any OpenAI-compatible endpoint at `LLM_BASE_URL` with `LLM_API_KEY`;
Chat Completions, or `{LLM_BASE_URL}/responses` for the models in `LLM_RESPONSES_MODELS`) or `anthropic`.
Model ids (`LLM_MODELS`) are sent to the provider verbatim. A user is just a login string; the harness
forwards no identity to the model or to MCP servers.

`LLM_DISCOVER_MODELS=true` makes every host (`harness-web`, the worker, the TUI) ask the provider at startup which
models it serves (`GET {base}/models`; Anthropic `/v1/models`, 8 s timeout) and append them to `LLM_MODELS`, so
`GET /api/models` and the web model picker list them and runs may use them. The configured ids stay first and
the default is unchanged; a provider that cannot be asked logs a warning and the configured list stands. It is off
by default because `LLM_MODELS` is an allow-list and discovery widens it to everything the key can reach; a project
file cannot set it. The list is read once per process start.

## Providers that route by session

`LLM_SESSION_HEADER=x-opencode-session` sends the thread id in that header on every model call. OpenCode Go
requires it (it routes a conversation to one backend, which keeps the prompt cache warm: 86% cached on the
benchmark against 58–68% through a router without it):

```
LLM_PROVIDER=openai LLM_BASE_URL=https://opencode.ai/zen/go/v1 LLM_API_KEY=… \
LLM_SESSION_HEADER=x-opencode-session LLM_MODELS=deepseek-v4.1-flash LLM_DEFAULT_MODEL=deepseek-v4.1-flash
```

## Configuration files and standards

The harness follows the conventions of the other coding-agent tools: a project directory `.har/` next to the
code (and `~/.har/` for the user), and the de-facto standards for agent instructions (`AGENTS.md`), skills
(`SKILL.md` folders) and MCP servers (`.mcp.json`). The directory name is one constant, `config.DirName`.

```
<repo>/
  AGENTS.md                  agent instructions (https://agents.md); else CLAUDE.md, else GEMINI.md
  .mcp.json                  MCP servers, the format Claude Code and others share
  .har/
    config.yml               project settings: selections and tuning only (see below); no secrets
    permissions.yml          allow/ask/deny rules (see [permissions](permissions.md))
    instructions.md          extra instructions, always loaded, highest priority
    mcp.json                 MCP servers, same format as .mcp.json
    skills/<name>/SKILL.md   project skills (also .agents/skills and .claude/skills)
    memory/                  logical prefix of memory paths
```

The user directory is the harness's home, like `~/.claude` or `~/.codex`: general settings and credentials live
there and never in a repository.

```
~/.har/                      $HAR_HOME when set
  config.yml                 your settings: endpoints, providers, model catalog, sandbox (full schema); no secrets
  trust.json                 the projects you trust, mode 0600 (written by `harness trust`)
  auth.json                  API keys by provider, mode 0600 (written by `harness auth login`)
  AGENTS.md                  your own instructions, loaded for every project (local backend)
  permissions.yml            allow/ask/deny rules for every project
  mcp.json                   your MCP servers
  skills/<name>/SKILL.md     your skills
  sessions/                  the saved conversations (LOCAL_STATE_DIR overrides)
  memory/                    memory files
```

`HAR_HOME` points the harness at another directory (tests, containers). `HARNESS_NO_USER_DIR=1` makes the
harness read nothing from it (no `auth.json`, `config.yml`, `AGENTS.md`, skills, MCP, permissions; sessions fall back to
`$XDG_STATE_HOME/harness`): set it on servers and CI that must be hermetic and give the key through the environment.
`harness config path` prints the user directory and the project directory in use; `harness config init` writes a commented
starter `config.yml` (0600, in a 0700 directory) when there is none.

The project directory is the nearest `.har/` found walking up from the working directory (or `LOCAL_WORKSPACE`) to the git
root. `harness -config-dump` prints the effective configuration with the source of every value that was set
(`flag`, `env`, `project file`, `user file`; secrets are masked) and the files that were read.

### Precedence

| Layer | Source | Wins over |
|---|---|---|
| 1 | command-line flags (`-model`, ...) | everything |
| 2 | process environment (`LLM_*`, `SANDBOX_*`, ...) | files and defaults |
| 3 | project `<repo>/.har/config.yml` | user file and defaults |
| 4 | user `~/.har/config.yml` | defaults |
| 5 | built-in defaults | |

Scalars and lists are replaced by the higher layer as a whole. Exceptions: `permissions.allow|ask|deny` add up across the two
files (a user `deny` is never lost; a project's `allow` only counts when it is trusted) unless the matching `AGENT_PERMISSIONS_*`
variable is set, and MCP servers merge by name (see below). Unknown keys are an error naming the key and the file (`.har/config.yml: unknown key "sandbox.bakend"`), a
plain `api_key` is refused, and a value of the wrong type is an error.

### What a project file may and may not do

The **user** (`~/.har/config.yml`, the environment, the flags) owns every endpoint, key and model catalog. A **project**
(`<repo>/.har/config.yml`) may only *select* among what you configured and *tune* behaviour. A key in the right-hand column in a
project file is an error naming the file, the key and where it belongs (`~/.har/config.yml`).

| A project file may (selection and tuning) | Only you may (user file, environment, flags) |
|---|---|
| `provider: <name>`: one you defined, or `openai` / `anthropic` | `providers` (any definition), `base_url`, `api_key_env`, `api_key` |
| `default_model`, `explore_model`, `limits.compaction_model` and the `agents:` section, each checked against **your** model list | `models`, `presets`, `default_preset` (which models exist) |
| `profile`, `tools`, `code_checks` | `session_header`, `cache_ttl`, `timeout_seconds` |
| `limits.*`, `skills.*`, `memory.*`, `context_windows`, `prompt_variants`, ... | the whole `sandbox:` section (`backend`, `url`, `image`, `api_key_env`, `workspace`, `egress_*`, limits ...) and `env:` |
| `permissions`: `ask`, `deny` and a `mode` as strict as yours or stricter | `mcp.allow_stdio` |
| instruction files, skills, nested `AGENTS.md` (data, as before) | `permissions.allow` and looser modes (unless trusted), MCP servers (unless trusted) |

A selected model that is not in your list is an error: `model X (project .har/config.yml agents.explore.model) is not configured by you:
add it to providers.<p>.models in ~/.har/config.yml`. A project cannot switch the provider of a role: roles use models of the
selected provider.

**Trust.** `~/.har/trust.json` (`{"projects": ["/abs/path"]}`, mode 0600, written atomically) lists the project roots you trust. A
trusted project may also add permission `allow` rules and loosen the mode, and define MCP servers (`<repo>/.mcp.json`,
`<repo>/.har/mcp.json`, `mcp.servers` of its `config.yml`; `http`/`sse` freely, `stdio` still needs your `mcp.allow_stdio` and the
local backend). An untrusted project's MCP servers are not started; `-config-dump` lists them with the hint to run `harness trust`.
Your own `~/.har/mcp.json`, `mcp.servers` of your file and `MCP_SERVERS` are always trusted.

```
harness trust            # trust the project of the current directory (its git root / .har parent)
harness trust ~/src/app  # or a given directory
harness trust list       # the trusted projects
harness untrust          # stop trusting (a path may be given)
HARNESS_TRUST_PROJECT=1 harness -p "..."   # trust the current project for this process only (CI)
```

A project's **stdio** MCP server (a program to run) needs one more step: after `harness trust`, approve its exact command with
`harness trust mcp [name]` (all pending servers without a name; `harness untrust mcp <name>` withdraws it). The approval is
recorded in `trust.json` for (project root, server name, SHA-256 of command, arguments and `env`); changing any of them, or
`harness untrust` of the project, drops it and the server is listed as not started until approved again. The check runs when
the config is loaded and again right before the process is spawned. In an **interactive** run (the TUI by default; `-auto`,
`-p` and headless hosts are not) a pending server is also offered for approval in the run itself: the model gets the tool
`mcp_approve__<server>_<hash>`, and calling it pauses the run with a normal approval card naming the project, the server, the
exact command, the arguments and the names (never the values) of its `env`. Approving records the same `trust.json` entry as
`harness trust mcp` and the server's tools are available from the **next** run (the current run's tools were built before the
decision; the tool's result says so). Denying leaves it pending and it is not asked again on that thread; a changed
command is a new tool and asks again. Where nobody can answer approvals (automatic mode, hosts without an approval gate) the
tool is refused and the server stays unstarted; `harness trust mcp` remains the way to approve it. Your own servers (`~/.har/mcp.json`, `MCP_SERVERS`) need no approval.

Trust is by project root; `HARNESS_NO_USER_DIR=1` ignores `trust.json` (only the environment variable can trust then).

#### The `agents:` section

Maps roles to the model (and profile) they use; it is allowed in both files, and a project's choices are validated against your catalog.

| Key | Sets | Notes |
|---|---|---|
| `agents.default.model` | `LLM_DEFAULT_MODEL` | the model of new runs |
| `agents.default.profile` | `AGENT_PROFILE` | `work`, `coding` or `minimal` |
| `agents.explore.model` | `EXPLORE_MODEL` | the read-only explore sub-agent |
| `agents.compaction.model` | `COMPACTION_MODEL` | summarising old context |
| `agents.title`, `agents.goal_judge`, `agents.default.reasoning_effort` | | not available yet (the title and goal-judge calls use the model of the run; the effort comes from `default_preset`): rejected |

```yaml
# <repo>/.har/config.yml
provider: opencode            # must be defined in ~/.har/config.yml
agents:
  default:    {model: deepseek-v4.1-pro, profile: coding}
  explore:    {model: deepseek-v4.1-flash}
  compaction: {model: deepseek-v4.1-flash}
permissions:
  mode: readonly              # only counts when as strict as yours
  deny: ["Bash(terraform apply*)"]
```

### config.yml

This table is the schema of the **user file**; a project file accepts the subset allowed above. Keys are translated to the environment variables of the "Configuration" section, so every existing setting works;
`env:` is the escape hatch for the ones without a nice key (`env: {HARNESS_MEMORY_MAX_ENTRIES: 500}`; secret-looking names such as
`*_KEY`, `*_PASSWORD`, `*_TOKEN` are refused).

| Key | Environment variable | Notes |
|---|---|---|
| `provider` | `LLM_PROVIDER` | `openai`, `anthropic`, or the name of an entry of `providers` |
| `providers` | (see below) | named providers: `type`, `base_url`, `session_header`, `cache_ttl`, `models`, `default_model`, `api_key_env` |
| `base_url` | `LLM_BASE_URL` | |
| `api_key_env` | `LLM_API_KEY` | the NAME of the environment variable that holds the key; unset is an error |
| `models` | `LLM_MODELS` | list |
| `default_model` | `LLM_DEFAULT_MODEL` | |
| `discover_models` | `LLM_DISCOVER_MODELS` | `true`: at startup add the provider's `GET {base}/models` to `models` (user file only) |
| `explore_model` | `EXPLORE_MODEL` | |
| `session_header` | `LLM_SESSION_HEADER` | |
| `cache_ttl` | `LLM_CACHE_TTL` | `5m` or `1h` |
| `timeout_seconds` | `LLM_TIMEOUT_SECONDS` | |
| `profile` | `AGENT_PROFILE` | `work`, `coding` or `minimal` |
| `tools` | `AGENT_TOOLS` | list (`+name`, `-name`) |
| `code_checks` | `CODE_CHECKS` | |
| `context_windows` | `LLM_CONTEXT_WINDOWS` | map `model: tokens` |
| `reasoning_models`, `vision_models` | `LLM_REASONING_MODELS`, `LLM_VISION_MODELS` | lists |
| `model_labels`, `prompt_variants` | `LLM_MODEL_LABELS`, `LLM_PROMPT_VARIANTS` | maps |
| `presets`, `default_preset` | `LLM_PRESETS`, `LLM_DEFAULT_PRESET` | |
| `limits.max_steps` | `AGENT_MAX_STEPS` | |
| `limits.max_tool_output_chars`, `limits.tool_output_spill` | `AGENT_MAX_TOOL_OUTPUT_CHARS`, `AGENT_TOOL_OUTPUT_SPILL_CHARS` | |
| `limits.step_retries`, `limits.stream_idle_seconds`, `limits.call_max_seconds`, `limits.max_output_tokens` | `LLM_STEP_RETRIES`, `LLM_STREAM_IDLE_SECONDS`, `LLM_CALL_MAX_SECONDS`, `LLM_MAX_OUTPUT_TOKENS` | |
| `limits.compaction_threshold`, `limits.compaction_model`, `limits.keep_recent_tool_calls` | `COMPACTION_THRESHOLD`, `COMPACTION_MODEL`, `COMPACTION_KEEP_RECENT_TOOL_RESULTS` | |
| `sandbox.backend` | `SANDBOX_BACKEND` | `local`, `opensandbox` or `none` (`none` ignores `sandbox.url`) |
| `sandbox.url`, `sandbox.image`, `sandbox.api_key_env` | `OPENSANDBOX_URL`, `OPENSANDBOX_IMAGE`, `OPENSANDBOX_API_KEY` | `api_key_env` is a variable name |
| `sandbox.use_server_proxy`, `sandbox.credential_proxy`, `sandbox.timeout_seconds`, `sandbox.command_timeout`, `sandbox.cpu`, `sandbox.memory` | `OPENSANDBOX_USE_SERVER_PROXY`, `OPENSANDBOX_CREDENTIAL_PROXY`, `SANDBOX_TIMEOUT_SECONDS`, `SANDBOX_COMMAND_TIMEOUT_SECONDS`, `SANDBOX_CPU`, `SANDBOX_MEMORY` | `credential_proxy` keeps the project's GitHub token outside the sandbox, see [docs/sandbox-adapters.md](sandbox-adapters.md) |
| `sandbox.egress_allowlist`, `sandbox.egress_unrestricted`, `sandbox.max_sandboxes_per_user` | `SANDBOX_EGRESS_ALLOWLIST`, `SANDBOX_EGRESS_UNRESTRICTED`, `MAX_SANDBOXES_PER_USER` | |
| `sandbox.workspace`, `sandbox.state_dir` | `LOCAL_WORKSPACE`, `LOCAL_STATE_DIR` | local backend |
| `permissions.mode` | `AGENT_PERMISSIONS` | `ask`, `auto-edit`, `yolo`, `yolo-unsafe`, `readonly` |
| `permissions.allow`, `.ask`, `.deny` | `AGENT_PERMISSIONS_ALLOW`, `_ASK`, `_DENY` | lists of rules, merged across the files |
| `skills.dir`, `skills.in_coding` | `SKILLS_DIR`, `SKILLS_IN_CODING` | |
| `memory.write_mode`, `.delete_mode`, `.default_scope`, `.embedding_backend`, `.recall_injection`, `.max_entries`, `.max_content_bytes`, `.consolidation_hours`, `.deleted_retention_days`, `.in_coding` | `HARNESS_MEMORY_*`, `MEMORY_IN_CODING` | |
| `mcp.servers` | `MCP_SERVERS` (merged) | map `name: {command,args,env}` or `{type,url,headers,tools}` |
| `mcp.allow_stdio` | `HARNESS_MCP_STDIO` | `true`; **user file only** (a project file that sets it is an error) |
| `agents.<role>.<key>` | see above | roles `default`, `explore`, `compaction` |
| `mcp.call_timeout_seconds` | `MCP_CALL_TIMEOUT_SECONDS` | |
| `env` | any setting | `ENV_NAME: value` |

Server and infrastructure settings (`POSTGRES_*`, `REDIS_*`, `ARTIFACTS_*`, `PORT`) stay in the environment.

```yaml
# ~/.har/config.yml  (the user file: the full schema)
provider: anthropic
api_key_env: ANTHROPIC_API_KEY
models: [claude-sonnet-5-5]
default_model: claude-sonnet-5-5
profile: coding
sandbox: {backend: local}
permissions:
  mode: ask
  allow: ["make test*"]
  deny:  ["Bash(terraform apply*)"]
mcp:
  servers:
    docs: {type: http, url: "https://docs.example.com/mcp", headers: {Authorization: "Bearer ${DOCS_TOKEN}"}}
```

### Named providers and API keys

`providers:` (user file only; a project file that has it is an error) defines providers you
select with `provider: <name>` (a project file may select one of yours) or `harness -provider <name>`. `type` is `openai` (any OpenAI-compatible endpoint) or
`anthropic`; a bare `provider: openai|anthropic` stays the built-in type with the `LLM_BASE_URL` default, and the flat
keys (`base_url`, `api_key_env`, ...) and every environment variable keep working. The values of the selected provider
sit above the flat keys of the files and below the environment (`LLM_BASE_URL` still wins).

```yaml
# ~/.har/config.yml
provider: opencode
providers:
  opencode:
    type: openai
    base_url: https://opencode.ai/zen/go/v1
    session_header: x-opencode-session
    models: [deepseek-v4.1-flash]
    default_model: deepseek-v4.1-flash
    api_key_env: OPENCODE_KEY      # optional
  anthropic:
    type: anthropic
```

**Key resolution**, first match wins, for the selected provider:

1. `LLM_API_KEY` in the environment (also a flat `api_key_env`, which fills it);
2. the environment variable named by the provider's `api_key_env`;
3. `~/.har/auth.json`, the entry named like the provider;
4. the conventional variable of its type: `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` (not for a named provider that sets
   its own `base_url`: an OpenAI key is not sent to another host).

With none, print mode (`-p`) and pipes stop with a message that says how to set one (`harness auth login <provider>`).
The terminal UI opens instead on a home screen that walks through it: pick a provider, paste a key (it goes to
`auth.json`), choose a model from the list the provider reports. The choice applies at once, no restart; `/connect` does
the same later, and `/model` switches among the models of the connected provider. A local
endpoint (`localhost`, `127.0.0.1`) needs no key. A plain `api_key` is refused in every `config.yml`; keys go in `auth.json`:

```json
{"providers": {"opencode": {"api_key": "sk-..."}, "anthropic": {"api_key": "sk-ant-..."}}}
```

`auth.json` must not be readable by the group or others (`mode & 0o077 == 0`); otherwise it is refused with an error naming
the file and `chmod 600`. The CLI creates it 0600 in a 0700 directory and rewrites it atomically. Keys are never logged;
`-config-dump` shows `LLM_API_KEY  ****  (auth.json)` (or the environment variable) and never the key.

```
harness auth login opencode            # prompts without echo; or: echo "$KEY" | harness auth login opencode -key-stdin
harness auth list                      # providers with a key, last 4 characters, and the file path
harness auth remove opencode
harness -provider opencode -p "hello"
```

### Instruction files

Read for the coding profile, lowest priority first (a later file wins where they disagree; they are all shown to the model):

| # | File | When |
|---|---|---|
| 1 | `~/.har/AGENTS.md` | local backend, or `HARNESS_USER_INSTRUCTIONS=1`; trusted |
| 2 | `.github/copilot-instructions.md`, `.cursorrules`, `.cursor/rules/*.mdc` with `alwaysApply: true` | only if the repo root has no `AGENTS.md` |
| 3 | `AGENTS.md`, else `CLAUDE.md`, else `GEMINI.md` (repo root) | the first that exists; the others are not also loaded |
| 4 | `.har/instructions.md` | always, last |

These go into the system prompt, computed once per run so the prompt stays byte-identical across steps (prompt cache). Each file
is cut at 16 KiB and the total at 48 KiB, with a `[…cut: read_file <path> for the rest]` or `size limit` marker. Text from the
repository is data: the run's context is reported untrusted (memory writes are only proposed) and the block cannot close itself;
only `~/.har/AGENTS.md` is trusted. Everything is read through the sandbox API, so remote sandboxes work; the user's file is the
only one read from the host, and only with the local backend or the explicit `HARNESS_USER_INSTRUCTIONS=1`.

* **`@path/to/file` imports** (Claude Code style) inside an instruction file are appended as `[imported from <path>]`; the path is
  relative to the file, at most 3 levels deep, each file once, 16 KiB each, never outside the workspace (symbolic links are
  resolved and refused if they leave it; in `~/.har/AGENTS.md` imports stay inside `~/.har`). `@alice`, e-mail addresses and
  anything inside code spans or fenced blocks are not imports.
* **Nested files**: an `AGENTS.md` (else `CLAUDE.md`) in a subdirectory applies to the files under it, the closest one wins. They are
  not in the prompt. The first time `read_file`, `write_file` or `edit_file` touches a path under such a directory in a run, the
  note is appended once to that tool's result as `[Instructions for pkg/sub (pkg/sub/AGENTS.md, repository notes for files under this
  directory): ...]` (8 KiB max). Cursor rules with `globs` work the same way, when a matching file is touched.
  Implemented by `sandboxtools.Options.AfterPath`, a generic hook that appends text to those three results.

### Compatibility

| Standard | Read | Not read |
|---|---|---|
| `AGENTS.md` | repo root, nested directories, `~/.har/AGENTS.md` | `AGENTS.override.md` (Codex), `~/.codex/` |
| `CLAUDE.md` | repo root when there is no `AGENTS.md`; nested directories; `@imports` | `~/.claude/CLAUDE.md`, `CLAUDE.local.md`, `.claude/settings.json`, hooks, slash commands |
| `GEMINI.md` | repo root when there is neither of the above | `~/.gemini/`, `@` imports of `.gemini` settings |
| Cursor | `.cursorrules`, `.cursor/rules/*.mdc` (`alwaysApply`, `globs`) | "agent requested" rules (description only), `.cursor/mcp.json` |
| Copilot | `.github/copilot-instructions.md` | `.github/instructions/*.instructions.md` |
| `.mcp.json` | `mcpServers`: `command/args/env`, `type: http\|sse` + `url/headers`; `${VAR}` and `${VAR:-default}` in values | `enabledMcpjsonServers` approvals, OAuth, `type: ws` |
| Agent Skills | `SKILL.md` folders in `.har`, `.agents`, `.claude` (project and user) | `allowed-tools`, plugin marketplaces |
| `.har/permissions.yml` | harness format | Claude Code `settings.json` permission rules |

### MCP servers

The sources below are fixed at start-up. Servers that a project or a user adds at run time come from the **marketplace**
(a catalog the product ships plus custom servers kept in the database, with their keys sealed): see [docs/marketplace.md](marketplace.md).

Sources, lowest to highest (a later one replaces a server of the same name): `~/.har/config.yml` `mcp.servers`, `~/.har/mcp.json`,
`<repo>/.mcp.json`, `<repo>/.har/mcp.json`, `<repo>/.har/config.yml` `mcp.servers`, `MCP_SERVERS`. Files use the shared format:

```json
{"mcpServers": {
  "docs": {"type": "http", "url": "https://docs.example.com/mcp", "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}},
  "legacy": {"type": "sse", "url": "https://old.example.com/sse"},
  "fs": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."], "env": {"DEBUG": "0"}}
}}
```

`${VAR}` (or `${VAR:-default}`) in `url`, `headers`, `env`, `command` and `args` comes from the process environment; an unset one
without a default is an error naming the server and the variable. The harness adds an optional `tools` allow-list.

The three project sources (`<repo>/.mcp.json`, `<repo>/.har/mcp.json`, `mcp.servers` of the project `config.yml`) are only started
when the project is **trusted** (`harness trust`, see above); otherwise they are listed by `-config-dump`. Your own `~/.har/mcp.json`,
`mcp.servers` in `~/.har/config.yml` and `MCP_SERVERS` are always trusted.

Stdio servers (`command`) start a child process, so they are the dangerous kind (a cloned repository can bring a `.mcp.json`).
The rules:

0. A stdio server that a project file defines also needs your approval of its exact command: `harness trust mcp <name>`, or the in-run prompt of an interactive run (see Trust).
1. They run only with `SANDBOX_BACKEND=local` and an explicit opt-in: `mcp.allow_stdio: true` in **`~/.har/config.yml`** or
   `HARNESS_MCP_STDIO=1`. A project `config.yml` that sets it is an error, so a repository cannot enable them for itself
   (`HARNESS_MCP_STDIO=0` in the environment turns the user file's setting off).
2. Otherwise they are skipped with a warning (listed by `-config-dump`), and the remote ones still work.
3. The tools of a stdio server go through the permission layer as **ask on first use**: one approval per tool and run (the usual
   "approve for the run"); an `allow` rule (`my-server__*(*)` style, see [permissions](permissions.md)) stops the question, `deny` blocks them, and
   `yolo` mode, as for all asks, does not prompt.
4. The child gets a reduced environment (`PATH`, `HOME`, `USER`, `LANG`, `TERM`, `TMPDIR`, ...) plus the server's own `env`; the
   harness's API keys and database passwords are not inherited.

## Configuration

`config.Load()` reads the environment, on top of the optional `config.yml` files (see [Configuration files and
standards](#configuration-files-and-standards) above; every variable below can also be set there). The harness-wide settings below use the `HARNESS_` prefix (no other
names are read); the model, sandbox and storage settings keep their descriptive names (`LLM_*`,
`SANDBOX_*`, `OPENSANDBOX_*`, `POSTGRES_*`, `REDIS_*`, `ARTIFACTS_*`, `AGENT_*`, see `config/`).

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_PRODUCTION` | unset | Any non-empty value turns production mode on (stricter startup checks); unset is debug mode |
| `HARNESS_WORKSPACE_CHECKPOINTS` | `true` | Local Git checkpoint of `/workspace` before every turn, with per-file history, diff and restore |
| `HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES` | `20971520` | Files larger than this are kept out of the checkpoints (`<= 0`: no limit) |
| `HARNESS_GIT_ARCHIVE_MAX_BYTES` | `52428800` | Largest `.git` archive stored after a run (a bigger one is not archived) |
| `HARNESS_MAX_UPLOAD_BYTES` | `104857600` | Largest accepted attachment |
| `HARNESS_MAX_CONCURRENT_UPLOADS` | `4` | Concurrent uploads per process |
| `HARNESS_MAX_CONCURRENT_UPLOADS_PER_USER` | `4` | Concurrent uploads per user and process |
| `HARNESS_PURGE_AFTER_DAYS` | `30` | Deleted threads are hard-deleted this many days later (`<= 0`: never) |
| `HARNESS_BLOB_GC_GRACE_HOURS` | `24` | A content blob is collected only after this many hours without a referencing version |
| `HARNESS_MEMORY_WRITE_MODE` | `warn` | `warn` records writes without a valid trigger, `strict` rejects them |
| `HARNESS_MEMORY_DELETE_MODE` | `soft` | `soft` keeps a deleted entry until the retention, `hard` removes it |
| `HARNESS_MEMORY_DEFAULT_SCOPE` | `personal` | Scope of new entries (`repository`, `workspace`, `personal`) |
| `HARNESS_MEMORY_EMBEDDING_BACKEND` | `simple` | Ranking backend (`simple`; `disabled`, `none`, `keyword` only change the reported capabilities) |
| `HARNESS_MEMORY_RECALL_INJECTION` | `true` | Put the recalled memories in the system prompt context |
| `HARNESS_MEMORY_MAX_ENTRIES` | `1000` | Entries per user (`<= 0`: no limit) |
| `HARNESS_MEMORY_MAX_CONTENT_BYTES` | `200000` | Size of one entry |
| `HARNESS_MEMORY_CONSOLIDATION_HOURS` | `168` | Period of the dedup and purge job (`<= 0`: disabled) |
| `HARNESS_MEMORY_DELETED_RETENTION_DAYS` | `30` | Soft-deleted entries are purged after this many days |
| `HARNESS_MCP_STDIO` | unset | `1` allows MCP servers that are local programs (stdio); local backend only. Wins over the user file's `mcp.allow_stdio` |
| `HARNESS_USER_INSTRUCTIONS` | unset | `1` reads the host user's `~/.har/AGENTS.md` also with a remote sandbox backend (always read with the local one) |
| `HARNESS_TRUST_PROJECT` | unset | `1` trusts the current project for this process (its `allow` permission rules and MCP servers apply); see `harness trust` |
| `HARNESS_TEST_DATABASE_URL` | unset | Postgres used by the `adapters/store/pgstore` tests (skipped when unset) |
| `HARNESS_TEST_S3_ENDPOINT`, `_ACCESS_KEY`, `_SECRET_KEY` | unset | S3-compatible endpoint for the artifact blob tests (skipped when unset) |
| `SENTRY_DSN` | unset | `harness-web` only. Reports every error the process logs (failed runs, agent panics, storage and sweeper failures, HTTP 500s, handler panics) to a Sentry-protocol tracker such as Rustrak, Sentry or GlitchTip. Events are grouped by log message, carry the run and thread id, and contain no request bodies, headers or user identity. Warnings and cancellations are not sent; an unreachable tracker never blocks a request |
| `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE` | `production` under `HARNESS_PRODUCTION`, else `development`; unset | Tags on every event |
| `SENTRY_BROWSER_DSN` | unset | `harness-web` only. Errors raised in the browser UI (uncaught errors, unhandled rejections, a crashed screen) go to this Sentry-protocol project. Use a project of its own, so UI and server errors do not mix. The UI reads it from `GET /api/client-config` at startup and loads the error SDK only then. Nothing the user typed is collected: no cookies, headers, request bodies, query strings, local variables or identity |
| `UMAMI_URL`, `UMAMI_WEBSITE_ID` | unset | `harness-web` only. Usage statistics with a self-hosted [Umami](https://umami.is): `UMAMI_URL` is its origin (`https://umami.example.com`), `UMAMI_WEBSITE_ID` the website's id. Both are needed. Page views follow the UI's hash routes; a signed-in person is identified by their login and nothing else (no name or email). The browser's do-not-track setting is honoured |

With either browser setting, the UI's Content-Security-Policy widens to exactly those origins (`script-src` for Umami, `connect-src` for both) and no further; with neither it stays `'self'`. A malformed value stops `harness-web` at startup.

Memory entries live in the store; their logical paths (`memories/semantic/facts.md`, ...) may be written with
the `.har/memory/` prefix, which is stripped. The workspace checkpoints leave `.har/tmp/` and
`.har/runtime/` out of the history.

