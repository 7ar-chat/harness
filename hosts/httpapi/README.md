# hosts/httpapi

`hosts/httpapi` (package `httpapi`) is a standalone HTTP server for any web front end, with a small browser UI
embedded. `app/cmd/harness-web` runs it:

```
harness-web -local                      # in a repository: coding agent on this directory, UI at http://127.0.0.1:8080/
HARNESS_WEB_ADDR=0.0.0.0:8080 HARNESS_WEB_TOKEN=secret OPENSANDBOX_URL=… harness-web   # remote sandbox, shared
```

`-local` sets `SANDBOX_BACKEND=local` and `AGENT_PROFILE=coding` (unless already set) and keeps sessions on disk
(`harness.OpenLocalStore`). Flags: `-addr` (default `HARNESS_WEB_ADDR`, else `127.0.0.1:8080`), `-model`, `-local`,
`-user` (the single user's login, default `local`). A non-loopback address is refused unless `HARNESS_WEB_TOKEN` is set.

**Routes** (all under `/api`, also served without the prefix; errors are `{"error":{"code","message"}}`; the full
table with bodies is the doc comment of `hosts/httpapi/httpserver.go`):

| Route | |
|---|---|
| `GET /healthz` | liveness, no auth |
| `GET /api/models` | allowed models and the default |
| `GET /api/sessions` | saved conversations (durable file store only, else 501) |
| `POST/GET /api/threads`, `GET/PATCH/DELETE /api/threads/{id}` | create, list, read with messages, rename, delete |
| `POST /api/threads/{id}/runs` | send a message, 202 with the run |
| `POST /api/threads/{id}/resume` | answer an approval (`decision` approve/deny, `always`) or an `ask_user` question (`text`) |
| `GET /api/threads/{id}/files?path=`, `.../files/download?path=` | list and download `/workspace` of the thread's sandbox (read-only, confined, `attachment`) |
| `GET /api/runs/{id}`, `GET /api/runs/{id}/events?after=` | run state; AG-UI events as SSE, resumed with `Last-Event-ID` or `after` |
| `POST /api/runs/{id}/cancel`, `/steer` | stop a run; add a message to a running one |

A foreign thread or run id is always a 404. **Auth**: `Options.Authenticate` (`func(*http.Request) (user string, err error)`)
decides who calls; the default is one local user, with an optional static bearer token (`HARNESS_WEB_TOKEN`).
`HARNESS_WEB_ORIGINS` is a comma-separated CORS allowlist; other cross-origin requests are refused (403), and without a
token the server only answers loopback `Host` names. Every response carries a strict CSP (`default-src 'self'`).

**UI** (`hosts/httpapi/ui`, `go:embed`, about 450 lines of plain HTML/CSS/JS, no build step, no CDN): conversation list,
streamed transcript with basic markdown, collapsible tool calls (edit diffs), status line (model, steps, tokens,
cache %), send / steer / cancel, approval and question prompts, a file panel with downloads, light and dark themes,
phone-width layout. With a token set the UI asks for it once and keeps it for the tab.

