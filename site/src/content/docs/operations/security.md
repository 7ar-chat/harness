---
title: "Security model"
description: "Who is trusted with what: user, project and sandbox boundaries, the controls the harness enforces, and the gaps it does not close."
---

The harness runs model-written commands and reads files that strangers may have written. This page describes the boundaries it draws, the check that enforces each one, and, last, what it does **not** protect you from. Every control below was checked against the code at v0.3.1.

## Trust boundaries

<svg class="diagram" viewBox="0 0 760 230" role="img" aria-label="Three trust levels: the user owns keys and endpoints, a project may only select and tighten, the sandbox runs untrusted code">
<rect class="lane" x="10" y="10" width="240" height="210"/>
<rect class="lane" x="260" y="10" width="240" height="210"/>
<rect class="lane" x="510" y="10" width="240" height="210"/>
<text class="b" x="25" y="36">User (trusted)</text>
<text class="b" x="275" y="36">Project (data)</text>
<text class="b" x="525" y="36">Sandbox (untrusted)</text>
<rect class="box" x="25" y="52" width="210" height="34"/><text class="s m" x="130" y="74">~/.har/config.yml, env, flags</text>
<rect class="box" x="25" y="94" width="210" height="34"/><text class="s m" x="130" y="116">auth.json (0600), trust.json (0600)</text>
<rect class="box" x="25" y="136" width="210" height="34"/><text class="s m" x="130" y="158">endpoints, keys, model catalog</text>
<rect class="box soft" x="275" y="52" width="210" height="34"/><text class="s m" x="380" y="74">.har/config.yml: select, tune</text>
<rect class="box soft" x="275" y="94" width="210" height="34"/><text class="s m" x="380" y="116">AGENTS.md, skills, .mcp.json</text>
<rect class="box soft" x="275" y="136" width="210" height="34"/><text class="s m" x="380" y="158">allow rules, MCP: only if trusted</text>
<rect class="box hot" x="525" y="52" width="210" height="34"/><text class="s m" x="630" y="74">commands the model writes</text>
<rect class="box hot" x="525" y="94" width="210" height="34"/><text class="s m" x="630" y="116">files the agent reads</text>
<rect class="box hot" x="525" y="136" width="210" height="34"/><text class="s m" x="630" y="158">egress allowlist, no keys inside</text>
<path class="arrow" d="M250 115 L258 115"/><path class="head" d="M258 111 L266 115 L258 119 Z"/>
<path class="arrow" d="M500 115 L508 115"/><path class="head" d="M508 111 L516 115 L508 119 Z"/>
</svg>

| Boundary | Who is on each side | What enforces it |
|---|---|---|
| user / project | You own endpoints, API keys, the model catalog and the sandbox section. A repository's `.har/config.yml` may only select among what you configured and tune behaviour. | `config` rejects forbidden keys in a project file; `harness trust` gates permission `allow` rules and MCP servers. |
| project / model | Repository text (`AGENTS.md`, skills, tool output) is data the model reads, not instructions of the harness. | Fencing, the untrusted-context flag, approvals. |
| model / host | Commands and file writes the model asks for. | The permission layer and the sandbox backend. |
| tenant / tenant | Users of one server. | Ownership checks that answer `404`; one sandbox per thread. |

## Local versus remote sandbox

The local backend (`SANDBOX_BACKEND=local`) runs commands with `bash` on your machine, with your rights. Its source says it plainly: "There is no isolation beyond path confinement of the file API". The permission layer is the only barrier in front of it.

A remote backend (OpenSandbox) puts each thread in its own container, with an egress policy and no model keys inside. See [Sandboxes](/docs/concepts/sandboxes/) and [Sandbox backends](/docs/integrations/sandbox-backends/).

<div class="callout warning">

**The local sandbox is not isolation.** `harness-web` refuses it in team mode and on any non-loopback address:

```text
harness-web: SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk
```

`docker-compose.yml` (the local demo) sets `HARNESS_ALLOW_LOCAL_SANDBOX=1` because the "local" machine is the container. Do not copy that line into a shared deployment.

</div>

## Permission layer

In front of `execute_command`, `write_file` and `edit_file`, the permission layer decides **allow**, **ask** or **deny** for each call. It is installed when the local backend is selected, or when the coding profile runs on a remote sandbox. Shell lines are split into their simple commands and the most severe verdict wins.

- Default mode on the local backend is `ask`. On a remote sandbox with the coding profile the default is `yolo`: nothing is asked, but `deny` rules still apply (`features_core.go`).
- `AGENT_PERMISSIONS` selects `ask`, `auto-edit`, `yolo`, `yolo-unsafe` (nothing is checked) or `readonly`.
- A denied call never runs; the model gets `Error: permission denied: <reason>. The action was not run ...`.
- A project's `.har/permissions.yml` can only **tighten**: its `ask` and `deny` rules and a mode as strict as yours are honoured; `allow` rules and a looser mode are ignored unless the project is trusted. Ignored items are listed by `harness -config-dump`.
- Built-in denials include `rm -rf /`, fork bombs, `git push --force` to main, and writes to `.git/`, `.env`, `id_rsa` and `*.pem`.

