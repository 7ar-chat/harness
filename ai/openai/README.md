# ai/openai

An `ai.Model` over the OpenAI **Chat Completions** protocol (`POST {BaseURL}/chat/completions`). It works with OpenAI and
with anything that copies the protocol: Azure OpenAI, OpenRouter, vLLM, Ollama, LiteLLM, internal gateways.

```go
model, err := openai.New(openai.Config{
    BaseURL: "https://api.openai.com/v1", // default
    APIKey:  os.Getenv("OPENAI_API_KEY"),
    Model:   "gpt-4o-mini",               // sent verbatim, slashes included
})
reply, err := model.Generate(ctx, []*ai.Message{ai.UserMessage("Say hello.")})
```

## What it does

- **Streams** server-sent events into `ai.Message` chunks: text, `reasoning_content`, tool-call fragments, and the final
  usage (`stream_options.include_usage` is requested). `Generate` makes one non-streamed call.
- **Maps options**: `ai.WithToolChoice` (`none` / `auto` / `required` / a named function), `WithTemperature`, `WithTopP`,
  `WithMaxTokens`, `WithReasoningEffort`, `WithPromptCacheKey`. `tool_choice` is only sent when tools are offered.
- **Sends images** from `ai.Part` as `image_url` content (a URL, or base64 data with its media type).
- **Detects a cut stream**: a stream that ends without `[DONE]` or a finish reason returns `io.ErrUnexpectedEOF`, so a
  dropped connection never looks like a complete answer.
- **Reports errors** as `*APIError` with the HTTP status and the provider's error code, including an `error` object
  that arrives in the middle of a stream. `StatusCode()` and `ErrorCode()` are what `ai/llm.Classify` reads.

## Configuration

| Field | Meaning |
|---|---|
| `BaseURL` | API root; `/chat/completions` is appended |
| `APIKey` | sent as `Authorization: Bearer`; omitted when empty |
| `Model` | model id |
| `HTTPClient` | your client: timeouts, proxies, tracing (default `http.DefaultClient`) |

Set connection and header timeouts on the client's transport. Do not set `http.Client.Timeout`: it would also cut a long
stream. Bound the gaps between chunks with `ai/agent.Caller` instead.

## Tests

`go test ./...` runs against in-process servers; no key or network is needed.
