---
title: "The Client contract"
description: "The hosts/client.Client interface, its DTOs and errors, the optional Extras, the in-process and HTTP implementations and the clienttest conformance suite."
---

`hosts/client` is the narrow contract between a front end (terminal UI, web UI, Slack bot, desktop app) and the harness engine. Write your front end against `client.Client` and it works the same in process (`inproc.New`) and against a remote [harness-web](/docs/reference/cli-harness-web/) (`client.HTTP`). It is a small module of its own (`github.com/7ar-chat/harness/hosts/client`); see [Use hosts/client and hosts/tui on their own](/docs/guides/use-client-tui/).

A `Client` acts for one user. The in-process one is built for a login; the HTTP one authenticates with a bearer token. Threads and runs of other users are reported as `ErrNotFound`.

## One function, two implementations

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http/httptest"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/hosts/client"
	"github.com/7ar-chat/harness/hosts/httpapi"
	"github.com/7ar-chat/harness/hosts/inproc"
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "demo")
	os.Setenv("LLM_DEFAULT_MODEL", "demo")
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	model := llmtest.NewFunc(func(llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: llmtest.Text("Hello from ", "the scripted model.")}
	})
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}})
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	go func() { _ = h.Run(ctx) }()

	// The same function drives any implementation of client.Client.
	converse := func(name string, c client.Client) {
		th, err := c.CreateThread(ctx, "demo")
		if err != nil {
			log.Fatal(err)
		}
		run, err := c.Send(ctx, th.ID, "hi", client.SendOptions{})
		if err != nil {
			log.Fatal(err)
		}
		events, err := c.Events(ctx, run.ID, 0)
		if err != nil {
			log.Fatal(err)
		}
		answer := ""
		for ev := range events { // closed after RUN_FINISHED or RUN_ERROR
			if ev.Type == agui.TextMessageContent {
				var e struct{ Delta string }
				_ = json.Unmarshal(ev.Data, &e)
				answer += e.Delta
			}
		}
		fmt.Printf("%s: %q\n", name, answer)

		_, err = c.GetThread(ctx, "no-such-thread")
		fmt.Println(name, "not found:", errors.Is(err, client.ErrNotFound))
	}

	// 1. In process: no network.
	converse("inproc", inproc.New(h, "alice", inproc.WithModels(cfg.LLMModels, cfg.LLMDefaultModel)))

	// 2. Over HTTP: the same Client against hosts/httpapi.
	srv := httptest.NewServer(httpapi.New(h, httpapi.Options{
		Token: "secret", LocalUser: "alice", Models: cfg.LLMModels, DefaultModel: cfg.LLMDefaultModel, NoUI: true,
	}))
	defer srv.Close()
	converse("http", client.HTTP(srv.URL, "secret"))

	// Optional operations: assert client.Extras.
	if ex, ok := client.HTTP(srv.URL, "secret").(client.Extras); ok {
		cmds, err := ex.Commands(ctx)
		fmt.Println("commands:", len(cmds) > 0, err)
	}
	_, err = client.HTTP(srv.URL, "wrong").Models(ctx)
	fmt.Println("wrong token:", errors.Is(err, client.ErrUnauthenticated), client.AsError(err).Status)
}
```

Output (the harness's own log lines go to stderr):

```text
inproc: "Hello from the scripted model."
inproc not found: true
http: "Hello from the scripted model."
http not found: true
commands: true <nil>
wrong token: true 401
```

## The interface

| Method | Purpose |
|---|---|
| `Models(ctx) (ModelList, error)` | Model ids runs may use, the default one, and (when the server has a model table) `Info` per id. |
| `CreateThread(ctx, title) (*Thread, error)` | Start a conversation; an empty title is set from the first message. |
| `ListThreads(ctx, limit) ([]Thread, error)` | The user's threads, pinned first then newest (`limit` ≤ 0: 50; at most 200). |
| `GetThread(ctx, id) (*Thread, error)` | A thread with its messages, pending approval or question and active run. |
| `RenameThread(ctx, id, ThreadPatch) (*Thread, error)` | Change the title and/or the pinned flag. |
| `DeleteThread(ctx, id) error` | Remove a thread and its runs. |
| `Send(ctx, threadID, text, SendOptions) (*Run, error)` | Add a user message and queue a run answering it. |
| `GetRun(ctx, runID) (*Run, error)` | A run's status. |
| `Events(ctx, runID, after int64) (<-chan Event, error)` | The run's [AG-UI events](/docs/reference/events/) with `Seq > after`, then live; the channel closes after the terminal event or when `ctx` is done. A broken connection is resumed from the last `Seq` with no gap and no duplicate. |
| `Steer(ctx, runID, text) error` | Add a message to a running run; the model sees it at its next step. `ErrRunNotActive` when the run ended. |
| `Cancel(ctx, runID) error` | Stop a run (a finished run is left alone). |
| `Resume(ctx, threadID, ResumeRequest) (*Run, error)` | Answer the interrupt a run ended with (a tool approval or an `ask_user` question) and queue the run that continues. |
| `ListFiles(ctx, threadID, path) ([]File, error)` | List a directory of the thread's workspace (`""` is its root). |
| `DownloadFile(ctx, threadID, path) (*Download, error)` | A file of the thread's workspace. |
| `Sessions(ctx, limit) ([]Session, error)` | Saved conversations. `ErrNotSupported` without a durable store. |

The terminal event of a run is `RUN_FINISHED` or `RUN_ERROR`. An `RUN_FINISHED` with outcome `interrupt` means the run waits for `Resume`.

## DTOs

The types are the wire format of [`hosts/httpapi`](/docs/reference/http-api/); changing a JSON tag changes the API. The ones that carry data you will read:

| Type | Fields |
|---|---|
| `Thread` | `ID`, `Title`, `Pinned`, `CreatedAt`, `UpdatedAt`, `ActiveRun` (`*ActiveRun{ID,Status}`, nil when idle), `PendingApproval` (`map[string]any`, its `id` is the `InterruptID` of the `Resume`), `PendingQuestion` (`*PendingQuestion`), `Messages` (set by `GetThread` only). |
| `Message` | `ID`, `RunID`, `Role` (`user`, `assistant`, `tool`), `Content`, `ToolCalls`, `ToolCallID`, `Error`, `Metadata`, `CreatedAt`, `Author` (`*Author{Login,Kind,OnBehalfOf,Name,AvatarURL}` in shared threads). |
| `Run` | `ID`, `ThreadID`, `Status` (`queued`, `running`, `completed`, `failed`, `cancelled`, `interrupted`), `Model`, `Error`, `ErrorCode`, `LastSeq`, `CreatedAt`, `Sandbox`. |
| `SendOptions` | `Model`, `ReasoningEffort`, `Skills`, `Interactive`, `Mode` (`ModeBuild` / `ModePlan`), `Attachments` (`[]Attachment{Name,MIME,Data}`), `Sandbox` (`auto`, `off`, `on`). |
| `ResumeRequest` | `InterruptID`, `Reason` (`ReasonToolApproval` or `ask_user`), `Decision` (`DecisionApprove`, `DecisionDeny`, `DecisionRespond`), `Always`, `Text`, `Model`, `ReasoningEffort`, `Interactive`, `Mode`. |
| `ThreadPatch` | `Title *string`, `Pinned *bool`; nil fields stay as they are. |
| `Event` | `Seq` (from 1, the resume point), `Type`, `Data` (`json.RawMessage`, the full event JSON including its type and timestamp). |
| `File` / `Download` | `Name`, `Path`, `Type` (`file` or `directory`), `Size`, `ModifiedAt` / `Path`, `Name`, `Data`. |
| `Session` | `ID`, `Title`, `Preview`, `Messages`, `LastStatus`, `Pinned`, `CreatedAt`, `UpdatedAt`. |
| `ModelList` / `ModelInfo` | `Models`, `Default`, `Info` / context window, max output, per-token prices (`Cost` computes dollars), vision, reasoning and tool-use flags. `TableLookup` and `FromModelInfo` build them from a `modelinfo` table. |
| `Command` | `Name`, `Description`, `Source` (`project`, `user`, `skill`, `builtin`), `ArgHint`. |

`PendingQuestion` (`InterruptID`, `RunID`, `ToolCallID`, `Question`, `Options`, `AllowFreeText`), `ActiveRun`, `QuestionOption` and `ToolCall` are aliases of the engine's own `core/store` types, so there is one definition.

## Errors

Errors are `*client.Error` values with `Status`, `Code` and `Message`; the in-process and HTTP clients return the same value. Test with `errors.Is` against the sentinels: `Is` matches errors of the same code, so a specific message does not break the match. `client.AsError(err)` returns the `*Error` (nil when it is another error).

| Sentinel | Status | Code | Message |
|---|---|---|---|
| `ErrUnauthenticated` | 401 | `unauthenticated` | Authentication required. |
| `ErrNotFound` | 404 | `not_found` | Not found. (Also what another user's thread or run gives.) |
| `ErrConflict` | 409 | `conflict` | Conflict. (The thread has a run in progress.) |
| `ErrInvalidRequest` | 400 | `invalid_request` | Invalid request. |
| `ErrInvalidPath` | 400 | `invalid_path` | Invalid path. |
| `ErrRunNotActive` | 409 | `run_not_active` | The run no longer accepts messages. |
| `ErrTooManySteers` | 429 | `too_many_steers` | Too many pending messages. |
| `ErrNotSupported` | 501 | `not_supported` | The store has no session listing. |
| `ErrNoSandbox` | 501 | `no_sandbox` | This server has no sandbox. |
| `ErrTooLarge` | 413 | `too_large` | The file is too large to download. |

`Errorf(sentinel, format, ...)` returns a copy with a specific message and `Wrap(sentinel, message, err)` keeps the cause for `errors.Unwrap`. Anything else is a 500 with code `internal`. An HTTP answer outside the error envelope becomes code `http_<status>`.

## Extras

`client.Extras` holds the operations beyond the core contract. Both `inproc.New` and `client.HTTP` implement it; check with a type assertion. An implementation or an older server that lacks one answers `ErrNotSupported`, so a newer front end still works against an older server.

| Method | Purpose |
|---|---|
| `Compact(ctx, threadID) (*CompactResult, error)` | Summarise the history now (`Compacted`, `TokensBefore`, `TokensAfter`). |
| `Undo(ctx, threadID) (*UndoResult, error)` | Remove the last user message with the answers after it and restore the workspace files to the snapshot taken before that run (a git workspace). `UndoResult` has `Prompt`, `Files`, `CanUndo`, `CanRedo`. |
| `Redo(ctx, threadID) (*UndoResult, error)` | Reapply the last undone step, until a new message is sent. |
| `Fork(ctx, threadID, messageID) (*Thread, error)` | Copy the thread up to and including `messageID` (`""`: the whole thread) into a new thread. |
| `Commands(ctx) ([]Command, error)` | The custom commands and skills invocable by name. |
| `ExpandCommand(ctx, name, args) (string, error)` | The prompt of `/name args`. |

The history operations need an idle thread: they return `ErrConflict` while a run is active.

## Implementations

### `inproc.New`

```go
func New(h *harness.Harness, user string, opts ...Option) client.Client
```

| Option | Effect |
|---|---|
| `WithModels(models, def)` | The ids `Models` reports and the default used when `SendOptions.Model` is empty (the engine does not expose its configuration). |
| `WithModelInfo(lookup)` | Where `Models` gets the description of each model; without it the list carries none. |
| `WithAuthor(harness.Author)` | Who messages and steers are recorded as coming from, when that is not the thread owner (a project member or a supervisor). |
| `WithProjectDir(dir)` | The directory `Commands` looks for the project's `.har/commands` from (default: the working directory). |

### `client.HTTP`

```go
func HTTP(baseURL, token string, opts ...HTTPOption) Client
```

`baseURL` is the server (`http://127.0.0.1:8080`); requests go to `<baseURL>/api/...`. `token` is the bearer token, empty for none.

