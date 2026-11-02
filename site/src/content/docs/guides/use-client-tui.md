---
title: "Use hosts/client and hosts/tui on their own"
description: "Write a front end (terminal UI, Slack bot, desktop app) against the narrow client.Client contract over HTTP, and reuse the Bubble Tea terminal UI with any backend."
---

`github.com/7ar-chat/harness/hosts/client` is the contract a front end is written against: threads, runs, a resumable event stream, steering, cancelling, answering approvals and questions, and a thread's files. `github.com/7ar-chat/harness/hosts/tui` is a terminal UI built on it. Use them when you build a chat front end (a Slack bot, a desktop app, a CLI) for a harness server you run elsewhere, without importing the engine. Both are separate modules; `tui` imports only `client` and `core/agui`.

```bash
go get github.com/7ar-chat/harness/hosts/client
go get github.com/7ar-chat/harness/hosts/tui   # only for the terminal UI
```

`client` pulls in `core` and `modelinfo`; with `tui` the dependency list is `core/agui`, `core/store`, `modelinfo`, `hosts/client` and `hosts/tui` (checked with `go list -deps`). The engine is not linked.

## One front end, any client

A `Client` acts for one user. There are two implementations that pass the same conformance suite (`hosts/clienttest`), so a front end behaves the same on either:

| Implementation | Where the engine is |
|---|---|
| `client.HTTP(baseURL, token)` | a remote server: the JSON + Server-Sent Events API of `harness-web` or any `hosts/httpapi` mount. `token` is sent as `Authorization: Bearer` |
| `inproc.New(h, user)` | the same process (`hosts/inproc`, in the main module) |

Swap one line to move from embedded to remote. The program below is a whole front end: it sends one message and prints the answer as it streams. It only knows `client.Client`.

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/tui"
)

// ask is a whole front end: it sends one message and prints the answer as it streams.
// It only knows client.Client, so it works on any implementation of it.
func ask(ctx context.Context, c client.Client, question string) error {
	th, err := c.CreateThread(ctx, "")
	if err != nil {
		return err
	}
	run, err := c.Send(ctx, th.ID, question, client.SendOptions{})
	if err != nil {
		return err
	}
	events, err := c.Events(ctx, run.ID, 0)
	if err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case agui.TextMessageContent:
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			fmt.Print(e.Delta)
		case agui.RunFinished, agui.RunError:
			fmt.Println()
		}
	}
	return nil
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := client.HTTP("http://127.0.0.1:18101", "secret") // a remote harness server and its bearer token

	if err := ask(ctx, c, "Say hello"); err != nil {
		log.Fatal(err)
	}

	// Errors are *client.Error values: test them with errors.Is.
	_, err := c.GetThread(ctx, "no-such-thread")
	fmt.Println("not found:", errors.Is(err, client.ErrNotFound), "| status:", client.AsError(err).Status, "| code:", client.AsError(err).Code)
	_, err = client.HTTP("http://127.0.0.1:18101", "wrong").ListThreads(ctx, 10)
	fmt.Println("unauthenticated:", errors.Is(err, client.ErrUnauthenticated))

	// Optional operations live in client.Extras; check with a type assertion.
	if x, ok := c.(client.Extras); ok {
		cmds, err := x.Commands(ctx)
		fmt.Println("commands:", len(cmds), "| err:", err)
	}

	// The terminal UI's print mode over the same client: the answer on stdout, an exit code.
	th, _ := c.CreateThread(ctx, "")
	be := &tui.ClientBackend{C: c, ThreadID: th.ID, ModelID: "gpt-test"}
	code := tui.RunPrint(ctx, be, "Say hello again", tui.PrintOptions{Stdout: os.Stdout, Stderr: os.Stderr})
	fmt.Println("exit code:", code)
}
```

Run against a server with a scripted model (below), it prints:

```text
Hello from the engine
not found: true | status: 404 | code: not_found
unauthenticated: true
commands: 10 | err: <nil>
Hello from the engine
exit code: 0
```

`ask` is the same function as the `ask` of the repository's `integration/client/example_test.go`, which runs it over `inproc.New`.

<div class="tabs">

<div class="tab" data-label="Server for the demo">

```go
package main

