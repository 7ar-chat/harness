# ai/llm/responses

An `ai.Model` over the OpenAI **Responses** API (`POST …/responses`, streamed), for models that are only or best served
there (the reasoning models).

```go
model, err := responses.New(responses.Config{
    URL:    "https://api.openai.com/v1/responses",
    Model:  "o4-mini",
    Header: http.Header{"Authorization": {"Bearer " + key}},
})
```

## Reasoning carried between calls

Calls are stateless (`store: false`) and ask for `reasoning.encrypted_content`. The encrypted reasoning items of an
answer travel on the answer message (`ItemsOf`) and are sent back, in order, before the answer's own items on the next
request, so the model keeps its reasoning across tool steps.

- Keep the message as it is and the items go back by themselves.
- To persist them, `MarshalItems(ItemsOf(msg))` gives bytes; `SetItems(msg, UnmarshalItems(raw))` puts them back on a
  reloaded message. Each item remembers which model produced it (`Config.Name`) and is only sent back to that model.
- If the provider rejects the items (a changed key or model), the call is re-sent once without them and they are not sent
  again. `Config.Logger` hears about it.

## Also

Function tools are sent in strict mode when their schema allows it (`Strict`). Errors are `*APIError` with status and
code, understood by `ai/llm.Classify`. `Config.OnRefusal` receives streamed refusal text.
