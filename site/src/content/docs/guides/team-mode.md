---
title: "Run team mode"
description: "Run harness-web -team for several people with OIDC login, shared projects, per-project memory, a GitHub App and a supervisor agent."
---

`harness-web -team` turns the web host into a multi-user app. People log in through OpenID Connect, share **projects** (one repository each) and see all of a project's agent **sessions** (threads) with who wrote each message. Every user also has a global agent, the **supervisor**, that watches their sessions. Use it for a team on one server; for one person on one machine, [`harness-web -local`](/docs/quickstart/web/) is enough.

```bash
OIDC_ISSUER=https://idp.example.com OIDC_CLIENT_ID=... OIDC_CLIENT_SECRET=... \
OIDC_REDIRECT_URL=https://harness.example.com/auth/callback OIDC_SESSION_KEY=$(openssl rand -hex 32) \
SANDBOX_BACKEND=opensandbox OPENSANDBOX_URL=http://sandboxes:8090 \
POSTGRES_HOST=db ... harness-web -team -addr 0.0.0.0:8080
```

Without `POSTGRES_HOST` the sessions are kept on disk (`~/.har/sessions`, or `LOCAL_STATE_DIR`) with the team data in `team.json` beside them, which suits one machine. `-team` is also switched on by `OIDC_ISSUER` or `HARNESS_TEAM=1`. The `-addr` flag defaults to `127.0.0.1:8080` (env `HARNESS_WEB_ADDR`).

<div class="callout warning">

**Team mode refuses the local sandbox.** `SANDBOX_BACKEND=local` runs the agent's commands on the server with the server's rights, so `harness-web` stops at start with `not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk`. Use a remote backend for a shared server. See [Security model](/docs/operations/security/) and [Sandboxes](/docs/concepts/sandboxes/).

</div>

## Flags and environment

| Flag or variable | Meaning |
|---|---|
| `-team` / `HARNESS_TEAM=1` | team mode (also implied by `OIDC_ISSUER`) |
| `-dev-users` | development only: team mode on a loopback address with no OIDC, acting as the user named by the `X-Harness-User` request header. Refused unless `-team` is set, `OIDC_ISSUER` is not, and the address is loopback |
| `-local` | the current directory as workspace; combined with `-team` it still needs `HARNESS_ALLOW_LOCAL_SANDBOX=1` |
| `-user` | login of the single user when team mode runs without OIDC (default `local`) |
| `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL` | the identity provider client; `OIDC_ISSUER` needs `OIDC_CLIENT_ID` and `OIDC_REDIRECT_URL` |
| `OIDC_SESSION_KEY` | signs the login cookie; set it, or sessions end at every restart |
| `OIDC_LOGIN_CLAIM` | the claim used as the user's login (default `preferred_username`, then the verified email, then `sub`) |
| `OIDC_ALLOWED_DOMAINS`, `OIDC_SCOPES` | comma-separated email domains allowed to log in; extra scopes |
| `HARNESS_SECRET_KEY` | encrypts connector tokens (default: the OIDC session key) |
| `HARNESS_PUBLIC_URL` | the public base URL, needed behind a proxy for the GitHub callback |
| `HARNESS_WEB_TOKEN` | a bearer token as an alternative way to authorise a non-loopback server |
| `POSTGRES_*`, `REDIS_HOST` | shared store and live feed for several replicas ([Postgres and Redis](/docs/guides/postgres-redis/)) |
| `GITHUB_APP_ID`, `GITHUB_APP_SLUG`, `GITHUB_APP_PRIVATE_KEY` or `GITHUB_APP_PRIVATE_KEY_FILE` | the GitHub App for private repositories |
| `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET` | OAuth credentials of the App, for project import |
| `TELEGRAM_BOT_TOKEN`, `TELEGRAM_BOT_USERNAME` | the Telegram bridge |
| `HARNESS_MEMORY_EXTRACT`, `HARNESS_MEMORY_EXTRACT_MODEL` | memory proposals after each run (on in team mode) and the model used |
| `HARNESS_MEMORY_EMBEDDING_BACKEND` and `_URL`, `_KEY`, `_MODEL` | `simple` (word overlap, default) or `openai` embeddings |

