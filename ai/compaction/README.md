# ai/compaction

Keeps a conversation inside a model's context window. `Compact` is a pure function: it estimates the prompt, and when
it is too large returns a smaller one. You decide when to call it and supply the model call that writes summaries.

```go
res, err := compaction.Compact(ctx, history, compaction.Options{
    Window:    128_000, // the model's context window, in tokens
    Threshold: 0.75,    // compact above this share
    Reserved:  systemAndToolsTokens,
    Previous:  summarySoFar,
    Summarize: func(ctx context.Context, previous, transcript string) (string, error) {
        // call a model with compaction.Instructions as the system prompt
    },
})
history, summarySoFar = res.Messages, res.Summary
```

## Two passes

1. **Shrink.** Tool results older than a recent tail become short previews (head and tail). Often enough.
2. **Summarize.** Everything before a recent tail is folded into one summary written by your `Summarize` function. If it
   fails, the head is dropped instead and `Result.Err` says why; the returned error is only ever the context's.

A tool call and its result are never split. `MarkRequest` and `MarkSteer` flag the user messages that start or redirect a
turn; a compaction that cuts inside a turn keeps those verbatim.

`Result.Action` is `none`, `shrunk`, `summarized` or `dropped`.

## Options worth knowing

| Option | Use |
|---|---|
| `Observed` | the size the provider reported for this prompt; more exact than the estimate |
| `Manual` | summarize now, whatever the size |
| `Force` | compact after the provider reported an overflow (the estimate was too low): keeps a smaller tail |
| `SummaryBudget` | bounds the summarizer's input; a larger fold is summarized chunk by chunk |
| `FilesHeading`, `FileTools` | summaries end with the list of files the conversation wrote (calls of `write_file`/`edit_file`, or `SetFiles`) |

`SetPosition` records where a message sits in your own log; `Result.Through` is the highest position a summary covers.

## Estimates

`EstimateText`, `EstimateMessages` and `EstimateTools` predict token counts without a provider call, per script (Latin,
Cyrillic, Greek, CJK, digits, punctuation, emoji), calibrated against o200k counts. They are for deciding *when* to
compact; use `Observed` when you have the real number.