Details and the rule syntax: [Approvals and permissions](/docs/concepts/approvals-permissions/).

## Project trust

A repository you clone can bring a `.har/` directory. Nothing in it can redirect your requests or your key:

| A project file may | Only you may (user file, environment, flags) |
|---|---|
| select `provider`, `default_model`, `agents`, `profile`; tune `limits`, `skills`, `memory` | `providers`, `base_url`, `api_key_env`, `models`, `presets`, `timeout_seconds`, the whole `sandbox:` section, `env:`, `mcp.allow_stdio` |

A forbidden key is an error that names the file, the key and where it belongs:

```text
/path/proj/.har/config.yml: base_url cannot be set in a project file (a repository must not choose where your requests and API key are sent): put it in ~/.har/config.yml (or the environment)
```

`harness trust` adds the current project root to `~/.har/trust.json`. Trust unlocks two things: the project's permission `allow` rules and its MCP servers. It does not unlock endpoints or keys, which stay yours. `HARNESS_TRUST_PROJECT=1` trusts the current project for one process (CI); `harness untrust` withdraws it. `HARNESS_NO_USER_DIR=1` ignores `trust.json`.

```bash
harness trust            # trust the project of the current directory
harness trust list       # print trust.json and the trusted roots
harness untrust          # stop trusting
```

## MCP stdio rules

An MCP server of type `stdio` is a program to run, so it has the strictest rules (see [MCP](/docs/concepts/mcp/)):

1. It starts only with the local backend **and** an opt-in from you: `mcp.allow_stdio: true` in `~/.har/config.yml`, or `HARNESS_MCP_STDIO=1`. A project `config.yml` that sets `mcp.allow_stdio` is an error. Otherwise the server is skipped with a warning and listed by `-config-dump`.
2. A stdio server defined by a **project** also needs your approval of its exact command: `harness trust mcp <name>`. The approval is bound to a SHA-256 of the command, the arguments and the `env`; changing any of them withdraws it.
3. Its tools go through the permission layer as **ask on first use**.
4. The child process gets a reduced environment: `PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_ALL`, `TERM`, `TMPDIR`, `SHELL`, plus the server's own `env`. Your API keys and database passwords are not inherited.

Remote (`http`/`sse`) project servers need only `harness trust`. Your own `~/.har/mcp.json` and `MCP_SERVERS` are always trusted.

## Secrets

- **No keys in configuration files.** `api_key`, `password`, `secret` and `token` in any `config.yml` are errors, as is any `env:` entry whose name contains `KEY`, `SECRET`, `PASSWORD`, `TOKEN` or `DSN`. Use `api_key_env` (the *name* of a variable) instead.
- **`auth.json` is the one file for keys** (`harness auth login <provider>`). It must be mode `0600`; a file readable by the group or others is refused, and the error never contains a key. `trust.json` has the same rule.
- **`harness -config-dump`** masks secrets and shows where each value came from.
- **Model keys never enter a sandbox.** The sandbox adapter guide states that the harness does not put them in the sandbox environment.
- **Connector tokens** (team mode) are stored encrypted with AES-256-GCM, with a key derived from `HARNESS_SECRET_KEY`, else the OIDC session key.
- **Memory** content, tags and queries are redacted for secret patterns before storage.

## Web host: CSP, CORS, authentication

`harness-web` listens on `127.0.0.1:8080` by default and refuses to start on any other address without a credential:

```text
harness-web: refusing to listen on 0.0.0.0:8080 without HARNESS_WEB_TOKEN: set a token, configure OIDC (OIDC_ISSUER), or bind a loopback address
```

Every response carries `Content-Security-Policy`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `X-Frame-Options: DENY`. The policy for the bundled UI is:

```text
default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
```

`script-src` and `connect-src` gain exactly the origins of `UMAMI_URL`, and `connect-src` that of `SENTRY_BROWSER_DSN`, and nothing else. File downloads are served as `attachment` with `Content-Security-Policy: sandbox` and `Cache-Control: no-store`, and the file name is sanitised.

| Control | Behaviour |
|---|---|
| Bearer token | With `HARNESS_WEB_TOKEN`, every API request needs `Authorization: Bearer <token>` (constant-time comparison). All requests then act as the single `-user`. `/healthz`, `/api/client-config` and the UI shell need no token. |
| Cross-origin | A request with an `Origin` that is not the request's own host is refused with `403 forbidden_origin` unless the origin is in `HARNESS_WEB_ORIGINS` (comma-separated; `*` allows any). |
| Host guard | A server with **neither** a token nor OIDC only answers `Host: localhost`, `127.0.0.1` or `::1` (DNS-rebinding guard). Behind a token or login the guard is off. |
| OIDC | Authorization-code flow with PKCE and a `state` check, run on the server. The browser holds only a signed `HttpOnly`, `SameSite=Lax` cookie (`Secure` when the redirect URL is `https`); sessions last 12 h. Only a **verified** email claims project invites. See [OIDC](/docs/integrations/oidc/). |
| Dev users | `-dev-users` (act as the `X-Harness-User` header) needs `-team`, no `OIDC_ISSUER`, and a loopback address, or the server will not start. |