import (
	"context"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/hosts/httpapi"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "gpt-test")
	cfg, _ := config.Load()
	model := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: llmtest.Text("Hello ", "from the engine")}
	})
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend()})
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = h.Run(context.Background()) }()
	log.Fatal(http.ListenAndServe("127.0.0.1:18101", httpapi.New(h, httpapi.Options{
		Token: "secret", Models: []string{"gpt-test"}, DefaultModel: "gpt-test", NoUI: true,
	})))
}
```

</div>

<div class="tab" data-label="Real server">

```bash
HARNESS_WEB_TOKEN=secret LLM_API_KEY=... harness-web -addr 127.0.0.1:8080
```

</div>

</div>

For a real server see [harness-web](/docs/reference/cli-harness-web/) and [Run the web product](/docs/quickstart/web/). The routes behind `client.HTTP` are on [HTTP API](/docs/reference/http-api/).

## The Client contract

| Method | What it does |
|---|---|
| `Models(ctx)` | the model ids runs may use, the default, and optional `Info` (window, prices, capabilities) by id |
| `CreateThread(ctx, title)` | start a conversation (empty title: titled from the first message) |
| `ListThreads(ctx, limit)`, `GetThread`, `RenameThread`, `DeleteThread` | manage threads; `GetThread` includes messages, `PendingApproval`, `PendingQuestion` and `ActiveRun` |
| `Send(ctx, threadID, text, SendOptions)` | add a user message and queue a run; follow it with `Events` |
| `GetRun(ctx, runID)` | a run's status |
| `Events(ctx, runID, after)` | the run's AG-UI events with `Seq > after`, then follows it live and closes the channel after the terminal event or when `ctx` ends |
| `Steer(ctx, runID, text)` | add a message to a running run; `ErrRunNotActive` when it already ended |
| `Cancel(ctx, runID)` | stop a run (a finished run is left alone) |
| `Resume(ctx, threadID, ResumeRequest)` | answer the interrupt a run ended with (a tool approval or an `ask_user` question) and queue the continuing run |
| `ListFiles`, `DownloadFile` | a thread's workspace |
| `Sessions(ctx, limit)` | saved conversations; `ErrNotSupported` without a durable store |

`SendOptions` carries `Model`, `ReasoningEffort`, `Skills`, `Interactive` (risky tool calls wait for a decision), `Mode` (`client.ModeBuild` or `client.ModePlan`), `Attachments` and `Sandbox` (`auto`, `off`, `on`). The field-level reference is [The Client contract](/docs/reference/client-contract/).

### Events

An `Event` is `{Seq int64, Type string, Data json.RawMessage}`: `Data` is the full AG-UI event JSON. `Seq` numbers a run's events from 1 and is the resume point. `Events` reconnects and resumes from the last `Seq` it received, so the stream has no gap and no duplicate; when the connection cannot be recovered the channel ends with a synthetic `RUN_ERROR` event with code `stream_lost`. Use the `core/agui` constants (`agui.TextMessageContent`, `agui.ToolCallStart`, `agui.RunFinished`, ...) to switch on `Type`; the catalogue is [Event types](/docs/reference/events/).

### Errors

Errors are `*client.Error` values with an HTTP status and a code, the same whether they come from the engine in process or from a server's `{"error":{"code","message"}}` answer. Test them with `errors.Is`:

| Value | Status | Code |
|---|---|---|
| `ErrUnauthenticated` | 401 | `unauthenticated` |
| `ErrNotFound` | 404 | `not_found` |
| `ErrConflict` | 409 | `conflict` |
| `ErrInvalidRequest`, `ErrInvalidPath` | 400 | `invalid_request`, `invalid_path` |
| `ErrRunNotActive` | 409 | `run_not_active` |
| `ErrTooManySteers` | 429 | `too_many_steers` |
| `ErrNotSupported` | 501 | `not_supported` |
| `ErrNoSandbox` | 501 | `no_sandbox` |
| `ErrTooLarge` | 413 | `too_large` |

`client.AsError(err)` returns the `*Error` for its `Status` and `Code`; `client.Errorf` and `client.Wrap` build your own from a sentinel. A user never sees another user's threads: they are reported as `ErrNotFound`.

### Extras

Operations a server may lack are in `client.Extras`: `Compact`, `Undo`, `Redo`, `Fork`, `Commands`, `ExpandCommand`. Check with a type assertion (`c.(client.Extras)`); an implementation or an older server answers `ErrNotSupported`, so a newer front end still works against it. Both `HTTP` and `inproc.New` implement it.

### HTTP options

| Option | Effect |
|---|---|
| `client.WithHTTPClient(c)` | the `http.Client` used (its `Timeout` must be zero: event streams are long-lived) |
| `client.WithBackoff(min, max)` | wait between stream reconnections: starts at `min`, doubles up to `max` (defaults 200 ms and 5 s) |
| `client.WithMaxReconnects(n)` | consecutive failed reconnections before giving up (default 8); one that delivers an event resets the count |

## A Slack bot or desktop app

A chat front end has the same shape as `ask`: keep a map from conversation to thread, `Send` each incoming message, collect the answer from `TEXT_MESSAGE_CONTENT` events and post it. For tool approvals, send with `Interactive: true`; a run that needs a decision finishes with an interrupt, the thread's `PendingApproval` holds the request (its `id` is the `InterruptID`), and `Resume` carries the user's choice. The sketch below is a working skeleton of that, with the chat library left out: its message handler calls `Reply`, an "approve" button calls `Approve`.

```go
// bot maps a chat channel to a thread and an answer to a message.
type bot struct {
	c       client.Client
	mu      sync.Mutex
	threads map[string]string // channel id -> thread id
}

