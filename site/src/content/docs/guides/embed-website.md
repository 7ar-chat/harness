---
title: Embed in a website
description: "Put the harness behind your own login in a Go web application: per-user threads, a tool of your site, a sandbox only when a request needs one, and an event stream relayed to the browser."
---

Use this guide when your application already has users and sessions, and you want an agent inside it. The harness stays a library in your process: you authenticate, you decide what each request may do, and the harness runs the conversation. The complete ~250-line version is [`examples/website`](https://github.com/7ar-chat/harness/tree/main/examples/website); this page walks through its choices.

## Add it to your application

```bash
go get github.com/7ar-chat/harness@latest
```

The root module is the engine: `harness.New`, the HTTP handler, the features and the stores. Three parts that carry heavy SDKs are opt-in imports, so a program that does not use them does not link them:

| You use | Import | Without it |
|---|---|---|
| Redis (several processes) | `github.com/7ar-chat/harness/redisinfra`: `redisinfra.Infra(rdb, store)` goes in `Options.Infra` | not linked |
| MCP servers (`MCP_SERVERS`, `.mcp.json`) | `_ "github.com/7ar-chat/harness/mcpfeature"` | `harness.New` fails and names the package |
| `SANDBOX_BACKEND=opensandbox` | `_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"` | the unknown-backend error says to import it |

<div class="callout breaking">

**v0.3 removed `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra`.** Redis is now `redisinfra.NewClient(cfg)` plus `redisinfra.Infra(rdb, store)` passed as `Options.Infra`. See [Upgrade notes](/docs/operations/upgrade-notes/) and [Choose a deployment shape](/docs/guides/deployment-shapes/).

</div>

## Identity: one user id per account

Every harness call takes a user id. It is the tenancy boundary: threads, runs, sandboxes, memory and artifacts are scoped by it, and another user's id is "not found", never "forbidden". Use the stable account id (a login or numeric id), not an email address that can change.

```go
// authenticate maps the site's session to a user id. This is the only place
// that trusts a credential: verify a cookie or JWT here, never a bare header
// from the browser. (The example uses a header set by an auth proxy.)
func authenticate(r *http.Request) (string, error) {
	if u := strings.TrimSpace(r.Header.Get("X-Site-User")); u != "" {
		return u, nil
	}
	return "", errors.New("not signed in")
}
```

## A tool of your site

A tool is a `tools.Provider`; a `Feature` registers it. The provider's `Tools` method is called once per run, so the closure captures `rc.Login` and cannot read another customer's data, whatever the model is told.

```go
type ordersTools struct{ orders Orders }

func (ordersTools) Name() string { return "orders" }

func (p ordersTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	type in struct {
		OrderID string `json:"orderId" jsonschema_description:"The order number, e.g. A-1042"`
	}
	t, err := ai.InferTool("lookup_order", "Look up the status of one of the signed-in customer's orders.",
		func(ctx context.Context, a in) (string, error) {
			status, ok := p.orders.Lookup(ctx, rc.Login, a.OrderID)
			if !ok {
				return "no such order for this customer", nil // same answer for "missing" and "not yours"
			}
			return status, nil
		})
	return []ai.BaseTool{t}, func() {}, err
}

func ordersFeature(orders Orders) harness.Feature {
	return harness.FeatureFunc("orders", func(b *harness.Builder) error {
		b.AddTools(ordersTools{orders})
		return nil
	})
}
```

Build the harness with the work profile plus your feature. Leave `Backend` nil to let `SANDBOX_BACKEND` / `OPENSANDBOX_URL` choose; with neither, there are no sandbox tools at all.

```go
h, err := harness.New(harness.Options{
	Config:   cfg,
	Features: append(harness.ProfileWork(), ordersFeature(orders)),
})
go h.Run(ctx) // executes queued runs in this process
```

More on tools: [Add a custom tool](/docs/guides/custom-tool/) and [Add a custom feature](/docs/guides/custom-feature/).

## Mount the HTTP API under a prefix

`hosts/httpapi` is the ready-made JSON + SSE API (threads, runs, events, cancel, steer, resume, files). Mount it behind your own authenticator; `http.StripPrefix` makes the routes `/agent/api/...`.

```go
mux := http.NewServeMux()
mux.Handle("/agent/", http.StripPrefix("/agent", httpapi.New(h, httpapi.Options{
	Authenticate: authenticate,
	DefaultModel: cfg.LLMDefaultModel,
	NoUI:         true, // the embedded UI uses absolute /api URLs: only mount it at the root
})))
```

<div class="callout note">

**Cross-origin browsers.** Same-origin requests need nothing more. If the browser app lives on another origin, add `AllowedOrigins: []string{"https://app.example.com"}`. Behind a reverse proxy keep SSE unbuffered and idle timeouts above the 15 s ping (`X-Accel-Buffering: no` is already set).

</div>

## Your own endpoint in front of `Send`

Put a thin endpoint of your own in front of `Send` to decide what the browser must not decide: the model, the sandbox, quotas. The decision about the sandbox is made **server-side** from what the user asked for.

```go
mode := harness.SandboxOff // questions, order lookups, searches: no sandbox, no cost
if body.Code {
	mode = harness.SandboxAuto // created on the first command, not before
}
run, err := s.h.Send(ctx, user, body.ThreadID, body.Message,
	harness.RunOptions{Model: s.model, Sandbox: mode})
switch {
case errors.Is(err, harness.ErrInvalidRequest):
	http.Error(w, err.Error(), http.StatusBadRequest)
case errors.Is(err, store.ErrNotFound): // a thread of another user looks missing
	http.Error(w, "not found", http.StatusNotFound)
case errors.Is(err, store.ErrRunActive):
	http.Error(w, "a run is already in progress on this thread", http.StatusConflict)
case err != nil:
	http.Error(w, "internal error", http.StatusInternalServerError)
default:
	// 202 {"threadId", "runId"}: the browser now opens the event stream
}
```

Per-user thread lists use the same id: `s.h.Store.ListThreads(ctx, user, 50, nil)`. A new thread is `s.h.NewThread(ctx, user, "")`.

### Sandbox per run

`RunOptions.Sandbox` (JSON field `sandbox`) is stored on the run, and resumed runs keep the mode of the thread's latest run.

| Value | Tools offered | `Backend.Create` | No backend configured |
|---|---|---|---|
| `auto` (default) | sandbox tools + yours | on the first command, never before | no sandbox tools |
| `off` | yours only | never | same as `auto` |
| `on` | like `auto` | like `auto` | `Send` fails: `ErrInvalidRequest` wrapping `ErrNoSandbox` (HTTP 400) |

Use `off` for support, Q&A and API tools; `auto` when the user asks for files, data or code; `on` for a feature that cannot work without one. See [Sandboxes](/docs/concepts/sandboxes/).

<div class="callout warning">

**Do not forward a browser-supplied `sandbox` value** to a user who has not paid for sandboxes. A sandbox runs commands; its cost and risk are yours.

</div>

## Relay the event stream and resume it

If the browser can reach `/agent/api/runs/{id}/events` directly, you are done: it is a Server-Sent Events stream where each `id:` is the event's sequence number in the run's durable log. To go through your own endpoint, resolve the run with the user id first (`Harness.Events` takes only a run id), then copy `id:` lines and honour `Last-Event-ID`:

```go
func (s *site) relay(w http.ResponseWriter, r *http.Request, user string) {
	runID := r.PathValue("run")
	if _, err := s.h.Store.GetUserRun(r.Context(), user, runID); err != nil { // ownership first
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	after, _ := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	for ev := range s.h.Events(r.Context(), runID, after) {
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, ev.Data)
		fl.Flush() // flush after every event
	}
}
```

`Events` replays the log from `after`, then follows the run live, and closes after `RUN_FINISHED` or `RUN_ERROR`, so opening the stream after the run finished returns everything. The browser's `EventSource` sends `Last-Event-ID` itself on reconnect; with `fetch` use `?after=<seq>` on the built-in handler. A run that ends with `RUN_FINISHED` outcome `interrupt` is waiting for an approval or an `ask_user` answer: send it to the handler's `POST .../resume` and stream the new run it returns. Event shapes: [Event types](/docs/reference/events/), routes: [HTTP API](/docs/reference/http-api/).

## Shut down cleanly

On SIGTERM stop accepting requests, call `srv.Shutdown`, then wait for `h.Run` to return. `Run` drains in-flight runs for `RUNTIME_SHUTDOWN_GRACE_SECONDS`; the rest end `interrupted` and the user can resume them.

```go
<-ctx.Done()
sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()
_ = srv.Shutdown(sctx) // stop taking requests; open SSE streams end with the context
<-runDone              // h.Run returned: in-flight runs finished or were interrupted
```

## Proven program

The pieces above were assembled into one program (scripted model, fake sandbox backend, `httptest` server) and run. It signs in as `alice`, asks about her order, relays the stream, resumes it from a sequence number and tries to read it as `bob`:

```text
relay events        -> 16 first: RUN_STARTED last: RUN_FINISHED
tool result in stream: true
resume after id 14  -> 2 events
relay as bob        -> 404
agent API, no login -> 401
agent API as alice  -> 200
sandboxes created   -> 0
```

The last line is the point of `Sandbox: off`: the question used a tool of the site and created no sandbox. To test your own site the same way, see [Test your agent](/docs/guides/testing/).

## Before you go to production

- Rate-limit `POST .../runs` per user and per IP in your layer; the harness enforces one active run per thread (`409 conflict`) but nothing else.
- Set `HARNESS_NO_USER_DIR=1` on servers so a server account's `~/.har` never reaches tenants' runs, and supply keys through the environment.
- Set `MAX_SANDBOXES_PER_USER`, `SANDBOX_EGRESS_ALLOWLIST` and `SANDBOX_TIMEOUT_SECONDS` if you give out sandboxes; deleting a thread kills its sandbox.
- Meter usage from `RUN_FINISHED.usage` (idempotently, keyed by run id) or server-side with `Options.Hooks` / `Builder.EndOfRun`.

The full checklist is in the [Production checklist](/docs/operations/production/) and [Security model](/docs/operations/security/). To scale beyond one process, see [Choose a deployment shape](/docs/guides/deployment-shapes/).