<div class="callout note">

**Set `OIDC_SESSION_KEY`** (at least 16 bytes). Without it the server generates a random key at start and logs a warning, so every restart ends all sessions.

</div>

## Tenant isolation

Threads and runs belong to a user. A request for a thread or run that belongs to someone else gets `404 not_found`, the same answer as for a missing id, so ids are not an existence oracle. `Send` and `Resume` check ownership even while a run is active on the thread (pinned by `TestSendAndResumeRefuseAThreadOfAnotherUser`). In team mode access is by project role: a non-member gets `404`, a role that is too low gets `403`. A project thread stays stored under its creator's login; members act on it through the web layer.

Each thread has its own sandbox and `MAX_SANDBOXES_PER_USER` (default 5) bounds how many one user can hold.

## Untrusted repository text

- `AGENTS.md`, `CLAUDE.md` and `.har/instructions.md` are rendered inside `<project_instructions source="project">` blocks with closing tags escaped, and the run's context is marked **untrusted**.
- A user-supplied skill is wrapped in `<skill source="user">` with its tags escaped, and marks the run untrusted when loaded explicitly.
- Compaction summaries, plans and recalled memories are wrapped by `FenceData`, which escapes any opening or closing form of the tag (any case, whitespace around `/`) so the text cannot close the block.
- Once the context holds tool output, an attachment or a summary, memory tools write **proposals** (`pending`) instead of active entries, so injected text cannot silently plant a lasting memory.

Fencing lowers the chance that the model obeys repository text; it is not a guarantee. The permission layer and the sandbox are what limit the damage if it does.

## Credential proxy and egress

With `OPENSANDBOX_CREDENTIAL_PROXY=true` (OpenSandbox server >= 0.2.0, egress sidecar >= 1.1.1 in `dns+nft` mode), a project's GitHub installation token is handed to the sidecar's Credential Vault, bound to that repository's paths on `github.com` and `api.github.com`. It never enters the sandbox; `git clone`, `git push` and `curl https://api.github.com/...` just work. If the broker refuses the token the checkout does **not** fall back to a file. Without a broker, the token is written to `/workspace/.work/git-credentials` where anything in the sandbox can read it.

Egress is deny-by-default with `SANDBOX_EGRESS_ALLOWLIST`: only listed hosts resolve and connect. With no list, the default deny ranges (`SANDBOX_EGRESS_DENY_CIDRS`: RFC 1918, link-local, CGNAT, `0.0.0.0/8`, IPv6 ULA and link-local) keep a sandbox away from other sandboxes, the host and the metadata service. Under `HARNESS_PRODUCTION`, an empty list is refused unless you set `SANDBOX_EGRESS_UNRESTRICTED=true`. See [Production checklist](/docs/operations/production/).

## Known gaps

<div class="callout warning">

**Read these before you expose the harness to people you do not trust.**

</div>

- **Local sandbox: no isolation.** Commands run with your rights. Glob rules match command *text* (`rm -rf*` does not see `rm -r -f`; the built-in checks do), and the path checks are lexical: symlinks out of the workspace are not followed.
- **`yolo-unsafe` checks nothing**, and `yolo` prompts for nothing.
- **A bearer token is one shared identity.** `HARNESS_WEB_TOKEN` maps every caller to a single user; use OIDC for people.
- **`GET /api/client-config` is public**: it returns the browser Sentry DSN and the Umami website id. A DSN is a write-only key, but anyone can read it.
- **No request rate limiting.** The harness enforces one active run per thread (`409`) and sandbox quotas, but limits on request rates belong in your reverse proxy. `MAX_STREAMS_PER_USER` is parsed and validated by `config`, but nothing else in the code reads it at v0.3.1, so do not rely on it as a limit.
- **`harness-web` does not drain on SIGTERM.** It only handles `SIGINT`; `SIGTERM` ends the process at once (observed exit status 143). The worker does handle both. See [Production checklist](/docs/operations/production/#7-graceful-shutdown).
- **Credential Vault** decrypts HTTPS for the bound hosts with the sidecar's own CA, which the sandbox image trusts.
- **The sandbox image** has been built and smoke-tested but, per the README, not yet run under a live OpenSandbox lifecycle server.
- **No tamper-proof audit trail.** The team audit log is an ordinary record in the team store, written by the server; see [Observability](/docs/operations/observability/#audit-log-and-live-feed).