func (b *bot) thread(ctx context.Context, channel string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id, ok := b.threads[channel]; ok {
		return id, nil
	}
	th, err := b.c.CreateThread(ctx, "chat "+channel)
	if err != nil {
		return "", err
	}
	b.threads[channel] = th.ID
	return th.ID, nil
}

// Reply sends the message and returns the whole answer. A run that ends waiting for
// the user is reported instead of answered.
func (b *bot) Reply(ctx context.Context, channel, text string) (string, error) {
	id, err := b.thread(ctx, channel)
	if err != nil {
		return "", err
	}
	run, err := b.c.Send(ctx, id, text, client.SendOptions{Interactive: true})
	if err != nil {
		return "", err
	}
	events, err := b.c.Events(ctx, run.ID, 0)
	if err != nil {
		return "", err
	}
	var answer strings.Builder
	for ev := range events {
		switch ev.Type {
		case agui.TextMessageContent:
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			answer.WriteString(e.Delta)
		case agui.RunError:
			var e struct{ Code, Message string }
			_ = json.Unmarshal(ev.Data, &e)
			return "", fmt.Errorf("run failed: %s: %s", e.Code, e.Message)
		case agui.RunFinished:
			th, err := b.c.GetThread(ctx, id)
			if err == nil && th.PendingApproval != nil {
				return fmt.Sprintf("%s\n(waiting for approval of %v)", answer.String(), th.PendingApproval["toolName"]), nil
			}
		}
	}
	return answer.String(), nil
}

