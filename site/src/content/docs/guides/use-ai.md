---
title: "Use the ai module on its own"
description: "Build a tool-calling agent with only github.com/7ar-chat/harness/ai: messages, tools from Go functions, the loop, provider clients, retries, compaction and an offline scripted model."
---

`github.com/7ar-chat/harness/ai` is a small Go library for LLM agents that does not need the rest of the harness: messages and tools, clients for the common providers, a tool-calling loop and context compaction. Use it when you want the loop and the provider clients but your own storage, sandbox and UI. It depends on the standard library and one JSON-schema reflector. For the full engine (threads, runs, events, sandboxes) see [Smallest embedding](/docs/quickstart/embed/) instead.

```bash
go get github.com/7ar-chat/harness/ai
```

## A complete agent, offline

The program below uses only the `ai` module and the scripted model `llmtest`, so it runs without a network or key. Turn 1 asks for the `weather` tool, turn 2 answers in text. With a real provider only the `model` line changes.

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/agent"
	"github.com/7ar-chat/harness/ai/llm"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
)

type cityArgs struct {
	City string `json:"city" jsonschema_description:"the city"`
}

func main() {
	ctx := context.Background()

	// A tool from a Go function: the schema is reflected from the input struct.
	weather, err := ai.InferTool("weather", "Current weather of a city.",
		func(ctx context.Context, in cityArgs) (string, error) {
			return in.City + ": 18C, clear", nil
		})
	if err != nil {
		log.Fatal(err)
	}
	info, err := weather.Info(ctx)
	if err != nil {
		log.Fatal(err)
	}

	// A scripted model: turn 1 asks for the tool, turn 2 answers in text.
	// With a real provider this is llm.New(...).Model(ctx, id) or openai.New(...).
	script := llmtest.New(
		llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "call_1", "weather", `{"city":"Lyon"}`), "tool_calls"),
		}},
		llmtest.Turn{Chunks: llmtest.Text("It is 18C and clear in Lyon.")},
	)
	model, err := script.WithTools([]*ai.ToolInfo{info}) // a copy that offers the tools
	if err != nil {
		log.Fatal(err)
	}

	res, err := agent.Run(ctx, agent.Config{
		Model:    model,
		Tools:    []ai.InvokableTool{weather},
		MaxSteps: 10,
		Caller:   &agent.Caller{Retries: 2}, // stream under a watchdog, retry transient failures
		OnTool: func(c ai.ToolCall, result string) {
			fmt.Printf("tool %s(%s) -> %s\n", c.Function.Name, c.Function.Arguments, result)
		},
	}, []*ai.Message{ai.UserMessage("Weather in Lyon?")})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("reply:", res.Reply.Content)
	fmt.Println("steps:", res.Steps, "messages:", len(res.Messages))
	for _, m := range res.Messages {
		fmt.Printf("  %-9s %q\n", m.Role, m.Content)
	}

	// Failures are data: classify an error to decide what to do.
	err = errors.New("status code: 503, overloaded")
	class := llm.Classify(err)
	fmt.Println("503 is", class, "retryable:", class.Retryable())

	// Code that takes an llm.Factory runs unchanged on the scripted fake.
	var f llm.Factory = llmtest.Factory{M: llmtest.New(llmtest.Turn{Chunks: llmtest.Text("pong")})}
	m, _ := f.Model(ctx, "any-id")
	reply, _ := m.Generate(ctx, []*ai.Message{ai.UserMessage("ping")})
	fmt.Println("factory model said:", reply.Content)
}
```

Output:

```text
tool weather({"city":"Lyon"}) -> Lyon: 18C, clear
reply: It is 18C and clear in Lyon.
steps: 2 messages: 4
  user      "Weather in Lyon?"
  assistant ""
  tool      "Lyon: 18C, clear"
  assistant "It is 18C and clear in Lyon."
503 is transient retryable: true
factory model said: pong
```

The module resolves to `github.com/7ar-chat/harness/ai v0.1.1` plus third-party packages only (the JSON-schema reflector and its dependencies); building it with `GOWORK=off` against the published module gives the same output.

## Messages and the model contract

`ai.Message` is one turn of a conversation, or a chunk of one while it streams: `Role`, `Content`, optional image `Parts`, `ToolCalls` for an assistant message, `ToolCallID` and `ToolName` for a tool result, `ReasoningContent`, `ResponseMeta` and an `Extra` map. Build them with `ai.UserMessage`, `ai.SystemMessage`, `ai.AssistantMessage` and `ai.ToolMessage`; `ai.ConcatMessages` joins streamed chunks into one message.

```go
type ChatModel interface {
	Generate(ctx context.Context, msgs []*Message, opts ...Option) (*Message, error)
	Stream(ctx context.Context, msgs []*Message, opts ...Option) (*StreamReader[*Message], error)
}

