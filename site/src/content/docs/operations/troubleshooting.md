---
title: "Troubleshooting"
description: "Real error messages of the harness commands and library, each with its cause and fix. Search for the text you see."
---

Every heading below is a message the harness prints or returns, copied from the code at v0.3.1. Messages marked as logs are warnings the process continues past; the others stop the command or the request. Paths and names in your output will differ.

<div class="callout tip">

**Start with `harness -config-dump`.** It prints the effective configuration, where each value came from, which files were read, whether the project is trusted and which MCP servers did not start and why. Secrets are masked. See [Observability](/docs/operations/observability/#harness--config-dump).

</div>

## Start-up and web server

### harness-web: refusing to listen on 0.0.0.0:8080 without HARNESS_WEB_TOKEN

Full text: `refusing to listen on 0.0.0.0:8080 without HARNESS_WEB_TOKEN: set a token, configure OIDC (OIDC_ISSUER), or bind a loopback address`.

- **Cause:** `-addr` (or `HARNESS_WEB_ADDR`) is not a loopback address and the server has neither a bearer token nor OIDC.
- **Fix:** set `HARNESS_WEB_TOKEN`, configure OIDC for team mode, or bind `127.0.0.1`. See [Run the web product](/docs/quickstart/web/).

### SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation

Full text: `SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk`.

- **Cause:** `harness-web` with the local backend in team mode, or listening beyond loopback. The local backend has no isolation.
- **Fix:** use `SANDBOX_BACKEND=opensandbox` with `OPENSANDBOX_URL`. If the "local machine" really is a throwaway container, set `HARNESS_ALLOW_LOCAL_SANDBOX=1` and accept the risk. See [Security model](/docs/operations/security/#local-versus-remote-sandbox).

### unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: local, none

Full text: `unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: local, none; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it`.

- **Cause:** since v0.3.0 the OpenSandbox backend registers itself only when its package is imported. Your program does not import it. (`harness`, `harness-web` and `harness-worker` do.)
- **Fix:** add `import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"`. For any other name, the message lists what is registered: check the spelling of `SANDBOX_BACKEND`, or register your own backend. See [Upgrade notes](/docs/operations/upgrade-notes/#opensandbox-blank-import-the-adapter).

### SANDBOX_BACKEND=opensandbox needs OPENSANDBOX_URL

- **Cause:** the backend is selected but no server address is set.
- **Fix:** set `OPENSANDBOX_URL` (or `sandbox.url` in the user `config.yml`). Setting only `OPENSANDBOX_URL` also selects the backend.

### harness: MCP servers are configured but MCP support is not linked

Full text: `harness: feature "mcp": harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"`.

- **Cause:** `harness.New` in your program found MCP servers in `MCP_SERVERS`, `.mcp.json` or a `config.yml`, but `mcpfeature` is not linked. A project server waiting for approval counts.
- **Fix:** add `import _ "github.com/7ar-chat/harness/mcpfeature"`, or remove the servers. See [Connect MCP servers](/docs/guides/connect-mcp/).

### SANDBOX_EGRESS_ALLOWLIST is empty in production

Full text: `SANDBOX_EGRESS_ALLOWLIST is empty in production: set it, or set SANDBOX_EGRESS_UNRESTRICTED=true to accept unrestricted sandbox egress`.

- **Cause:** `HARNESS_PRODUCTION` is set, the backend is OpenSandbox and the allowlist is empty.
- **Fix:** list the hosts a sandbox may reach (`SANDBOX_EGRESS_ALLOWLIST=github.com,pypi.org,...`), or accept unrestricted egress explicitly. See [Production checklist](/docs/operations/production/#what-harness_production-does).

### SANDBOX_EGRESS_ALLOWLIST: "https://pypi.org" is not a host name, IP address or CIDR

- **Cause:** an entry has a scheme, port or path. `SANDBOX_EGRESS_DENY_CIDRS` has the matching `is not a valid CIDR or IP address`.
- **Fix:** write bare host names: `pypi.org,files.pythonhosted.org`.

### RUNTIME_SHUTDOWN_GRACE_SECONDS (60) + 12 s must fit RUNTIME_TERMINATION_GRACE_SECONDS (30)

- **Cause:** the drain time plus 12 s of interrupt budget exceeds the termination grace (`RUNTIME_TERMINATION_GRACE_SECONDS`, default 30).
- **Fix:** raise `RUNTIME_TERMINATION_GRACE_SECONDS` to match the pod or container stop grace, or lower the shutdown grace.

### model "x" is not allowed (LLM_MODELS)

- **Cause:** `-model` (or the default) is not in the allow-list `LLM_MODELS`. `LLM_DISCOVER_MODELS` widens the list from the provider, if it is on.
- **Fix:** add the model to `LLM_MODELS` or pick one from the list. A request that names such a model gets `invalid request: model "x" is not allowed` (HTTP 400).

### LLM_PROVIDER must be openai, anthropic or the name of a provider defined under providers:

- **Cause:** `LLM_PROVIDER` or `-provider` names a provider that no `config.yml` defines.
- **Fix:** define it under `providers:` in `~/.har/config.yml`, or use `openai` or `anthropic`. See [LLM providers](/docs/integrations/llm-providers/).

### no API key for provider openai: run `harness auth login openai`, or export LLM_API_KEY

Full text: `no API key for provider openai: run `harness auth login openai`, or export LLM_API_KEY, or export OPENAI_API_KEY`.

- **Cause:** the CLI found no key for the selected provider. Endpoints on `localhost` need none.
- **Fix:** run `harness auth login <provider>` (stores the key in `~/.har/auth.json`, mode 0600) or export the variable named in the message. The order of lookup: `LLM_API_KEY`, the provider's `api_key_env`, `auth.json`, then `OPENAI_API_KEY` or `ANTHROPIC_API_KEY` (not for a named provider with its own `base_url`).

### harness: no model: pass -model or set LLM_DEFAULT_MODEL

- **Cause:** the CLI has no model to start with. (Through `-remote` the message is `no model: pass -model (the server has no default)`.)
- **Fix:** pass `-model` or set `default_model` / `LLM_DEFAULT_MODEL`.

### (no sandbox: use -local, SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)

- **Cause:** a notice from `harness-web`, not an error: no sandbox backend is configured, so the agent answers without tools.
- **Fix:** run with `-local`, or configure a backend. For a run that needs one: `this run requires a sandbox but the harness has none: set Options.Backend or SANDBOX_BACKEND`.

### OIDC_ISSUER needs OIDC_CLIENT_ID and OIDC_REDIRECT_URL (https://host/auth/callback)

- **Cause:** `OIDC_ISSUER` is set without the client id or the redirect URL.
- **Fix:** set both. The redirect URL must be exactly the one registered at the identity provider. See [OIDC](/docs/integrations/oidc/).

### OIDC_ISSUER needs team mode (-team)

- **Cause:** `-team=false` (or no `HARNESS_TEAM=1`) with an issuer set. Team mode is on by default when `OIDC_ISSUER` is set, so this appears only if you turned it off.
- **Fix:** run with `-team`.

### httpserver: OIDC discovery: Get "...openid-configuration": dial tcp ...: connection refused

- **Cause:** the server could not fetch `<issuer>/.well-known/openid-configuration` at start. Wrong issuer URL, or the provider is not reachable from the container.
- **Fix:** check `OIDC_ISSUER` from inside the container network. The issuer must match what the browser is redirected to.

### httpserver: no OIDC session key; sessions end when the server restarts

- **Cause:** log (warning). `OIDC_SESSION_KEY` is empty or shorter than 16 bytes.
- **Fix:** set `OIDC_SESSION_KEY` to a stable random value (`openssl rand -hex 32`).

### -dev-users needs -team, no OIDC_ISSUER, and a loopback address

- **Cause:** `-dev-users` (act as the `X-Harness-User` header) is only for development.
- **Fix:** drop it, or run `harness-web -team -dev-users -local` on `127.0.0.1` without OIDC.

### client error tracking: "<invalid>" is not a DSN (want https://<key>@<host>/<project>)

- **Cause:** `SENTRY_BROWSER_DSN` is malformed. Related: `errtrack: [Sentry] DsnParseError: invalid scheme` for `SENTRY_DSN`; `client analytics: the Umami URL and the website id go together` when only one of `UMAMI_URL` / `UMAMI_WEBSITE_ID` is set.
- **Fix:** copy the DSN from the tracker; set both Umami variables or neither. `UMAMI_URL` is an origin without a path.

## Configuration files and trust

### /home/me/.har/auth.json is readable by other users (mode 0644): refusing to use it

Full text: `<file> is readable by other users (mode 0644): refusing to use it; run: chmod 600 <file>`.

- **Cause:** `auth.json` (or `trust.json`) can be read by the group or others. The harness refuses to use a key file that is.
- **Fix:** `chmod 600` the file as the message says. `harness auth login` always writes mode 0600.

### auth.json: not valid JSON (expected {"providers": {"<name>": {"api_key": "..."}}})

- **Cause:** a hand-edited `auth.json` with a syntax error or a wrong type. The error says "syntax error at byte N" or "wrong type for X" and never echoes the content.
- **Fix:** repair it, or remove the entry and run `harness auth login` again.

### config.yml: unknown key "sandbox.bakend"

Full text: `/home/me/.har/config.yml: unknown key "sandbox.bakend"`.

- **Cause:** a key that is not in the schema (usually a typo). Unknown keys are errors, not ignored. A wrong value type gives `limits.max_steps: expected an integer, got "many"`.
- **Fix:** correct the key. See [config.yml keys](/docs/reference/config-yml/). `env.FOO is not a setting of the harness` is the same check for the `env:` section.

### api_key: secrets do not belong in a configuration file

Full text: `api_key: secrets do not belong in a configuration file; set api_key_env to the NAME of the environment variable that holds it`.

- **Cause:** `api_key`, `password`, `secret` or `token` in a `config.yml`. The same message appears for `providers.<name>.api_key`, and for `env:` entries whose name contains `KEY`, `SECRET`, `PASSWORD`, `TOKEN` or `DSN` (`env.LLM_API_KEY: ... export LLM_API_KEY in the environment`).
- **Fix:** put the key in the environment and name it with `api_key_env`, or run `harness auth login <provider>`.

### api_key_env: the environment variable NOPE_NOT_SET is not set

- **Cause:** `api_key_env` names a variable that is empty in this process.
- **Fix:** export it, or fix the name. The `sandbox.api_key_env` key has the same check.

### base_url cannot be set in a project file (a repository must not choose where your requests and API key are sent)

Full text: `<repo>/.har/config.yml: base_url cannot be set in a project file (...): put it in ~/.har/config.yml (or the environment)`.

- **Cause:** the repository's `.har/config.yml` sets a key only you may set: `providers`, `base_url`, `api_key_env`, `models`, `presets`, `timeout_seconds`, `sandbox.*`, `env`, `session_header`, `cache_ttl`. The reason in parentheses depends on the key.
- **Fix:** move the key to your `~/.har/config.yml`. This is deliberate: see [Security model](/docs/operations/security/#project-trust).

### model zzz (project .har/config.yml default_model) is not configured by you

Full text: `model zzz (project .har/config.yml default_model) is not configured by you: add it to models in ~/.har/config.yml`.

- **Cause:** a project selected a model that is not in your list. The provider variant is `provider "evilco" (project .har/config.yml provider) is not configured by you: define it under providers.evilco in ~/.har/config.yml, ...`.
- **Fix:** add the model or provider to your user file, or change the project's selection.

### mcp.allow_stdio cannot be set in a project file (a repository must not enable running its own programs)

- **Cause:** a repository tried to switch on stdio MCP servers for itself.
- **Fix:** set `mcp.allow_stdio: true` in `~/.har/config.yml` or export `HARNESS_MCP_STDIO=1`.

### mcp: stdio server skipped (log) / stdio servers need the local backend and an opt-in

- **Cause:** a stdio MCP server is configured but the backend is not `local` or the opt-in is missing. `-config-dump` lists it under "mcp servers not started".
- **Fix:** use the local backend and `HARNESS_MCP_STDIO=1`, or use an `http` server. See [MCP](/docs/concepts/mcp/).

### the project is not trusted, run `harness trust` to enable

- **Cause:** `-config-dump` shows project MCP servers (`.mcp.json`, `.har/mcp.json`, `mcp.servers`) that do not start because the project is not in `trust.json`. Project `allow` permission rules are ignored for the same reason.
- **Fix:** `harness trust` in the project, or `HARNESS_TRUST_PROJECT=1` for one process.

### a project's stdio server needs your approval of this exact command, run `harness trust mcp fs`

- **Cause:** log (`mcp: project stdio server not approved`). The project is trusted, but a stdio server it defines has not been approved, or its command, arguments or `env` changed since.
- **Fix:** `harness trust mcp <name>` (all pending servers without a name), or approve it when an interactive run asks. Related errors: `<root> is not trusted: run `harness trust` first`, `no project stdio MCP server is waiting for approval (it may already be approved, or not allowed: stdio needs the local backend and mcp.allow_stdio)`, and the tool error `the project's stdio MCP server is not approved: approve it when asked, or run `harness trust mcp <name>``.

### /path is not trusted (see: harness trust list)

- **Cause:** `harness untrust` on a path that is not in `trust.json`.
- **Fix:** run `harness trust list` to see the exact (symlink-resolved) path.

## Workers, Redis and Postgres

### harness-worker: connect to Redis: dial tcp 127.0.0.1:6379: connection refused

- **Cause:** the worker connects to Redis at start (unless `-standalone`). `REDIS_HOST` defaults to `redis`, so an unset variable points at a host that may not exist.
- **Fix:** set `REDIS_HOST` (and `REDIS_PORT`, `REDIS_PASSWORD`, or `USE_SENTINEL` with `REDIS_SENTINEL_HOSTS`). With `PING_DB_AT_STARTUP=false` the check is skipped and the failure shows up later in the queue loop. See [Redis](/docs/integrations/redis/).

### harness-worker: no shared store: set POSTGRES_HOST (and the other POSTGRES_* settings), or pass -standalone

- **Cause:** a worker without `-standalone` needs a store the web process shares.
- **Fix:** set `POSTGRES_HOST`, `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` and `POSTGRES_SSLMODE` (default `require`). Migrations are applied at start. `connect to Postgres: ...` and `migrate Postgres: ...` are the follow-on errors for a wrong address or credentials.

### harness-worker: worker: the run queue is in memory, private to this process

Full text: `worker: the run queue is in memory, private to this process, so no other process can queue work for it (connect Redis, or pass Standalone)`.

- **Cause:** the worker ended up with an in-process queue, which no other process can reach.
- **Fix:** give it Redis, or run `-standalone` for a demo or test.

### readyz answers 503: not ready: queue consumer group not ready

- **Cause:** `GET /readyz` on the worker. The consumer group could not be created yet (Redis down), or `queue loop stuck: no queue read completed for ...`, or `draining: no longer claiming runs` during shutdown.
- **Fix:** check Redis reachability; look for `runtime: cannot create consumer group` in the log. During a rolling stop, 503 is expected.

### worker: no sandbox backend (set OPENSANDBOX_URL or SANDBOX_BACKEND=local): agents have no tools

- **Cause:** log (warning) at worker start.
- **Fix:** set `OPENSANDBOX_URL` and `SANDBOX_EGRESS_ALLOWLIST` on the worker: each worker has its own sandboxes. See [Postgres and Redis for several processes](/docs/guides/postgres-redis/).

## Runs, clients and API

### 401 unauthenticated: Authentication required.

- **Cause:** a missing or wrong `Authorization: Bearer <token>`, or no session cookie in team mode.
- **Fix:** send the token configured as `HARNESS_WEB_TOKEN`; for `harness -remote`, set `HARNESS_REMOTE_TOKEN`.

### 403 forbidden_origin: Origin not allowed.

- **Cause:** a browser on another origin called the API. Same-origin requests need no entry.
- **Fix:** list the origin in `HARNESS_WEB_ORIGINS` (comma-separated). `403 forbidden_host: Host not allowed.` is the DNS-rebinding guard of a server with no token and no OIDC: browse it as `localhost` or `127.0.0.1`.

### 404 not_found: Not found.

- **Cause:** the thread or run does not exist, was deleted, or belongs to another user. The answer is the same on purpose. In team mode, a non-member also gets 404; a viewer who writes gets `403 forbidden: You do not have permission to do this.`
- **Fix:** use the id with the account that created it, or ask for project membership.

### 409 conflict: Conflict.

- **Cause:** the thread already has an active run (one at a time), or an undo or redo has nothing to do. The detailed history messages are `The thread has a run in progress.`, `Nothing to undo.` and `Nothing to redo.`
- **Fix:** wait for the run, or cancel it (`POST /api/runs/{id}/cancel`) first. See [HTTP API](/docs/reference/http-api/).

### stream_lost: the event stream of run ... was lost after 8 reconnection attempts

- **Cause:** the client lost the SSE connection and `hosts/client` gave up after its reconnect budget (default 8 attempts, back-off from 200 ms to 5 s), or the server answered a non-retryable 4xx. The client ends the stream with a synthetic `RUN_ERROR` whose code is `stream_lost`.
- **Fix:** the run itself is unaffected; reopen `GET /api/runs/{id}/events?after=<last seq>`. Raise the budget with `client.WithMaxReconnects(n)`. Check proxies that cut idle streams: the server pings every 15 s. See [The Client contract](/docs/reference/client-contract/).

### run failed: The AI model could not answer. Please try again in a moment.

- **Cause:** the provider call failed (RUN_ERROR code `model_error`). Clients never see the provider's text; the log record `run failed` has the detail (for example `status code: 401, code: invalid_api_key`).
- **Fix:** read the `detail` in the server log, or in `SENTRY_DSN` events; check the key, the base URL and the model name.

### Error: permission denied: ... The action was not run and the permission policy does not let it run

- **Cause:** a tool result (not a failure of the harness): a `deny` rule or the built-in denials stopped a command or file write.
- **Fix:** if it is wrong, change the rule in `.har/permissions.yml` or `~/.har/permissions.yml`. Project `allow` rules only count when the project is trusted. See [Approvals and permissions](/docs/concepts/approvals-permissions/).

### cannot start a sandbox: the user already has 5 active sandboxes in other conversations

- **Cause:** a tool result: `MAX_SANDBOXES_PER_USER` (default 5, one sandbox per thread) is reached.
- **Fix:** delete old conversations (deleting a thread kills its sandbox) or raise the limit.

## Go modules and releases

### go get: invalid version: unknown revision

Example: `go: github.com/7ar-chat/harness@v9.9.9: invalid version: unknown revision v9.9.9`.

- **Cause:** the tag does not exist, or it was pushed seconds ago and the module proxy has not seen it yet. A module in a subdirectory needs its own prefixed tag (`core/v0.1.0`).
- **Fix:** check `git tag` on the remote; if it is new, wait a minute and retry. Never move or re-tag. See [Releasing and versioning](/docs/guides/releasing/).

### undefined: harness.NewRedisClient (also harness.RedisInfra, Options.Redis)

- **Cause:** removed in v0.3.0.
- **Fix:** use `redisinfra.NewClient` and `redisinfra.Infra`, see [Upgrade notes](/docs/operations/upgrade-notes/#redis-optionsredis-becomes-redisinfra).
