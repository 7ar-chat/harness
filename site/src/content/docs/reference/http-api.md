---
title: "HTTP API"
description: "Every route of hosts/httpapi and the team-mode product, with request and response shapes, errors, authentication and the SSE event stream."
---

`hosts/httpapi` exposes a `harness.Harness` as a JSON API plus one Server-Sent Events stream per run. `harness-web` runs it; you can also mount it in your own server. The wire types are exactly the DTOs of `hosts/client`, so [`client.HTTP`](/docs/reference/client-contract/) is a typed client for everything on this page. Event payloads are listed in [Event types](/docs/reference/events/).

## Mount it in your program

<div class="tabs">

<div class="tab" data-label="Go (mount)">

```go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "demo")
	os.Setenv("LLM_DEFAULT_MODEL", "demo")
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// A scripted model, so this runs offline: every run answers the same text.
	model := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: llmtest.Text("Hello from ", "the scripted model.")}
	})
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}})
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = h.Run(context.Background()) }()

	api := httpapi.New(h, httpapi.Options{
		Token:        "secret", // every request needs: Authorization: Bearer secret
		Models:       cfg.LLMModels,
		DefaultModel: cfg.LLMDefaultModel,
		NoUI:         true,
	})
	mux := http.NewServeMux()
	mux.Handle("/", api) // routes are served under /api and without the prefix
	log.Println("listening on http://127.0.0.1:8099")
	log.Fatal(http.ListenAndServe("127.0.0.1:8099", mux))
}
```

</div>

<div class="tab" data-label="curl">

```bash
B=http://127.0.0.1:8099
H='Authorization: Bearer secret'

curl -s $B/healthz                                   # {"status":"ok"}  (no auth)
curl -s -H "$H" $B/api/models                        # {"models":["demo"],"default":"demo"}

T=$(curl -s -H "$H" -X POST -d '{"title":"demo"}' $B/api/threads | jq -r .id)
R=$(curl -s -H "$H" -X POST -d '{"message":"hi"}' $B/api/threads/$T/runs | jq -r .id)
curl -s -N -H "$H" $B/api/runs/$R/events             # the SSE stream, until RUN_FINISHED
```

</div>

</div>

This program and every request in the curl tab were run against each other. `NoUI` leaves out the embedded browser UI, which uses absolute `/api` URLs and belongs at the root. To mount the API under a prefix use `http.StripPrefix`; other options are `Authenticate`, `AllowedOrigins`, `AllowedHosts`, `ModelInfo`, `PingEvery` (default 15 s), `LocalUser`, `Extension` and `Client`.

## Conventions

| | |
|---|---|
| Prefix | Every route below is served under `/api` and without the prefix. `GET /healthz` is the only route that needs neither. |
| Bodies | JSON, at most 2 MiB; a run-creation body may be larger (attachments are base64, up to `MaxAttachments`×`MaxAttachmentBytes`×4/3 plus 2 MiB). An empty body is fine: every field is optional. A malformed body is 400 `invalid_request`. |
| `limit` | `?limit=` on list routes: default 50, at most 200. |
| Other users' ids | A thread or run that belongs to another user is always 404 `not_found`, never 403. |
| Errors | `{"error":{"code","message"}}` with the status below. Unexpected errors are 500 `internal` (`An internal error occurred.`) and logged. |

### Errors

