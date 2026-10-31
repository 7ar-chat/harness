---
title: "Run the web product"
description: "Start harness-web on your repository, share it behind a token, run the team stack with Docker Compose, or read what the production compose file deploys."
---

`harness-web` (source: `app/cmd/harness-web`) serves the harness over HTTP: the JSON and Server-Sent Events API plus a browser UI embedded in the binary. Run it on your own repository with `-local`, put a token in front of it to share it, or run team mode (OIDC login, projects, shared sessions) with Docker Compose. It is also what `harness -remote` attaches to ([Run the CLI and TUI](/docs/quickstart/cli/)).

## On your machine

From a checkout, build once (`make web` builds the browser UI first; see [Install](/docs/quickstart/install/)), then in the repository you want the agent to work on:

```bash
make web && make build        # in the harness checkout
cd ~/my-project
/path/to/harness/bin/harness-web -local
```

```text
harness-web: http://127.0.0.1:8080/  (model gpt-5.6-terra)
```

The model shown is `-model` or `LLM_DEFAULT_MODEL`; configure the provider like the CLI does ([config.yml](/docs/reference/config-yml/) or `LLM_*` variables, key from `harness auth login` or `LLM_API_KEY`). Open the URL in a browser.

`-local` sets `SANDBOX_BACKEND=local` and `AGENT_PROFILE=coding` unless they are already set, and keeps sessions on disk (`LOCAL_STATE_DIR`, default `~/.har/sessions`). The agent's commands then run on your machine with your rights.

<div class="callout warning">

**`-local` is for loopback.** The local sandbox has no isolation. The server refuses it in team mode or on a non-loopback address: `SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk`. Setting `HARNESS_ALLOW_LOCAL_SANDBOX=1` accepts that risk; do so only inside a container you control.

</div>

## Flags and environment

`harness-web -h` lists:

| Flag | Meaning |
|---|---|
| `-addr ADDR` | Listen address (env `HARNESS_WEB_ADDR`, default `127.0.0.1:8080`) |
| `-model ID` | Default model id (default `LLM_DEFAULT_MODEL`); must be in `LLM_MODELS` |
| `-local` | Work on the current directory: local sandbox, coding profile, durable sessions |
| `-user LOGIN` | Login of the single local user (default `local`) |
| `-team` | Team mode: OIDC login, projects, shared threads (also on when `OIDC_ISSUER` is set or `HARNESS_TEAM=1`) |
| `-dev-users` | Team mode on a loopback address: act as the user named by the `X-Harness-User` header (development only) |

| Variable | Meaning |
|---|---|
| `HARNESS_WEB_ADDR` | Listen address; the default of `-addr` |
| `HARNESS_WEB_TOKEN` | Bearer token for the API. Required for a non-loopback address unless OIDC is configured |
| `HARNESS_WEB_ORIGINS` | Comma-separated CORS allow-list, for a browser app on another origin |

A server without any credential is reachable only on loopback. Binding elsewhere fails with:

```text
harness-web: refusing to listen on 0.0.0.0:18091 without HARNESS_WEB_TOKEN: set a token, configure OIDC (OIDC_ISSUER), or bind a loopback address
```

### Share it behind a token

```bash
HARNESS_WEB_ADDR=0.0.0.0:8080 HARNESS_WEB_TOKEN=secret \
OPENSANDBOX_URL=http://sandbox.internal:8090 harness-web
```

Every request except `GET /healthz` needs `Authorization: Bearer secret`; `GET /api/models` without it answers `{"error":{"code":"unauthenticated","message":"Authentication required."}}`. The token identifies one shared user (`-user`, default `local`); for several people use team mode. Run the agents in a remote sandbox ([Sandbox backends](/docs/integrations/sandbox-backends/)) rather than `local`. `harness -remote http://host:8080` with `HARNESS_REMOTE_TOKEN=secret` attaches the terminal UI to it. The routes are in [HTTP API](/docs/reference/http-api/) and the command line in [harness-web](/docs/reference/cli-harness-web/).

## Docker Compose

```bash
docker compose up --build
```

then open http://localhost:8080 and sign in as `alice@example.com` or `bob@example.com` with the password `password`. `docker-compose.yml` runs three services:

| Service | Image | Role |
|---|---|---|
| `postgres` | `postgres:16` | Database `harness` (user and password `harness`), volume `pgdata`, with a `pg_isready` health check |
| `dex` | `dexidp/dex:v2.41.1` | A development OpenID Connect provider configured by `examples/dev-oidc/dex.yaml`; publishes ports 5556 and 8080 |
| `harness-web` | built from `Dockerfile` | The team web app; shares Dex's network namespace (`network_mode: "service:dex"`), so `localhost:5556` is the same issuer URL for your browser and for the server |

The image's default command is `-local -addr 0.0.0.0:8080`, and the compose file sets `OIDC_ISSUER`, so the server runs in team mode with Postgres (`POSTGRES_HOST=postgres`). Agents run in the container's local sandbox, one directory per session, kept in the `state` volume at `/data`; the compose file sets `HARNESS_ALLOW_LOCAL_SANDBOX=1` for that reason.

