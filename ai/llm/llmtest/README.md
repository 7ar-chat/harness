# ai/llm/llmtest

A scripted fake `ai.Model` for tests. No network, no key, and the failure paths are as easy to reach as the happy one.

```go
model := llmtest.New(
    llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "weather", `{"city":"Lyon"}`), "tool_calls")}},
    llmtest.Turn{Chunks: llmtest.Text("18C and clear.")},
)
// use it wherever an ai.Model is expected, then:
for _, c := range model.Calls() { /* c.Messages, c.Tools, c.ToolChoice, c.ReasoningEffort */ }
```

Each call consumes the next `Turn` and streams its chunks like a provider. A `Turn` can instead:

| Field | Effect |
|---|---|
| `Err` | fail the call before any chunk (a provider error: use an `openai.APIError` to set a status) |
| `FailAfter` | fail the stream after the chunks |
| `Block` | go silent after the chunks until the context ends (to test a watchdog) |
| `ChunkDelay`, `KeepAlive`, `Pings` | a slow stream, silent reasoning with provider events, a gateway pinging a hung upstream |
| `RetryAfter`, `Refusal` | the `Retry-After` hint and refusal text a real transport would record in `llm.CallInfo` |

`NewFunc` computes each turn from the call. `Factory{M: model}` implements `llm.Factory`. Helpers build chunks:
`Text`, `ToolCall`, `Reasoning`, `Finish`, `WithUsage`, `WithCachedUsage`.
