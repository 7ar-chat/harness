# modelinfo

What a model can do and what it costs: context window, output limit, capabilities (vision, reasoning, tools) and
per-token prices, read from [LiteLLM's public price table](https://github.com/BerriAI/litellm). No dependencies.

```
go get github.com/7ar-chat/harness/modelinfo
```

```go
table, err := modelinfo.Load(ctx, modelinfo.Options{CacheDir: dir}) // fetches the table, keeps a cached copy
info, ok := table.Lookup("gpt-4o-mini", "openai")                  // the second argument is a provider hint
fmt.Println(info.ContextWindow, info.MaxOutput, info.Vision)

usd, priced := modelinfo.Cost(info, modelinfo.Usage{In: 12_000, Out: 800, Cached: 9_000, Prompt: 12_000})
```

- **Matching is conservative.** An id is matched exactly, then with a provider prefix, then as the same name under another
  provider or a dated release of the same model. It never matches across unrelated models, so a wrong price is rarer than a
  missing one. `ok` is false when nothing matches.
- **Cost is honest about unknowns.** `priced` is false when the table has no price for the model: zero then means
  "unknown", not "free". Cached tokens are billed at the cache-read price when the table has one. Models with a higher
  price above a prompt size (`Tier`) use it when `Usage.Prompt` crosses the threshold.
- **Offline use.** `Options.File` reads a local table, `Offline` uses the cache however old it is, and `Override` is your
  own file in the same schema whose models win. `LoadAsync` returns a table at once and fills it in the background.
- `DefaultContextWindow` (128000) is what a caller can fall back on for an unknown model.

The table is data, not authority: check prices that matter against the provider.

## License

MIT. See [LICENSE](LICENSE).
