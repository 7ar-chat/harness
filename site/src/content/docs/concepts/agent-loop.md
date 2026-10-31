---
title: The agent loop
description: "How one run executes, step by step - the shared loop in ai/agent, the policy of a persisted run, the budgets, guards, steering and cancellation rules, and what interrupted means."
---

A run is a loop: ask the model, run the tools it called, feed the results back, repeat until the model answers without a tool call or the step budget ends. There is **one** loop implementation, `ai/agent.Loop`. It owns the shape of a run and asks a **Policy** for everything else. The harness's policy (`runPolicy` in `engine/agent`) persists messages, emits [AG-UI events](/docs/concepts/threads-runs-events/), applies the guards and handles steering. You rarely touch any of this; read this page to know what a run does when it misbehaves, and which environment variables bound it.

## The shape of a step

For each step, in order:

```text
BeginStep -> Request -> Call [-> Recover -> Request -> Call] -> Replied -> Next [-> Tools] -> EndStep
```

| Phase | What the harness's policy does |
|---|---|
| `BeginStep` | Emits `STEP_STARTED` (`step-1`, `step-2`, ...), injects pending [steers](#steering), runs [compaction](/docs/concepts/compaction/) if the history is near the context window, checks changed deliverables, adds step notes and budget reminders. |
| `Request` | Builds the model input. On the last step (or when the run is stuck on failing calls) it appends a nudge and forbids new tool calls. |
| `Call` | Streams the model call as AG-UI events, under the [watchdog](#model-calls-watchdog-and-retries). |
| `Recover` | Asked once, only when the call failed with no answer: after a context overflow it compacts the history and the same step is tried again. Any other failure is not recovered here. |
| `Replied` | Counts usage, applies the provider's content filter, stores the assistant message. |
| `Next` | Decides: `Finish`, `Continue` (ask again) or `RunTools`. |
| `Tools` | Runs the answer's tool calls ([scheduling](#tool-scheduling)). |
| `EndStep` | Runs for every step that began, also when a phase failed: closes the step (`STEP_FINISHED`) and flushes the events. |

`Loop(ctx, maxSteps, policy)` returns nil when a step finished the run, `ErrStepLimit` when the budget ended first, `ctx.Err()` when the context ended between steps, or the error a phase returned.

<div class="callout note">

**One loop, two policies.** `engine/agent` is the policy of a persisted run. The `explore` sub-agent runs the same loop through `ai/agent.Run`, whose policy keeps the conversation in a slice. This is stated in the "Known gaps" of [docs/architecture.md](https://github.com/7ar-chat/harness/blob/main/docs/architecture.md): there is no second loop to keep in sync.

</div>

### The loop without the harness

`ai/agent` has no store, no events and no approvals, so you can use it on its own (see [Use the ai module on its own](/docs/guides/use-ai/)). This policy has no model: each step "answers" and asks for another, so the loop runs until the budget ends.

```go
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/agent"
)

// counter is a Policy with no model: every step "answers" and asks for another step.
type counter struct{ agent.Base }

func (counter) BeginStep(_ context.Context, s *agent.Step) error {
	fmt.Printf("step %d begins (last=%v)\n", s.N, s.Last)
	return nil
}
func (counter) Request(context.Context, *agent.Step) ([]*ai.Message, []ai.Option) { return nil, nil }
func (counter) Call(_ context.Context, s *agent.Step) (*ai.Message, error) {
	return ai.AssistantMessage(fmt.Sprintf("answer %d", s.N), nil), nil
}
func (counter) Next(context.Context, *agent.Step, *ai.Message) (agent.Action, error) {
	return agent.Continue, nil
}
func (counter) Tools(context.Context, *agent.Step, []ai.ToolCall) (bool, error) { return false, nil }
func (counter) EndStep(_ context.Context, s *agent.Step)                      { fmt.Printf("step %d ends\n", s.N) }

func main() {
	n, err := agent.Loop(context.Background(), 3, counter{})
	fmt.Println("steps:", n, "| step limit:", errors.Is(err, agent.ErrStepLimit))
}
```

```text
step 1 begins (last=false)
step 1 ends
step 2 begins (last=false)
step 2 ends
step 3 begins (last=true)
step 3 ends
steps: 3 | step limit: true
```

`agent.Base` supplies the phases you leave alone (`Next` finishes on an answer without tool calls and on the last step). `Step.Last` is how a policy knows the budget is about to end.

## Budgets and limits

All of these are environment variables read by `config.Load()`; each also has a key in `config.yml` (see [Environment variables](/docs/reference/env-vars/) and [config.yml keys](/docs/reference/config-yml/)).

| Variable | Default | Bounds |
|---|---|---|
| `AGENT_MAX_STEPS` | `60` | Model calls in one run. The last step is made with tool calls forbidden and a nudge ("You have reached the step limit; answer with what you have."). |
| `LLM_TIMEOUT_SECONDS` | `300` | The wait for the first chunk of a model call. |
| `LLM_STREAM_IDLE_SECONDS` | `90` | The gap between two chunks of a stream. |
| `LLM_CALL_MAX_SECONDS` | `1200` | One model call, whatever the stream sends. |
| `LLM_STEP_RETRIES` | `3` | How often a failed model call is re-issued. `0` never retries. |
| `AGENT_MAX_TOOL_OUTPUT_CHARS` | `30000` | The most characters of one tool result the model reads. |
| `AGENT_TOOL_OUTPUT_SPILL_CHARS` | `20000` | A longer tool output is written to a file instead. Must not exceed the cap above. |

For reasoning models, the first-chunk and per-call budgets are doubled at reasoning effort `high` and tripled at `xhigh`, so a long silent reasoning is not mistaken for a stall.

The last step is not an error by itself: if the model answers in text on it, the run completes. The run fails with `RUN_ERROR` code `max_steps` only if the model still asks for tools on the last step, or if it ran out of budget without producing a file it was writing.

## Model calls, watchdog and retries

A model call is a stream under `ai/agent.Caller`:

- **Watchdog.** It cancels an attempt whose first chunk takes longer than `LLM_TIMEOUT_SECONDS`, whose chunks come further apart than `LLM_STREAM_IDLE_SECONDS`, or that lasts longer than `LLM_CALL_MAX_SECONDS`. A provider event that is not a chunk (silent reasoning) restarts the budget; an SSE keep-alive comment does not.
- **Retry.** A retryable failure (timeout, 429, 5xx, connection reset) is re-issued with the same request, up to `LLM_STEP_RETRIES` times, with exponential backoff and jitter that never goes below the provider's `Retry-After`. The failed attempt's events are closed (its tool calls get the result `(stream interrupted, retrying)`), nothing of it is persisted, and a `work.retry` event (`{attempt, reason}`) precedes the new attempt.
- **Failure codes.** When retries run out the run ends with `RUN_ERROR` code `model_timeout`, `rate_limited` or `model_error`. A provider that stops the answer with `finish_reason=content_filter` gives `content_filter`; a model that returns neither text nor tool call twice in a row (after one nudge) gives `empty_answer`.
- **Cut answers.** `finish_reason=length` on a text answer makes the model continue where it stopped (at most twice per run). Tool calls cut by the output limit are never run; each gets an error result asking the model to re-issue them in smaller pieces.
- **Context overflow.** One `Recover` per step: the history is compacted and the step retried once.

## Tool scheduling

The tool calls of one answer run in the model's order, with one exception: **consecutive read-only calls run in parallel**, at most 4 at a time. A tool is read-only when it implements `ReadOnly() bool` and returns true (and no approval gate stands in front of it). Results are emitted and stored in call order whatever the order of completion, so the log is deterministic. Everything else runs alone.

Among the sandbox tools, `read_file`, `view_file`, `search_files`, `list_files` and `job_output` are read-only; `execute_command`, `write_file`, `edit_file` and `job_kill` are not. See [Tools](/docs/concepts/tools/).

A panic in a tool is caught and becomes an error result; one broken tool does not end the run. A tool that needs the user (`ask_user`, or a risky call in interactive mode) stops the run after its step, with an [interrupt outcome](/docs/concepts/threads-runs-events/#interrupts).

### Output caps and spill

Before the model sees a tool result:

1. If it is longer than `AGENT_TOOL_OUTPUT_SPILL_CHARS`, the full text is written to `/workspace/.work/tool-output/` in the run's sandbox and the model gets the head (8000 characters), the tail (4000), the path and a hint to page it with `read_file` or `search_files`. A JSON-object result stays valid JSON (large strings are cut in place; `spilled` and `outputPath` are added).
2. Without a running sandbox (an MCP or web tool in a thread that has none), the output cannot be kept: it is cut with a note that the full text was not kept.
3. Whatever remains is capped at `AGENT_MAX_TOOL_OUTPUT_CHARS`: the head (two thirds) and the tail are kept, with a marker between.

## Guards

The policy watches for runs that are stuck and nudges the model before it wastes the budget. These are fixed behaviours, not settings:

| Guard | Trigger | Effect |
|---|---|---|
| Identical calls | The same tool with the same arguments 3 times in a row | The 3rd is not executed; the model gets a refusal telling it to change its approach. Polling a still-running job with `job_output` never counts. |
| Alternation | A-B-A-B-A-B between two calls | The 6th is not executed. |
| Repeated error | The same error 3 times in a turn | A note is appended to the result. |
| Repeated result | The same call returning the same result 3 times | A note tells the model to use the result it has. |
| Failed calls | 5 failed tool calls in a row | A note to step back (and to `ask_user` if only the user can decide). |
| Failed calls | 8 in a row | The next step is forced to answer without tools. |
| Empty answer | No text and no tool call | One nudge, then `empty_answer`. |
| Stop reminders | A tool (for example the plan) objects to the model stopping | Up to 2 reminders per run. |
| Late-budget reminder | Periodically from step 9 on | A "step N of M" note with open plan items and unverified deliverables. |

Features add their own checks, for example [verification](/docs/concepts/verify-and-explore/) refuses to let the agent finish on an unverified deliverable.

## Steering

`Steer(ctx, user, runID, text)` adds a message to a **running** run. The message is pushed on the run's control bus (shared between processes when you use [Redis](/docs/concepts/infra/)) and delivered at the start of the run's next step, as a user message prefixed `[The user added while you were working]: `. The run emits `CUSTOM work.steer` with `delivered: true`.

The rules that matter:

- **Between steps, not during.** A step already in flight (a model call, a running tool) is not interrupted; the steer waits for the next `BeginStep`.
- **A steer on the final answer is not lost.** When the model answers without tool calls, the run stops accepting steers and drains the list. A steer that arrived meanwhile gets its own extra step. One that arrives after the list is closed fails with `queue.ErrSteersClosed` ("the run no longer accepts messages"), as does a steer on a finished run.
- **Undelivered steers are kept.** A steer accepted but not delivered when the run ends is stored as a message of the thread and announced with `work.steer` `delivered: false`; the next run sees it.
- Up to 20 steers are pending per run.

This program proves delivery. A tool blocks until a steer is sent; the model then answers having seen it. It runs offline:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/engine/tools"
)

type none struct{}

// slow offers a tool that blocks until the program lets it go, so the run is inside a step.
type slow struct {
	started chan struct{}
	release chan struct{}
}

func (slow) Name() string { return "slow" }
func (s slow) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("slow_lookup", "A lookup that takes a while.", func(context.Context, none) (string, error) {
		close(s.started)
		<-s.release
		return "found", nil
	})
	return []ai.BaseTool{t}, nil, err
}

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	check(err)

	// Calls the tool; once a steer is in the history, answers.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == ai.RoleUser && strings.Contains(m.Content, "while you were working") {
				fmt.Println("model sees:", m.Content)
				return llmtest.Turn{Chunks: llmtest.Text("Done, in French.")}
			}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "slow_lookup", `{}`), "tool_calls")}}
	})
	tool := slow{started: make(chan struct{}), release: make(chan struct{})}
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: model},
		Features: harness.ProfileMinimal(), Tools: []tools.Provider{tool},
	})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "Look it up", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
	check(err)

	<-tool.started // the run is inside step 1
	check(h.Steer(ctx, "alice", run.ID, "Answer in French."))
	close(tool.release)

	for ev := range h.Events(ctx, run.ID, 0) {
		if ev.Type == agui.Custom {
			var e struct {
				Name  string         `json:"name"`
				Value map[string]any `json:"value"`
			}
			_ = json.Unmarshal(ev.Data, &e)
			if e.Name == agui.CustomSteer {
				fmt.Println("event:", e.Name, "delivered =", e.Value["delivered"])
			}
		}
		if agui.IsTerminal(ev.Type) {
			fmt.Println("terminal:", ev.Type)
			break
		}
	}
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

