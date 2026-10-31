# ai/llm/anthropic

An `ai.Model` over the Anthropic **Messages** API (`POST /v1/messages`, streamed).

```go
model, err := anthropic.New(anthropic.Config{
    APIKey:   os.Getenv("ANTHROPIC_API_KEY"),
    Model:    "claude-sonnet-4-5",
    CacheTTL: "1h", // "" or "5m", or "1h"
})
```

- **Prompt caching.** Breakpoints are set after the tools, after the system prompt and after the last message, so a long
  agent history is read from the cache on the next call. `NoCache` turns them off.
- **Same shapes as the other clients.** Tool use becomes `ai.ToolCall`; tool results become `tool_result` blocks of the
  following user message; images become image blocks. Stop reasons are mapped to `stop`, `tool_calls` and `length`.
- **Usage** includes cache reads and writes (`PromptTokenDetails.CachedTokens`, `CacheWriteTokens`).
- **Errors** are `*APIError` with the HTTP status and Anthropic's error type; `ai/llm.Classify` understands them
  (overloaded and rate-limit responses are retryable, an over-long prompt is an overflow).
- `Config.BaseURL` (default `https://api.anthropic.com`) and `Config.Header` allow a proxy or gateway.