| Status | Code | Meaning |
|---|---|---|
| 400 | `invalid_request` | Bad JSON, a missing or invalid field. |
| 400 | `invalid_path` | A file path outside `/workspace`. |
| 401 | `unauthenticated` | `Authentication required.` |
| 403 | `forbidden_origin`, `forbidden_host` | CORS or Host guard. See [harness-web](/docs/reference/cli-harness-web/#cors-and-host-checks). |
| 404 | `not_found` | Unknown id, or one that belongs to another user. |
| 409 | `conflict`, `run_not_active` | The thread has a run in progress; the run no longer accepts messages. |
| 413 | `too_large` | The file is too large to download. |
| 429 | `too_many_steers` | Too many pending steers on the run. |
| 501 | `not_supported`, `no_sandbox` | The store has no session listing; the server has no sandbox. |

## Authentication

`Options.Authenticate` is `func(*http.Request) (user string, err error)`. A non-nil error or an empty user answers 401. It takes precedence over the older `User` and `Token` options.

The default is `httpapi.LocalAuthenticator(user, token)`: a single user (default `local`); with a non-empty token every request must carry `Authorization: Bearer <token>` (compared in constant time). Plug in your own for several users (a session cookie, a JWT, a proxy header). The user returned is the tenancy boundary: threads, runs, files and memory are scoped by it.

```bash
curl -s $B/api/models
# {"error":{"code":"unauthenticated","message":"Authentication required."}}
curl -s -H "$H" -H "Origin: http://evil.example" $B/api/models
# {"error":{"code":"forbidden_origin","message":"Origin not allowed."}}
```

## Routes

### Server

| Route | Auth | Answer |
|---|---|---|
| `GET /healthz` | no | `{"status":"ok"}` |
| `GET /api/client-config` | no | `{}` or `{"sentry":{...},"umami":{...}}` for the browser UI |
| `GET /api/models` | yes | `{"models":[...],"default":"...","info":{id:ModelInfo}}`; `info` appears when a model table is configured |
| `GET /api/sessions?limit=` | yes | `[Session]`: saved conversations. 501 `not_supported` without a durable store |
| `GET /api/commands` | yes | `[Command]`: custom commands, skills and built-ins (`name`, `description`, `source`, `argHint`) |
| `POST /api/commands/{name}/expand` | yes | Body `{"args"}` → `{"prompt"}`: the template with `$ARGUMENTS` and `$1`.. replaced |
| `GET /`, `GET /ui/...` | no | The embedded browser UI (unless `NoUI`) |

### Threads

| Route | Body | Answer |
|---|---|---|
| `POST /api/threads` | `{"title"}` (empty: titled from the first message) | 201 `Thread` |
| `GET /api/threads?limit=` | | `[Thread]`, pinned first then newest |
| `GET /api/threads/{id}` | | `Thread` with `messages`, `pendingApproval`, `pendingQuestion`, `activeRun` |
| `PATCH /api/threads/{id}` | `{"title","pinned"}` (either may be omitted) | `Thread` |
| `DELETE /api/threads/{id}` | | 204 |
| `POST /api/threads/{id}/compact` | | 200 `{"compacted","tokensBefore","tokensAfter"}` |
| `POST /api/threads/{id}/undo` | | 200 `{"prompt","files","canUndo","canRedo"}`: removes the last prompt and its answers and restores the workspace files |
| `POST /api/threads/{id}/redo` | | 200, same shape |
| `POST /api/threads/{id}/fork` | `{"messageId"}` (empty: the whole thread) | 201 `Thread` titled `<old> (fork)` |

The last four are the optional [`client.Extras`](/docs/reference/client-contract/#extras) operations. `compact`, `undo` and `redo` need the thread to be idle (409 `conflict`).

### Runs

| Route | Body | Answer |
|---|---|---|
| `POST /api/threads/{id}/runs` | `{"message","model","reasoningEffort","skills","interactive","sandbox","mode","attachments"}` | 202 `Run` |
| `POST /api/threads/{id}/resume` | `{"interruptId","reason","decision","always","text","model","reasoningEffort","interactive","mode"}` | 202 `Run` (the run that continues) |
| `GET /api/runs/{id}` | | `Run` |
| `GET /api/runs/{id}/events?after=` | | `text/event-stream` |
| `POST /api/runs/{id}/cancel` | | 202 `{"status":"cancelling"}` |
| `POST /api/runs/{id}/steer` | `{"text"}` | 202 `{"status":"accepted"}`; 409 `run_not_active` when the run ended |

Run-creation fields:

| Field | Meaning |
|---|---|
| `message` | The user message. |
| `model` | Model id; empty uses the server's default model. |
| `reasoningEffort` | `low`, `medium`, `high`, ... or empty for the model's default. |
| `skills` | Skill names requested explicitly for this run. |
| `interactive` | Risky tool calls wait for a decision (see `resume`). |
| `sandbox` | `auto` (default), `off` (no sandbox tools), `on` (400 `invalid_request` when the server has no sandbox). |
| `mode` | `build` (default) or `plan` (read-only). |
| `attachments` | `[{"name","mime","data"}]`, images as base64; at most `MaxAttachments`, each at most `MaxAttachmentBytes` (5 MiB). |

A run ending with `RUN_FINISHED` outcome `interrupt` waits for an answer. Post it to `resume`: for a tool approval, `interruptId` is the approval id, `decision` is `approve`, `deny` or `respond`, `always: true` approves the tool for the rest of the run chain, `text` is the free text of a respond; for an `ask_user` question, `interruptId` is the question's id, `reason` is `ask_user` and `text` is the answer. See [Approvals and permissions](/docs/concepts/approvals-permissions/).

Run states are `queued`, `running`, `completed`, `failed`, `cancelled` and `interrupted`; `errorCode` carries the `RUN_ERROR` code of a failed run.

### Files

| Route | Answer |
|---|---|
| `GET /api/threads/{id}/files?path=` | `[File]` (`name`, `path`, `type` `file`\|`directory`, `size`, `modifiedAt`): a directory of the thread's `/workspace` (empty path: the root). Read-only and confined. 501 `no_sandbox` without a sandbox |
| `GET /api/threads/{id}/files/download?path=` | The file bytes as `application/octet-stream` with `Content-Disposition: attachment`, `Content-Security-Policy: sandbox`, `Cache-Control: no-store`. 413 `too_large` above 100 MiB |

## Event stream

`GET /api/runs/{id}/events` replays the run's recorded events after a point, then follows the run live and ends after its terminal event (`RUN_FINISHED` or `RUN_ERROR`). Each message is:

```text
id: <seq>
data: <the AG-UI event as one JSON object>

```

`seq` numbers the run's events from 1. Resume from a point with `?after=<seq>` or the `Last-Event-ID` header (when both are given the header wins); the stream then has no gap and no duplicate. The response starts with the comment `: connected`, and a `: ping` comment is sent every `PingEvery` (15 s) so proxies keep the stream open. The headers include `Cache-Control: no-cache` and `X-Accel-Buffering: no`.

A real stream (timestamps removed, ids shortened):

```text
: connected

id: 1
data: {"protocolVersion":"1.0","runId":"…","threadId":"…","type":"RUN_STARTED"}

id: 2
data: {"stepName":"step-1","type":"STEP_STARTED"}

id: 3
data: {"messageId":"…","role":"assistant","type":"TEXT_MESSAGE_START"}

id: 4
data: {"delta":"Hello from ","messageId":"…","type":"TEXT_MESSAGE_CONTENT"}
```

Asking for `?after=9` on a finished 10-event run returns only event 10 and closes. `client.HTTP(...).Events` does the reconnecting for you; when it cannot recover it ends the channel with a synthetic `RUN_ERROR` whose code is `stream_lost` (see [Event types](/docs/reference/events/#stream_lost)).

<div class="callout note">

**An id from another user is a 404 on the events route too.** `Harness.Events` itself takes no user; the HTTP layer checks ownership before opening the stream.

</div>

## Extending the server

`Options.Extension` plugs a product on top: a `Policy` (who may see a thread, who is recorded as the author, what is logged) and extra routes. `Server.Route(pattern, handler)` registers an authenticated route under `/api` and without the prefix, and `WriteJSON`, `WriteError`, `Fail`, `Decode`, `DecodeLimit` and `LimitParam` are exported for the handlers. `app/product` is the extension that implements team mode.

## Team mode routes

These routes exist only with `harness-web -team` (package `app/product`) and use the same error format and prefix. Roles are `owner`, `member` and `viewer`; a non-member gets 404 and a role that is too low gets 403 `forbidden` (`You do not have permission to do this.`). See [Run team mode](/docs/guides/team-mode/).

Changes to the core routes in team mode: `POST /api/threads` also accepts `"projectId"` (the caller must be a member) and `GET /api/threads?project=<id>` lists a project's shared threads.

| Route | Needs | Purpose |
|---|---|---|
| `GET /auth/login`, `GET /auth/callback`, `POST /auth/logout` | | OIDC login (authorization code flow with PKCE, signed `HttpOnly` cookie). Served without the `/api` prefix only. |
| `GET /api/me` | login | The user and their projects with roles |
| `PATCH /api/me/profile` | login | `{"displayName"}` |
| `PUT /api/me/avatar`, `DELETE /api/me/avatar` | login | Upload (PNG, JPEG, WebP or GIF, at most 512 KB) or remove the avatar |
| `GET /api/users/{login}/avatar` | login | The avatar image |
| `GET /api/projects`, `POST /api/projects` | login | List; create `{"name","description","repoUrl","defaultBranch"}` (name 1 to 100 characters, repo URL `https`, `ssh` or `git`) → 201 |
| `GET /api/projects/{pid}` | viewer | One project with the caller's `role` |
| `PATCH /api/projects/{pid}`, `DELETE /api/projects/{pid}` | owner | Update (any of the create fields); delete |
| `GET /api/projects/{pid}/members` | viewer | Members and roles |
| `PUT /api/projects/{pid}/members/{login}`, `DELETE /api/projects/{pid}/members/{login}` | owner | `{"role"}`; remove |
| `GET /api/projects/{pid}/invites`, `POST /api/projects/{pid}/invites`, `DELETE /api/projects/{pid}/invites?email=` | owner | Invitations `{"email","role"}` (role defaults to `member`); a known email is added at once, otherwise the invite is claimed when that verified email logs in |
| `GET /api/projects/{pid}/audit` | member | Who sent, steered, approved, cancelled, changed members or memory |
| `GET /api/projects/{pid}/github`, `POST /api/projects/{pid}/github/connect` | owner for connect | GitHub App status; look up the installation of the project's repository |
| `GET /api/overview` | login | Snapshot: `{"personal":[...],"projects":[{project..., threads}]}` |
| `GET /api/events` | login | SSE live feed of what the caller may see: `thread.created`, `thread.deleted`, `run.started`, `run.finished` (with a `status` such as `waiting_approval`), `message.sent`, `member.changed`, `project.changed`, `memory.changed` |
| `GET /api/seen`, `POST /api/seen` | login | `{threadId: lastSeen}`; mark `{"threadId","at"}` as seen (204) |
| `GET /api/supervisor` | login | The caller's assistant thread |
| `GET /api/supervisor/settings`, `PUT /api/supervisor/settings` | login | `{"watch","stuckMinutes"}`; 501 `not_supported` when the supervisor is off |
| `GET /api/connectors` | login | State of the GitHub and Telegram connectors |
| `GET /api/connectors/github/connect`, `DELETE /api/connectors/github`, `GET /api/connectors/github/repos` | login | Start the OAuth flow; disconnect; the repositories the user can reach |
| `GET /auth/github/callback` (also under `/api`) | cookie | The OAuth return |
| `POST /api/projects/import` | login | `{"repo"}`: create a project from a GitHub repository |
| `POST /api/connectors/telegram/link`, `POST /api/connectors/telegram/test`, `DELETE /api/connectors/telegram` | login | Link code; test message; unlink |

Memory pages (also served by the plain single-user server, they need the Memory feature; otherwise 501 `not_supported`):

| Route | Purpose |
|---|---|
| `GET /api/memory`, `POST /api/memory` | List `{"enabled","entries"}`; create `{"title","content","tags","path","pinned"}`. `?project=<id>` selects a project's memory, with `&mine=1` the caller's own layer inside it |
| `PATCH /api/memory/{mid}`, `DELETE /api/memory/{mid}` | Update `{"title","content","tags","pinned"}`; delete |
| `POST /api/memory/{mid}/confirm`, `POST /api/memory/{mid}/reject` | Resolve an entry the agent proposed |
| `GET /api/memory/stats` | What the memory holds and what the service did |
| `GET /api/projects/{pid}/memory/export`, `POST /api/projects/{pid}/memory/import-agents` | Export a project's memory; import its `AGENTS.md` |

Thread and project shapes in team mode extend `Thread` with `projectId`, `createdBy`, `kind` and `status`.