Your model provider is not in the compose file. It is read from `~/.har/config.yml` and `~/.har/auth.json` on the host, mounted read-only at `/root/.har/`. Point `HAR_CONFIG` and `HAR_AUTH` at other files to change which ones.

Variables you may set in your shell or in a `.env` file next to the compose file: `OIDC_SESSION_KEY` and `HARNESS_SECRET_KEY` (both default to development-only values; change them), `HARNESS_MEMORY_EXTRACT` (default `true`), `HARNESS_MEMORY_EXTRACT_MODEL`, `HARNESS_MEMORY_EMBEDDING_BACKEND` (default `simple`), `HARNESS_MEMORY_EMBEDDING_URL`, `HARNESS_MEMORY_EMBEDDING_KEY`, `HARNESS_MEMORY_EMBEDDING_MODEL`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `GITHUB_APP_ID`, `GITHUB_APP_SLUG` and `GITHUB_APP_PRIVATE_KEY_FILE`.

<div class="callout note">

**This stack is for local trials.** It uses a development identity provider with fixed passwords, development default secrets and the local sandbox. For anything shared replace Dex with your identity provider and the local sandbox with a remote one, as the production file does.

</div>

## Production compose file

`docker-compose.prod.yml` is a deployment for Dokploy (a Compose service built from the GitHub repository, auto deploy on push). It runs one service, `harness-web`, built from the same `Dockerfile`:

- command `-team -addr 0.0.0.0:8080`, port 8080 exposed to the Dokploy network (`dokploy-network`, external), `restart: unless-stopped`, a health check on `http://127.0.0.1:8080/healthz`;
- `HARNESS_PRODUCTION=1`, `AGENT_PROFILE=coding`, `LLM_PROVIDER=openai`, `SANDBOX_BACKEND=opensandbox`;
- the volume `harness-data` at `/data` holds sessions and team data (the file store, since no Postgres is configured);
- agents run in OpenSandbox containers, not in the web container, reached through the server proxy (`OPENSANDBOX_USE_SERVER_PROXY=true`), with `OPENSANDBOX_CREDENTIAL_PROXY=true` so GitHub authentication is added by the egress sidecar and no token enters a sandbox.

Secrets and per-install values come from the Dokploy environment of the service, never from the file:

| Variable | Required | Meaning |
|---|---|---|
| `HARNESS_HOST` | yes | Public host name; builds `HARNESS_PUBLIC_URL` and the OIDC redirect URL `https://<host>/auth/callback` |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET` | yes | Your identity provider and the client registered for the redirect URL above |
| `OIDC_SESSION_KEY` | yes | Signs the login cookie (`openssl rand -hex 32`) |
| `HARNESS_SECRET_KEY` | yes | Encrypts connector tokens (`openssl rand -hex 32`) |
| `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODELS`, `LLM_DEFAULT_MODEL` | yes | The OpenAI-compatible model endpoint |
| `LLM_DISCOVER_MODELS` | no | Default `true`: add the provider's model list to `LLM_MODELS` at startup; `false` keeps `LLM_MODELS` only |
| `OPENSANDBOX_URL`, `OPENSANDBOX_API_KEY`, `OPENSANDBOX_IMAGE` | yes | The OpenSandbox server and the image sandboxes start from |
| `SANDBOX_EGRESS_ALLOWLIST` | no | Hosts a sandbox may reach; the default is GitHub, PyPI, npm, the Go proxy and crates.io. Production refuses an empty list unless `SANDBOX_EGRESS_UNRESTRICTED=true` |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME` | no | Telegram bot; run one replica only, the bot long-polls |
| `GITHUB_APP_ID`, `GITHUB_APP_SLUG`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `GITHUB_APP_PRIVATE_KEY` | no | GitHub App for private repositories and project import (the PEM on one line in double quotes, `\n` for line breaks) |
| `SENTRY_DSN`, `SENTRY_RELEASE`, `SENTRY_BROWSER_DSN` | no | Error tracking for the server and for the browser UI |
| `UMAMI_URL`, `UMAMI_WEBSITE_ID` | no | Usage statistics; both or neither |

Create the identity provider client with the redirect URI `https://<HARNESS_HOST>/auth/callback`, point a domain at the `harness-web` service on port 8080, and check [Production checklist](/docs/operations/production/) and [Security model](/docs/operations/security/) before exposing it. Telegram and GitHub are covered in [GitHub and Telegram](/docs/integrations/github-telegram/) and OIDC in [OIDC](/docs/integrations/oidc/).

## Team mode in brief

`harness-web -team` turns the web host into a multi-user app. People log in with OpenID Connect (authorization-code flow with PKCE), share projects (a repository each) and see all agent sessions of a project with who wrote each message. Project roles are owner, member and viewer. Every user also has a global agent that watches their sessions, and memory is kept per project and per user. Sessions are on disk by default; with `POSTGRES_HOST` they are in Postgres, and with `REDIS_HOST` several replicas share the live feed.

For development, `harness-web -team -dev-users -local` (loopback only, no OIDC) acts as the user named by the `X-Harness-User` header. See [Run team mode](/docs/guides/team-mode/) for projects, GitHub, memory review and replicas.
