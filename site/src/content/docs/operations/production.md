---
title: "Production checklist"
description: "What to set, check and back up before running harness-web (and optionally harness-worker) for real users."
---

`docker-compose.prod.yml` is the reference production composition: `harness-web` in team mode with OIDC login, the file store on a volume, agents in a remote OpenSandbox and the model from any OpenAI-compatible endpoint. This page walks through it as a checklist and says, for each item, what the code enforces and what it leaves to you.

## What `HARNESS_PRODUCTION` does

Any non-empty value turns production mode on (`"1"`, `"true"` and `"yes"` are alike; unset or empty is debug mode). In the code at v0.3.1 it has exactly two effects:

| Effect | Where |
|---|---|
| A remote (`opensandbox`) backend with an empty `SANDBOX_EGRESS_ALLOWLIST` refuses to build, unless `SANDBOX_EGRESS_UNRESTRICTED=true`. The message is `SANDBOX_EGRESS_ALLOWLIST is empty in production: set it, or set SANDBOX_EGRESS_UNRESTRICTED=true to accept unrestricted sandbox egress`. Every process that creates sandboxes (web, worker, CLI) goes through this check. | `config.EgressMode`, called by `harness.NewSandboxBackend` |
| The Sentry `environment` tag defaults to `production` instead of `development` (`SENTRY_ENVIRONMENT` overrides it). | `harness-web` |

<div class="callout note">

**It does not check anything else.** No flag turns on OIDC, TLS or a secret key for you. Everything below that is not egress is your configuration. In debug mode an empty allowlist means unrestricted egress (with the default deny ranges), logged at startup.

</div>

## The checklist

### 1. Authentication

- Team mode with OIDC: `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL=https://<host>/auth/callback`. The identity provider needs a client with exactly that redirect URI. See [OIDC](/docs/integrations/oidc/).
- `OIDC_SESSION_KEY` (`openssl rand -hex 32`). Without it every restart ends all sessions.
- `HARNESS_SECRET_KEY` (`openssl rand -hex 32`) encrypts connector tokens (GitHub, Telegram); it falls back to the session key.
- `OIDC_ALLOWED_DOMAINS` if only some email domains may log in.
- The server refuses a non-loopback address with neither OIDC nor `HARNESS_WEB_TOKEN`. A single token is one shared identity: use it for a private API, not for people.

### 2. Secrets

