# Team mode: projects, shared sessions and the global agent

`harness-web -team` turns the web host into a multi-user app. Several people log in (OpenID Connect), share
**projects** (a repository each) and see all of a project's agent **sessions** (threads), with who wrote each
message. Every user also has a **global agent** (the supervisor) that watches their sessions. Memory is
per project (shared by its members) and per user (global).

```
OIDC_ISSUER=https://idp.example.com OIDC_CLIENT_ID=… OIDC_CLIENT_SECRET=… \
OIDC_REDIRECT_URL=https://harness.example.com/auth/callback OIDC_SESSION_KEY=$(openssl rand -hex 32) \
POSTGRES_HOST=db …  harness-web -team -addr 0.0.0.0:8080
```

Without `POSTGRES_HOST` the sessions are kept on disk (`~/.har/sessions`, with the team data in `team.json` beside
them), which suits one machine. For development, `harness-web -team -dev-users -local` (loopback only, no OIDC)
acts as the user named by the `X-Harness-User` header.

## Model

| | |
|---|---|
| user | a person from the identity provider; the key is the *login* claim (`OIDC_LOGIN_CLAIM`, default `preferred_username`, else the verified email, else `sub`). Pick a claim that never changes for a person. |
| project | name, repository URL, default branch, GitHub installation. Members have a role: **owner** (members, settings, repository, delete), **member** (create sessions, send, steer, approve), **viewer** (read only). A project always has an owner. |
| session | a thread. A project session is visible to every member; it is *stored* under the login of its creator, and members act on it through the web layer, which checks membership (404 for non-members, 403 for a role that is too low) and records the real actor. Personal sessions (no project) stay private to their owner. |
| message author | `store.Message.AuthorLogin / AuthorKind / OnBehalfOf`: the user who typed a message or steer, or the supervisor acting for a user. The UI shows it; in a session with several speakers the model also sees `[alice] …` before each user message. Single-author sessions keep their old prompts. |
| audit | `GET /api/projects/{id}/audit`: who sent, steered, approved (with the tool), cancelled, changed members or memory. |
| memory | `GET/POST /api/memory[?project=ID]`. Agents in a project session read and write the **project** memory (so no member's personal memory leaks into a shared session); in personal sessions and in the supervisor, the user's own. |
| live feed | `GET /api/events` (SSE): `thread.created`, `run.started`, `run.finished` (with status `waiting_approval`, …), `member.changed`… filtered by what the subscriber may see. `GET /api/overview` is the matching snapshot. |

Invitations: `POST /api/projects/{id}/invites {email, role}`. A user with that email already known is added at
once; otherwise the invite is claimed when the email logs in (only a **verified** email claims invites).

## Login (OIDC)

The server runs the authorization-code flow with PKCE (`/auth/login`, `/auth/callback`, `POST /auth/logout`) and
keeps a signed HttpOnly `SameSite=Lax` cookie, so the SSE streams (which cannot send headers) authenticate and no
token reaches JavaScript. `OIDC_ALLOWED_DOMAINS` restricts logins by email domain. `OIDC_SESSION_KEY` signs the
cookie; set it, or sessions end at every restart.

## GitHub

Set up a **GitHub App** (repository contents: read & write; pull requests: read & write if agents open them) and give
the server `GITHUB_APP_ID`, `GITHUB_APP_SLUG` and `GITHUB_APP_PRIVATE_KEY` (or `…_FILE`). A project owner sets the
repository URL, installs the App on that repository and presses *Check again*
(`POST /api/projects/{id}/github/connect`), which finds the installation. When a session's sandbox is created the
repository is cloned into `/workspace/repo` on a branch `har/<thread>`; the clone and pushes use a short-lived
installation token (never a member's personal token), kept in a credential file of the sandbox and renewed at the
start of each run. Public repositories clone without an App. The token is never placed on a command line.

## My assistant (the supervisor)

`GET /api/supervisor` returns the caller's supervisor thread (kind `supervisor`). It runs without a sandbox, with
tools to look and to act on every session the user can see:

* `list_sessions`, `read_session` — free.
* `session_send`, `session_approve`, `session_answer`, `session_cancel`, `session_spawn` — registered as risky
  (`approvals.RegisterRisky`), so the user confirms each in the supervisor's chat (approving a tool "always" for
  the run gives it autonomy for that chain). What it sends is stored as authored by the supervisor on behalf of the user.

`PUT /api/supervisor/settings {watch, stuckMinutes}` turns on the watcher: sessions that wait for the user, fail,
or run silently for `stuckMinutes` are queued and, in batches (at most every two minutes), put to the supervisor
as a `[watcher]` message that it answers once.

## Memory

Three layers, all with the same API (`/api/memory`): **project** (`?project=ID`, shared by members), **me in this
project** (`?project=ID&mine=1`, private; used when that user is the one asking) and **global** (no parameter,
personal sessions). In a shared session the agent sees the project layer and the asking user's private layer, never
another member's.

What an agent gets at the start of a run (`features/memory/recall.go`):

1. the **pinned** entries (`metadata.pinned`, up to 2,500 characters) of every layer: the always-on block. The
   repository's `AGENTS.md` (or `CLAUDE.md`) is imported as a pinned entry when a project is imported from GitHub
   and on demand (`POST /api/projects/{id}/memory/import-agents`); `GET /api/projects/{id}/memory/export` renders the
   memory as markdown to commit back;
2. the entries that **match the user's last message**, by words (with light stemming and stop-word filtering) and, with
   an embedder, by meaning; at most 6, within a character budget;
3. with no match, the latest decisions, facts and constraints.

Pending proposals are never recalled. The agent can also `memory_search` (both layers, results labelled) and
`memory_create` with `audience: "me"` for personal preferences.

**Learning.** After every completed run a model (`HARNESS_MEMORY_EXTRACT_MODEL`, default the run's own) reads the
exchange and proposes durable knowledge: decisions and their reason, conventions, constraints, commands that worked,
pitfalls, preferences. Each candidate is compared with the entries that look like it: a new fact is added, a changed
one becomes a **proposed update** of its entry, a known one is skipped. Everything arrives as `pending` and is used
only after a member keeps it in the review queue. On by default in team mode; `HARNESS_MEMORY_EXTRACT=false` turns it
off. An entry that changes keeps its previous versions in `metadata.history` (10 kept, newest first) with the thread
that changed it.

**Embeddings.** `HARNESS_MEMORY_EMBEDDING_BACKEND=openai` (with `_URL`, `_KEY`, `_MODEL`; the URL and key default to the
LLM's) gives entries and queries vectors from an OpenAI-compatible `/embeddings` endpoint; they are stored per entry
(`work_memory_embeddings`), recomputed when the text changes, and compared in the application, which is fine for the
few thousand entries a memory holds (move to pgvector beyond that). Without it the "simple" backend is word overlap.

**Measuring.** `GET /api/memory/stats` shows counts and what the service did (runs with recall, items recalled,
proposals made and skipped, embeddings). `go test ./features/memory -run Eval -v` prints recall@1, recall@3 and MRR on
a small paraphrase benchmark, for words alone and with embeddings (set `HARNESS_EVAL_EMBEDDING_URL` to try a real
model); regression floors fail the test if ranking gets worse. On that benchmark words alone find a paraphrased
question's memory in the top three only a third of the time, which is why embeddings matter.

**Storage.** Postgres, or with the local store the journal (`journal.jsonl`) now persists memory entries too (they were
volatile before). Coding agents need `MEMORY_IN_CODING=true`; team mode sets it.

## Profile and connectors

Each user has a **display name** (default: first and last name from the identity provider's `given_name` /
`family_name`, else `name`; their own choice survives later logins) and an uploaded **avatar** (PNG, JPEG, WebP or
GIF up to 512 KB, type checked from the bytes; no SVG), shown on every message, member list and session.
`PATCH /api/me/profile`, `PUT/DELETE /api/me/avatar`, `GET /api/users/{login}/avatar`.

**Connectors** (`GET /api/connectors`; tokens are stored encrypted with `HARNESS_SECRET_KEY`, else the OIDC session key):

* **GitHub — project import.** Give the GitHub App OAuth credentials (`GITHUB_APP_CLIENT_ID`,
  `GITHUB_APP_CLIENT_SECRET`; callback `<public url>/auth/github/callback`, set `HARNESS_PUBLIC_URL` behind a proxy).
  A user links their account; `GET /api/connectors/github/repos` lists the repositories they can reach through the
  App's installations and `POST /api/projects/import {repo}` creates the project already connected to its installation
  (the server looks the repository up with the user's own token; the client's word is not trusted).
* **Telegram — talk to your assistant.** Create a bot with @BotFather and set `TELEGRAM_BOT_TOKEN`
  (`TELEGRAM_BOT_USERNAME` optional). The profile page shows a deep link with a one-time code; `/start <code>`
  links the chat. Messages go to the assistant (steer, answer its question, or a new run), its replies come back,
  and an approval it waits for arrives with Approve / Deny buttons; `/status` lists sessions. Sessions that wait for
  a user with Telegram linked produce a short note (unless the watcher is on, which hands them to the assistant).
  The bot long-polls, so no public webhook is needed; run it in one replica only.

## Wiring it yourself

`harness.Options` gains `MemoryKey` (which memory a run uses) and `Tools`/`Features` for the supervisor and
`httpserver.RepoCheckout`; `httpserver.Options` gains `Team`, `OIDC`, `Supervisor` and `GitHub`.
`cmd/harness-web/build.go` is the reference composition.

## Known limits

* Unread state is not stored per user on the server (the UI keeps last-seen locally).
* The watcher and the live feed are per process: with several `harness-web` replicas, events for runs
  started on another replica reach this one only through the run's own stream (not the feed).
* A thread stays stored under its creator's login; deleting that user's data would need a transfer step.

## Trying it locally with OIDC

`examples/dev-oidc/run.sh` starts Dex in Docker (users `alice@example.com` and `bob@example.com`, password
`password`) and `harness-web -local` on http://localhost:8080 with OIDC login (`OIDC_LOGIN_CLAIM=name`, so the
logins are `alice` and `bob`). A user's first login registers them; projects can then be shared by invite or by
login. With `-local` in team mode every session gets its own local sandbox directory.

## Docker Compose

`docker compose up --build` runs Postgres, Dex (OIDC; `alice@example.com` / `bob@example.com`, password `password`)
and harness-web on http://localhost:8080. `harness-web` shares Dex's network namespace so `localhost:5556` is the
same issuer URL for your browser and the server. Your model provider is read from `~/.har/config.yml` and
`~/.har/auth.json` (override with `HAR_CONFIG` / `HAR_AUTH`); agents run in the container's local sandbox, one
directory per session, kept in the `state` volume. Set `OIDC_SESSION_KEY` and the `GITHUB_APP_*` variables in your
environment or a `.env` file. For anything beyond local use, replace Dex with your identity provider and the
local sandbox with a remote one.