Everything else (`LLM_*`, `SANDBOX_*`, `OPENSANDBOX_*`) is the same as for a single user; the full list is [Environment variables](/docs/reference/env-vars/) and the command is described in [harness-web](/docs/reference/cli-harness-web/). In team mode `MEMORY_IN_CODING=true` and `HARNESS_MEMORY_EXTRACT=true` are set as defaults.

## Try it without an identity provider

`-dev-users` lets you exercise the API as several people with a header (verified against a running server on loopback):

```bash
harness-web -team -dev-users -addr 127.0.0.1:18099 &

curl -s -H 'X-Harness-User: alice' localhost:18099/api/me
curl -s -H 'X-Harness-User: alice' -X POST \
  -d '{"name":"demo","repoUrl":"https://github.com/7ar-chat/harness"}' localhost:18099/api/projects
curl -s -H 'X-Harness-User: bob' localhost:18099/api/projects     # [] : bob is not a member
```

The second call returns the project with `"role":"owner"` for alice; bob sees `[]` until he is invited. The server prints `harness-web: team mode without OIDC_ISSUER: everyone is local` as a reminder that this is not authentication. To get a sandbox in this mode add `-local` and `HARNESS_ALLOW_LOCAL_SANDBOX=1`.

For a real login on your machine, `examples/dev-oidc/run.sh` starts Dex in Docker (users `alice@example.com` and `bob@example.com`, password `password`, logins `alice` and `bob` through `OIDC_LOGIN_CLAIM=name`) and `harness-web -local` on `http://localhost:8080`. `docker compose up --build` runs Postgres, Dex and harness-web together.

## Model

| Concept | What it is |
|---|---|
| user | a person from the identity provider; the key is the *login* claim. Pick a claim that never changes for a person |
| project | name, repository URL, default branch, GitHub installation. Roles: **owner** (members, settings, repository, delete), **member** (create sessions, send, steer, approve), **viewer** (read only). A project always has an owner |
| session | a thread. A project session is visible to every member and stored under the login of its creator; the web layer checks membership (404 for non-members, 403 for a role that is too low) and records the real actor. Personal sessions (no project) stay private |
| message author | `AuthorLogin`, `AuthorKind`, `OnBehalfOf` on a message. The UI shows it; in a session with several speakers the model also sees `[alice] ...` before each user message |
| audit | `GET /api/projects/{id}/audit`: who sent, steered, approved (with the tool), cancelled, changed members or memory |
| live feed | `GET /api/events` (SSE): `thread.created`, `run.started`, `run.finished` (with status such as `waiting_approval`), `member.changed`, filtered by what the subscriber may see. `GET /api/overview` is the matching snapshot |
| seen / unread | `GET /api/seen` and `POST /api/seen`: the caller's last visit per thread, stored per user |

Invitations: `POST /api/projects/{id}/invites {email, role}`. A user whose email is already known is added at once; otherwise the invite is claimed when that email logs in (only a **verified** email claims invites).

## Login

The server runs the OIDC authorization-code flow with PKCE (`/auth/login`, `/auth/callback`, `POST /auth/logout`) and keeps a signed HttpOnly `SameSite=Lax` cookie, so SSE streams (which cannot send headers) authenticate and no token reaches JavaScript. The identity provider needs a client whose redirect URI is `OIDC_REDIRECT_URL`. Details of the flow and provider setup are in [OIDC](/docs/integrations/oidc/).

## GitHub

