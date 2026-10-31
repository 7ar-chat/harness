---
title: "harness-web"
description: "Flags, environment, address and token rules, CORS, CSP and the built-in endpoints of the harness-web server."
---

`harness-web` serves the harness over HTTP: the [JSON and SSE API](/docs/reference/http-api/) of `hosts/httpapi` and its built-in browser UI. Use it on your machine in a repository (`-local`), as a shared server behind a token or a login, or in [team mode](/docs/guides/team-mode/). To run the agents in separate processes, pair it with [harness-worker](/docs/reference/cli-harness-worker/).

```bash
harness-web -local            # coding agent on the current directory, UI at http://127.0.0.1:8080/
```

```bash
HARNESS_WEB_ADDR=0.0.0.0:8080 HARNESS_WEB_TOKEN=secret OPENSANDBOX_URL=http://sandboxes:8090 harness-web
```

On start it prints `harness-web: http://<addr>/  (model <id>)` on stdout. Without a sandbox it adds `(no sandbox: use -local, SANDBOX_BACKEND=local or OPENSANDBOX_URL; the agent has no tools)` on stderr. `SIGINT` shuts the server down (5 s to drain).

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `HARNESS_WEB_ADDR`, else `127.0.0.1:8080` | Listen address. |
| `-model` | `LLM_DEFAULT_MODEL` | Default model id. It must be allowed (`LLM_MODELS`), else `model "<id>" is not allowed (LLM_MODELS)`. |
| `-local` | off | Work on the current directory: sets `SANDBOX_BACKEND=local` and `AGENT_PROFILE=coding` unless already set, and keeps threads, runs and event logs on disk (`harness.OpenLocalStore`, `LOCAL_STATE_DIR`). |
| `-user` | `local` | Login of the single local user. |
| `-team` | on when `OIDC_ISSUER` is set or `HARNESS_TEAM=1` | Team mode: OIDC login, projects, shared threads. See [Run team mode](/docs/guides/team-mode/). It also sets `MEMORY_IN_CODING=true` and `HARNESS_MEMORY_EXTRACT=true` unless already set. |
| `-dev-users` | off | Team mode on a loopback address without OIDC: act as the user named by the `X-Harness-User` header (development only). |

Exit codes: `0` after a clean shutdown or `-h`, `1` for any startup refusal (`harness-web: <reason>` on stderr), `2` for a flag parse error.

## Address and credential rules

| Situation | Result |
|---|---|
| Loopback address (`127.0.0.1`, `::1`, `localhost`), no token, no OIDC | Allowed. Only requests whose `Host` is `localhost`, `127.0.0.1` or `::1` are answered (403 `forbidden_host` otherwise): a guard against DNS rebinding. |
| Non-loopback address, no `HARNESS_WEB_TOKEN`, no OIDC | Refused: `refusing to listen on <addr> without HARNESS_WEB_TOKEN: set a token, configure OIDC (OIDC_ISSUER), or bind a loopback address`. |
| `HARNESS_WEB_TOKEN` set | Every API request must carry `Authorization: Bearer <token>`; all requests act as `-user`. The `Host` guard is off, so the server can sit behind a public name. |
| `OIDC_ISSUER` set and `-team=false` | Refused: `OIDC_ISSUER needs team mode (-team)`. (`OIDC_ISSUER` alone turns `-team` on by default.) |
| `OIDC_ISSUER` without `OIDC_CLIENT_ID` and `OIDC_REDIRECT_URL` | Refused: `OIDC_ISSUER needs OIDC_CLIENT_ID and OIDC_REDIRECT_URL (https://host/auth/callback)`. |
| `-dev-users` without `-team`, or with OIDC, or on a non-loopback address | Refused: `-dev-users needs -team, no OIDC_ISSUER, and a loopback address`. |
| `-team` without OIDC | Allowed, with the notice `team mode without OIDC_ISSUER: everyone is <user>` (local development). |
| `SANDBOX_BACKEND=local` in team mode or on a non-loopback address | Refused, unless `HARNESS_ALLOW_LOCAL_SANDBOX=1`. |

<div class="callout warning">

**The local sandbox is not isolation.** `SANDBOX_BACKEND=local` runs the agent's commands on the machine with your rights. `harness-web` refuses it wherever another person or the network can reach the agent; `HARNESS_ALLOW_LOCAL_SANDBOX=1` accepts the risk. Use [OpenSandbox](/docs/integrations/sandbox-backends/) for a shared server and read the [security model](/docs/operations/security/).

</div>

## Environment

`harness-web` also reads every `LLM_*`, `SANDBOX_*`, `POSTGRES_*`, `REDIS_*` and other setting listed in [Environment variables](/docs/reference/env-vars/). The ones it reads itself:

