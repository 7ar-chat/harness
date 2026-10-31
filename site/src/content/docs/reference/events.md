---
title: "Event types"
description: "Every AG-UI event a run streams, including the harness-specific work.* custom events, run outcomes, error codes and the synthetic stream_lost error."
---

A run streams its progress as [AG-UI](https://docs.ag-ui.com) events: run and step boundaries, text and reasoning messages, tool calls, interrupts and `CUSTOM` events named `work.*`. The vocabulary lives in `core/agui`. Every event is one JSON object with a `type` and a `timestamp` (milliseconds); the event log numbers them with a `seq` from 1, which is the resume point of [`Client.Events`](/docs/reference/client-contract/) and of the [SSE stream](/docs/reference/http-api/#event-stream). For the concepts see [Threads, runs and events](/docs/concepts/threads-runs-events/).

## A real run

A run of the minimal profile with an application tool `lookup_order` and a scripted model (timestamps removed, ids shortened):

```text
 1 {"protocolVersion":"1.0","runId":"…","threadId":"…","type":"RUN_STARTED"}
 2 {"stepName":"step-1","type":"STEP_STARTED"}
 3 {"parentMessageId":"…","toolCallId":"c1","toolCallName":"lookup_order","type":"TOOL_CALL_START"}
 4 {"delta":"{\"id\":\"A-17\"}","toolCallId":"c1","type":"TOOL_CALL_ARGS"}
 5 {"toolCallId":"c1","type":"TOOL_CALL_END"}
 6 {"content":"order A-17: shipped","messageId":"…","role":"tool","toolCallId":"c1","type":"TOOL_CALL_RESULT"}
 7 {"name":"work.usage","type":"CUSTOM","value":{"cachedInputTokens":0,"contextTokens":961,"contextWindow":128000,"inputTokens":0,"outputTokens":0,"reasoningTokens":0,"totalTokens":0}}
 8 {"stepName":"step-1","type":"STEP_FINISHED"}
 9 {"stepName":"step-2","type":"STEP_STARTED"}
10 {"messageId":"…","role":"assistant","type":"TEXT_MESSAGE_START"}
11 {"delta":"It ","messageId":"…","type":"TEXT_MESSAGE_CONTENT"}
12 {"delta":"order A-17: shipped","messageId":"…","type":"TEXT_MESSAGE_CONTENT"}
13 {"messageId":"…","type":"TEXT_MESSAGE_END"}
14 {"name":"work.usage","type":"CUSTOM","value":{…}}
15 {"stepName":"step-2","type":"STEP_FINISHED"}
16 {"outcome":{"type":"success"},"runId":"…","threadId":"…","type":"RUN_FINISHED","usage":[{"model":"demo","inputTokens":0,"outputTokens":0,"totalTokens":0,"reasoningTokens":0,"cachedInputTokens":0}]}
```

`agui.CheckOrder` verifies a recorded stream the way strict AG-UI clients do: `RUN_STARTED` first and only once, at most one terminal event and nothing after it, every `*_START` closed by its `*_END` with the same id, content and args only inside an open span, and nothing open at `RUN_FINISHED`. A text message always ends before the next tool call starts.

## AG-UI events

| Type | When | Fields |
|---|---|---|
| `RUN_STARTED` | The run begins. | `threadId`, `runId`, `protocolVersion` (`"1.0"`). |
| `STEP_STARTED` / `STEP_FINISHED` | One model call and its tool calls: `step-1`, `step-2`, ... | `stepName`. |
| `REASONING_START` / `REASONING_END` | The model streams reasoning text. | `messageId`. |
| `REASONING_MESSAGE_START` | Opens the reasoning message. | `messageId`, `role` (`"reasoning"`). |
| `REASONING_MESSAGE_CONTENT` | A chunk of reasoning. | `messageId`, `delta`. |
| `REASONING_MESSAGE_END` | Closes it. | `messageId`. |
| `TEXT_MESSAGE_START` | The assistant starts to answer. | `messageId`, `role` (`"assistant"`). |
| `TEXT_MESSAGE_CONTENT` | A chunk of the answer. | `messageId`, `delta`. |
| `TEXT_MESSAGE_END` | The answer (or the part before a tool call) ends. | `messageId`. |
| `TOOL_CALL_START` | The model names a tool call. | `toolCallId`, `toolCallName`, `parentMessageId`. |
| `TOOL_CALL_ARGS` | A chunk of the JSON arguments. | `toolCallId`, `delta`. |
| `TOOL_CALL_END` | The arguments are complete. | `toolCallId`. |
| `TOOL_CALL_RESULT` | The tool finished, or the call was refused. | `messageId`, `toolCallId`, `content`, `role` (`"tool"`), `metadata` (`{"isError": true}` on failure). |
| `STATE_SNAPSHOT` | The agent updated its plan (`update_plan`). | `snapshot`: `{"plan": [{id, title, status}]}`, status `pending`, `in_progress` or `completed`. |
| `CUSTOM` | A harness-specific event. | `name`, `value`: see below. |
| `RUN_FINISHED` | The run ended with an outcome. | `threadId`, `runId`, `outcome`, `usage`. |
| `RUN_ERROR` | The run failed. | `code`, `message`. |

`RUN_FINISHED` and `RUN_ERROR` are terminal (`agui.IsTerminal`); a stream ends after one of them.

A model call that fails with a retryable error closes the failed attempt's events (its tool calls get a "retried" error result), persists nothing of it, and emits `work.retry` before the new attempt.

## Outcomes

`RUN_FINISHED.outcome.type` is one of:

| Outcome | Meaning | Run status |
|---|---|---|
| `success` | The agent answered. | `completed` |
| `cancelled` | [`Cancel`](/docs/reference/client-contract/) stopped the run. | `cancelled` |
| `interrupt` | The run waits for the user. `outcome.interrupts` lists what it waits for. | `completed` (a question is recorded on the thread) |

`usage` is a list with one entry per model: `model`, `inputTokens`, `outputTokens`, `totalTokens`, `reasoningTokens`, `cachedInputTokens`. Sub-agent usage is reported under the sub-agent's own model name.

Each interrupt in `outcome.interrupts`:

| Field | Meaning |
|---|---|
| `id` | The interrupt id: the `interruptId` of the [`resume`](/docs/reference/http-api/#runs) request. |
| `reason` | `tool_approval` (a risky tool call paused) or `ask_user` (the agent asks a question). |
| `message` | The tool name for an approval; the question for `ask_user`. |
| `toolCallId` | The paused or asking tool call. |
| `responseSchema` | Approval: `{"decisions": ["approve","deny","respond"], "allowFreeText": true}`. Question: `{"options": [{label, description?}], "allowFreeText": bool}`. |
| `approval` | Approval only: the approval record (`id`, `kind`, `toolName`, `toolInputPreview`, `status`, `scope`, `createdAt`, ...). |

## Error codes

`RUN_ERROR.code`; the run status is `failed` except for `interrupted`.

| Code | Meaning |
|---|---|
| `model_error` | The model could not answer: "The AI model could not answer. Please try again in a moment." |
| `model_timeout` | The model call (connect, first chunk, or a gap of `LLM_STREAM_IDLE_SECONDS` between chunks) exceeded its budget after retries. |
| `rate_limited` | 429 retries exhausted. |
| `content_filter` | The provider stopped the answer (`finish_reason=content_filter`); nothing of it is kept. |
| `empty_answer` | No text and no tool call twice in a row (after one nudge); refusals count as empty. |
| `max_steps` | The agent did not finish within its step budget. |
| `interrupted` | The runtime stopped (or the application restarted) while the run was in progress. Send the message again. |
| `internal` | "An internal error occurred." The detail is logged, never sent. |
| `stream_lost` | Not sent by the server; see below. |

### `stream_lost`

`client.HTTP` produces a synthetic `RUN_ERROR` with code `stream_lost` and the cause as message when an event stream cannot be reconnected (after `WithMaxReconnects` failures, or a 4xx other than 429). It closes the channel so that a consumer waiting for a terminal event stops. It is not in the event log and has no `seq` in the run's history; the run itself may well have finished. Fetch `GetRun` to find out. The terminal client uses a similar synthetic code, `stream_failed`, when the backend's stream breaks.

## Custom events

`CUSTOM` events carry a `name` and a `value`. They are live progress information; the durable record is in the thread's messages.

| Name | When | `value` |
|---|---|---|
| `work.usage` | At the end of each step. | Cumulative for the run: `inputTokens`, `outputTokens`, `totalTokens`, `reasoningTokens`, `cachedInputTokens`, `contextTokens` (size of the last prompt), `contextWindow`. |
| `work.title` | A new thread got its title. | `title`. |
| `work.tool_output` | A command prints while it runs (`execute_command`). | `toolCallId`, `stream` (`stdout` or `stderr`), `text`. |
| `work.steer` | A message steered the run, or was kept for the next run. | `id`, `text`, `delivered` (true: this run's model got it; false: kept for the next run), and when known `author`, `authorKind`, `onBehalfOf`. |
| `work.subagent` | The `explore` sub-agent works (see below). | `toolCallId`, `phase`, `agent`, `step`, `model`, `usage`, plus per phase. |
| `work.approval` | A risky tool call needs a decision, or got one. | `phase` (`required` or `resolved`) and `approval` (the approval record, `status` `pending`, `resolved` or `cancelled`). |
| `work.compaction` | Context compaction. | `phase` (`started`, `completed`, `failed`, `cancelled`), `trigger` (`auto` or `manual`), `tokensBefore`, and on completion `tokensAfter` and `summarizedMessages`. |
| `work.goal` | The thread's goal was judged or changed. | `text`, `status` (`active`, `met`, `exhausted`, `cleared`), `turnsUsed`, `maxTurns`, `createdAt`, `updatedAt`, and `lastVerdict` (`met`, `reason`, `blockers`, `at`), `nextRunId`. |
| `work.memory` | The agent changed its memory. | `action` (`created`, `updated`, `proposed`, `deleted`), `id`, `path`, `title`, `text`, `status`, `contentHash`; `proposal`, `mode`, `proposed` when relevant. |
| `work.artifact` | A file under `/workspace` became (or changed as) an artifact. | `artifactId`, `path`, `name`, `version`, `sizeBytes`, `mimeType`, `kind`, `change` (`created`, `updated`, `deleted`), `toolCallId` (absent for end-of-run snapshots). |
| `work.artifact_skipped` | A workspace file is not saved. | `path`, `sizeBytes`, `maxBytes`, `reason` (`too_large`), `toolCallId`. |
| `work.verification` | A checker ran on a deliverable, or an answer's verification summary. | Per file: `toolCallId`, `path`, `checker`, `ok`, `summary`. Per answer: `messageId`, `verified`, `failed`, `unverified`. |
| `work.retry` | Before a new attempt of a failed model call. | `attempt` (the new attempt number), `reason` (the error class of the failed attempt, for example `transient`, `rate_limited` or `timeout`). |

### `work.subagent`

The `explore` tool runs a read-only sub-agent. Its progress is streamed under the tool call so a UI can show the steps live. At most 200 events are sent per call; the `end` event always goes out.

| `phase` | Extra fields |
|---|---|
| `start` | `task` (clipped to 400 characters). |
| `step` | none: a new model call; `step` increments. |
| `tool` | `tool`: `{name, args, result, isError}` (a one-line summary of the call and its first result line). |
| `text` | `text`: a piece of the sub-agent's text (clipped). |
| `end` | `reportChars`, `failed`. |

Every phase carries `toolCallId` (the explore call), `agent` (`"explore"`), `step`, `model` and `usage` (`{in, out, cached}`).

## Consuming events

<div class="callout tip">

**Decode `Data` yourself.** `client.Event.Data` is the whole event JSON. Switch on `Event.Type`, and for `CUSTOM` on `name`; ignore names you do not know, so a newer server keeps working. In Go, `agui.Event` is `map[string]any`, and the constants (`agui.TextMessageContent`, `agui.CustomUsage`, `agui.CodeMaxSteps`, ...) name every type, custom event and code on this page.

</div>

```go
for ev := range events {
	switch ev.Type {
	case agui.TextMessageContent:
		var e struct{ Delta string }
		_ = json.Unmarshal(ev.Data, &e)
		fmt.Print(e.Delta)
	case agui.RunError, agui.RunFinished:
		// terminal: the channel closes after it
	}
}
```

The full loop is in [The Client contract](/docs/reference/client-contract/#one-function-two-implementations) and [Smallest embedding](/docs/quickstart/embed/).
