---
title: Compaction
description: "How the harness keeps a long conversation inside the model's context window: masking old tool results, shrinking, summarizing, and what is persisted."
---

A long run fills the model's context window with tool output. The `ai/compaction` package keeps the prompt inside the window, and the engine calls it before every model call. You rarely call it yourself in an embedding; use it directly when you build your own loop (see [Use the ai module on its own](/docs/guides/use-ai/)).

## The package on its own

`compaction.Compact` is a pure function: it estimates the prompt and, when it is too large, returns a smaller one. You supply the model call that writes summaries. This program runs offline:

```go
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/compaction"
)

func main() {
	history := []*ai.Message{ai.UserMessage("Find the bug in main.go")}
	for i := range 6 {
		id := fmt.Sprintf("call_%d", i)
		history = append(history,
			ai.AssistantMessage("", []ai.ToolCall{{ID: id, Function: ai.FunctionCall{Name: "read_file", Arguments: `{"path":"main.go"}`}}}),
			ai.ToolMessage(strings.Repeat("line of code\n", 400), id, ai.WithToolName("read_file")))
	}
	history = append(history, ai.AssistantMessage("The bug is in the loop bound.", nil))

	// 1. Over the threshold: old tool results become previews first.
	res, err := compaction.Compact(context.Background(), history, compaction.Options{
		Window:    8000, // the model's context window, in tokens
		Threshold: 0.75, // compact above this share of it
	})
	fmt.Println(res.Action, "| err:", err, "| smaller:", res.TokensAfter < res.TokensBefore)

	// 2. Manual: fold everything into a summary written by your model call.
	res, _ = compaction.Compact(context.Background(), history, compaction.Options{
		Window: 8000,
		Manual: true,
		Summarize: func(_ context.Context, previous, transcript string) (string, error) {
			// call a model here, with compaction.Instructions as the system prompt
			return "Goal: find the bug in main.go. The loop bound is wrong.", nil
		},
	})
	fmt.Println(res.Action, "|", len(history), "->", len(res.Messages), "messages |", res.Summary)
}
```

Output:

```text
shrunk | err: <nil> | smaller: true
summarized | 14 -> 0 messages | Goal: find the bug in main.go. The loop bound is wrong.
```

## Two passes

1. **Shrink.** Tool results older than a recent tail become short previews (head and tail). This is often enough.
2. **Summarize.** Everything before a recent tail is folded into one summary written by `Options.Summarize`. If it fails, the head is dropped instead and `Result.Err` says why. The returned error is only ever the context's.

A tool call and its result are never split. `Result.Action` is `none`, `shrunk`, `summarized` or `dropped`. Other options:

| Option | Use |
|---|---|
| `Observed` | The prompt size the provider reported; more exact than the estimate. |
| `Manual` | Summarize now, whatever the size; a manual fold covers every message. |
| `Force` | Compact after the provider reported an overflow: keeps a smaller tail. |
| `SummaryBudget` | Bounds the summarizer's input; a larger fold is summarized chunk by chunk. |
| `Previous` | The summary so far; it is merged into the new one. |
| `FilesHeading`, `FileTools` | Summaries end with the list of files the conversation wrote (calls of `write_file` and `edit_file` by default). |

`EstimateText`, `EstimateMessages` and `EstimateTools` predict token counts without a provider call, per script (Latin, Cyrillic, Greek, CJK, digits, punctuation, emoji), calibrated against o200k counts. They decide *when* to compact; the engine prefers the provider-reported size once it has one.

## In the engine

Before every model call the agent loop runs the check with the run's window, a threshold and its estimate of the system prompt and tool definitions. Three things happen at different pressure levels:

| Mechanism | When | Effect |
|---|---|---|
| **In-run masking** | The next prompt passes `COMPACTION_MASK_FROM_FRACTION` of the window (default `0.5`) | Tool results older than the newest `COMPACTION_KEEP_RECENT_TOOL_RESULTS` (default `6`) are replaced by short stubs in the model input. The stored history is not changed, and `load_skill`, `ask_user` and `update_plan` results are never masked. `COMPACTION_KEEP_RECENT_TOOL_RESULTS=0` disables masking; `COMPACTION_MASK_FROM_FRACTION=0` masks from the first step instead of under pressure. |
| **Automatic compaction** | The prompt passes the threshold | Shrink, then summarize. After one, the engine waits until the history has grown again before checking. |
| **Overflow compaction** | The provider reports a context overflow | Compacts with `Force` and retries the call once. |

The window of a model comes from `LLM_CONTEXT_WINDOWS` (`model=tokens,...`; 128000 for models not listed). The threshold is `COMPACTION_THRESHOLD`, a share between 0 and 1; when unset it is `(window - reserve) / window`, where the reserve is the model's max output (`LLM_MAX_OUTPUT_TOKENS`, default 16000) plus 20000 tokens, kept between 0.3 and 0.95.

| Variable | Default | Meaning |
|---|---|---|
| `COMPACTION_THRESHOLD` | unset | Share of the window that triggers compaction. |
| `COMPACTION_MODEL` | the run's model | The model that writes summaries (a cheaper one is fine). |
| `COMPACTION_KEEP_RECENT_TOOL_RESULTS` | `6` | Newest tool results kept in full by masking. |
| `COMPACTION_MASK_FROM_FRACTION` | `0.5` | Share of the window at which masking starts. |

## Manual compaction

* **TUI:** `/compact` (alias `/summarize`) summarizes the conversation now and prints `compacted: <before> → <after> tokens`, or `nothing to compact`. It refuses while a run is active.
* **HTTP:** `POST /api/threads/{id}/compact`; see the [HTTP API](/docs/reference/http-api/).
* **Library:** `Harness.Compact(ctx, user, threadID, model)` creates a run of kind `compact`, waits for it, and returns a `CompactResult{Compacted, TokensBefore, TokensAfter}`. It fails with `store.ErrRunActive` when the thread has a run in progress.

A compaction run changes only the summary: it takes no workspace snapshot and no checkpoint archive. A resumed run, a goal turn and a compaction keep the sandbox mode of the thread's latest run.

## What is persisted

* The **summary** is stored on the thread (`store.ThreadSummary`: the text and `ThroughPosition`, the highest message position it covers). The next run loads the summary plus the messages after that position, so a restarted process continues from the compacted state. Compaction itself deletes no message.
* A completed compaction is recorded (`store.Compaction`: trigger `auto` or `manual`, tokens before and after) so a UI can show its divider after a reload.
* The run's event stream carries `CUSTOM` events named `work.compaction` with `phase` `started`, `completed`, `failed` (the head was dropped) or `cancelled`, plus `trigger`, `tokensBefore`, `tokensAfter` and `summarizedMessages`. See [Event types](/docs/reference/events/).

After an automatic compaction, the engine adds a restored-context message with the user's latest request verbatim, so the model does not lose what it was asked.
