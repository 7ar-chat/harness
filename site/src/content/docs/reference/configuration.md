---
title: "Configuration files"
description: "Where the harness reads settings from (flags, environment, .har/config.yml of the project and of the user), what a repository may change, trust, providers and API keys."
---

The harness reads its settings from four layers: command-line flags, the process environment, the `.har/config.yml` of the project and the `~/.har/config.yml` of the user, on top of built-in defaults. Use this page to know which layer wins, what a cloned repository is allowed to set, and where API keys live. The keys of the file are listed in [config.yml keys](/docs/reference/config-yml/), the variables in [Environment variables](/docs/reference/env-vars/).

## Quick start

```bash
harness config init            # writes a commented ~/.har/config.yml (0600) when there is none
harness auth login anthropic   # stores the API key in ~/.har/auth.json (0600), prompts without echo
harness config path            # which user and project directory are in use
harness -config-dump           # the effective configuration, with the source of every value
```

`config init` never overwrites: when the file exists it prints `<file> already exists; left as it is`. `config path` prints:

```text
user dir:    /tmp/docs-check/cfg/home
project dir: /tmp/docs-check/cfg/repo/.har
```

The user directory is `(none)` when the home directory is unknown. With `HARNESS_NO_USER_DIR` set it reads `<dir> (not read: HARNESS_NO_USER_DIR is set)`. The project directory is `(none)` when no `.har/` was found.

## Directory layout

The directory name `.har` is one constant (`config.DirName`). A repository carries instructions, MCP servers and project settings; the user directory carries credentials and personal settings.

```text
<repo>/
  AGENTS.md                  agent instructions; else CLAUDE.md, else GEMINI.md
  .mcp.json                  MCP servers (the format Claude Code and others share)
  .har/
    config.yml               project settings: selections and tuning only, no secrets
    permissions.yml          allow / ask / deny rules
    instructions.md          extra instructions, always loaded, highest priority
    mcp.json                 MCP servers, same format as .mcp.json
    skills/<name>/SKILL.md   project skills (also .agents/skills and .claude/skills)
    memory/                  logical prefix of memory paths

~/.har/                      or $HAR_HOME
  config.yml                 your settings: endpoints, providers, model catalog, sandbox
  auth.json                  API keys by provider, mode 0600 (harness auth login)
  trust.json                 the projects you trust, mode 0600 (harness trust)
  AGENTS.md                  your own instructions, loaded for every project (local backend)
  permissions.yml            allow / ask / deny rules for every project
  mcp.json                   your MCP servers
  model-prices.json          optional override of the model price table
  skills/<name>/SKILL.md     your skills
  sessions/                  the saved conversations
  memory/                    memory files
  cache/                     downloaded data (model prices)
```

The project directory is the nearest `.har/` found walking up from the working directory (or from `LOCAL_WORKSPACE`) to the git root, the first directory that holds a `.git` entry. The user directory itself is never taken for a project directory.

### HAR_HOME and HARNESS_NO_USER_DIR

| Variable | Effect |
|---|---|
| `HAR_HOME` | Use this directory instead of `~/.har` (tests, containers). |
| `HARNESS_NO_USER_DIR` | Any non-empty value except `0`, `false`, `no`, `off`: read nothing from the user directory (no `auth.json`, `config.yml`, `AGENTS.md`, skills, MCP, permissions, `trust.json`). The sessions fall back to `$XDG_STATE_HOME/harness`, else `~/.local/state/harness`. |

<div class="callout tip">

**Hermetic servers.** On servers and CI set `HARNESS_NO_USER_DIR=1` and pass the key through the environment (`LLM_API_KEY`). Commands that write to the user directory (`harness auth`, `harness config init`, `harness trust`) still use `HAR_HOME` or `~/.har`: they do not honour `HARNESS_NO_USER_DIR`.

</div>

## Precedence

| Layer | Source | Wins over |
|---|---|---|
| 1 | command-line flags (`-model`, `-provider`, ...) | everything |
| 2 | process environment (`LLM_*`, `SANDBOX_*`, ...) | files and defaults |
| 3 | project `<repo>/.har/config.yml` | user file and defaults |
| 4 | user `~/.har/config.yml` | defaults |
| 5 | built-in defaults | |

A higher layer replaces a scalar or a list as a whole. Two exceptions: `permissions.allow`, `ask` and `deny` add up across the two files (a user `deny` is never lost), unless the matching `AGENT_PERMISSIONS_ALLOW|ASK|DENY` variable is set, which replaces them; and MCP servers merge by name (see [MCP](/docs/concepts/mcp/)).

A file is validated strictly. An unknown key is an error that names the file and the key:

```text
harness: ~/.har/config.yml: unknown key "sandbox.bakend"
```