// Approve answers a tool approval the thread is waiting for and returns the run that continues.
func (b *bot) Approve(ctx context.Context, channel string) (*client.Run, error) {
	id, err := b.thread(ctx, channel)
	if err != nil {
		return nil, err
	}
	th, err := b.c.GetThread(ctx, id)
	if err != nil {
		return nil, err
	}
	if th.PendingApproval == nil {
		return nil, client.Errorf(client.ErrConflict, "nothing to approve")
	}
	approval, _ := th.PendingApproval["id"].(string)
	return b.c.Resume(ctx, id, client.ResumeRequest{
		InterruptID: approval, Reason: client.ReasonToolApproval, Decision: client.DecisionApprove,
	})
}
```

Against the demo server, `Reply(ctx, "C123", "Say hello")` returns `"Hello from the engine"` and `Approve` with nothing pending returns `conflict: nothing to approve`. The approval path itself (a pending tool call) was not exercised: the scripted model calls no tools.

<div class="callout warning">

**Authenticate the user, not just the bot.** A `Client` acts for one user. A bot that serves several people through one token makes them all the same user and shares their threads. Use one token per person (a server whose `Authenticate` maps tokens to users), or run a harness per tenant. See [Security model](/docs/operations/security/).

</div>

## The terminal UI

`tui.Run(ctx, backend, tui.Options{...})` is the interactive Bubble Tea UI: streaming answers rendered as Markdown, expandable tool calls, diffs, approvals and questions, plans, reasoning, a status line with tokens and cost, model switching and input history. Its parts:

| Piece | What it is |
|---|---|
| `tui.Backend` | what the UI needs from a harness: `Model`, `Send`, `Events`, `Steer`, `Cancel`, `Resume`. One thread, one model |
| `tui.ClientBackend` | the `Backend` over any `client.Client`: set `C`, `ThreadID`, `ModelID`, and optionally `Interactive`, `ReasoningEffort`, `RunMode`, `RemoteURL` |
| `tui.Model` | the Bubble Tea model (`tui.New(backend, options)`); pure with respect to the world, events arrive as messages and every effect goes through the `Backend` |
| `tui.RunPrint(ctx, backend, prompt, PrintOptions)` | the non-interactive mode for scripts: the answer on stdout, tool activity on stderr, and an exit code (`ExitOK` 0, `ExitFailed` 1, `ExitInterrupted` 2 when the run waits for the user). `PrintOptions.JSON` prints the events as JSON lines |
| `tui.Options` | `StateDir` (theme, recent models, input history; empty keeps it in memory), `ConfigData` (the text of a `tui.json`), `Workspace` (root of `@file` mentions and `!commands`; empty for a remote session), `Connector`, `Attention` |

The optional interfaces (`ModelBackend`, `SessionBackend`, `CommandBackend`, `HistoryBackend`, `StatusBackend`, ...) light up palette entries such as model switching, sessions and undo when the backend implements them; `ClientBackend` implements them through `client.Extras`.

```go
be := &tui.ClientBackend{
	C:           client.HTTP("https://agent.example.com", token), // any client.Client
	ThreadID:    thread.ID,
	ModelID:     "gpt-4o",
	Interactive: true, // risky tool calls wait for approval
}
err := tui.Run(ctx, be, tui.Options{StateDir: stateDir})
```

To drive something other than a `client.Client`, implement `tui.Backend` yourself. A fixed backend that satisfies it, and the model built over it, compile and render:

```go
type fixed struct{}

func (fixed) Model() string { return "demo" }
func (fixed) Send(context.Context, string) (string, error) { return "run-1", nil }
func (fixed) Events(context.Context, string) <-chan client.Event {
	ch := make(chan client.Event)
	close(ch)
	return ch
}
func (fixed) Steer(context.Context, string, string) error                { return nil }
func (fixed) Cancel(context.Context, string) error                       { return nil }
func (fixed) Resume(context.Context, tui.ResumeRequest) (string, error) { return "run-2", nil }

m := tui.New(fixed{}, tui.Options{StaticCursor: true}) // m.View() is the rendered screen
```

The render functions (`RenderMarkdown`, `RenderDiff`, `RenderToolCall`, `RenderStatus`, ...) are exported and pure, for reuse in your own UI. Settings come from a `tui.json` (`Options.ConfigData`). The shipped `harness` command is this package wired to a local engine or a remote server: [harness (CLI and TUI)](/docs/reference/cli-harness/) and [Run the CLI and TUI](/docs/quickstart/cli/).

The prices and context windows a UI shows come from `Models().Info` and `client.ModelInfo`; where they come from is described in [Use core and modelinfo on their own](/docs/guides/use-core-modelinfo/).

## Checks

The programs on this page were built and run in scratch modules: `ask`, the error checks, `Extras.Commands` and `tui.RunPrint` against a live server (a `hosts/httpapi` mount with a scripted model), the bot skeleton and the fixed `tui.Backend`. They were also vetted with `GOWORK=off` against the published module versions.