Create a **GitHub App** (repository contents read and write; pull requests read and write if agents open them) and give the server `GITHUB_APP_ID`, `GITHUB_APP_SLUG` and `GITHUB_APP_PRIVATE_KEY` (or `GITHUB_APP_PRIVATE_KEY_FILE`). A project owner sets the repository URL, installs the App on it and presses *Check again* (`POST /api/projects/{id}/github/connect`), which finds the installation. When a session's sandbox is created the repository is cloned into `/workspace/repo` on a branch `har/<thread>`; clone and pushes use a short-lived installation token (never a member's personal token), renewed at the start of each run and never placed on a command line. Public repositories clone without an App.

For project import, add the App's OAuth credentials (`GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, callback `<public url>/auth/github/callback`). A user links their account, `GET /api/connectors/github/repos` lists the repositories they can reach through the App's installations and `POST /api/projects/import {repo}` creates the project already connected.

<div class="callout note">

**Where the token lives depends on the sandbox backend.** With a backend that implements credential brokering (OpenSandbox with `OPENSANDBOX_CREDENTIAL_PROXY=true`) the token never enters the sandbox. Without it the token is written to a credential file in the sandbox, `/workspace/.work/git-credentials`, where anything running there can read it. See [GitHub and Telegram](/docs/integrations/github-telegram/).

</div>

## The supervisor (My assistant)

`GET /api/supervisor` returns the caller's supervisor thread. It runs without a sandbox, with tools to look at and act on every session the user can see:

- `list_sessions`, `read_session`: free.
- `session_send`, `session_approve`, `session_answer`, `session_cancel`, `session_spawn`: registered as risky, so the user confirms each in the supervisor's chat. What it sends is stored as authored by the supervisor on behalf of the user.

`PUT /api/supervisor/settings {watch, stuckMinutes}` turns on the watcher: sessions that wait for the user, fail, or run silently for `stuckMinutes` are queued and put to the supervisor in batches (at most every two minutes) as a `[watcher]` message that it answers once. The default `stuckMinutes` is 10.

## Memory layers

Three layers share one API (`/api/memory`):

| Layer | Query | Who sees it |
|---|---|---|
| project | `?project=ID` | every member |
| me in this project | `?project=ID&mine=1` | the user only, used when that user is the one asking |
| global | no parameter | personal sessions |

In a shared session the agent sees the project layer and the asking user's private layer, never another member's. At the start of a run it gets the pinned entries of every layer (up to 2,500 characters), then the entries matching the user's last message (at most 6), or the latest decisions, facts and constraints when nothing matches. Pending proposals are never recalled.

After each completed run a model reads the exchange and proposes durable knowledge (decisions and reasons, conventions, constraints, commands that worked, pitfalls, preferences). New facts and changed ones arrive as `pending` and are used only after a member keeps them in the review queue. `HARNESS_MEMORY_EXTRACT=false` turns this off. `POST /api/projects/{id}/memory/import-agents` imports the repository's `AGENTS.md` as a pinned entry and `GET /api/projects/{id}/memory/export` renders the memory as markdown to commit back. Concepts and tools are in [Memory](/docs/concepts/memory/).

## Several replicas

Run `harness-web -team` behind a load balancer as several replicas with Postgres (`POSTGRES_HOST`, shared team tables and sessions) and Redis (`REDIS_HOST`). With `REDIS_HOST` set each replica publishes the feed events it produces on the Redis channel `work:feed` and delivers those of the others, so a browser on replica B sees a run started through replica A.

- Delivery is best effort (pub/sub, no replay): a client that was disconnected refetches `/api/overview` on reconnect.
- The watcher runs in every replica and reacts to its own events, so an attention note is queued once.
- The Telegram bridge long-polls: run it in one replica only.

## Deploying

`docker-compose.prod.yml` is the production composition (Dokploy): harness-web with `-team`, OIDC, agents in a remote OpenSandbox reached through the server proxy and the model from any OpenAI-compatible endpoint. Its variables are listed at the top of the file. A public host name is fine behind OIDC or a `HARNESS_WEB_TOKEN`; only a server with neither is limited to loopback host names. Hardening is in the [Production checklist](/docs/operations/production/).

## Known limits

A thread stays stored under its creator's login; deleting that user's data would need a transfer step.