```text
model sees: [The user added while you were working]: Answer in French.
event: work.steer delivered = true
terminal: RUN_FINISHED
```

## Cancel, interrupted and crashes

A run ends in exactly one of these ways:

| Cause | Status | Terminal event |
|---|---|---|
| The model answered | `completed` | `RUN_FINISHED` outcome `success` (or `interrupt` when it waits for the user) |
| `Cancel` by the user | `cancelled` | `RUN_FINISHED` outcome `cancelled` |
| A failure of the model or the harness | `failed` | `RUN_ERROR` with a code (`model_error`, `max_steps`, ...) |
| The runtime that owned it stopped | `interrupted` | `RUN_ERROR` code `interrupted` |

**Cancel.** A queued run is cancelled at once (its log is `RUN_STARTED`, `RUN_FINISHED` cancelled). A running run gets a cancel request recorded in the store and published on the bus; the runtime cancels the run's context, and a heartbeat poll is the fallback when the bus is unreachable. A tool call in flight ends with the result "The tool call was cancelled by the user.", the steps are closed, and the run finishes `cancelled` with its usage so far. Cancelling a finished run does nothing.

**Interrupted** means the run did not end by its own logic: the process executing it stopped. It happens in two ways, both ending with `RUN_ERROR` code `interrupted` and the message "The run was interrupted (the runtime stopped). Send your message again to continue.":