| Option | Default | Effect |
|---|---|---|
| `WithHTTPClient(*http.Client)` | no overall timeout | The client used for requests. Its `Timeout` must be zero because event streams are long-lived. |
| `WithBackoff(min, max)` | 200 ms, 5 s | Wait between reconnections of an event stream; doubles up to `max`. |
| `WithMaxReconnects(n)` | 8 | Consecutive failed reconnections tolerated before giving up. A reconnection that delivers an event resets the count. |

<div class="callout note">

**`stream_lost`.** When an event stream cannot be recovered, the channel ends with a synthetic `RUN_ERROR` event with code `stream_lost`, so a consumer waiting for a terminal event stops. See [Event types](/docs/reference/events/#stream_lost).

</div>

## Testing an implementation

`hosts/clienttest` is the conformance suite: any implementation (in process, over HTTP, a recording replay, a gRPC client) must pass it.

```go
func TestMyClient(t *testing.T) {
	clienttest.Run(t, func(t *testing.T, h *harness.Harness, user string) client.Client {
		return inproc.New(h, user, inproc.WithModels(clienttest.Models, clienttest.DefaultModel))
	})
}
```

The suite builds its own harness (a scripted model, a fake sandbox) and asks the factory for clients of two users over it. It covers models, the thread lifecycle, send and stream to `RUN_FINISHED`, sandbox mode, resuming the stream from a `seq`, steering, cancelling, the `ask_user` round trip, validation, ownership, sessions without a durable store, the approval round trip and files; on unix also undo, redo, fork, compact and the local-sandbox approval flow. See [Test your agent](/docs/guides/testing/).
