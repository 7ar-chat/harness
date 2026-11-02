# ai/agent

The tool-calling loop: ask the model, run the tools it calls, feed the results back, repeat until it answers in plain
text or the step budget ends. One loop serves a three-line script and a service that persists every step.

## Run: the simple way

```go
res, err := agent.Run(ctx, agent.Config{
    Model:    model,                      // already offers the tools (ai.Model.WithTools)
    Tools:    []ai.InvokableTool{weather},
    MaxSteps: 10,                         // the last step forbids tools, so the model has to answer
    LimitNudge: "Step limit reached: answer now from what you found.",
    Caller:   &agent.Caller{Retries: 2},  // stream, watchdog, retry
    Parallel: true,                       // run consecutive ReadOnly tool calls together
    OnTool:   func(c ai.ToolCall, result string) { log.Println(c.Function.Name, "->", result) },
}, []*ai.Message{ai.UserMessage("...")})
```

`Result` holds the whole transcript (`Messages`), the last answer (`Reply`) and the number of steps. A failed model call
returns the transcript so far with the error; a cancelled context returns `ctx.Err()`.

Hooks: `OnStep`, `OnChunk`, `OnReply`, `OnTool`. `Steer` adds messages before a call (a user interjecting). `Invoke` replaces
how a tool call runs, for example to add approval or output limits.

## Loop and Policy: when the conversation lives somewhere else

`Run` keeps the conversation in a slice. When it lives in a database, when each step must emit events, or when answers
are stored as they arrive, implement `agent.Policy` and call `agent.Loop`. The loop owns the shape of a run:

```
BeginStep -> Request -> Call [-> Recover -> Request -> Call] -> Replied -> Next [-> Tools] -> EndStep
```

- `Recover` is asked once when a call failed with no answer (compact the context after an overflow, then try again).
- `Next` decides what an answer means: `Finish`, `Continue` (ask again, for instance after an empty answer) or `RunTools`.
- `EndStep` runs for every step that began, also when a later phase failed, so step-closing events are never missed.

Embed `agent.Base` for the phases you leave alone. `Run` itself is a `Policy` over a slice, in under 100 lines.

## Caller: a streamed call you can trust

`Caller` makes one model call as a stream under a watchdog and retries the failures worth retrying.

- **Watchdog**: it cancels an attempt whose first chunk takes longer than `FirstChunk`, whose chunks come further apart
  than `Idle`, or that lasts longer than `Ceiling`. A provider event that is not a chunk (reasoning, keep-alive events)
  restarts the budget; an SSE comment line does not. Cut calls are classified as timeouts and retried.
- **Retry**: `llm.Classify` decides; delays come from `llm.Backoff` and never go below the provider's `Retry-After`.
- **Attempt**: you pass a factory for an `Attempt` per call. It sees each chunk, is told when the stream ends (`Settle`)
  and when the attempt failed (`Abort`), so what you emitted for a failed attempt can be undone before the next one.

## Building blocks

`Schedule` runs a step's calls in the model's order, batching consecutive parallel-safe ones; `Parallel` bounds
concurrency; `SafeInvoke` turns a tool panic into an error; `CapOutput` and `Truncate` bound what the model reads.