- **Graceful shutdown.** When the context passed to `Harness.Run` ends (SIGTERM on the worker), the runtime stops claiming runs and lets in-flight runs finish for `RUNTIME_SHUTDOWN_GRACE_SECONDS` (default 15). Runs still going after that are cancelled and finished as `interrupted`, with a few more seconds to write their terminal event.
- **A crash.** A running run's runtime refreshes its heartbeat every 10 seconds. Every runtime runs a sweeper (every 30 seconds; with a shared [Coordinator](/docs/concepts/infra/) only one acts at a time) that marks runs whose heartbeat is older than 60 seconds as `interrupted` and appends the `RUN_ERROR` to their log. With Redis it first checks the run's liveness key, so a Postgres outage does not interrupt live runs. If the log already holds a terminal event, no second one is added: the run's status is made to match it.

Interrupted work is never silently lost: the messages and events written so far are persisted, and the user sends a message to continue. With the local file store the same marking happens at the next start (see [the store](/docs/concepts/store/#how-it-persists)). Recovery across processes is covered in [Infrastructure](/docs/concepts/infra/#crashed-runtimes-and-recovery).

<div class="callout tip">

**Resume from the right place.** After any of these, `Events(ctx, runID, lastSeq)` replays what was written and then the terminal event. A UI that tracks `seq` shows the same final state whether it was connected throughout or reconnected after the crash.

</div>

## Related

- [Tools](/docs/concepts/tools/): what the loop runs, and how outputs are capped.
- [Compaction](/docs/concepts/compaction/): what happens when the history outgrows the context window.
- [Approvals and permissions](/docs/concepts/approvals-permissions/): the gate in front of risky calls.
- [Event types](/docs/reference/events/): the events a step emits.