| Variable | Meaning |
|---|---|
| `HARNESS_WEB_ADDR` | Default of `-addr`. |
| `HARNESS_WEB_TOKEN` | Static bearer token for the API. |
| `HARNESS_WEB_ORIGINS` | Comma-separated CORS allowlist of browser origins (`https://app.example.com`); `*` allows any. |
| `HARNESS_TEAM` | `1` turns team mode on (like `-team`). |
| `HARNESS_PUBLIC_URL` | Public address users reach the server at; OAuth callbacks of the connectors are built from it. |
| `HARNESS_SECRET_KEY` | Key that encrypts connector tokens; else the OIDC session key. Without either, connectors are unavailable. |
| `HARNESS_ALLOW_LOCAL_SANDBOX` | `1` accepts the local sandbox on a shared server. |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL`, `OIDC_SCOPES`, `OIDC_LOGIN_CLAIM`, `OIDC_ALLOWED_DOMAINS`, `OIDC_SESSION_KEY` | Team-mode login. See [OIDC](/docs/integrations/oidc/). |
| `GITHUB_APP_ID`, `GITHUB_APP_SLUG`, `GITHUB_APP_PRIVATE_KEY` or `GITHUB_APP_PRIVATE_KEY_FILE` | GitHub App of team mode. |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME` | Telegram bridge (team mode only). See [GitHub and Telegram](/docs/integrations/github-telegram/). |
| `SENTRY_DSN`, `SENTRY_ENVIRONMENT`, `SENTRY_RELEASE`, `HARNESS_PRODUCTION` | Server error tracking. The environment tag is `SENTRY_ENVIRONMENT`, else `production` when `HARNESS_PRODUCTION` is set, else `development`. |
| `SENTRY_BROWSER_DSN`, `UMAMI_URL`, `UMAMI_WEBSITE_ID` | Browser error reporting and usage statistics, handed to the UI by `/api/client-config`. |
| `HARNESS_MODEL_PRICES` | Source of the model price table; `off` disables it. |

In team mode the main store is Postgres when `POSTGRES_HOST` is set (the team tables live beside the threads); otherwise the durable local store, with the team data in `team.json` next to it. With `REDIS_HOST` set, the live feed is shared between replicas.

## The API prefix

Every API route is served under `/api` and, for compatibility, also without the prefix. The browser UI is served at `/` and `/ui/`. The full route list is in [HTTP API](/docs/reference/http-api/).

## Built-in endpoints

| Endpoint | Auth | Answer |
|---|---|---|
| `GET /healthz` | none | `{"status":"ok"}`. Use it for liveness. |
| `GET /api/client-config` (also `/client-config`) | none | What the browser UI needs for its own error reporting and statistics. `{}` when none is configured. |

`/api/client-config` is public because the UI reads it before anyone has logged in. It returns `{"sentry": {"dsn", "environment", "release"}, "umami": {"scriptUrl", "websiteId"}}`, each object present only when configured (a DSN is a write-only key). A malformed `SENTRY_BROWSER_DSN` (not `https://<key>@<host>/<project>`), or only one of `UMAMI_URL` and `UMAMI_WEBSITE_ID`, stops the server at startup.

## CORS and Host checks

| Request | Behaviour |
|---|---|
| Same origin | Allowed. |
| `Origin` in `HARNESS_WEB_ORIGINS` (or `*`) | Allowed; the answer carries `Access-Control-Allow-Origin`, `Access-Control-Expose-Headers: Content-Disposition` and `Vary: Origin`. A preflight `OPTIONS` gets 204 with the methods `GET, POST, PATCH, DELETE, OPTIONS` and the headers `Authorization, Content-Type, Last-Event-ID, X-Harness-User`. |
| Any other cross-origin request | 403 `forbidden_origin` (`Origin not allowed.`). |
| `Host` not allowed (token-less loopback server) | 403 `forbidden_host` (`Host not allowed.`). |

## Content-Security-Policy

Every response carries a strict policy, plus `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer` and `X-Frame-Options: DENY`. Without Sentry or Umami configured it is:

```text
default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data: https:; connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'
```

When `UMAMI_URL` is set its origin is added to `script-src` and `connect-src`; the Sentry host is added to `connect-src`. Nothing else is opened. File downloads add `Content-Security-Policy: sandbox`, `Cache-Control: no-store` and `Content-Disposition: attachment`.

<div class="callout note">

**The browser UI is a build artifact.** It is embedded from `hosts/httpapi/ui`. A checkout that has not run `make web` serves a notice at `/` ("The web UI is not built into this binary") while the API works.

</div>

To attach the terminal UI to a running server use `harness -remote URL` ([harness](/docs/reference/cli-harness/)); to mount the API inside your own program see [HTTP API](/docs/reference/http-api/#mount-it-in-your-program).
