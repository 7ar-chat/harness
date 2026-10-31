---
title: "config.yml keys"
description: "Every key of ~/.har/config.yml and <repo>/.har/config.yml, the environment variable it sets, which keys a project may use, and the exact errors."
---

`config.yml` is the file form of the environment variables: each key is translated to one variable of [Environment variables](/docs/reference/env-vars/), so a setting works the same in either place. The file exists once per user (`~/.har/config.yml`, the full schema) and once per project (`<repo>/.har/config.yml`, a subset). Layers, trust and providers are explained in [Configuration files](/docs/reference/configuration/); this page is the schema.

```yaml
# ~/.har/config.yml
provider: anthropic
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

Check what a file does with `harness -config-dump`. The sample above produces `LLM_PROVIDER`, `LLM_MODELS`, `LLM_DEFAULT_MODEL`, `AGENT_PROFILE`, `SANDBOX_BACKEND`, `AGENT_PERMISSIONS`, `AGENT_PERMISSIONS_ALLOW` and `AGENT_PERMISSIONS_DENY` entries, each marked `(user file)`.

## How values are read

| Kind | YAML | Becomes |
|---|---|---|
| string, integer, boolean, number | a scalar; an integer must parse as one, a boolean as `true`/`false` | the variable's text |
| list | `[a, b]` (a single string is a one-item list) | `a,b` |
| map | `{model: value}` (a string is passed through as written) | `model=value,...` |
| rules | `["rule one", "rule two"]` | one rule per line |

The scalar of a list item must be a single value; a nested list or map is an error. Unknown keys, wrong types and secrets stop the load with an error that names the file.

<div class="callout note">

**A key without a value is not a way to unset.** `profile:` (null) sets an empty string, which behaves like an unset variable; for an integer, boolean, number or list key a null is an error (`limits.max_steps: expected an integer, got ""`, `models: expected a list, got <nil>`). Leave a key out to keep the default.

</div>

## Top-level keys

Column "Project" says whether `<repo>/.har/config.yml` may set the key (`yes`), or only the user file, the environment and the flags (`user`). A key marked `user` in a project file is an error.

| Key | Environment variable | Project | Notes |
|---|---|---|---|
| `provider` | `LLM_PROVIDER` | yes | `openai`, `anthropic`, or a name defined under `providers`. A project may only select `openai`, `anthropic` or a provider you defined |
| `base_url` | `LLM_BASE_URL` | user | endpoint of the OpenAI-compatible or Anthropic API |
| `api_key_env` | `LLM_API_KEY` | user | the NAME of the variable that holds the key; the variable must be set |
| `models` | `LLM_MODELS` | user | list, the allow-list of model ids |
| `default_model` | `LLM_DEFAULT_MODEL` | yes | checked against your model list when it comes from a project |
| `discover_models` | `LLM_DISCOVER_MODELS` | user | `true`: at startup add the provider's model list to `models` |
| `explore_model` | `EXPLORE_MODEL` | yes | model of the read-only explore sub-agent |
| `session_header` | `LLM_SESSION_HEADER` | user | request header that carries the thread id |
| `cache_ttl` | `LLM_CACHE_TTL` | user | `5m` or `1h` (Anthropic prompt cache) |
| `timeout_seconds` | `LLM_TIMEOUT_SECONDS` | user | integer |
| `profile` | `AGENT_PROFILE` | yes | `work`, `coding` or `minimal` |
| `tools` | `AGENT_TOOLS` | yes | list (`+name`, `-name`, or a bare list that replaces the default set) |
| `code_checks` | `CODE_CHECKS` | yes | `.ext=command;...` syntax checks after file edits |
| `context_windows` | `LLM_CONTEXT_WINDOWS` | yes | map `model: tokens` |
| `reasoning_models` | `LLM_REASONING_MODELS` | yes | list |
| `vision_models` | `LLM_VISION_MODELS` | yes | list |
| `model_labels` | `LLM_MODEL_LABELS` | yes | map `model: label` |
| `prompt_variants` | `LLM_PROMPT_VARIANTS` | yes | map `model: variant` |
| `presets` | `LLM_PRESETS` | user | the stops of the thinking slider, written as one string |
| `default_preset` | `LLM_DEFAULT_PRESET` | user | |
| `providers` | (none) | user | named providers, see [below](#providers) |
| `agents` | (several) | yes | see [below](#agents) |
| `env` | any setting | user | see [below](#env) |
| `mcp` | (several) | partly | see [below](#mcp) |

## limits

| Key | Environment variable | Project |
|---|---|---|
| `limits.max_steps` | `AGENT_MAX_STEPS` | yes |
| `limits.max_tool_output_chars` | `AGENT_MAX_TOOL_OUTPUT_CHARS` | yes |
| `limits.tool_output_spill` | `AGENT_TOOL_OUTPUT_SPILL_CHARS` | yes |
| `limits.step_retries` | `LLM_STEP_RETRIES` | yes |
| `limits.stream_idle_seconds` | `LLM_STREAM_IDLE_SECONDS` | yes |
| `limits.call_max_seconds` | `LLM_CALL_MAX_SECONDS` | yes |
| `limits.max_output_tokens` | `LLM_MAX_OUTPUT_TOKENS` | yes |
| `limits.compaction_threshold` | `COMPACTION_THRESHOLD` | yes |
| `limits.compaction_model` | `COMPACTION_MODEL` | yes |
| `limits.keep_recent_tool_calls` | `COMPACTION_KEEP_RECENT_TOOL_RESULTS` | yes |

`limits.max_output_tokens` is a string of `model=tokens` pairs, not a map: `"deepseek-v4.1-pro=8000"`. The meaning and defaults are in [Environment variables](/docs/reference/env-vars/); `COMPACTION_MASK_FROM_FRACTION` has no key and is set through `env:`.

## sandbox

The whole section is user-only. A project that has any `sandbox` key gets: `sandbox cannot be set in a project file (the sandbox backend, its address and its limits belong to you, not to a repository): put it in ~/.har/config.yml (or the environment)`.

| Key | Environment variable | Notes |
|---|---|---|
| `sandbox.backend` | `SANDBOX_BACKEND` | `local`, `opensandbox` or `none`; `none` also ignores `sandbox.url` |
| `sandbox.url` | `OPENSANDBOX_URL` | |
| `sandbox.api_key_env` | `OPENSANDBOX_API_KEY` | the NAME of the variable that holds the key |
| `sandbox.image` | `OPENSANDBOX_IMAGE` | |
| `sandbox.use_server_proxy` | `OPENSANDBOX_USE_SERVER_PROXY` | boolean |
| `sandbox.credential_proxy` | `OPENSANDBOX_CREDENTIAL_PROXY` | boolean |
| `sandbox.workspace` | `LOCAL_WORKSPACE` | local backend |
| `sandbox.state_dir` | `LOCAL_STATE_DIR` | local backend, also the session store |
| `sandbox.timeout_seconds` | `SANDBOX_TIMEOUT_SECONDS` | idle lifetime of a sandbox |
| `sandbox.command_timeout` | `SANDBOX_COMMAND_TIMEOUT_SECONDS` | default command timeout |
| `sandbox.cpu` | `SANDBOX_CPU` | |
| `sandbox.memory` | `SANDBOX_MEMORY` | |
| `sandbox.egress_allowlist` | `SANDBOX_EGRESS_ALLOWLIST` | list of hosts, IPs or CIDRs |
| `sandbox.egress_unrestricted` | `SANDBOX_EGRESS_UNRESTRICTED` | boolean |
| `sandbox.max_sandboxes_per_user` | `MAX_SANDBOXES_PER_USER` | integer |

A backend other than the three is refused in the file: `sandbox.backend must be local, opensandbox or none, got "docker"`. See [Sandbox backends](/docs/integrations/sandbox-backends/).

## permissions

| Key | Environment variable | Project |
|---|---|---|
| `permissions.mode` | `AGENT_PERMISSIONS` | yes, only as strict as the user's, or when trusted |
| `permissions.allow` | `AGENT_PERMISSIONS_ALLOW` | only when the project is trusted |
| `permissions.ask` | `AGENT_PERMISSIONS_ASK` | yes |
| `permissions.deny` | `AGENT_PERMISSIONS_DENY` | yes |

`mode` is `ask`, `auto-edit`, `yolo`, `yolo-unsafe` or `readonly`. The three rule lists add up across the user and project file; an untrusted project's `allow` and looser `mode` are dropped and reported under `ignored:` in `-config-dump`. In the environment the rules are separated by newlines or `;;`. See [Approvals and permissions](/docs/concepts/approvals-permissions/).

## skills and memory

| Key | Environment variable | Project |
|---|---|---|
| `skills.dir` | `SKILLS_DIR` | yes |
| `skills.in_coding` | `SKILLS_IN_CODING` | yes |
| `memory.write_mode` | `HARNESS_MEMORY_WRITE_MODE` | yes |
| `memory.delete_mode` | `HARNESS_MEMORY_DELETE_MODE` | yes |
| `memory.default_scope` | `HARNESS_MEMORY_DEFAULT_SCOPE` | yes |
| `memory.embedding_backend` | `HARNESS_MEMORY_EMBEDDING_BACKEND` | yes |
| `memory.recall_injection` | `HARNESS_MEMORY_RECALL_INJECTION` | yes |
| `memory.max_entries` | `HARNESS_MEMORY_MAX_ENTRIES` | yes |
| `memory.max_content_bytes` | `HARNESS_MEMORY_MAX_CONTENT_BYTES` | yes |
| `memory.consolidation_hours` | `HARNESS_MEMORY_CONSOLIDATION_HOURS` | yes |
| `memory.deleted_retention_days` | `HARNESS_MEMORY_DELETED_RETENTION_DAYS` | yes |
| `memory.in_coding` | `MEMORY_IN_CODING` | yes |

See [Skills](/docs/concepts/skills/) and [Memory](/docs/concepts/memory/).

## mcp

| Key | Environment variable | Project | Notes |
|---|---|---|---|
| `mcp.servers` | `MCP_SERVERS` (merged by name) | yes, started only when trusted | map `name: {command, args, env}` or `{type, url, headers, tools}` |
| `mcp.allow_stdio` | `HARNESS_MCP_STDIO` | user | boolean, must be `true` or `false`; local backend only |
| `mcp.call_timeout_seconds` | `MCP_CALL_TIMEOUT_SECONDS` | yes | integer |

Any other `mcp.*` key is an error: `unknown key "mcp.bogus"`. A project that sets `mcp.allow_stdio` gets `mcp.allow_stdio cannot be set in a project file (a repository must not enable running its own programs): put it in ~/.har/config.yml (or the environment)`. A server entry accepts only `type`, `url`, `headers`, `command`, `args`, `env` and `tools`; `${VAR}` and `${VAR:-default}` are expanded from the process environment. See [Connect MCP servers](/docs/guides/connect-mcp/).

## providers

User file only. Each entry is a named provider; the name must match `^[A-Za-z0-9][A-Za-z0-9._-]*$`.

| Key | Meaning |
|---|---|
| `providers.<name>.type` | `openai` or `anthropic` (default `openai`, or `anthropic` for a provider named `anthropic`) |
| `providers.<name>.base_url` | endpoint; must be a valid URL |
| `providers.<name>.session_header` | sets `LLM_SESSION_HEADER` |
| `providers.<name>.cache_ttl` | sets `LLM_CACHE_TTL` |
| `providers.<name>.models` | list, sets `LLM_MODELS`; the catalog a project may select from |
| `providers.<name>.default_model` | sets `LLM_DEFAULT_MODEL` |
| `providers.<name>.api_key_env` | the NAME of the variable that holds the key |

The selected provider's values sit above the flat keys of the files and below the environment. See [LLM providers](/docs/integrations/llm-providers/).

## agents

Allowed in both files. `agents.<role>.<key>` sets one variable:

| Key | Environment variable |
|---|---|
| `agents.default.model` | `LLM_DEFAULT_MODEL` |
| `agents.default.profile` | `AGENT_PROFILE` |
| `agents.explore.model` | `EXPLORE_MODEL` |
| `agents.compaction.model` | `COMPACTION_MODEL` |

## env

User file only. `env:` is the escape hatch for the settings without a key: a map of `ENV_NAME: value`.

```yaml
env:
  HARNESS_MEMORY_MAX_ENTRIES: 500
  COMPACTION_MASK_FROM_FRACTION: 0.6