- Keep every secret in the service environment (Dokploy) or your secret manager, never in the compose file, a `config.yml` or an image. The compose file lists them in its header comment.
- `config.yml` refuses `api_key` and secret-looking `env:` names, so a key cannot be committed by accident. See [Security model](/docs/operations/security/#secrets).
- Set `HARNESS_NO_USER_DIR=1` on servers and workers (recommended in `docs/embedding.md`): nothing is then read from `~/.har`, and the process is hermetic. Supply the model key with `LLM_API_KEY`.

### 3. Sandbox server

- `SANDBOX_BACKEND=opensandbox`, `OPENSANDBOX_URL`, `OPENSANDBOX_API_KEY`, `OPENSANDBOX_IMAGE`. The process must import the backend: `harness-web`, `harness` and `harness-worker` already do. A program you write needs a blank import, see [Upgrade notes](/docs/operations/upgrade-notes/).
- `OPENSANDBOX_USE_SERVER_PROXY=true` when the web container is not on the Docker host's network, so all execd traffic goes through the OpenSandbox server.
- `SANDBOX_EGRESS_ALLOWLIST` (comma-separated host names, IPs or CIDRs; no scheme, port or path). The compose default allows GitHub, PyPI, npm, the Go proxy and crates.io. Add your mirrors and APIs.
- `OPENSANDBOX_CREDENTIAL_PROXY=true` keeps the GitHub token out of the sandbox. It needs `opensandbox-server` >= 0.2.0, an egress sidecar >= 1.1.1 in `dns+nft` mode, and a default-deny allowlist that includes the bound hosts.
- Never let `harness-web` use `SANDBOX_BACKEND=local` here: it is refused in team mode unless `HARNESS_ALLOW_LOCAL_SANDBOX=1`.
- Bound cost with `MAX_SANDBOXES_PER_USER` (default 5), `SANDBOX_TIMEOUT_SECONDS` (default 1800), `SANDBOX_COMMAND_TIMEOUT_SECONDS` (default 120), `SANDBOX_CPU` and `SANDBOX_MEMORY`.

### 4. Model

`LLM_PROVIDER`, `LLM_BASE_URL`, `LLM_API_KEY`, `LLM_MODELS`, `LLM_DEFAULT_MODEL`. `LLM_DISCOVER_MODELS` (default `true` in the compose file) adds the provider's `GET /models` list to the allow-list at startup; set `false` to offer only `LLM_MODELS`. A model outside the allow-list is rejected (`model "x" is not allowed`).

### 5. Storage: file store, Postgres, Redis, workers

| Shape | Set | What lives where |
|---|---|---|
| Single `harness-web` (the prod compose) | nothing | Sessions and `team.json` on the `/data` volume (`LOCAL_STATE_DIR=/data/state`). One replica. |
| Several `harness-web` replicas | `POSTGRES_HOST` (+ `POSTGRES_*`), `REDIS_HOST` | Threads and team data in Postgres; migrations run at startup. Redis carries the live feed (`work:feed`), so every replica sees every event. |
| Workers | `harness-worker` with the same `POSTGRES_*` and `REDIS_*` | Consumes runs queued by another process through the library. |

<div class="callout note">

**`harness-web` executes its own runs.** Its build wires Redis only for the feed bus; it does not queue runs for `harness-worker`. Workers serve an application of yours that calls `Send` and never `Run`. See [Postgres and Redis for several processes](/docs/guides/postgres-redis/) and [Choose a deployment shape](/docs/guides/deployment-shapes/).

</div>

Postgres: `POSTGRES_SSLMODE` defaults to `require` (set `disable` only on a private network); the schema defaults to `work`; `DB_POOL_MAX_CONNS` defaults to 20. `PING_DB_AT_STARTUP` (default `true`) makes a bad host fail at start rather than at the first request. The Telegram bridge long-polls: run it in one replica only.

### 6. Health endpoints

| Process | Endpoint | Meaning |
|---|---|---|
| `harness-web` | `GET /healthz` | `200 {"status":"ok"}`, no authentication. Used by the compose healthcheck (`curl -fsS http://127.0.0.1:8080/healthz`, 30 s interval, 20 s start period). It does not test Postgres, Redis or the sandbox server. |
| `harness-worker` | `GET /healthz` | `200 ok` while the process is up. |
| `harness-worker` | `GET /readyz` | `200 ok` while the queue loop reads; `503 not ready: ...` when the consumer group cannot be created, the loop is stuck, or the worker is draining. |
| `harness-worker` | `GET /metrics` | Prometheus text, see [Observability](/docs/operations/observability/#worker-metrics). |

The worker's admin server binds `127.0.0.1:9090` by default. Set `HARNESS_WORKER_ADDR=0.0.0.0:9090` inside a container and keep the port off the public network: it has no authentication.

### 7. Graceful shutdown

- **`harness-worker`** handles `SIGINT` and `SIGTERM`: it stops claiming runs, reports `harness_worker_draining 1` and `/readyz` 503, lets in-flight runs finish for `RUNTIME_SHUTDOWN_GRACE_SECONDS` (default 15), then interrupts the rest. Interrupted runs are recorded as `interrupted`, never lost. Make the orchestrator's stop grace longer than that.
- The configuration is validated at start: `RUNTIME_SHUTDOWN_GRACE_SECONDS` + 12 s must fit `RUNTIME_TERMINATION_GRACE_SECONDS` (default 30), or the process refuses to start with `RUNTIME_SHUTDOWN_GRACE_SECONDS (60) + 12 s must fit RUNTIME_TERMINATION_GRACE_SECONDS (30), the pod's termination grace`. Raise both together.
- **`harness-web`** only traps `SIGINT` (it then stops accepting connections and gives requests 5 s). On `SIGTERM`, which is what `docker stop` and most orchestrators send, the process ends immediately (exit status 143, observed). Runs in flight are then not finished by this process; a later process's sweeper marks runs whose runtime stopped heartbeating as `interrupted`. If you need a drain, front the container with an init that translates the signal, or send `SIGINT`.

### 8. Volumes and backups

- The Dockerfile declares `VOLUME /data` and sets `LOCAL_STATE_DIR=/data/state`. In the prod compose the named volume `harness-data` holds the journal of sessions (`journal.jsonl`) and `team.json` (users, projects, members, audit). **Back it up**; losing it loses every conversation and project.
- With Postgres, back up the database as usual. Deleted threads are hard-deleted after `HARNESS_PURGE_AFTER_DAYS` (default 30; `<= 0` never).
- Workspaces live in the sandbox containers and are disposable: the harness checkpoints `/workspace` (`HARNESS_WORKSPACE_CHECKPOINTS`, default on) and restores it into a fresh sandbox.
- Artifacts use the store, or S3 when `ARTIFACTS_*` is configured; production buckets must exist (`ARTIFACTS_S3_CREATE_BUCKET` is for local RustFS).

### 9. Version pinning

- `build: .` builds the checkout Dokploy fetched. For a reproducible deploy, deploy a tag (for example `v0.3.1`), not a moving branch, and turn off auto deploy on push if you do not want every commit shipped.
- The `app` module (`harness-web`) has its own tags; `app/v0.2.0` requires the root `v0.3.0`. See the [tag table](/docs/operations/upgrade-notes/#module-tags).
- Pin `OPENSANDBOX_IMAGE` to a digest or a tag you built, not `latest`. The default image is `python:3.11-slim`; the repository's `sandbox-image/` builds a fuller one.
- A program that embeds the harness should pin the root module in `go.mod`; `make check-embed` builds a sample against the published versions.

### 10. Edge and logs

- Terminate TLS in front (Dokploy's Traefik): `harness-web` serves HTTP and sets the cookie `Secure` when `OIDC_REDIRECT_URL` starts with `https://`. Set `HARNESS_PUBLIC_URL` to the public origin.
- Add rate limits and upload size limits at the proxy; the harness does not rate-limit requests.
- Logs: `harness-web` logs warnings and errors only; the worker logs info. Set `SENTRY_DSN` to collect errors. See [Observability](/docs/operations/observability/).

## Compose environment at a glance

```yaml
services:
  harness-web:
    build: .
    command: ["-team", "-addr", "0.0.0.0:8080"]
    environment:
      HARNESS_PRODUCTION: "1"
      HARNESS_PUBLIC_URL: https://${HARNESS_HOST}
      OIDC_ISSUER: ${OIDC_ISSUER}
      OIDC_CLIENT_ID: ${OIDC_CLIENT_ID}
      OIDC_CLIENT_SECRET: ${OIDC_CLIENT_SECRET}
      OIDC_REDIRECT_URL: https://${HARNESS_HOST}/auth/callback
      OIDC_SESSION_KEY: ${OIDC_SESSION_KEY}
      HARNESS_SECRET_KEY: ${HARNESS_SECRET_KEY}
      SANDBOX_BACKEND: opensandbox
      OPENSANDBOX_URL: ${OPENSANDBOX_URL}
      OPENSANDBOX_USE_SERVER_PROXY: "true"
      OPENSANDBOX_CREDENTIAL_PROXY: "true"
      SANDBOX_EGRESS_ALLOWLIST: github.com,api.github.com,pypi.org
    volumes:
      - harness-data:/data
```

This is an excerpt of `docker-compose.prod.yml`; the full file also passes the model, GitHub App, Telegram, Sentry and Umami variables.
