# ai

A small Go library for building LLM agents: messages and tools, clients for the common providers, a tool-calling
loop, and context compaction. The root package is the shared vocabulary; everything else is optional and builds on it.

```
go get github.com/7ar-chat/harness/ai
```

- **Few dependencies.** The standard library and one JSON-schema reflector. No framework, no code generation.
- **Streaming is the default.** Every client streams; `Generate` is a stream joined into one message.
- **Failures are data.** Provider errors carry a status and a code; `llm.Classify` turns them into "retry",
  "shrink the request" or "give up".
- **Testable without a network.** `ai/llm/llmtest` scripts a model, its failures and its stalls.

## A complete agent

```go
weather, _ := ai.InferTool("weather", "Current weather of a city.",
    func(ctx context.Context, in struct {
        City string `json:"city" jsonschema_description:"the city"`
    }) (string, error) {
        return in.City + ": 18C, clear", nil
    })
info, _ := weather.Info(ctx)

base, _ := openai.New(openai.Config{BaseURL: "https://api.openai.com/v1", APIKey: key, Model: "gpt-4o-mini"})
model, _ := base.WithTools([]*ai.ToolInfo{info})

res, err := agent.Run(ctx, agent.Config{
    Model:  model,
    Tools:  []ai.InvokableTool{weather},
    Caller: &agent.Caller{Retries: 2}, // stream, cut a silent call, retry transient failures
}, []*ai.Message{ai.UserMessage("Weather in Lyon?")})

fmt.Println(res.Reply.Content)
```

## Packages

| Package | What it is |
|---|---|
| [`ai`](.) | `Message`, `ToolCall`, `ToolInfo`, `Model`, call options, streams, `InferTool`, `ConcatMessages` |
| [`ai/openai`](openai) | Chat Completions client for OpenAI and compatible servers (Azure, OpenRouter, vLLM, Ollama, gateways) |
| [`ai/llm`](llm) | provider factory, error classification, retry delays |
| [`ai/llm/anthropic`](llm/anthropic) | Anthropic Messages client with prompt-cache breakpoints |
| [`ai/llm/responses`](llm/responses) | OpenAI Responses client with encrypted reasoning carried between calls |
| [`ai/agent`](agent) | the loop, streaming under a watchdog, retries, parallel tool scheduling |
| [`ai/compaction`](compaction) | token estimates and keeping a conversation inside a context window |
| [`ai/llm/llmtest`](llm/llmtest) | a scripted fake model for tests |

## The model contract

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

Anything that implements `Model` works with `ai/agent`. A new provider is one type with these three methods.

## Tools

A tool is an `InvokableTool`: it describes itself (`Info`) and runs on a JSON arguments object (`InvokableRun`), returning
the text the model reads. `InferTool` builds one from a Go function, deriving the schema from the input struct.
Mark a tool read-only (`ReadOnly() bool`) and the loop can run several of its calls in one step at the same time.

## Stability

Version 0.x: the vocabulary (`Message`, `Model`, tools) is settled; `agent.Policy`, `agent.Caller`'s `Attempt` and the
`Extra` keys some packages use may still change. Pin a version.

## License

MIT. See [LICENSE](LICENSE).
