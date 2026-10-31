---
title: "Environment variables"
description: "Every environment variable the harness reads, grouped by prefix, with its default and meaning."
---

`config.Load()` reads these variables, and so do the commands `harness`, `harness-web` and `harness-worker`. Most of them can also be written in a `config.yml` (the key is in [config.yml keys](/docs/reference/config-yml/)); the rest, mainly infrastructure and secrets, are environment-only. The environment wins over both files, and a command-line flag wins over the environment ([Configuration files](/docs/reference/configuration/)).

"Default" is the value when the variable is unset or empty. A dash means no default (the feature is off or the value is derived, as the meaning says). Lists are separated by commas. Variables that hold a secret are marked **secret**: they are never accepted in a `config.yml` and are masked by `harness -config-dump`.

<div class="callout note">

**Booleans** accept what Go's `strconv.ParseBool` accepts (`1`, `t`, `true`, `0`, `f`, `false`, ...). `HARNESS_PRODUCTION` and `HARNESS_NO_USER_DIR` are the exceptions described in their rows.

</div>

## LLM_* (model provider)

| Variable | Default | Meaning |
|---|---|---|
| `LLM_PROVIDER` | `openai` | `openai` (any OpenAI-compatible endpoint at `LLM_BASE_URL`), `anthropic`, or the name of a provider defined under `providers:`; for a defined provider the variable holds its type. Any other value fails: `LLM_PROVIDER must be openai, anthropic or the name of a provider defined under providers: in a config.yml, got "x"` |
| `LLM_BASE_URL` | - | API root. Empty: `https://api.openai.com/v1`, or `https://api.anthropic.com` for `anthropic` |
| `LLM_API_KEY` | - | **secret.** API key; first in the [key resolution order](/docs/reference/configuration/#api-key-resolution). `OPENAI_API_KEY` / `ANTHROPIC_API_KEY` are read as the last fallback |
| `LLM_DEFAULT_MODEL` | `gpt-5.6-terra` | Model of new runs. Added to `LLM_MODELS` when missing |
| `LLM_MODELS` | `gpt-5.6-sol,gpt-5.6-terra` | Allow-list of model ids |
| `LLM_DISCOVER_MODELS` | false | At startup ask the provider for its model list and add it to `LLM_MODELS`. A provider that cannot be asked is logged and the configured list stands |
| `LLM_TIMEOUT_SECONDS` | `300` | Time allowed for the first chunk of a model call, and the HTTP timeout of the provider client |
| `LLM_SESSION_HEADER` | - | Name of a request header that carries the thread id, for providers that route by session (OpenCode Go: `x-opencode-session`) |
| `LLM_CACHE_TTL` | - (`5m`) | Anthropic prompt-cache lifetime: `5m` or `1h` |
| `LLM_CONTEXT_WINDOWS` | `gpt-5.6-sol=272000,gpt-5.6-terra=272000` | `model=tokens,...` (tokens at least 1000). A model not listed has a 128000 window, a DeepSeek model of `LLM_MODELS` 1000000 |
| `LLM_REASONING_MODELS` | `gpt-5.6-sol,gpt-5.6-terra` | Models that accept a reasoning effort. DeepSeek models of `LLM_MODELS` join the default list |
| `LLM_VISION_MODELS` | `gpt-5.6-sol,gpt-5.6-terra` | Models that receive image parts |
| `LLM_MODEL_LABELS` | `gpt-5.6-sol=GPT-5.6 Sol,gpt-5.6-terra=GPT-5.6 Terra` | `model=Label,...` shown in the model list; default is the id |
| `LLM_PRESETS` | `fast=gpt-5.6-terra:low,balanced=gpt-5.6-terra:medium,thinking=gpt-5.6-sol:medium,deep=gpt-5.6-sol:high` | The stops of the thinking slider, `id=model:effort,...`. `none` disables it. The built-in value is dropped (and logged) when its models are not in `LLM_MODELS` / `LLM_REASONING_MODELS`; an explicit value is validated strictly |
| `LLM_DEFAULT_PRESET` | `balanced` | The stop of new threads; must be in `LLM_PRESETS` |
| `LLM_REASONING_EFFORTS` | `gpt-5.6-sol=none\|low\|medium\|high\|xhigh,gpt-5.6-terra=none\|low\|medium\|high\|xhigh` | `model=effort\|effort,...`: the efforts a model accepts. A reasoning model not listed accepts `minimal`, `low`, `medium`, `high`. Known efforts: `none`, `minimal`, `low`, `medium`, `high`, `xhigh` |
| `LLM_PROMPT_VARIANTS` | `gpt-5.6-sol=gpt-5.6,gpt-5.6-terra=gpt-5.6` | `model=variant,...` with the variants `coding`, `deepseek`, `default`, `gpt-5.6`. Not listed: `coding` under `AGENT_PROFILE=coding`, `deepseek` for a name containing `deepseek`, else `default`. Under the coding profile the default value is ignored unless the variable is set |
| `LLM_RESPONSES_MODELS` | `gpt-5.6-sol,gpt-5.6-terra` | Models called through the OpenAI Responses API (`{base}/responses`); `none` sends every model to Chat Completions. Only the `openai` provider has one |
| `LLM_STREAM_IDLE_SECONDS` | `90` | Longest gap between two chunks of a model stream (at least 1) |
| `LLM_CALL_MAX_SECONDS` | `1200` | Ceiling of one model call whatever the stream sends (at least 1) |
| `LLM_STEP_RETRIES` | `3` | How often a failed model call is re-issued as a whole step (at least 0) |
| `LLM_MAX_OUTPUT_TOKENS` | - (16000 per model) | `model=tokens,...` (at least 256): output reserved below the window, compaction triggers at window minus (this + 20000). Not sent to the provider |
| `LLM_REASONING_PASSBACK_MODELS` | - (DeepSeek models of `LLM_MODELS`) | Models whose assistant messages carry `reasoning_content` back |
| `LLM_PROMPT_CACHE_KEY` | `true` | Send `prompt_cache_key` (the thread id) to the OpenAI provider. Turn off for endpoints that reject unknown fields; never sent for DeepSeek models |

See [LLM providers](/docs/integrations/llm-providers/).

## AGENT_* and tool profile

| Variable | Default | Meaning |
|---|---|---|
| `AGENT_PROFILE` | - (`work`) | `work`, `coding` or `minimal`; else `AGENT_PROFILE must be work, coding or minimal, got "x"` |
| `AGENT_TOOLS` | - | Comma list over the profile's tools: `+name` adds, `-name` removes, a bare list replaces the set. `memory` stands for the memory tools, `skills` for `load_skill` |
| `AGENT_MAX_STEPS` | `60` | Model steps per run (values below 1 become 1) |
| `AGENT_MAX_TOOL_OUTPUT_CHARS` | `30000` | Tool output is cut beyond this |
| `AGENT_TOOL_OUTPUT_SPILL_CHARS` | `20000` | Longer tool outputs are written to `/workspace/.work/tool-output/`. Must not exceed `AGENT_MAX_TOOL_OUTPUT_CHARS` |
| `AGENT_PERMISSIONS` | - (`ask` on the local backend) | `ask`, `auto-edit`, `yolo`, `yolo-unsafe` or `readonly`. Applies to the local backend and the coding profile |
| `AGENT_PERMISSIONS_ALLOW` | - | Extra allow rules, separated by newlines or `;;`. Replaces the files' rules when set |
| `AGENT_PERMISSIONS_ASK` | - | Extra ask rules, same form |
| `AGENT_PERMISSIONS_DENY` | - | Extra deny rules, same form |
| `EXPLORE_MODEL` | - (the run's model) | Model of the read-only explore sub-agent; added to `LLM_MODELS` when missing |
| `CODE_CHECKS` | - | `.ext=command;...` syntax checks run after `write_file` / `edit_file`; `{path}` is the file. On by default with `AGENT_PROFILE=coding` |
| `SKILLS_IN_CODING` | false | Keep `load_skill` and the skills index in the coding profile when no skill is installed |
| `MEMORY_IN_CODING` | false | Keep the memory tools and prompt in the coding profile |

See [Features and profiles](/docs/concepts/features-profiles/) and [Approvals and permissions](/docs/concepts/approvals-permissions/).

## COMPACTION_*

| Variable | Default | Meaning |
|---|---|---|
| `COMPACTION_THRESHOLD` | - (window minus output reserve) | Share of the context window that triggers compaction; must be at least 0 and below 1 |
| `COMPACTION_MODEL` | - (the run's model) | Model that summarises old context |
| `COMPACTION_KEEP_RECENT_TOOL_RESULTS` | `6` | Older tool results are masked in the model input |
| `COMPACTION_MASK_FROM_FRACTION` | `0.5` | Masking starts once the prompt passes this share of the window; `0` masks from the first step; must be at least 0 and below 1 |

See [Compaction](/docs/concepts/compaction/).

## SANDBOX_*, OPENSANDBOX_* and LOCAL_*

| Variable | Default | Meaning |
|---|---|---|
| `SANDBOX_BACKEND` | - | `local`, `opensandbox`, `none`, or a backend a host registered. Unset: `opensandbox` when `OPENSANDBOX_URL` is set, else `none` (no sandbox tools). `opensandbox` needs the blank import of `github.com/7ar-chat/harness/adapters/sandbox/opensandbox`; `harness`, `harness-web` and `harness-worker` link it already |
| `LOCAL_WORKSPACE` | - (the working directory) | Local backend: the directory seen as `/workspace`. Also where the project directory is searched from |
| `LOCAL_STATE_DIR` | - | Local backend state (per-sandbox directories, `/skills`, job records; default `<user cache dir>/harness`), and the session store of the commands (default `<user dir>/sessions`) |
| `OPENSANDBOX_URL` | - | OpenSandbox lifecycle server |
| `OPENSANDBOX_API_KEY` | - | **secret.** API key of that server |
| `OPENSANDBOX_IMAGE` | `python:3.11-slim` | Image of new sandboxes |
| `OPENSANDBOX_USE_SERVER_PROXY` | `false` | Reach sandboxes through the server's proxy |
| `OPENSANDBOX_CREDENTIAL_PROXY` | `false` | Create sandboxes with the egress sidecar's credential vault so GitHub authentication is added outside the sandbox. Needs opensandbox-server 0.2.0 or later and egress 1.1.1 or later in dns+nft mode |
| `SANDBOX_TIMEOUT_SECONDS` | `1800` | Idle lifetime of a sandbox |
| `SANDBOX_COMMAND_TIMEOUT_SECONDS` | `120` | Default command timeout |
| `SANDBOX_CPU` | `1` | CPU of a sandbox |
| `SANDBOX_MEMORY` | `2Gi` | Memory of a sandbox |
| `SANDBOX_EGRESS_ALLOWLIST` | - | Hosts (`*.x` allowed), IPs or CIDRs a sandbox may reach; no scheme, port or path: `SANDBOX_EGRESS_ALLOWLIST: "x" is not a host name, IP address or CIDR (no scheme, port or path)` |
| `SANDBOX_EGRESS_UNRESTRICTED` | `false` | Accept unrestricted egress in production. Without an allow-list and in production, an `opensandbox` backend is refused: `SANDBOX_EGRESS_ALLOWLIST is empty in production: set it, or set SANDBOX_EGRESS_UNRESTRICTED=true to accept unrestricted sandbox egress` |
| `SANDBOX_EGRESS_DENY_CIDRS` | `172.16.0.0/12,10.0.0.0/8,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10,0.0.0.0/8,fc00::/7,fe80::/10,::ffff:0:0/96` | Blocked in unrestricted mode. Every entry must be a CIDR or an IP |
| `SANDBOX_UNHEALTHY_RECREATE_SECONDS` | `300` | A sandbox unhealthy for longer (and checked at least 3 times) is recreated |
| `MAX_SANDBOXES_PER_USER` | `5` | Sandboxes per user |

See [Sandboxes](/docs/concepts/sandboxes/) and [Sandbox backends](/docs/integrations/sandbox-backends/).

## POSTGRES_*

Set `POSTGRES_HOST` to use Postgres: `harness-web -team` and `harness-worker` look at whether the variable is present at all. Use [Stores](/docs/integrations/stores/) for the setup.

| Variable | Default | Meaning |
|---|---|---|
| `POSTGRES_HOST` | `postgres` | Host. Its presence (even with the default) selects Postgres in `harness-web -team`; `harness-worker` fails without it unless `-standalone`: `no shared store: set POSTGRES_HOST (and the other POSTGRES_* settings), or pass -standalone` |
| `POSTGRES_PORT` | `5432` | Port |
| `POSTGRES_DB` | `db` | Database |
| `POSTGRES_USER` | `postgres` | User |
| `POSTGRES_PASSWORD` | empty | **secret.** Password |
| `POSTGRES_SCHEMA` | `work` | Schema, used as `search_path` and migrated at startup |
| `POSTGRES_SSLMODE` | `require` | `sslmode` of the connection (set `disable` for a local Postgres) |
| `DB_POOL_MAX_CONNS` | `20` | Pool size |
| `PING_DB_AT_STARTUP` | `true` | `false` starts without touching Postgres and Redis (lazy connections) |

## REDIS_* and USE_SENTINEL

Redis is the shared event log, queue and coordination for several processes ([Redis](/docs/integrations/redis/)). The variables build the client of `redisinfra.NewClient(cfg)`; the process decides to use it (`harness-web` in team mode when `REDIS_HOST` is present, `harness-worker` always unless `-standalone`).

| Variable | Default | Meaning |
|---|---|---|
| `REDIS_HOST` | `redis` | Host. Its presence enables the shared feed in `harness-web -team` |
| `REDIS_PORT` | `6379` | Port |
| `REDIS_PASSWORD` | - | **secret.** Password (also the Sentinel password) |
| `REDIS_DB` | `0` | Database number |
| `REDIS_POOL_SIZE` | `256` | Connections; must exceed the concurrent event-stream readers, each holds one during a blocking read |
| `USE_SENTINEL` | `false` | Use a Sentinel failover client |
| `REDIS_SENTINEL_HOSTS` | - | Comma-separated Sentinel addresses |
| `REDIS_MASTER_NAME` | `mymaster` | Sentinel master name |

## ARTIFACTS_*, AWS_* and uploads

`ARTIFACTS_S3_*` and `AWS_*` are parsed and validated by `config.Load()` into `Config`, but none of the commands of this repository builds an S3 blob store from them: `s3blobs.Options` takes bucket, endpoint, region, keys, path style and prefix as plain values, which a host passes in `harness.Options.Blobs`. The upload limits are likewise carried by `Config` for a host that serves uploads. See [Artifacts and checkpoints](/docs/concepts/artifacts-checkpoints/).

| Variable | Default | Meaning |
|---|---|---|
| `ARTIFACTS_S3_BUCKET` | - | Bucket |
| `ARTIFACTS_S3_ENDPOINT` | - | S3-compatible endpoint (empty: AWS) |
| `ARTIFACTS_S3_REGION` | `us-east-1` | Region |
| `ARTIFACTS_S3_PATH_STYLE` | `true` | Path-style addressing |
| `ARTIFACTS_S3_PREFIX` | `harness/` | Prefix of every key |
| `ARTIFACTS_S3_CREATE_BUCKET` | false | Create the bucket at startup when missing (local RustFS) |
| `AWS_ACCESS_KEY_ID` | - | **secret.** Access key |
| `AWS_SECRET_ACCESS_KEY` | - | **secret.** Secret key |
| `HARNESS_MAX_UPLOAD_BYTES` | `104857600` | Largest attachment (413 `file_too_large` beyond) |
| `HARNESS_MAX_CONCURRENT_UPLOADS` | `4` | Concurrent uploads per process (429 `too_many_uploads` beyond) |
| `HARNESS_MAX_CONCURRENT_UPLOADS_PER_USER` | `4` | Concurrent uploads per user and process |
| `PREVIEW_MAX_CONCURRENCY` | `2` | Office-to-PDF conversions per process |
| `HARNESS_PURGE_AFTER_DAYS` | `30` | Deleted threads are hard-deleted this many days later (0 or less: never) |
| `HARNESS_BLOB_GC_GRACE_HOURS` | `24` | A content blob is collected after this many hours without a referencing version (0 or less: 24) |
| `HARNESS_WORKSPACE_CHECKPOINTS` | `true` | Local Git checkpoint of `/workspace` before every turn, with per-file history, diff and restore |
| `HARNESS_WORKSPACE_CHECKPOINT_MAX_FILE_BYTES` | `20971520` | Files larger than this stay out of the checkpoints (0 or less: no limit) |
| `HARNESS_GIT_ARCHIVE_MAX_BYTES` | `52428800` | Largest `.git` archive stored after a run; a bigger one is not archived |

## MEMORY and SKILLS

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_MEMORY_WRITE_MODE` | `warn` | `warn` records writes without a valid trigger, `strict` rejects them |
| `HARNESS_MEMORY_DELETE_MODE` | `soft` | `soft` keeps a deleted entry until the retention ends, `hard` removes it |
| `HARNESS_MEMORY_DEFAULT_SCOPE` | `personal` | Scope of new entries: `personal`, `repository` or `workspace` |
| `HARNESS_MEMORY_EMBEDDING_BACKEND` | `simple` | `simple` (bag-of-words cosine) or `openai` (vectors from an OpenAI-compatible endpoint); `disabled`, `none`, `keyword` only change the reported capabilities |
| `HARNESS_MEMORY_RECALL_INJECTION` | `true` | Put the recalled memories in the system prompt context |
| `HARNESS_MEMORY_MAX_ENTRIES` | `1000` | Entries per user (0 or less: no limit) |
| `HARNESS_MEMORY_MAX_CONTENT_BYTES` | `200000` | Size of one entry |
| `HARNESS_MEMORY_CONSOLIDATION_HOURS` | `168` | Period of the dedup and purge job (0 or less: disabled) |
| `HARNESS_MEMORY_DELETED_RETENTION_DAYS` | `30` | Soft-deleted entries are purged after this many days |
| `HARNESS_MEMORY_EXTRACT` | false | Propose memories from each completed run (pending until confirmed). `harness-web -team` sets it to `true` unless already set |
| `HARNESS_MEMORY_EXTRACT_MODEL` | - (the run's model) | Model that proposes the memories |
| `HARNESS_MEMORY_EMBEDDING_URL` | - (`LLM_BASE_URL`, else `api.openai.com`) | Endpoint of the `openai` embedding backend |
| `HARNESS_MEMORY_EMBEDDING_KEY` | - (`LLM_API_KEY`) | **secret.** Key of that endpoint |
| `HARNESS_MEMORY_EMBEDDING_MODEL` | - | Embedding model of the `openai` backend |
| `SKILLS_DIR` | - (`~/.har/skills`) | User skills directory, one `<name>/SKILL.md` per skill |

The write, delete and scope values are validated: `HARNESS_MEMORY_WRITE_MODE must be warn or strict, got "x"`, and likewise for the other two. See [Memory](/docs/concepts/memory/) and [Skills](/docs/concepts/skills/).

## MCP_*

| Variable | Default | Meaning |
|---|---|---|
| `MCP_SERVERS` | - | JSON array of servers, `[{"name": "docs", "url": "https://..."}]`; every entry needs `name` and `url` or `command`, names are unique. The highest-priority MCP source and always trusted |
| `MCP_CALL_TIMEOUT_SECONDS` | `120` | Bound of one MCP tool call (0 or less: 120) |
| `HARNESS_MCP_STDIO` | unset | `1` allows MCP servers that are local programs (stdio); local backend only. Wins over the user file's `mcp.allow_stdio`; `0` turns it off |
| `APPROVAL_RISKY_TOOL_PATTERNS` | `create\|send\|update\|delete\|post\|publish\|write` | Case-insensitive regexp alternatives: an MCP tool with a name word starting with one needs approval in interactive mode |

MCP support is opt-in in a Go program: a blank import of `github.com/7ar-chat/harness/mcpfeature`, otherwise `harness.New` fails with `harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"`. See [MCP](/docs/concepts/mcp/).

## RUNTIME_*, approvals and streams

| Variable | Default | Meaning |
|---|---|---|
| `RUNTIME_CONCURRENCY` | `16` | Runs executed at once by a process (values below 1 become 1); `harness-worker -concurrency` overrides it |
| `RUNTIME_SHUTDOWN_GRACE_SECONDS` | `15` | How long in-flight runs may finish on shutdown before they are interrupted |
| `RUNTIME_TERMINATION_GRACE_SECONDS` | `30` | The pod's termination grace; `RUNTIME_SHUTDOWN_GRACE_SECONDS` plus 12 must fit: `RUNTIME_SHUTDOWN_GRACE_SECONDS (40) + 12 s must fit RUNTIME_TERMINATION_GRACE_SECONDS (30), the pod's termination grace` |
| `MAX_STREAMS_PER_USER` | `8` | Concurrent event streams of one user per process (429 `too_many_streams` beyond); at least 1 |
| `APPROVAL_TTL_HOURS` | `72` | A pending approval older than this expires; at least 1 |

## HARNESS_* (process and product)

| Variable | Default | Meaning |
|---|---|---|
| `HARNESS_PRODUCTION` | unset | Any non-empty value turns production mode on: unrestricted sandbox egress must be accepted explicitly, and `SENTRY_ENVIRONMENT` defaults to `production`. Unset is debug mode |
| `HARNESS_USER_INSTRUCTIONS` | false | Read the host user's `~/.har/AGENTS.md` also with a remote sandbox backend (the local one always reads it) |
| `HARNESS_TRUST_PROJECT` | unset | Any value except empty, `0`, `false`, `no`, `off` trusts the current project for this process |
| `HAR_HOME` | `~/.har` | The user directory |
| `HARNESS_NO_USER_DIR` | unset | Any value except empty, `0`, `false`, `no`, `off`: read nothing from the user directory |
| `HARNESS_MODEL_PRICES` | LiteLLM's public price table | File path or `http(s)` URL of the model price table; `off` disables it. `~/.har/model-prices.json` overrides single models |
| `HARNESS_REMOTE_TOKEN` | - | **secret.** Bearer token `harness -remote` sends to `harness-web` |
| `HARNESS_WORKER_ADDR` | `127.0.0.1:9090` | Admin listen address of `harness-worker` (`-addr` overrides) |
| `HARNESS_WEB_ADDR` | `127.0.0.1:8080` | Listen address of `harness-web` (`-addr` overrides) |
| `HARNESS_WEB_TOKEN` | - | **secret.** Bearer token for the API. Required on a non-loopback address unless OIDC is configured: `refusing to listen on <addr> without HARNESS_WEB_TOKEN: set a token, configure OIDC (OIDC_ISSUER), or bind a loopback address` |
| `HARNESS_WEB_ORIGINS` | - | Comma-separated CORS allow-list |
| `HARNESS_TEAM` | unset | `1` turns on team mode (as `-team`) |
| `HARNESS_PUBLIC_URL` | - (OIDC redirect URL, else the request) | Address users reach the server at; OAuth callbacks are built from it |
| `HARNESS_SECRET_KEY` | - (the OIDC session key) | **secret.** Encrypts the connector tokens kept in the store; without a key, connectors are unavailable |
| `HARNESS_ALLOW_LOCAL_SANDBOX` | unset | `1` accepts the local sandbox backend in team mode or on a non-loopback address, where `harness-web` otherwise refuses it |

See [Run team mode](/docs/guides/team-mode/) and [Security model](/docs/operations/security/).

<div class="callout warning">

**The local sandbox is not isolation.** `harness-web` refuses `SANDBOX_BACKEND=local` in team mode or on a non-loopback address with `SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk`.

</div>

`harness-web -local` sets `SANDBOX_BACKEND=local` and `AGENT_PROFILE=coding`, and `-team` sets `MEMORY_IN_CODING=true` and `HARNESS_MEMORY_EXTRACT=true`, each only when the variable is not already set.

## OIDC_*

Read by `harness-web` in team mode ([OIDC](/docs/integrations/oidc/)). `OIDC_ISSUER` without `-team` fails with `OIDC_ISSUER needs team mode (-team)`; without the next two: `OIDC_ISSUER needs OIDC_CLIENT_ID and OIDC_REDIRECT_URL (https://host/auth/callback)`.

| Variable | Default | Meaning |
|---|---|---|
| `OIDC_ISSUER` | - | Issuer URL; its presence turns on team mode and OIDC login |
| `OIDC_CLIENT_ID` | - | Client id (required with an issuer) |
| `OIDC_CLIENT_SECRET` | - | **secret.** Client secret |
| `OIDC_REDIRECT_URL` | - | Callback, `https://host/auth/callback` (required with an issuer) |
| `OIDC_SCOPES` | `profile,email` | Comma-separated scopes besides `openid` |
| `OIDC_LOGIN_CLAIM` | - (`preferred_username`, then `email`, then `sub`) | Claim used as the user's login; pick one that never changes for a person |
| `OIDC_ALLOWED_DOMAINS` | - | Comma-separated email domains allowed to log in |
| `OIDC_SESSION_KEY` | random at start | **secret.** Signs the session cookies (at least 16 bytes); without it sessions end at restart |

## GITHUB_* and TELEGRAM_*

Read by `harness-web` in team mode ([GitHub and Telegram](/docs/integrations/github-telegram/)).

| Variable | Default | Meaning |
|---|---|---|
| `GITHUB_APP_ID` | - | GitHub App id; its presence turns the App on |
| `GITHUB_APP_SLUG` | - | App slug |
| `GITHUB_APP_PRIVATE_KEY` | - | **secret.** PEM private key |
| `GITHUB_APP_PRIVATE_KEY_FILE` | - | File with the PEM key; wins over `GITHUB_APP_PRIVATE_KEY`. One of the two is required: `GITHUB_APP_ID needs GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE` |
| `GITHUB_APP_CLIENT_ID` | - | OAuth client id of the App |
| `GITHUB_APP_CLIENT_SECRET` | - | **secret.** OAuth client secret |
| `TELEGRAM_BOT_TOKEN` | - | **secret.** Bot token; used in team mode only |
| `TELEGRAM_BOT_USERNAME` | - | Bot username, a leading `@` is stripped |

## SENTRY_*, UMAMI_* and GLITCHTIP_API_DSN

Read by `harness-web` only. `SENTRY_DSN` reports every error the process logs (failed runs, agent panics, storage failures, HTTP 500s) to a Sentry-protocol tracker such as Rustrak, Sentry or GlitchTip, without request bodies, headers or user identity. See [Observability](/docs/operations/observability/).

| Variable | Default | Meaning |
|---|---|---|
| `SENTRY_DSN` | - | **secret.** Server-side error tracker |
| `SENTRY_ENVIRONMENT` | `production` under `HARNESS_PRODUCTION`, else `development` | Environment tag on every event, also sent to the browser |
| `SENTRY_RELEASE` | - | Release tag on every event |
| `SENTRY_BROWSER_DSN` | - | **secret.** DSN for errors raised in the browser UI; use a project of its own. The UI reads it from `GET /api/client-config` |
| `UMAMI_URL` | - | Origin of a self-hosted Umami, `https://umami.example.com` |
| `UMAMI_WEBSITE_ID` | - | Umami website id; both Umami variables are needed |
| `GLITCHTIP_API_DSN` | - | **secret.** Parsed into `Config` (`GlitchtipDSN`); no command of this repository reads it |

A malformed value stops `harness-web` at startup: `client error tracking: "<redacted>" is not a DSN (want https://<key>@<host>/<project>)`, `client analytics: the Umami URL and the website id go together`, `client analytics: "x" is not an origin (want https://umami.example.com)`. With a browser setting the UI's Content-Security-Policy widens to exactly those origins.

## PORT

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `8000` | Parsed into `Config.Port` (integer). `harness-web` listens on `HARNESS_WEB_ADDR`, not on `PORT`, so setting it changes nothing there |

## Also read

Standard variables read by a command, not harness settings.

| Variable | Used by | Meaning |
|---|---|---|
| `XDG_STATE_HOME` | session store | With `HARNESS_NO_USER_DIR`, sessions go to `$XDG_STATE_HOME/harness`, else `~/.local/state/harness` |
| `HOME` | `harness-web -team` | Fallback `$HOME/.har/sessions` for the team file when `LOCAL_STATE_DIR` is unset |
| `VISUAL`, `EDITOR` | TUI | Editor for composing a message and for exporting a conversation (`code --wait` works) |
| `TERM_PROGRAM` | TUI | Selects the desktop-notification escape sequence |
| `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`, `TERM`, `TMPDIR`, `SHELL` | stdio MCP servers | The only variables a stdio server inherits, plus its own `env` |

## Tests and benchmarks

These are read by test and benchmark code only:

| Variable | Meaning |
|---|---|
| `HARNESS_TEST_DATABASE_URL` | Postgres used by the `adapters/store/pgstore` tests (skipped when unset) |
| `HARNESS_TEST_S3_ENDPOINT`, `HARNESS_TEST_S3_ACCESS_KEY`, `HARNESS_TEST_S3_SECRET_KEY` | S3-compatible endpoint for the artifact blob tests (skipped when unset) |
| `BENCH_TASKS`, `BENCH_TIER`, `BENCH_OUT`, `BENCH_MIN_PASS`, `BENCH_NO_CACHE_KEY` | The `bench` suite |
| `HARNESS_PROMPT_DUMP`, `PROMPTSIZE_PROFILE`, `LLMTEST_CHAT_ONLY`, `HARNESS_EVAL_EMBEDDING_URL`, `HARNESS_EVAL_EMBEDDING_KEY`, `HARNESS_EVAL_EMBEDDING_MODEL`, `HARNESS_FAKE_MCP`, `FAKE_MCP_FLAG` | Other test helpers |

<div class="callout note">

**Internal `HARNESS_*` names are not settings.** `HARNESS_N`, `HARNESS_BUDGET`, `HARNESS_PATH`, `HARNESS_PATTERN`, `HARNESS_JOB_DIR`, `HARNESS_STAGING_DIR` and similar are parameters the harness passes to the small scripts it runs inside a sandbox. Setting them in the process environment has no effect.

</div>
