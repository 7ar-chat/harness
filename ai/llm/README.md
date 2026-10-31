# ai/llm

What sits between a model client and an application: a factory that picks the right client for a model id, and the
functions that decide what to do when a call fails.

## Provider

```go
p := llm.New(llm.Options{
    Provider:        "openai",                   // or "anthropic"
    BaseURL:         "https://api.openai.com/v1",
    APIKey:          key,
    Models:          []string{"gpt-4o", "o4-mini"}, // allow-list: any other id is refused
    ResponsesModels: []string{"o4-mini"},        // called through the Responses API
    TimeoutSeconds:  60,                         // connect + response headers
})
model, err := p.Model(ctx, "gpt-4o") // an ai.Model; built once, then reused
```

All models share one HTTP client, so the timeouts, the optional session header and `CallInfo` apply to every call.
`Discover` asks the provider which models it serves.

`llm.Factory` is the one-method interface (`Model(ctx, name)`) your code should accept; `Provider` and
`llmtest.Factory` implement it.

## Failures

```go
class := llm.Classify(err)      // fatal | transient | rate_limited | timeout | overflow
if class.Retryable() { time.Sleep(llm.Backoff(attempt, time.Second, retryAfter, rand.Float64())) }
if llm.IsContextOverflow(err)  { /* shrink the request: see ai/compaction */ }
```

`Classify` reads `StatusCode()` and `ErrorCode()` from the error through interfaces, so any client that exposes them
works, and falls back to the message for errors that carry none. A 429 is always a rate limit; an overflow needs a
400/413 or the `context_length_exceeded` code; DNS and TLS failures are fatal. `ParseRetryAfter` reads `Retry-After`,
`retry-after-ms` and the `x-ratelimit-reset-*` headers.

## CallInfo

Some things a call learns are not in the message stream. `WithCallInfo(ctx)` returns a context whose calls record the
provider's `Retry-After` hint, streamed refusal text and the stream's liveness (`OnActivity`) into a `CallInfo`. The
`ai/agent` watchdog uses the last one.

## Sub-packages

[`anthropic`](anthropic) and [`responses`](responses) are the other two clients; [`llmtest`](llmtest) is the fake model.
