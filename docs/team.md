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

## The global agent (supervisor)

`GET /api/supervisor` returns the caller's supervisor thread (kind `supervisor`). It runs without a sandbox, with
tools to look and to act on every session the user can see:

* `list_sessions`, `read_session` — free.
* `session_send`, `session_approve`, `session_answer`, `session_cancel`, `session_spawn` — registered as risky
  (`approvals.RegisterRisky`), so the user confirms each in the supervisor's chat (approving a tool "always" for
  the run gives it autonomy for that chain). What it sends is stored as authored by the supervisor on behalf of the user.

`PUT /api/supervisor/settings {watch, stuckMinutes}` turns on the watcher: sessions that wait for the user, fail,
or run silently for `stuckMinutes` are queued and, in batches (at most every two minutes), put to the supervisor
as a `[watcher]` message that it answers once.

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
