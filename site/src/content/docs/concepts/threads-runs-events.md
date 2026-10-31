---
title: Threads, runs and events
description: "The data model every front end shares (a thread holds messages, each message sent starts a run, a run streams numbered AG-UI events) and the Harness calls that drive it."
---

A **thread** is a conversation owned by one user. Sending a message to a thread queues a **run**: one execution of the [agent loop](/docs/concepts/agent-loop/). The run writes an ordered log of **events** (the AG-UI protocol) that you stream to a UI, replay later, or ignore. Everything in the harness (the TUI, the HTTP API, your own code) is built from the six facade calls below.

## A run, end to end

This program runs offline: the model is the scripted `llmtest` model, which first calls a tool and then answers with its result. It prints every event of the run with its `seq`, then replays the tail of the log.

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/tools"
)

type lookupArgs struct {
	ID string `json:"id" jsonschema_description:"the order id"`
}

type orders struct{}

func (orders) Name() string { return "orders" }
func (orders) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("lookup_order", "Look up an order.", func(_ context.Context, in lookupArgs) (string, error) {
		return "order " + in.ID + " shipped", nil
	})
	return []ai.BaseTool{t}, nil, err
}

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	check(err)

	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == ai.RoleTool {
				return llmtest.Turn{Chunks: llmtest.Text("It ", m.Content, ".")}
			}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "lookup_order", `{"id":"A-17"}`), "tool_calls")}}
	})

	h, err := harness.New(harness.Options{
		Config:  cfg,
		Models:  llmtest.Factory{M: model},
		Backend: sandboxtest.NewBackend(),
		Tools:   []tools.Provider{orders{}},
	})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }() // executes queued runs in this process

	th, err := h.NewThread(ctx, "alice", "")
	check(err)
	run, err := h.Send(ctx, "alice", th.ID, "Where is A-17?", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
	check(err)

	for ev := range h.Events(ctx, run.ID, 0) {
		fmt.Printf("%2d %s %s\n", ev.Seq, ev.Type, customName(ev))
		if agui.IsTerminal(ev.Type) {
			break
		}
	}

	// A late reader replays from any seq: here, everything after seq 8.
	fmt.Println("-- replay after seq 8")
	for ev := range h.Events(ctx, run.ID, 8) {
		fmt.Printf("%2d %s %s\n", ev.Seq, ev.Type, customName(ev))
		if agui.IsTerminal(ev.Type) {
			break
		}
	}
}