type Model interface {
	ChatModel
	WithTools(tools []*ToolInfo) (Model, error) // returns a copy that offers the tools
}
```

Anything that implements `ai.Model` works with `ai/agent`; a new provider is one type with these methods. Every client streams, and `Generate` is a stream joined into one message. Call options (`ai.WithTemperature`, `WithMaxTokens`, `WithReasoningEffort`, `WithToolChoice`, `WithPromptCacheKey`) are passed per call.

## Tools

A tool is an `ai.InvokableTool`: it describes itself (`Info`) and runs on a JSON arguments object (`InvokableRun`), returning the text the model reads. `ai.InferTool(name, description, fn)` builds one from a Go function; the parameter schema is reflected from the input struct (`json` and `jsonschema` tags) and a non-string result is rendered as JSON. A tool that has no side effects can implement `agent.ReadOnly` (`ReadOnly() bool`), so that with `Config.Parallel` several of its calls in one step run at the same time. The harness-level view of tools, with approvals and sandboxes, is [Tools](/docs/concepts/tools/).

## The loop

`agent.Run(ctx, cfg, msgs)` asks the model, runs the tools it calls, feeds the results back and repeats until the model answers in plain text or the step budget ends.

| `agent.Config` field | Meaning |
|---|---|
| `Model` | must already offer the tools (`WithTools`) |
| `Tools` | the tools the loop may run, by the name in their `ToolInfo` |
| `MaxSteps`, `LimitNudge` | the number of model calls (default `agent.DefaultMaxSteps`); the last one forbids tools, after the nudge message was appended |
| `Caller` | make each call as a stream under a watchdog and retry transient failures |
| `Parallel`, `MaxParallel` | run consecutive `ReadOnly` calls together |
| `OnStep`, `OnChunk`, `OnReply`, `OnTool` | hooks |
| `Steer` | messages to add before a call (a user interjecting) |
| `Invoke` | replaces how one tool call runs, for example to add approval or output limits |
| `Options` | `ai.Option`s passed to every model call |

`Result` holds the whole transcript (`Messages`), the last answer (`Reply`) and `Steps`. A failed model call returns the transcript so far with the error; a cancelled context returns `ctx.Err()`.

When the conversation lives in a database, or each step must emit events, implement `agent.Policy` and call `agent.Loop`: the loop owns the phases (`BeginStep`, `Request`, `Call`, `Recover`, `Replied`, `Next`, `EndStep`) and `agent.Base` supplies the ones you leave alone. This is what the harness engine does; see [The agent loop](/docs/concepts/agent-loop/).

`agent.Caller` makes one streamed call under a watchdog: it cancels an attempt whose first chunk takes longer than `FirstChunk`, whose chunks come further apart than `Idle`, or that lasts longer than `Ceiling`, and retries what `llm.Classify` deems worth retrying with delays from `llm.Backoff` that never go below the provider's `Retry-After`.

## Providers

`ai/llm` is a factory that picks the client for a model id:

```go
p := llm.New(llm.Options{
	Provider:        "openai",                      // or "anthropic"
	BaseURL:         "https://api.openai.com/v1",
	APIKey:          os.Getenv("LLM_API_KEY"),
	Models:          []string{"gpt-4o", "o4-mini"}, // allow-list: any other id is refused
	ResponsesModels: []string{"o4-mini"},           // called through the Responses API
	TimeoutSeconds:  60,                            // connect + response headers
})
model, err := p.Model(ctx, "gpt-4o") // an ai.Model, built once and reused
```

All models share one HTTP client. Accept the one-method `llm.Factory` interface (`Model(ctx, name)`) in your code: `*llm.Provider` and `llmtest.Factory` both implement it. The clients are separate packages: `ai/openai` (Chat Completions for OpenAI and compatible servers such as Azure, OpenRouter, vLLM, Ollama), `ai/llm/anthropic` (Messages API with prompt-cache breakpoints) and `ai/llm/responses` (OpenAI Responses with encrypted reasoning carried between calls). Configuration of providers in the harness is described in [LLM providers](/docs/integrations/llm-providers/).

### Failures

```go
class := llm.Classify(err) // fatal | transient | rate_limited | timeout | overflow
if class.Retryable() {
	time.Sleep(llm.Backoff(attempt, time.Second, retryAfter, rand.Float64()))
}
if llm.IsContextOverflow(err) { /* shrink the request: see ai/compaction */ }
```

`Classify` reads `StatusCode()` and `ErrorCode()` from the error through interfaces, so any client that exposes them works. A 429 is always a rate limit; an overflow needs a 400/413 or the `context_length_exceeded` code; DNS and TLS failures are fatal.

## Compaction

`ai/compaction` keeps a conversation inside a context window. `Compact` is a pure function: it estimates the prompt and, when it is too large, returns a smaller one. You decide when to call it and supply the model call that writes summaries (use `compaction.Instructions` as the system prompt of that call).

```go
res, err := compaction.Compact(ctx, history, compaction.Options{
	Window:    128_000, // the model's context window, in tokens
	Threshold: 0.75,    // compact above this share
	Summarize: func(ctx context.Context, previous, transcript string) (string, error) {
		return "", fmt.Errorf("call a model with compaction.Instructions as the system prompt")
	},
})
// res.Action is none | shrunk | summarized | dropped; res.Messages is the new history
```

First tool results older than a recent tail become short previews; then everything before a recent tail is folded into one summary from your `Summarize` function (if it fails, the head is dropped and `Result.Err` says why). A tool call and its result are never split. On a short history `Action` is `none` and the messages are unchanged (verified with a one-message history). The engine-level behaviour is [Compaction](/docs/concepts/compaction/).

## Testing without a network

`ai/llm/llmtest` scripts a model: a list of `Turn`s, one per call, each streaming `Chunks` the way a provider would. A turn can also fail (`Err`, `FailAfter`), stall (`Block`, `ChunkDelay`, `KeepAlive`, `Pings`), report a refusal or a `RetryAfter` hint, so the retry and watchdog paths can be exercised. `Model.Calls()` and `Requests()` record every call for assertions. Helpers: `llmtest.Text(...)`, `ToolCall(idx, id, name, args)`, `Finish(msg, reason)`, `WithUsage`, `Reasoning`. See also [Test your agent](/docs/guides/testing/).

<div class="callout note">

**Stability.** The module is version 0.x. The vocabulary (`Message`, `Model`, tools) is settled; `agent.Policy`, the `Attempt` of `agent.Caller` and the `Extra` keys some packages use may still change. Pin a version; see [Releasing and versioning](/docs/guides/releasing/).

</div>
