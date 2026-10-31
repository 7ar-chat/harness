---
title: "Smallest embedding"
description: "Run one conversation through the harness from your own Go program: first offline with a scripted model and a custom tool, then with a real model from the configuration."
---

Embedding means creating a `harness.Harness` in your process, starting its run loop and talking to it with four calls: `NewThread`, `Send`, `Events` and `Run`. Use this page to see the whole path once; [Embed in a website](/docs/guides/embed-website/) adds auth, per-user endpoints and the HTTP API.

The first variant needs no network and no API key: a scripted model calls a tool of your own and then answers with its result. This is the program `make check-embed` builds against the published module.

## The program

<div class="tabs">

<div class="tab" data-label="Scripted model">

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/engine/tools"
)

type orderArgs struct {
	ID string `json:"id" jsonschema_description:"the order id"`
}

// ordersTools is the application's own tool set: a tools.Provider is asked for its tools once per run.
type ordersTools struct{}

func (ordersTools) Name() string { return "orders" }

func (ordersTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("lookup_order", "Look up an order by id.", func(_ context.Context, in orderArgs) (string, error) {
		return "order " + in.ID + ": shipped (for " + rc.Login + ")", nil
	})
	return []ai.BaseTool{t}, nil, err
}

func main() {
	// config.Load reads LLM_* from the environment; a scripted model needs only names.
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	// A scripted model: first it calls the tool, then it answers with the tool's result.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == ai.RoleTool {
				return llmtest.Turn{Chunks: llmtest.Text("Your ", m.Content)}
			}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "lookup_order", `{"id":"A-17"}`), "tool_calls")}}
	})

	h, err := harness.New(harness.Options{
		Config: cfg,
		Models: llmtest.Factory{M: model},
		Tools:  []tools.Provider{ordersTools{}},
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }() // executes queued runs in this process

	th, err := h.NewThread(ctx, "alice", "")
	if err != nil {
		log.Fatal(err)
	}
	run, err := h.Send(ctx, "alice", th.ID, "Where is order A-17?", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
	if err != nil {
		log.Fatal(err)
	}

	var answer string
	for ev := range h.Events(ctx, run.ID, 0) {
		if ev.Type == agui.TextMessageContent {
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			answer += e.Delta
		}
		if agui.IsTerminal(ev.Type) {
			fmt.Println("terminal event:", ev.Type)
			break
		}
	}
	fmt.Println("answer:", answer)
}
```

</div>

<div class="tab" data-label="Real model">

```go
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/engine/tools"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

type orderArgs struct {
	ID string `json:"id" jsonschema_description:"the order id"`
}

type ordersTools struct{}

func (ordersTools) Name() string { return "orders" }

func (ordersTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("lookup_order", "Look up an order by id.", func(_ context.Context, in orderArgs) (string, error) {
		return "order " + in.ID + ": shipped (for " + rc.Login + ")", nil
	})
	return []ai.BaseTool{t}, nil, err
}

func ordersFeature() harness.Feature {
	return harness.FeatureFunc("orders", func(b *harness.Builder) error {
		b.AddTools(ordersTools{})
		return nil
	})
}

func main() {
	cfg, err := config.Load() // ~/.har/config.yml, <repo>/.har/config.yml, then LLM_*, SANDBOX_*, ...
	if err != nil {
		log.Fatal(err)
	}
	if err := cfg.RequireLLMKey(); err != nil {
		log.Fatal(err)
	}
	h, err := harness.New(harness.Options{
		Config:   cfg, // models and sandbox backend come from the configuration
		Features: append(harness.ProfileWork(), ordersFeature()),
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runDone := make(chan error, 1)
	go func() { runDone <- h.Run(ctx) }()

	mux := http.NewServeMux()
	mux.Handle("/agent/", http.StripPrefix("/agent", httpapi.New(h, httpapi.Options{
		Authenticate: func(r *http.Request) (string, error) { // your session check goes here
			if u := r.Header.Get("X-Site-User"); u != "" {
				return u, nil
			}
			return "", errors.New("not signed in")
		},
		DefaultModel: cfg.LLMDefaultModel,
		NoUI:         true,
	})))
	srv := &http.Server{Addr: "127.0.0.1:8080", Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.ListenAndServe() }()

	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	<-runDone
}
```

</div>

</div>

Run the scripted program in an empty module:

```bash
mkdir try && cd try && go mod init example.com/try
# save the program as main.go
go mod tidy && go run .
```

The output is:

```text
terminal event: RUN_FINISHED
answer: Your order A-17: shipped (for alice)
```

The harness also logs run start and finish on stderr (`INFO run started ...`); the two lines above are stdout. This sample was run against the published `v0.3.1` modules as well as the local checkout.

## What each call does

**`config.Load()`** builds a `*config.Config` from built-in defaults, `~/.har/config.yml`, the nearest `<repo>/.har/config.yml` and the process environment, in that order of increasing priority. See [Configuration files](/docs/reference/configuration/). The scripted program sets the three `LLM_*` variables itself only so that it does not depend on your files.

**`llmtest.NewFunc` and `llmtest.Factory{M: model}`** replace the real model. The function receives every model call (`c.Messages`) and returns one scripted turn: here a tool call to `lookup_order`, then, once a tool message is in the history, a text answer. `Options.Models` is the `llm.Factory` that builds chat models; leave it nil and the harness builds the provider named by `LLM_PROVIDER`. See [Test your agent](/docs/guides/testing/).

**`tools.Provider`** is how your application adds tools. `Tools` is called once per run with a `tools.RunContext`; `rc.Login` is the user the run belongs to, so the closure cannot read another tenant's data. `ai.InferTool` derives the JSON schema from the argument struct. See [Add a custom tool](/docs/guides/custom-tool/) and [Tools](/docs/concepts/tools/).

**`harness.New(harness.Options{...})`** wires the engine. Only `Config` is required. With no `Store` it keeps everything in memory (lost on restart), with no `Infra` the queue and event log live in the process, and with no `Backend` it picks one from `SANDBOX_BACKEND` or `OPENSANDBOX_URL`, else none: the agent then has no sandbox tools and a prompt that says so. That is why the scripted run needs no sandbox. See [Infrastructure](/docs/concepts/infra/) and [The store](/docs/concepts/store/).

**`go h.Run(ctx)`** consumes queued runs and executes them in this process. A web tier that only calls `Send` and leaves execution to workers does not call it; see [Postgres and Redis for several processes](/docs/guides/postgres-redis/).

**`h.NewThread(ctx, user, title)`** creates a conversation. Every method takes the user id, which is the tenancy boundary: a foreign id is "not found", never "forbidden".

**`h.Send(ctx, user, threadID, text, harness.RunOptions{...})`** queues a run and returns it at once. `RunOptions.Sandbox` is `SandboxAuto` (the default), `SandboxOff` or `SandboxOn`; `SandboxOff` gives the run only your tools and the no-sandbox prompt. See [Sandboxes](/docs/concepts/sandboxes/).

**`h.Events(ctx, run.ID, after)`** returns a channel of the run's events from the durable log, starting after sequence number `after` (0 for all). Each event is an [AG-UI](https://docs.ag-ui.com) event whose `Data` is JSON; the program adds up the `delta` of every `TEXT_MESSAGE_CONTENT` and stops at the first terminal event (`agui.IsTerminal`). `ev.Seq` is the resume point for a reconnect. See [Threads, runs and events](/docs/concepts/threads-runs-events/) and [Event types](/docs/reference/events/).

Other calls on the same `Harness` are `Steer` (add a message to a running run), `Cancel` and `Resume` (answer an approval or an `ask_user` question). See [The Go facade](/docs/reference/harness-api/).

## With a real model

The second tab is the shape of `examples/website`, cut down to its core. There is no scripted model and no explicit `Models` or `Backend`: `config.Load()` supplies the provider, base URL, key and models from your files and environment, and `harness.New` builds the model clients and the sandbox backend from it. Two other differences:

- `Features: append(harness.ProfileWork(), ordersFeature())` installs the default work profile and adds a `harness.Feature` that registers the tool with `b.AddTools`. A feature is the unit for adding tools, prompt sections and hooks; see [Features and profiles](/docs/concepts/features-profiles/) and [Add a custom feature](/docs/guides/custom-feature/).
- `httpapi.New(h, httpapi.Options{...})` is the ready-made JSON and Server-Sent Events API. `Authenticate` maps a request to your user id; it is the only place that trusts a credential, so verify a session or token there, never a bare header from a browser as this sketch does. `NoUI: true` leaves out the embedded browser UI because the site has its own. See [Embed in a website](/docs/guides/embed-website/) and [HTTP API](/docs/reference/http-api/).

To run it, give `config.Load` a model:

```bash
export LLM_BASE_URL=https://api.openai.com/v1 LLM_API_KEY=sk-... \
       LLM_MODELS=gpt-5 LLM_DEFAULT_MODEL=gpt-5
go run .
```

then, from another shell:

```bash
curl -s -X POST -H 'X-Site-User: alice' localhost:8080/agent/api/threads
# {"id":"<thread id>", ...}
curl -s -X POST -H 'X-Site-User: alice' -d '{"message":"Where is order A-17?","sandbox":"off"}' \
  localhost:8080/agent/api/threads/<thread id>/runs
# {"id":"<run id>", ...}
curl -N -H 'X-Site-User: alice' localhost:8080/agent/api/runs/<run id>/events
```

The last request streams the run's events as SSE. The server shape of this tab was exercised against a local OpenAI-compatible stub; no real provider was called.

<div class="callout warning">

**Do not trust a request header in production.** `X-Site-User` stands in for your session check. Anything the browser can set can be forged. Also set `HARNESS_NO_USER_DIR=1` on servers so a server account's `~/.har` is never read; see [Production checklist](/docs/operations/production/).

</div>

<div class="callout note">

**Opt-in imports.** MCP servers in the configuration need `_ "github.com/7ar-chat/harness/mcpfeature"`, and `SANDBOX_BACKEND=opensandbox` needs `_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"`; without them `harness.New` fails with an error that names the package. Redis is `redisinfra.Infra(rdb, store)` passed as `Options.Infra`. See the [module map](/docs/introduction/modules/).

</div>

Next: [Embed in a website](/docs/guides/embed-website/), or [choose a deployment shape](/docs/guides/deployment-shapes/).