// customName is the "name" of a CUSTOM event ("" for the others).
func customName(ev store.Event) string {
	var e struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(ev.Data, &e)
	return e.Name
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

Output (the `INFO` log lines go to stderr and are left out):

```text
 1 RUN_STARTED 
 2 STEP_STARTED 
 3 TOOL_CALL_START 
 4 TOOL_CALL_ARGS 
 5 TOOL_CALL_END 
 6 TOOL_CALL_RESULT 
 7 CUSTOM work.usage
 8 STEP_FINISHED 
 9 STEP_STARTED 
10 TEXT_MESSAGE_START 
11 TEXT_MESSAGE_CONTENT 
12 TEXT_MESSAGE_CONTENT 
13 TEXT_MESSAGE_CONTENT 
14 TEXT_MESSAGE_END 
15 CUSTOM work.usage
16 STEP_FINISHED 
17 RUN_FINISHED 
-- replay after seq 8
 9 STEP_STARTED 
10 TEXT_MESSAGE_START 
11 TEXT_MESSAGE_CONTENT 
12 TEXT_MESSAGE_CONTENT 
13 TEXT_MESSAGE_CONTENT 
14 TEXT_MESSAGE_END 
15 CUSTOM work.usage
16 STEP_FINISHED 
17 RUN_FINISHED 
```

The first model call (step 1) asked for the tool, the second (step 2) streamed the answer. The number of `TEXT_MESSAGE_CONTENT` events depends on how the provider chunks its stream.

## The data model

The types live in `core/store`. Every method on the facade takes the user's login; it is the tenancy boundary, and a thread or run of another user is `store.ErrNotFound` ("not found", never "forbidden").

| Type | What it is | Fields worth knowing |
|---|---|---|
| `store.Thread` | A conversation. | `ID`, `UserLogin`, `Title`, `ActiveRun` (the queued or running run, if any), `Plan`, `Goal`, `PendingQuestion`, `PendingApproval` |
| `store.Run` | One execution on a thread. | `Status`, `Kind` (`chat`, `goal`, `compact`), `Model`, `Sandbox`, `Mode`, `Error`/`ErrorCode`, `LastSeq`, `RuntimeID`, `HeartbeatAt` |
| `store.Message` | A persisted chat message. | `Role` (`user`, `assistant`, `tool`, `reasoning`), `Content`, `ToolCalls`, `ToolCallID`, `Position`, `RunID` |
| `store.Event` | One persisted AG-UI event of a run. | `Seq`, `Type`, `Data` (the full event JSON) |

A run's `Status` is one of `queued`, `running`, `completed`, `failed`, `cancelled` or `interrupted`; `store.IsTerminal(status)` is false only for the first two. A thread has at most one queued or running run: sending to a busy thread fails with `store.ErrRunActive`.

<div class="callout note">

**Two different "interrupted".** The run *status* `interrupted` (and the `RUN_ERROR` code `interrupted`) means the process that owned the run stopped; see [what interrupted means](/docs/concepts/agent-loop/#cancel-interrupted-and-crashes). A run that waits for a person is different: it is `completed`, and its `RUN_FINISHED` carries the `interrupt` outcome described [below](#interrupts).

</div>

## The facade calls

| Call | Does | Notes |
|---|---|---|
| `h.NewThread(ctx, user, title)` | Creates a thread. | An empty title is filled from the first message. |
| `h.Send(ctx, user, threadID, text, harness.RunOptions{...})` | Stores the user message and queues a run. | `Model` is required and must be allowed by the configuration. Errors wrap `harness.ErrInvalidRequest`. `RunOptions.Sandbox` is `auto`, `off` or `on` (see [Sandboxes](/docs/concepts/sandboxes/)). |
| `h.Events(ctx, runID, after)` | Streams the run's events with `seq > after`, then follows it live. | The channel closes after the terminal event. |
| `h.Cancel(ctx, user, runID)` | Stops a run. | Cancelling a finished run does nothing. |
| `h.Steer(ctx, user, runID, text)` | Adds a message the model sees at its next step. | `queue.ErrSteersClosed` once the run has ended. `SteerAs` records the author. |
| `h.Resume(ctx, user, threadID, harness.ResumeRequest{...})` | Answers the interrupt a run ended with and queues the continuation. | Follow the returned run with `Events`. |
| `h.Run(ctx)` | Executes queued runs in this process until `ctx` is done. | Web processes that only call `Send` never call `Run`; [workers](/docs/concepts/infra/#the-worker-split) do. |

`Events` takes no user: check that the run belongs to the caller yourself (`h.Store.GetUserRun(ctx, user, runID)`) before handing a run id to an end user. The HTTP API and `hosts/client` already do.

The same calls exist behind the [Client contract](/docs/reference/client-contract/), with an in-process and an HTTP implementation, and behind the [HTTP API](/docs/reference/http-api/). The full signatures are in [The Go facade](/docs/reference/harness-api/).

## Events

A run's log is a sequence of [AG-UI](https://docs.ag-ui.com) events: JSON objects with a `type`. `core/agui` defines the vocabulary. The page [Event types](/docs/reference/events/) lists every field; this is the shape.

**Type constants** (`agui.RunStarted` ... `agui.StateSnapshot`):

| Group | Types |
|---|---|
| Run | `RUN_STARTED`, `RUN_FINISHED`, `RUN_ERROR` |
| Steps | `STEP_STARTED`, `STEP_FINISHED` (`stepName` is `step-1`, `step-2`, ...) |
| Text | `TEXT_MESSAGE_START`, `TEXT_MESSAGE_CONTENT`, `TEXT_MESSAGE_END` |
| Reasoning | `REASONING_START`, `REASONING_END`, `REASONING_MESSAGE_START`, `REASONING_MESSAGE_CONTENT`, `REASONING_MESSAGE_END` |
| Tools | `TOOL_CALL_START`, `TOOL_CALL_ARGS`, `TOOL_CALL_END`, `TOOL_CALL_RESULT` |
| Other | `STATE_SNAPSHOT`, `CUSTOM` |

`CUSTOM` events carry a `name` and a `value`. The names defined as constants are `work.tool_output` (live output of a running tool), `work.steer`, `work.usage`, `work.subagent`, `work.compaction`, `work.title`, `work.goal`, `work.memory`, `work.approval` and `work.retry` (a model call is being retried). Features add their own, such as artifact and verification events; a client should ignore names it does not know.

**Order.** `agui.CheckOrder(events)` verifies what strict AG-UI clients require, and the harness's own tests run it over recorded runs: `RUN_STARTED` first and only once; every `*_START` closed by its `*_END` with the same id; content and args only inside an open span; at most one terminal event and nothing after it.

### Seq and resuming

Each event has a `seq`, counting from 1 within its run. Pass the last `seq` you processed as `after` and `Events` continues from the next one, without a gap or a duplicate. Over HTTP the `seq` is the SSE `id`, and a reconnecting browser resumes with `Last-Event-ID` (or `?after=`). `hosts/client` reconnects by itself and drops events it already delivered.

Events are persisted: the log of a finished run replays in full, from any thread, any process, any time later (with the in-memory defaults, until the process exits; see [the store](/docs/concepts/store/)).

### Terminal events

A run stream ends with exactly one of two events, and `agui.IsTerminal(type)` is true for both:

- `RUN_FINISHED` with an `outcome.type` of `success`, `cancelled` or `interrupt`, plus the token `usage` per model.
- `RUN_ERROR` with a `code` and a safe `message`. The codes: `model_error`, `model_timeout`, `rate_limited`, `content_filter`, `empty_answer`, `max_steps`, `interrupted` and `internal`. Raw provider errors are logged on the server, never sent to clients.

Read the stream until a terminal event, not until the channel closes: a UI that waits for `RUN_FINISHED` only would hang on an error.

### Interrupts

A run that needs a person ends as a completed run whose `RUN_FINISHED` has `outcome.type: "interrupt"` and a list of `interrupts`. Each has an `id`, a `reason`, a `message`, the `toolCallId` and a `responseSchema`. There are two reasons:

- `tool_approval`: a risky tool call is paused before it runs (interactive runs). The schema offers the decisions `approve`, `deny` and `respond`. See [Approvals and permissions](/docs/concepts/approvals-permissions/).
- `ask_user`: the model called the `ask_user` tool with a question and optional choices.

Answer with `Resume`, passing the interrupt's `id` and `reason`; the question or approval also stays on the thread (`PendingQuestion`, `PendingApproval`). This program, also run offline, shows the whole round trip:

```go
var interrupt agui.Interrupt
for ev := range h.Events(ctx, run.ID, 0) {
	if ev.Type != agui.RunFinished {
		continue
	}
	var fin struct {
		Outcome struct {
			Type       string           `json:"type"`
			Interrupts []agui.Interrupt `json:"interrupts"`
		} `json:"outcome"`
	}
	check(json.Unmarshal(ev.Data, &fin))
	fmt.Println("outcome:", fin.Outcome.Type, "reason:", fin.Outcome.Interrupts[0].Reason)
	interrupt = fin.Outcome.Interrupts[0]
	break
}

next, err := h.Resume(ctx, "alice", th.ID, harness.ResumeRequest{
	InterruptID: interrupt.ID, Reason: interrupt.Reason, Text: "Friday", Model: "m",
})
check(err)
for ev := range h.Events(ctx, next.ID, 0) { /* the continuation: a new run */ }
```

With a model that calls `ask_user` and then answers with the reply, it prints `outcome: interrupt reason: ask_user`, then streams `Booked: Friday` from the second run. The first run's status is `completed`; the answer starts a new run in the same thread.

### stream_lost

`Events` of `hosts/client` over HTTP reconnects when the connection drops. When it cannot recover, it ends the channel with a synthetic `RUN_ERROR` whose `code` is `stream_lost`, so a consumer that waits for a terminal event never hangs. The event is not in the run's log; the run itself may still be going, and you can resume with the last `seq` you saw. The in-process facade has no connection to lose and never produces it.

## Related

- [The agent loop](/docs/concepts/agent-loop/): what happens between `RUN_STARTED` and the terminal event.
- [Event types](/docs/reference/events/): every event and field.
- [Smallest embedding](/docs/quickstart/embed/): the same calls in the shortest program.