A value of the wrong type is an error too (`limits.max_steps: expected an integer, got "many"`), as is a plain secret (see [auth.json](#authjson)).

## What a project file may and may not do

The user (the user file, the environment, the flags) owns every endpoint, key and model catalog. A repository you clone may only select among what you configured and tune behaviour.

| A project file may (selection and tuning) | Only you may (user file, environment, flags) |
|---|---|
| `provider`: one you defined, or `openai` / `anthropic` | `providers`, `base_url`, `api_key_env`, `api_key` |
| `default_model`, `explore_model`, `limits.compaction_model` and the `agents:` section, each checked against your model list | `models`, `presets`, `default_preset`, `discover_models` |
| `profile`, `tools`, `code_checks` | `session_header`, `cache_ttl`, `timeout_seconds` |
| `limits.*`, `skills.*`, `memory.*`, `context_windows`, `reasoning_models`, `vision_models`, `model_labels`, `prompt_variants`, `mcp.call_timeout_seconds` | the whole `sandbox:` section and `env:` |
| `permissions`: `ask`, `deny`, and a `mode` as strict as yours or stricter | `mcp.allow_stdio` |
| `permissions.allow`, looser modes and `mcp.servers` only when the project is [trusted](#trust) | |

A forbidden key is an error that names the file, the key, the reason and where it belongs:

```text
harness: <repo>/.har/config.yml: base_url cannot be set in a project file (a repository must not choose where your requests and API key are sent): put it in ~/.har/config.yml (or the environment)
```

Selecting something you did not configure is refused as well:

```text
harness: <repo>/.har/config.yml: provider "evil" (project .har/config.yml provider) is not configured by you: define it under providers.evil in ~/.har/config.yml, or choose openai, anthropic or a provider you defined
harness: model nope (project .har/config.yml agents.explore.model) is not configured by you: add it to providers.opencode.models in ~/.har/config.yml
```

The model catalog is the `models` list of the selected provider (`providers.<name>.models`, else `models`), plus the models you set yourself.

## Trust

`~/.har/trust.json` lists the project roots you trust (`{"projects": ["/abs/path"]}`, mode 0600, rewritten atomically). An untrusted project can only tighten permissions. A trusted project may also add permission `allow` rules, loosen the mode, and define MCP servers.

```bash
harness trust               # trust the project of the current directory
harness trust ~/src/app     # or a given directory
harness trust list          # the trusted projects
harness untrust             # stop trusting (a path may be given)
harness trust mcp [name]    # approve the exact command of a project's stdio MCP server
harness untrust mcp name    # withdraw that approval
HARNESS_TRUST_PROJECT=1 harness -p "..."    # trust the current project for this process only (CI)
```

What an untrusted project loses is reported, not silent. `-config-dump` ends with an `ignored:` block:

```text
ignored:
  <repo>/.har/config.yml: permissions.allow (1 rule(s)) ignored: the project is not trusted (run `harness trust` to honour it)
  <repo>/.har/config.yml: permissions.mode "yolo" ignored: it is looser than "ask" and the project is not trusted (run `harness trust` to honour it)
```

Modes rank from strictest to loosest: `readonly`, `ask` (also the empty mode), `auto-edit`, `yolo`, `yolo-unsafe`. The project sources of MCP servers (`<repo>/.mcp.json`, `<repo>/.har/mcp.json`, `mcp.servers` of the project file) are started only for a trusted project, and a stdio server of a project also needs `harness trust mcp`. See [Connect MCP servers](/docs/guides/connect-mcp/) and [Approvals and permissions](/docs/concepts/approvals-permissions/).

<div class="callout note">

**Trust is by project root.** With `HARNESS_NO_USER_DIR` set, `trust.json` is not read: only `HARNESS_TRUST_PROJECT` can trust a project then. `HARNESS_TRUST_PROJECT` accepts any value except empty, `0`, `false`, `no`, `off`.

</div>

## The agents section

`agents:` maps roles to the model (and profile) they use. It is allowed in both files; a project's choices are validated against your catalog.

| Key | Sets | Notes |
|---|---|---|
| `agents.default.model` | `LLM_DEFAULT_MODEL` | the model of new runs |
| `agents.default.profile` | `AGENT_PROFILE` | `work`, `coding` or `minimal` |
| `agents.explore.model` | `EXPLORE_MODEL` | the read-only explore sub-agent |
| `agents.compaction.model` | `COMPACTION_MODEL` | summarising old context |

Other roles are refused: `agents.title` and `agents.goal_judge` fail with `agents.<role> is not available yet: that role uses the model of the run`, `agents.default.reasoning_effort` with `... is not available yet: choose a preset (default_preset) in your user file`, any other role with `unknown key "agents.foo" (roles: default, explore, compaction)`.

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

## Named providers

`providers:` (user file only) defines the providers you select with `provider: <name>` or `harness -provider <name>`. `type` is `openai` (any OpenAI-compatible endpoint) or `anthropic`. A bare `provider: openai|anthropic` stays the built-in type with the default endpoint (`https://api.openai.com/v1`, `https://api.anthropic.com`) and the flat keys keep working.

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

The keys of a provider are `type`, `base_url`, `session_header`, `cache_ttl`, `models`, `default_model` and `api_key_env`. A name must match `^[A-Za-z0-9][A-Za-z0-9._-]*$`. The values of the selected provider sit above the flat keys of the files and below the environment: `LLM_BASE_URL` still wins, and the provider's `default_model` beats a flat `default_model` of the user file (a model chosen by the project beats the provider's). `-config-dump` shows the provider as `provider:     opencode (type openai)`.

## API key resolution

The key of the selected provider is the first match of:

1. `LLM_API_KEY` in the environment (a flat `api_key_env` of a file fills it);
2. the environment variable named by the provider's `api_key_env`;
3. `~/.har/auth.json`, the entry named like the provider;
4. the conventional variable of its type, `OPENAI_API_KEY` or `ANTHROPIC_API_KEY`, but not for a named provider that sets its own `base_url`: an OpenAI key is never sent to another host.

With none, the command stops and says how to set one. An endpoint on the local machine (`localhost`, `127.0.0.1`, `::1`) needs no key.

```text
no API key for provider opencode: run `harness auth login opencode`, or export LLM_API_KEY, or export OPENCODE_KEY (the api_key_env of provider opencode)
```

## auth.json

`~/.har/auth.json` is the only file API keys may be written in. A plain `api_key`, `password`, `secret` or `token` in any `config.yml` is refused:

```text
harness: ~/.har/config.yml: api_key: secrets do not belong in a configuration file; set api_key_env to the NAME of the environment variable that holds it
```

```json
{"providers": {"opencode": {"api_key": "sk-..."}, "anthropic": {"api_key": "sk-ant-..."}}}
```

```bash
harness auth login opencode            # prompts without echo; or: echo "$KEY" | harness auth login opencode -key-stdin
harness auth list                      # providers with a key (last 4 characters) and the file path
harness auth remove opencode
harness -provider opencode -p "hello"
```

<div class="callout warning">

**A readable `auth.json` is refused.** The file must not be readable by the group or others (`mode & 0o077 == 0`), otherwise every command fails with `<file> is readable by other users (mode 0644): refusing to use it; run: chmod 600 <file>`. The CLI creates it 0600 in a 0700 directory and rewrites it atomically. Keys are never logged: `-config-dump` shows `LLM_API_KEY  ****  (auth.json)`.

</div>

## Instruction files

`AGENTS.md` (else `CLAUDE.md`, else `GEMINI.md`), Cursor and Copilot rules, `.har/instructions.md` and the user's `~/.har/AGENTS.md` are read for the coding profile and put in the system prompt; see [Titles, project docs, commands](/docs/concepts/titles-docs-commands/). `~/.har/AGENTS.md` is read with the local sandbox backend, or with a remote one when `HARNESS_USER_INSTRUCTIONS=1`.

## Inspect the result

`harness -config-dump` prints the effective configuration and exits. Only values that were set are listed, each with its source (`flag`, `env`, `project file`, `user file`); secrets are masked.

```text
project root: /tmp/docs-check/me/app
project file: /tmp/docs-check/me/app/.har/config.yml
user file:    /tmp/docs-check/me/.har/config.yml
auth file:    (none)
trust:        project not trusted (run `harness trust` to let it add allow rules and MCP servers)
provider:     opencode (type openai)
precedence:   flag > env > project file > user file > default

AGENT_MAX_STEPS                          40                             (project file)
AGENT_PERMISSIONS                        ask                            (user file)
AGENT_PERMISSIONS_DENY                   Bash(terraform apply*)         (user file)
AGENT_PROFILE                            coding                         (project file)
EXPLORE_MODEL                            deepseek-v4.1-flash            (project file)
LLM_BASE_URL                             https://opencode.ai/zen/go/v1  (user file)
LLM_DEFAULT_MODEL                        deepseek-v4.1-pro              (project file)
LLM_MODELS                               deepseek-v4.1-pro,deepseek-v4.1-flash (user file)
LLM_PROVIDER                             opencode                       (user file)
LLM_API_KEY                              ****                           (env OPENCODE_KEY)

ignored:
  /tmp/docs-check/me/app/.har/config.yml: permissions.allow (1 rule(s)) ignored: the project is not trusted (run `harness trust` to honour it)
```

Any name that contains `KEY`, `SECRET`, `PASSWORD`, `TOKEN` or `DSN` is masked as `****`, which includes `LLM_MAX_OUTPUT_TOKENS`. MCP servers read, started or skipped are listed after the table.