```

The name must be one of the harness settings (the variables in [Environment variables](/docs/reference/env-vars/) that come from `config`), and a name that contains `KEY`, `SECRET`, `PASSWORD`, `TOKEN` or `DSN` is refused, which also rules out `LLM_MAX_OUTPUT_TOKENS`, `LLM_PROMPT_CACHE_KEY` and `POSTGRES_PASSWORD`. Use the `limits.max_output_tokens` key for the first. Values are written as text.

## Errors

The messages are printed as `harness: <file>: <message>`. Verbatim, with the file prefix left out:

| Cause | Message |
|---|---|
| unknown key | `unknown key "sandbox.bakend"` |
| unknown key in `agents` | `unknown key "agents.foo" (roles: default, explore, compaction)` |
| unknown key under a provider | `unknown key "providers.x.foo"` |
| wrong type | `limits.max_steps: expected an integer, got "many"` (also `expected true or false, got ...`, `expected a number, got ...`) |
| a section expected | `limits must be a section (key: value pairs)` |
| a plain secret (`api_key`, `password`, `secret`, `token`) | `api_key: secrets do not belong in a configuration file; set api_key_env to the NAME of the environment variable that holds it` |
| `api_key_env` names an unset variable | `api_key_env: the environment variable NOPE_UNSET is not set` |
| unknown name under `env` | `env.NOPE is not a setting of the harness` |
| secret-looking name under `env` | `env.LLM_API_KEY: secrets do not belong in a configuration file; export LLM_API_KEY in the environment` |
| invalid provider type | `providers.x.type must be openai or anthropic, got "foo"` |
| role not available | `agents.title is not available yet: that role uses the model of the run` |
| reasoning effort per role | `agents.default.reasoning_effort is not available yet: choose a preset (default_preset) in your user file` |
| key forbidden in a project | `base_url cannot be set in a project file (a repository must not choose where your requests and API key are sent): put it in ~/.har/config.yml (or the environment)` |

The reasons that follow `cannot be set in a project file` are, by key: `providers`, `base_url`, `api_key_env`, `api_key`, `session_header`, `cache_ttl`: "a repository must not choose where your requests and API key are sent"; `models`, `presets`, `default_preset`, `discover_models`: "a repository must not decide which models exist"; `timeout_seconds`: "it tunes the connection to your provider, which is yours to configure"; `sandbox`: "the sandbox backend, its address and its limits belong to you, not to a repository"; `env`: "env: can set any setting, including the ones a repository may not set"; `mcp.allow_stdio`: "a repository must not enable running its own programs".

## Example: a user file and a project file

```yaml
# ~/.har/config.yml
provider: opencode
providers:
  opencode:
    type: openai
    base_url: https://opencode.ai/zen/go/v1
    session_header: x-opencode-session
    models: [deepseek-v4.1-pro, deepseek-v4.1-flash]
    default_model: deepseek-v4.1-flash
    api_key_env: OPENCODE_KEY
profile: coding
limits: {max_steps: 80, compaction_threshold: 0.8}
sandbox: {backend: local, command_timeout: 180}
permissions:
  mode: ask
  deny: ["Bash(terraform apply*)"]
mcp: {allow_stdio: false, call_timeout_seconds: 60}
```

```yaml
# <repo>/.har/config.yml
provider: opencode
agents:
  default:    {model: deepseek-v4.1-pro, profile: coding}
  explore:    {model: deepseek-v4.1-flash}
  compaction: {model: deepseek-v4.1-flash}
limits: {max_steps: 40}
permissions:
  mode: readonly
  deny: ["Bash(terraform apply*)"]
```

<div class="callout tip">

**Start from the generated file.** `harness config init` writes a commented starter `config.yml` with the common keys, so you can uncomment instead of typing.

</div>
