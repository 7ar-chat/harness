# Work Chat harness v3 — "real jobs" contract

Implements `~/agent-loops/ROADMAP.md` (all items except 0.1, the pip credential). Bug IDs
(`A-`/`B-`/`C-`) refer to `~/agent-loops/STUDIO-BUGS.md`; evidence for each mechanism is in
`~/agent-loops/COMPARISON.md`. This file is the shared contract between the parallel workstreams
(see "Workstreams" at the end). When behaviour here changes, update this file in the same commit.
Everything not mentioned keeps the v2 contract (`README.md`, `docs/harness-v2.md`, `openapi.yaml`).

## 1. Run errors and outcomes

`RUN_ERROR.code` gains:
- `model_timeout` — the model call (connect, first byte, or a gap of `LLM_STREAM_IDLE_SECONDS`, default 90,
  between chunks) exceeded its budget after retries;
- `empty_answer` — the model returned no text and no tool call twice in a row (after one nudge);
- `content_filter` — the provider stopped the answer (`finish_reason=content_filter`);
- `rate_limited` — 429 retries exhausted.
Existing: `model_error`, `interrupted`, `max_steps`, `internal`. A refusal is treated like an empty answer
(one nudge, then `empty_answer`). `finish_reason=length` never executes the tool calls of that message: they
get the error result *"Your previous message was cut off by the output limit before the tool call was
complete. Re-issue it in smaller pieces (e.g. write the file in parts)."* and the loop continues.

A stream that breaks mid-step is retried as a whole step (up to `LLM_STEP_RETRIES`, default 3, exponential
backoff with jitter, honouring `Retry-After`): the partial assistant text/tool calls of the failed attempt are
**not** persisted; already-emitted `TEXT_MESSAGE_START`/`TOOL_CALL_START` of the failed attempt are closed with
their `*_END`, the tool calls get a `TOOL_CALL_RESULT` with `metadata.isError=true, metadata.retried=true` and
content *"(stream interrupted, retrying)"*, and a `CUSTOM work.retry {attempt, reason}` precedes the new
attempt. Clients render a retried attempt as superseded.

## 2. Model-facing tools (new / changed)

All tool descriptions render in full (no comma truncation; a golden-schema test guards this).

| Tool | Input | Result (JSON string) |
|---|---|---|
| `read_file` (changed) | `{path, offset?, limit?, pages?, sheet?}` | text files: numbered lines as today, footer always correct; **PDF/DOCX/PPTX/XLSX/ODF**: extracted text via markitdown (`pages` for PDF/PPTX ranges like `"1-3,7"`, `sheet` for XLSX), with a header `[document: N pages/slides/sheets; showing …]`; **images** (png/jpeg/webp/gif ≤ 5 MiB): `{path, image: true}` + the image attached for vision models (see §3); other binaries: error with a hint |
| `view_file` (new) | `{path, pages?}` | renders PDF/DOCX/PPTX/XLSX pages (LibreOffice → PDF → PNG, 110 dpi, max 6 pages per call) into `/workspace/.work/renders/`; result `{path, pages: [{page, imagePath}], totalPages}`; the PNGs are attached as images for vision models; non-vision models get an error "this model cannot view images; use read_file and the skill's inspect script" |
| `search_files` (new) | `{pattern, path? (default /workspace), glob?, ignoreCase?, maxResults? (default 100, ≤ 500)}` | ripgrep in the sandbox: `{matches: [{path, line, text}], truncated}` |
| `execute_command` (changed) | `{command, timeoutSeconds? (default 120, ≤ 1800), background?: bool}` | finished: `{stdout, stderr, exitCode, truncated?, outputPath?, stderrPath?}` (stdout: first 4 KB + last 8 KB, stderr: 1 KB + 3 KB; `outputPath`/`stderrPath` = `/workspace/.work/jobs/<jobId>/stdout`/`stderr` when cut); still running at `timeoutSeconds` or `background:true`: `{jobId, status: "running", stdoutTail, stderrTail, outputPath}` — the command keeps running |
| `job_output` (new) | `{jobId, waitSeconds? (≤ 300)}` | `{jobId, status: "running" \| "finished" \| "killed", exitCode?, stdoutTail, stderrTail, outputPath}` |
| `job_kill` (new) | `{jobId}` | `{jobId, status: "killed"}` (`"finished"` when the job had already ended) |
| `edit_file` (changed) | unchanged | `{path, replacements, matched: "exact" \| "whitespace"}`; exact match first; if none, a whitespace/indentation-tolerant match **only when unique** (the new text takes the file's indentation; `replaceAll` needs exact matches); on a miss the error lists the 3 closest regions with line numbers; edits of a file changed since the agent last read/wrote it are refused (`"file changed since you last read it; read it again"`), and so are edits of a file not read or written in the thread (read-before-edit; per runtime process, a resumed run on another replica re-reads first) |

Any tool output larger than `AGENT_TOOL_OUTPUT_SPILL_CHARS` (default 20000) is written in full to
`/workspace/.work/tool-output/<toolCallId>.txt`; the model receives head (8k) + tail (4k) + `outputPath` + a
hint to page it with `read_file`/`search_files`. Nothing is silently dropped. (`execute_command` reports
`outputPath` too.)

Read-only tools (`read_file`, `view_file`, `list_files`, `search_files`, `load_skill`, `job_output`,
web-search and MCP tools flagged read-only) implement `tools.ReadOnly` and may run in parallel within a step
(max 4); writes and commands run sequentially in the model's order. Events keep the order of the model's tool
calls (`TOOL_CALL_RESULT` events are emitted in call order even if results finish out of order).

## 3. Images

Image parts (from `read_file`, `view_file`, attachments) are sent only to vision models
(`LLM_VISION_MODELS`), at most 6 images / 6 MiB per request; an image is sent in the step after the tool that
produced it and replaced by a text stub (`[image /workspace/.work/renders/p1.png shown earlier]`) in later
steps (A-13).

## 4. Verification

- **Checker contract (skills).** Every built-in skill ships checkers that print one JSON object on stdout
  `{"ok": bool, "file": "<path>", "checks": [{"name", "ok", "detail"}], "next_step"?: string}` and exit 0 when
  `ok`, 2 when not ok, 1 on usage error: `docx/scripts/inspect_docx.py --check <file>`,
  `pptx/scripts/fit_text.py <file>` (+ `inspect_pptx.py --check`), `xlsx/scripts/recalc.py <file>`,
  `pdf/scripts/extract.py --check <file>`, `charts/scripts/make_chart.py --check <png>`.
- **Post-write validation.** When a tool call created or changed a file under `/workspace/outputs/` (artifact
  snapshot), the runtime runs the matching checker in the sandbox (by extension; ≤ 60 s) and appends its
  summary to that tool's result; it emits `CUSTOM work.verification {toolCallId, path, checker, ok, summary}`.
- **Verification StopGuard.** When the model wants to stop and a deliverable changed in this run has no
  passing checker result *after its last change* (or, for docx/pptx/pdf with a vision model, no `view_file`
  of it after its last change), the stop is refused once with a reminder naming the files and what is missing.
  If the model stops again anyway, the final assistant message gets
  `metadata.verification = {verified: [path…], unverified: [{path, reason}]}` and the prompt rule makes the
  model say what it could not verify. Assistant messages always carry `metadata.verification` when the run
  changed deliverables.
- **Goal judge** gets, per deliverable, its last checker result and up to 2,000 chars of extracted text, and
  returns `{met, reason, blockers: string[]}`; `Goal.lastVerdict` gains `blockers`.

## 5. Context

- Default windows: `gpt-5.6-sol=272000,gpt-5.6-terra=272000`; compaction threshold = `window − reserve`
  (`reserve` = max output tokens + 20k), not 0.75 × window.
- In-run masking: tool results older than the last `COMPACTION_KEEP_RECENT_TOOL_RESULTS` (default 6) tool
  results are replaced in the model input by a stub `[<tool> result, <size>, exit <code> — re-run or read
  <outputPath> if needed]` (the stored message is unchanged; the UI shows full results).
- Summaries are built from all folded messages (map-reduce over chunks sized to the summarizer's window).
  After a compaction the runtime re-injects (as a context message after the summary): the current plan, the
  text of the skills loaded in this run, the list of deliverables with their last checker status, and the
  last user request verbatim.
- `CUSTOM work.compaction` gains `phase: "cancelled"`.
- `CUSTOM work.usage` gains `cachedInputTokens` (when the provider reports it); `TokenUsage` too.
- Reasoning passback for models in `LLM_REASONING_PASSBACK_MODELS` (default: the DeepSeek models): assistant
  messages carry `reasoning_content` within and across runs.
- Request layout is cache-stable: static system prompt first, dynamic context last (§6), old messages never
  rewritten between steps of a run except by masking (applied identically on every step).

## 6. System prompt

New prompt (ROADMAP Appendix A) in `internal/agent/prompts/` with a per-model registry:
`LLM_PROMPT_VARIANTS` (`model=variant,…`; variants `default`, `gpt-5.6`, `deepseek`, `legacy` (the v2 prompt,
kept for A/B)); default variant `default`. Skills appear once (index). The memory policy is ~6 lines. Dynamic
context (date, user, goal, memory recall, summary) is the last section.

### Stream C2 refinements of §2–§4, §6 (implemented on `rj/tools`)

- **read_file**: text pages stop at 2,000 lines *or* ~18,000 characters (below the spill threshold, so the
  `Use offset=N` footer is always exact); images return `{path, image: true, bytes, note?}` (`note` for models
  that cannot see images). Legacy/ODF formats (`.doc/.xls/.ppt/.odt/.ods/.odp/.rtf`) are converted by LibreOffice
  first. The sandbox image gains `ripgrep`; `search_files` falls back to Python without it.
- **Tool images**: tools queue images through `tools.ImageSink` (`CanViewImages`, `AttachImage(tools.Image{Path,
  MIMEType, Data})`); `agent.Harness.Vision` (LLM_VISION_MODELS) decides. Delivery (§3 caps, stubs) is stream
  C1's `internal/agent/images.go` (queue: `image_queue.go`), the one image path since the merge.
- **Checkers** (developer D): docx `inspect_docx.py --check`, pptx `fit_text.py`, xlsx/xlsm `recalc.py
  --check-only` (plain `recalc.py` rewrites the file), pdf `extract.py --check`, png/jpg/svg `make_chart.py
  --check`, plus generic validators for py (py_compile), json, csv. The checker's skill is installed first; at most
  3 files are checked per tool call (others: "not checked automatically"). Exit 1 or any code but 0/2, a timeout or
  no JSON = "could not run" (`failed: true`): unverified, not a verdict.
- **Post-write validation** runs before the call's `TOOL_CALL_RESULT` (whose content ends with `[verification] path —
  summary` lines); `work.verification {toolCallId, path, checker, ok, summary}` follows it; the tool message gets
  `metadata.verification: [{path, checker, ok, failed?, summary}]`. A checker the model runs itself through
  `execute_command` (contract JSON naming a file under `/workspace/outputs/`) counts the same way.
- **StopGuard / answers**: the verification reminder is part of the (at most two) stop reminders and is given once
  per run. `metadata.verification {verified, unverified: [{path, reason}]}` is on every *answer* (assistant message
  without tool calls) of a run whose turn changed deliverables; files with no checker (and no view requirement) are
  in neither list; deleted files are dropped. A live `work.verification {messageId, verified, unverified}` follows
  the answer when one list is non-empty. "This run" means the turn: a run resumed after an approval or an answer
  rebuilds the state from the turn's stored messages (artifact and verification metadata, `view_file` calls).
- **Rehydration**: `verify.Verifier` implements stream C1's `agent.DeliverableSource` (the turn's deliverables,
  `OK` nil when no runnable check followed the last change).
- **Goal judge**: `goals.Hook.Evidence` (verify) gives ≤ 8 deliverables with checker result and ≤ 2,000
  characters fenced as `<deliverable_text>`; a failing checker overrules `met` and becomes a blocker; the next goal
  run's message ends with "Still missing:" and the blockers.
- **Stuck detection** (4.3): identical ×3 and A-B-A-B-A-B are refused (before an approval would be asked); the
  3rd occurrence of an error text gets a note; 5 failed calls in a row a note pointing to `ask_user`; after 8 the
  next call is sent with `tool_choice: none` and "Too many tool calls in a row failed (8). …". Tools implementing
  `tools.TurnObserver` (the plan StopGuard) get the turn's earlier calls in resumed runs (A-21).
- **Budget reminder** (4.4): before steps 9, 17, 25… a `<system-reminder>` "Step n of N (k left)." + open plan items
  + unverified deliverables (not persisted).
- **Usage** (4.5): `runtime.Finish.AddUsage`; title and judge tokens are merged into `RUN_FINISHED.usage` (per
  model); title, judge and summarizer calls use `reasoning_effort: low` on reasoning models.
- **Links** (4.6): the answer checker's one corrective note also names saved files the answer mentions without a
  workspace-file link (absolute paths, or names of this run's outputs).
- **Reasoning efforts** (A-29): `LLM_REASONING_EFFORTS` (`model=effort|…`; GPT-5.6 Sol/Terra
  `none|low|medium|high|xhigh`, other reasoning models `minimal|low|medium|high`). Runs, goals and skill sessions
  asking an effort the model does not accept → `400 invalid_request`; a stored one is sent as the nearest accepted
  (ties go up: `minimal` → `low`); `/models` items gain `reasoningEfforts`.
- **§6 prompt**: variant resolution = `LLM_PROMPT_VARIANTS` entry, else `deepseek` when the model name contains
  "deepseek", else `default` (default config maps GPT-5.6 Sol/Terra to `gpt-5.6`). Sections render per variant
  (`agent.Input.Variant`, `agent.Legacy`, `agent.Part`); dynamic sections go to `agent.Harness.Context`. Sizes for a
  typical run (o200k tokens): legacy 3,564, default 1,410, gpt-5.6 1,452, deepseek 869. Since the merge with stream A, the v3 `<deliverables>` block also carries A's
  50 MiB file-size rule (legacy gets it from the file-reference section): about 30 tokens more each. An explicitly attached
  user/global SKILL.md reports itself (`agent.ReportUntrusted`): the run's context is untrusted from its start (A-11).

## 7. Approvals

- Resolve body gains `scope?: "once" | "run"` (default `once`). `run` approves this tool *kind* for the
  rest of the run and its resume runs (stored on the run chain); the UI offers "Allow for this task".
- Interactive mode gates: `execute_command`, `edit_file`/`write_file` **outside** `/workspace/outputs/` and
  `/workspace/.work/`, deletes/moves outside those dirs, and MCP tools whose name matches
  `create|send|update|delete|post|publish|write` (configurable `APPROVAL_RISKY_TOOL_PATTERNS`). Writes inside
  `/workspace/outputs/` and `/workspace/.work/` never need approval.
- Approvals expire after `APPROVAL_TTL_HOURS` (default 72) → status `expired`, tool result "The approval
  expired.".
- Fix B-13 (approval decision payload sent to an ask_user interrupt → `400 invalid_request`).
- One decision point (merge of B and C2): `approvals.Gate.NeedsApproval` (stream B) applies all the rules above
  (paths resolved relative to `/workspace`, `..` cleaned; MCP tools by `APPROVAL_RISKY_TOOL_PATTERNS` per name word,
  plus the v2 delete/remove/move/rename prefixes) and skips the tool kinds in `run.ApprovedTools` (scope `run`,
  inherited along the run chain by the store). The agent keeps no approval logic of its own: it only refuses a call
  the loop guard would refuse before asking for its approval.

## 8. Notifications (job finished while away)

- `Thread` gains `unread: boolean` (a run of this thread reached a terminal state after `lastSeenAt`) and
  `lastRun: {id, status, finishedAt} | null`.
- `POST /threads/{threadId}/seen` `{}` → `204` (sets `lastSeenAt = now`). Opening a thread calls it.
- `GET /threads?unread=1` filters. `GET /notifications/summary` → `{unreadThreads: number}`.
- Frontend: unread dot + "Finished" label in the thread list, count in the sidebar/module entry and the tab
  title, and a browser notification (Notification API, permission asked once from Settings, never on load)
  when a run the user started ends while the tab is hidden.

## 9. HTTP contract fixes

- JSON error envelope on unknown routes (404 `not_found`) and wrong methods (405 `method_not_allowed`).
- `POST /runs/{id}/cancel` and `POST /threads/{id}/compact` accept an empty body.
- `Idempotency-Key` header on `POST /threads/{id}/runs` (24 h; same key + same body → same response).
- All emitted error codes in the `openapi.yaml` enum (a test diffs handlers' codes vs the enum).
- `files/content` only serves paths under `/workspace` (`400 invalid_path` otherwise).
- Runs of soft-deleted threads → `404`.
- Concurrent SSE streams capped per user (`MAX_STREAMS_PER_USER`, default 8 → `429 too_many_streams`).

<!-- stream B (runtime & API): the contract of §7–§9 as implemented -->
### §7–§9 as implemented (stream B)

- **§7** — `scope` is accepted on `POST /runs/{runId}/approvals/{approvalId}/resolve` and on a resume entry's
  `payload.scope`; it only takes effect with `approve`. The tool *kind* is the normalized tool name (lower case, `-`/`_`
  removed); approved kinds live on `work_runs.approved_tools` and are inherited by every run resuming the run (approval
  decisions and `ask_user` answers), not by a new message. `Approval` gains `scope: "once" | "run"` and status
  `expired`. Path rule: every path argument (`path`, `paths`, `source`, `destination`, `target`, `from`, `to`, `file`,
  `filePath`, `file_path`; relative = under `/workspace`, cleaned) must be under `/workspace/outputs/` or
  `/workspace/.work/` for a write/edit/delete/move to skip approval; no path = ask. MCP tools: a name word (snake, kebab,
  camelCase) matching `^(APPROVAL_RISKY_TOOL_PATTERNS)` case-insensitively, or the v2 delete/remove/move/rename prefix
  rule. Expiry runs as a sweeper job every minute and at decision time; deciding an expired approval is `409
  approval_expired` (new code); the next run announces it (`work.approval resolved`, `status: "expired"`). B-13: `409
  no_pending_question`.
- **§8** — `Thread.lastRun` is the thread's latest run by creation (also while queued/running, `finishedAt: null`);
  `unread` counts any run finished after `lastSeenAt` (threads that existed before migration `0100` count as seen; new
  threads start seen). `POST /threads/{threadId}/seen` takes `{}` or no body. `GET /threads?unread=1` combines with
  `search` and the cursor (`unread=0` or absent: all; other values `400`).
- **§9** — 404/405 envelope codes `not_found` / `method_not_allowed` (new code). Body-less POST (no `Content-Type`)
  is accepted on `/runs/{id}/cancel`, `/threads/{id}/compact` and `/threads/{id}/seen` only. `Idempotency-Key`: ≤ 200
  chars, scoped per user and thread; a key whose request was refused before the run existed is released; replays of
  a streaming request stream the same run. `too_many_streams` is per user **and api process**. `/threads` is
  keyset-paged on the immutable `createdAt` (pinned first, then newest *created* first — no longer by `updatedAt`).
  Also: `compact` with an unknown `model` is `400 invalid_model`; `POST /artifacts/{id}/restore` during an active run is
  `409 workspace_restore_blocked`; `DJANGO_PRODUCTION` is on for any non-empty value.
- **Runtime** — the runtime process's `/health` is `503 {"status":"unhealthy","error"}` when its queue loop is stuck;
  new settings `RUNTIME_TERMINATION_GRACE_SECONDS` (30), `MAX_STREAMS_PER_USER` (8), `APPROVAL_TTL_HOURS` (72),
  `APPROVAL_RISKY_TOOL_PATTERNS`. Migrations `0100_notifications`, `0101_thread_list_created`, `0102_approvals_v3`.
<!-- end stream B -->

## 10. DeepSeek models

Model ids are sent verbatim to the provider (a `deepseek/…` id, as OpenRouter names it, is recognised as DeepSeek by
name only). DeepSeek models default to prompt variant `deepseek`, reasoning passback on, vision off,
window 1,000,000 with compaction at `window − reserve`. `/models` items gain `promptVariant`.

## 11. Frontend (summary)

New/changed UI: retried-attempt rendering (`work.retry`); `view_file` card with page thumbnails (served via
`files/content`); `search_files` card; background job card (`execute_command` with `jobId`, then `job_output`
updates: status, exit code, output tail; no Kill button in v3 — `job_kill` is model-side only); verification badges on file chips and deliverables (`work.verification`,
`metadata.verification`, "not verified" warning); approval "Allow for this task"; notifications (§8); goal
blockers; usage incl. cached tokens; all v3 error codes with messages in six locales; the C-12…C-14,
C-28…C-32 fixes; remote images in markdown blocked (C-31).

<!-- C1 (loop & context) block: begin -->
## C1 loop & context — precisions (implemented on `rj/loop`)

These complete §1, §2 (spill, ReadOnly, parallel), §3, §5 and §10; where they differ from the text above, this block wins.

- **§1 retries.** `CUSTOM work.retry {attempt, reason}` precedes *every* re-issued model call, also one that failed
  before its first chunk; `attempt` is the number of the new attempt (2 = first retry), `reason` is `timeout`,
  `rate_limited` or `transient`. Backoff: `1 s·2ⁿ⁻¹` (≤ 30 s) × jitter [0.5, 1.5), never below `Retry-After`
  (also `retry-after-ms`, `x-ratelimit-reset-*`; capped at 120 s). When the last attempt fails, its announced calls
  get `TOOL_CALL_RESULT {metadata.isError: true}` with content `(stream interrupted)` (no `retried`), and the run
  ends with `model_timeout`, `rate_limited` or `model_error`. A cancelled stream keeps its partial text (v2).
  `LLM_TIMEOUT_SECONDS` bounds connect + response headers + first chunk; `LLM_STREAM_IDLE_SECONDS` each later gap.
- **§1 finish_reason.** A text answer cut by `length` is persisted and continued with the unpersisted note
  *"Your previous answer was cut off by the output limit. Continue exactly where it stopped, without repeating what
  you already wrote."* (at most 2 per run). `content_filter`: nothing of the answer is persisted; calls it announced
  get an error result `(stopped by the content filter)`. An empty answer on the step limit's last step ends the run
  with `empty_answer` (no step is left for the nudge).
- **§1 AG-UI order.** Within an attempt the text message ends before its first `TOOL_CALL_START` and every tool call
  ends before the next one starts (text arriving after a tool call reopens the same `messageId`).
- **§2 spill.** The preview (head 8000 + marker with the path and a `read_file`/`search_files` hint + tail 4000) is
  what the model, the `TOOL_CALL_RESULT` and the stored message carry; the agent writes the file through
  `tools.OutputSpiller` (`tools.SandboxSpiller`, the thread's sandbox). Without a sandbox the output is cut to head +
  tail with a marker. MCP results are capped at 1 Mi characters so the spill keeps them whole.
- **§2 ReadOnly.** `type ReadOnly interface { ReadOnly() bool }` in `internal/tools/tools.go`. Only consecutive
  read-only calls that need no approval run in parallel; their live `work.tool_output` is emitted right before their
  result. MCP tools are read-only when annotated `readOnlyHint`.
- **§3 images API.** Tools only queue: `tools.AttachImage(ctx, tools.Image{Path, MIMEType, Data}) bool` (false: the
  model has no vision) and `tools.CanViewImages(ctx)`, through `tools.ImageSink{CanViewImages() bool; AttachImage(Image)}`
  (`internal/tools/images.go`, the same file as C2's) implemented by the agent's emitter (`Harness.Vision`). The agent
  drains the queue after the step's tools; `internal/agent/images.go` is the one place that turns queued images and
  attachment images into message parts (one user message after the step's results) and applies the 6-image / 6 MiB
  cap, send-once and the `[image <path> shown earlier]` stubs.
- **§5 compaction.** `reserve` = `LLM_MAX_OUTPUT_TOKENS` (per model, default 16000; not sent to the provider) +
  20000; `COMPACTION_THRESHOLD`, when set, still overrides (share of the window). Summaries are built in chunks of at
  most the summarizer's budget and 100k tokens (180 s per call).
- **§5 masking.** Stub format `[<tool> result, <size>, exit <code> — re-run or read <outputPath> if needed]`
  (size `2.0 KB`; exit and path only when the result reports them; otherwise `— re-run if needed]`). Never masked:
  `load_skill`, `ask_user`, `update_plan` results and results under 600 characters. `0` disables masking.
- **§5 rehydration.** A user message `<context_after_compaction>…</context_after_compaction>` right after the system
  prompt for the rest of the run (not persisted), after any compaction that changed the history (not a manual
  `compact` run). Skills: only those whose `load_skill` result was folded or cut. Last user request: only when it was
  folded. Deliverables come from `agent.Harness.Deliverables` (`agent.DeliverableSource`:
  `Deliverables(ctx, run) []agent.Deliverable{Path, Checker, OK *bool, Summary}`), provided by the verification
  hooks (C2); none are listed until it is wired.
- **§5 cache.** Calls of `openai`-provider models send `prompt_cache_key` = the thread id (`LLM_PROMPT_CACHE_KEY`,
  default on). DeepSeek rejects unknown body fields, so DeepSeek calls never send it; `LLM_PROMPT_CACHE_KEY=false` turns it off for other endpoints that do.
- **§5 passback.** Rebuilt across runs from the `reasoning` rows of each run (attached to the next assistant message
  of the same run). GPT-5.6 stays on Chat Completions: the gateway serves the Responses API with
  `reasoning.encrypted_content` (`docs/spike-responses-api.md`), but a Responses client is not built yet.
- **§10 DeepSeek.** Ids such as `DeepSeek-V4-Flash`, `DeepSeek-V4-Pro` (or `deepseek/…` on a router) use the plain
  OpenAI Chat Completions path `{LLM_BASE_URL}/chat/completions`. Defaults (when the lists do not name them): window 1000000, added to
  `LLM_REASONING_MODELS` (reasoning only comes with `reasoning_effort`), passback on, no vision, prompt variant
  `deepseek`. `/models` reports `promptVariant` = `config.Config.PromptVariant(model)`, which is C2's
  (`LLM_PROMPT_VARIANTS`, `config/prompts.go`; C1's placeholder was deleted at the merge). `tools.ReadOnly` is in `internal/tools/readonly.go`, identical
  to C2's.
<!-- C1 (loop & context) block: end -->

<!-- R (Responses API) block: begin -->
## R Responses API — reasoning carried across steps and runs (implemented on `rj/responses`, roadmap 2.6)

Where it differs from §5 and the C1 block ("GPT-5.6 stays on Chat Completions"), this block wins.

- **Selection.** `LLM_RESPONSES_MODELS` (default `gpt-5.6-sol,gpt-5.6-terra`; `none` disables) picks the models
  `llm.Provider` serves with `internal/llm/responses` (a `model.ToolCallingChatModel`); every other model, DeepSeek
  included, keeps the eino-ext Chat Completions client unless listed in `LLM_RESPONSES_MODELS`. Path
  `{LLM_BASE_URL}/responses`, with the provider's HTTP client (`LLM_TIMEOUT_SECONDS`, CallInfo/Retry-After).
- **Request.** `stream:true`, `store:false`, `include:["reasoning.encrypted_content"]`, `reasoning:{effort: <run's
  reasoningEffort, when LLM_REASONING_MODELS has the model>, summary:"auto"}`, `prompt_cache_key` = thread id
  (`LLM_PROMPT_CACHE_KEY`), function tools with `strict:true` only when the schema already meets strict mode (every
  property required, `additionalProperties:false`, basic keywords), `parallel_tool_calls:true` and `tool_choice`
  (`none` on the step limit's last step). Input: system/user messages (images as `input_image`), then per assistant
  message its reasoning items, its text, its `function_call` items; tool results as `function_call_output`.
- **Stream mapping.** `output_text.delta` → content; `reasoning_summary_text.delta` → `ReasoningContent` (AG-UI
  `REASONING_*`, a `reasoning` row as before; gpt-5.6 sends no summary today); `output_item.added` function_call →
  a tool call with `Index` = its rank and its `call_id`, `function_call_arguments.delta` → its argument deltas;
  `refusal.delta` → the refusal (empty-answer handling); `response.completed` → `finish_reason` `stop` / `tool_calls`
  and usage (`input_tokens_details.cached_tokens` → `cachedInputTokens`, `output_tokens_details.reasoning_tokens`);
  `response.incomplete` → `length` (`content_filter` when that is the reason); `error` and `response.failed` →
  errors whose code decides the class (`rate_limit_exceeded` 429, `server_error` 5xx, `context_length_exceeded`
  overflow, others fatal); a stream that ends without a terminal event is transient. Retries, `finish_reason`,
  empty answers and the AG-UI event order are therefore the agent's, unchanged.
- **Reasoning items.** The items of `output_item.done` (`id`, `encrypted_content`, `summary`, tagged with the Work
  model id) ride on the last chunk's `Extra["responses_reasoning_items"]`, are stored on the assistant message
  (`work_messages.reasoning_items` jsonb, migration `0200_reasoning_items.sql`; not in the HTTP API) and are put
  back on that message by `BuildHistory`. They are only sent to the model that produced them, right before the
  message's own items, and only when the message has text or calls. Masking never touches assistant messages and
  compaction keeps or folds whole messages, so an item is never separated from its function call: a folded turn takes
  its items with it.
- **Rejected items.** A `400` about the reasoning items (`invalid_encrypted_content`, "encrypted content … could not
  be verified") is retried once without them and logged (`llm: the provider rejected the reasoning items`); those item
  ids are not sent again by the process. No `work.retry` is emitted (it is not a model failure).
- **Tests.** The runtime suite runs twice (`TestMain`): with the scripted model, then through the real Responses client
  and `llmtest`'s fake Responses server (spike event shapes, a reasoning item at the start of every turn), which
  refuses what the provider refuses (`store`, unanswered `function_call`, output without call, unknown encrypted
  content → `invalid_encrypted_content`) plus an orphaned reasoning item (`llmtest.Violations()`).
  `LLMTEST_CHAT_ONLY=1` skips the second pass. Fixtures recorded from the gateway: `internal/llm/testdata/responses/`.
- **Gateway facts (2026-10-07).** gpt-5.6-terra/-sol: reasoning items of terra are accepted by sol; a corrupted
  `encrypted_content` is `400 invalid_encrypted_content`; orphaned reasoning items and invalid JSON `arguments` are
  accepted; an unanswered `function_call` is `400`. On **Chat Completions** gpt-5.6 refuses function tools together
  with `reasoning_effort` (`400 "use /v1/responses or set reasoning_effort to 'none'"`).
- **Live check (2026-10-07, gpt-5.6-terra, the agent loop through the local proxy-gateway).** A 3-calc task
  (memory_search + 3 `calc` steps + answer, 5 calls) completed on both paths with the right answer. Responses, effort
  medium: 12,759 input (10,097 cached), 173 output (69 reasoning) tokens, the step-1 reasoning item (≈1.5 KB) resent on
  calls 2–5; Responses, no effort: 12,695 / 10,049 / 136 (34); Chat Completions, no effort (with an effort it is the
  400 above): 12,962 / 10,271 / 201 (92). A second run on the thread resent the first run's 4 reasoning items before
  their calls and answered correctly.
<!-- R (Responses API) block: end -->

## Workstreams (branches from `work-chat-real-jobs` @ this commit)

| Stream | Branch / worktree | Owns |
|---|---|---|
| A security & sandbox | `rj/security` `wt-rj-security` | `internal/classification`, `internal/sandbox`, `internal/artifacts`, `internal/workspace`, `internal/syncx`, `internal/auth`, egress wiring in `cmd/work/main.go` |
| B runtime & API | `rj/runtime` `wt-rj-runtime` | `internal/runtime`, `internal/eventlog`, `internal/queue`, `internal/redisx`, `internal/migrate`, `internal/store` (except artifact files), `internal/httpapi`, `internal/approvals`, `internal/agui` test helpers, notifications backend |
| C1 loop & context | `rj/loop` `wt-rj-loop` | `internal/agent/agent.go`, `history.go` (`BuildHistory`), `images.go`, `internal/llm`, `internal/compaction`, `internal/tools/mcptools`, `internal/tools/tools.go` (`ReadOnly`, spill), DeepSeek provider |
| C2 prompt, tools & verification | `rj/tools` `wt-rj-tools` | `internal/agent/prompts/**` (new), the system-prompt function in `history.go`, `internal/agent/guards.go`, `internal/tools/sandboxtools`, `internal/tools/harnesstools`, `internal/skills` (registry/attach/package), `internal/goals`, `internal/titles`, `internal/memory/prompt.go`, verification hooks |
| D skills content | `rj/skills` `wt-rj-skills` | `skills/builtin/**` |
| E frontend | `rj/frontend` `wt-rj-frontend` | `frontend/src/**` (work-chat only) |
| F eval | `rj/eval` `wt-rj-eval` | `work/evals/**` (new), chaos test harness under `work/internal/testkit/**` (new) |

Shared files (`config`, `cmd/work/main.go`, `README.md`, `openapi.yaml`, this file): additive edits only, in
one clearly delimited block per stream; the coordinator resolves merge conflicts.

<!-- stream A (security & sandbox) — begin -->
## Stream A contract changes (security & sandbox)

HTTP / events:
- `GET /threads/{id}/files/content` serves only paths under `/workspace` after cleaning `..` (else `400 invalid_path`).
- `POST /threads/{id}/attachments`: labels are read from the content whatever the name (PDF header lines, label
  literals, document information, XMP; OOXML of any variant incl. `.docm/.xlsm/.pptm/.dotx/.xltx/.potx`; ODF
  `meta.xml`; OLE2), the most restrictive label decides (`400 confidentiality_not_allowed`); archives are detected by
  content: zip (also with a prefix), tar, gzip, bzip2 are scanned (entry paths like `docs/x.pdf` for a `.tar.gz`),
  7z/RAR/xz/zstd/lzip/cab are refused (`400 invalid_archive`); OOXML/ODF are documents, not archives. New code
  `400 signed_document_not_supported` (digitally signed Office file). `targetPath` inside `node_modules`, `.venv`,
  `__pycache__` is `400 invalid_target_path`. `invalid_path` and `signed_document_not_supported` are in the
  `openapi.yaml` error enum.
- Downloads/previews: a document that already carries labels is relabelled (provider-owned properties
  `MSIP_Label_*`/GVData replaced: exactly one label); one already labelled C4 / C3-Amundi-Only, or whose label part
  is unreadable, is `502 classification_failed`; an OOXML package declaring > 200× its size expanded (past 8 MiB) is
  `502` before any part is read.
- New `CUSTOM work.artifact_skipped {path, sizeBytes, maxBytes, reason: "too_large", toolCallId?}`: files above the
  artifact limit (50 MiB or the blob cap) are **refused, not versioned by reference**; emitted once per file state
  per process, where `work.artifact` would be; not persisted. The file-reference prompt tells the model.
- Office preview cache is per owner (`preview/<sha256(login)[:40]>/<sha>.pdf`, queued for the blob GC, refreshed on hits).

Configuration (additive): every process building a sandbox backend (`api` too) applies the production egress
refusal and the same policy (`sandbox.BackendFromConfig`); `SANDBOX_EGRESS_DENY_CIDRS` default adds `100.64.0.0/10,
0.0.0.0/8,fc00::/7,fe80::/10`; deny entries must parse as CIDR/IP and allowlist entries as host (`*.x`)/IP/CIDR
(startup fails otherwise). `FILE_CLASSIFICATION_MAX_CONCURRENT`
(default 4).

Go APIs other streams use:
- `sandbox.KeepAlive(ctx, sb) (stop func())` renews a manager-issued sandbox every `min(RenewEvery, Timeout/3)` until
  stopped or ctx ends (background jobs build on it); `Exec` of a manager-issued sandbox does it automatically while
  the command runs.
- `sandbox.Quiet(ctx)`: an `Acquire` on behalf of the API; when it creates the sandbox, the restore/recreation notice
  is kept in Redis (`work:sbx:notice:<thread>`, 24 h) and returned once (`NoticeOf`/`WasRecreated`) by the next
  non-quiet `Acquire`.
- `sandbox.NewManager` takes `sandbox.Store` = `store.SandboxStore` + `SwapThreadSandbox` + `UserSandboxes` (PG and
  memstore implement them). `ManagerOptions.LockTTL` is now the TTL of renewed locks (default 30 s); new `LockWait`
  (default 3 min) and `HealthRecheck` (default 30 s). `OnCreate` runs **before** the sandbox is recorded for its
  thread: no other caller sees it until the restore finished; a creation losing the compare-and-set is killed.
  When the quota is reached, recorded sandboxes the provider no longer has are forgotten first.
- `store.ArtifactStore.QueueBlob(ctx, key)`; the blob GC also keeps keys referenced by `work_workspace_git`.
- `syncx.KeyedMutex.LockWait(ctx, key, wait)`; a workspace restore that cannot take the thread's artifact lock writes
  nothing (files stay pending).
<!-- stream A — end -->

## Review round 1 — contract changes (coordinator)

- **Verification in tool results.** For tools whose result is a JSON object, post-write verification is a field,
  not a text suffix: `"verification": [{"path", "checker", "ok", "summary"}]`. Text results keep the
  `\n\n[verification] …` suffix. The frontend parses JSON results with this field.
- **Spill keeps JSON valid.** For JSON-object results, spill truncates the large string fields in place
  (`stdout`/`stderr`/`content`… head + marker + tail) and adds `outputPath` and `"spilled": true`; the result
  stays valid JSON. Text results keep head + marker + tail.
- **Verification verdict = latest result.** A deliverable is verified only if the latest check after its last
  change passed; a failing check after a passing one means not verified. Model-run checker output counts only
  when it is the contract JSON with a non-empty `checks` list and `file` resolving to that deliverable; the
  runtime's own check wins over a model-run one.
- **`work.verification` answer form** gains `failed: [{path, checker, summary}]` (checked and failing), next to
  `verified` and `unverified` (never checked). `metadata.verification` likewise.
- **Images** that are not charts are checked with `opens` + `not_blank` only (no size/dpi/aspect).
- **Stop kills foreground commands.** Cancelling a run kills every foreground command it started (background
  jobs keep running until the sandbox expires or `job_kill`).
- **Confidentiality of derived files.** `view_file`, `read_file` on documents and Office previews refuse a
  source whose label is disallowed (C4 / C3-Amundi-Only) → tool error / `400 confidentiality_not_allowed`;
  `files/content` never serves `/workspace/.work/renders/**` (`404`) — the UI shows page images through a
  dedicated `GET /threads/{id}/renders/{renderId}/{page}.png` only when the source's classification allows it.
- **Paths resolve symlinks.** `files/content` and the approval work-area check resolve symlinks in the sandbox
  (`realpath`) before deciding; a path leaving `/workspace` → `400 invalid_path` / approval required.
- **Skill save-draft confidentiality refusal** is `400 confidentiality_not_allowed` (no longer `422
  invalid_package`).
- **Expired approval on reload.** `Thread.pendingApproval` may carry `status: "expired"` for the latest run's
  approval when it expired and nothing happened since.
- `openapi.yaml` documents `work.artifact_skipped`.

### Review round 1 — security and infra fixes (`rj/fix-sec`)

- **Approval arguments.** The gate decodes the top-level argument object itself: a key repeated in any case
  (`strings.EqualFold`), a path field (`path, paths, source, destination, target, from, to, file, filePath,
  file_path`) spelled in another case, trailing data or a non-string path always needs approval (write, edit,
  delete, move; MCP tools are gated by name only, their arguments are not checked). Work-area paths are then resolved with `approvals.Gate.Resolver`
  (`approvals.SandboxResolver{Sandboxes}`: the run's sandbox, acquired quietly when missing, then
  `sandbox.RealPaths(ctx, sb, paths)` = `realpath -m -z`, script marker `# work:realpath`); a failure asks.
- **Derived files.** `classification.SourceAllowed(content []byte) bool` (= not `DocumentDisallowed`) and
  `classification.ErrSourceNotAllowed`. Helper for tools: `renders.CheckSource(ctx, sb sandbox.Sandbox, path)
  (label string, err error)` (reads ≤ 100 MiB; larger or disallowed → `ErrSourceNotAllowed`; `renders.IsNotAllowed(err)`,
  tool text `renders.NotAllowedMessage`). Called by view_file (before rendering), read_file's document branch,
  `sandboxtools.ExtractText`; the artifact Office preview checks `SourceAllowed` before converting
  (`400 confidentiality_not_allowed`).
- **Renders.** view_file returns `{path, renderId, pages: [{page, imagePath}], totalPages, note?, notice?}`, with
  `imagePath = /workspace/.work/renders/<renderId>/p<N>.png` (`renderId` omitted for an image shown as is). It then
  calls `sandboxtools.Options.Renders` (`sandboxtools.RenderRecorder`, implemented by `renders.New(store, mgr)`):
  `RecordRender(ctx context.Context, threadID, sourcePath, renderID string, pages []int) error`, which re-reads the
  source, stores `work_renders (thread_id, render_id, source_path, label, allowed, pages)` (migration
  `0300_renders.sql`; pages merged) through `store.RenderStore` (`PutRender`, `GetRender`).
  `GET /threads/{id}/renders/{renderId}/{page}.png` (page ≥ 1, the N of `p<N>.png`) serves `image/png` only for a
  recorded page of an allowed source that is a PNG inside its folder (no symlink); disallowed → `400
  confidentiality_not_allowed`, anything else `404`. `files/content` answers `404` under `/workspace/.work/renders/`
  and resolves symlinks first (`400 invalid_path` outside `/workspace`).
- **Expired approval.** `ApprovalStore.LatestExpiredApproval(ctx, threadID) (*Approval, error)`; GET
  /threads/{id} expires a due pending approval and shows the latest run's expired one as `pendingApproval`.
- **Infra.** `classification.Service.Classify(ctx, …)` and `artifacts.Service.Deliver(ctx, …)` take a context (the
  slot wait returns `ctx.Err()` wrapped in `ErrClassification`); migrate takes a session advisory lock on one dedicated
  pool connection and runs every migration on it; `InterruptStaleRuns` runs the alive checks without row locks and
  re-checks the heartbeat in the update; a server error resets the shutdown budget; idempotent run replay comes after
  the ownership check (`404`); `SANDBOX_EGRESS_DENY_CIDRS` default adds `::ffff:0:0/96`; skill save-draft
  confidentiality refusal is `400 confidentiality_not_allowed` with the entry path in `error.details`.

<!-- review round 1 (rj/fix-agent): begin -->
### Review round 1 — as implemented on `rj/fix-agent` (agent, tools, verification, skills)

Where it differs from the text above (§2–§6, the C1/C2 blocks), this block wins.

- **Stop** kills the foreground command of a cancelled `execute_command` (`job-kill` with a context that survives the
  cancellation, ≤ 10 s); `background:true` jobs and commands that already outlived `timeoutSeconds` keep running.
- **Loop guard.** A `job_output` call whose previous identical call found the job `running` is never refused (identical ×3
  nor A-B-A-B); the exemption ends with the first non-running result.
- **Verification in results.** JSON-object results get `"verification": [{path, checker, ok, summary, nextStep?}]` (and
  `"verificationPending": {paths, note}` for files checked later); text results keep the `[verification]` lines. The tool
  message's `metadata.verification` items also carry `failed` and `modelRun`.
- **Spill.** The threshold is `min(AGENT_TOOL_OUTPUT_SPILL_CHARS, AGENT_MAX_TOOL_OUTPUT_CHARS)`; a spill threshold above the
  cap fails at startup. Files are `/workspace/.work/tool-output/<callId>-<8 hex>.txt` (unique). A JSON-object result keeps its
  shape: the largest strings are cut in place (head + marker naming the file + tail) and it gains `"spilled": true` and
  `outputPath` (`spillPath` when it already has an `outputPath`); a JSON result made of long arrays becomes
  `{"spilled", "outputPath", "preview"}`. `tools.SandboxSpiller` never creates a sandbox (`Get`, not `Acquire`): without a
  running one (MCP, web search) the result is cut with the note "the full output could not be kept" (`"truncated": true`
  for JSON).
- **Verdict = latest result after the last change.** A model-run check is recorded *before* the changes of the same call;
  it counts only when the command ran a `.py`/`.sh` script, its stdout ends with the contract JSON with a non-empty
  `checks` list and `file` resolves to a deliverable of the turn (absolute; relative under `/workspace`; else the one
  deliverable ending with that path). A passing model-run result never overrides the runtime's own verdict on the same
  content; a failing one always counts. Answer form: `{verified, failed: [{path, checker, summary}], unverified: [{path,
  reason}]}` (failing files are in `failed` only), also on the live `work.verification {messageId, …}`.
- **More than 3 changed files in one call:** the others are checked before the next model call (3 per step), their results
  given to the model in a `<system-reminder>` and announced as `work.verification` with the changing call's `toolCallId`.
  Reminders name the command that checks a file (`agent.CheckCommander`); `.py/.json/.csv` are checked by the shipped
  `python3 /skills/xlsx/scripts/validate_file.py FILE` (installed with the xlsx skill).
- **Views** count only when the model was sent all their page images (the per-request cap may drop some); a resumed run
  counts a stored `view_file` when a model call followed it. Images the cap left out are stubbed `[image <path> not shown (at
  most 6 images and 6 MiB per request): view it again if you need it]`, in that request and later; sent ones `shown
  earlier`. Attachment image parts carry their artifact id (`agent.ImageAttachmentKey`); attachments send at most
  `agent.MaxRequestImages` (6) images.
- **Images that are not charts** (no make_chart.py marker) get `opens` + `not_blank` only. **Checkers** print an absolute
  `file`. **pptx `overlap`** fails only in decks built by `build_deck.py` (marked); in other decks it is a warning, unless
  `fit_text.py --baseline ORIGINAL.pptx` shows it is new.
- **LibreOffice** runs with a fresh profile per conversion (`--norestore -env:UserInstallation=…`) in `view_file`,
  `read_file` and Office previews.
- **Stall watchdog.** Any byte of the SSE body (events the client does not forward, keep-alives) restarts the current
  budget (first chunk, then idle); the first-chunk budget is ×2 at `high` and ×3 at `xhigh` reasoning effort.
- **Cache.** The goal's turn counter moved from the system prompt to `agent.Harness.Tail` (`goals.TurnsSection`): a
  `<system-reminder>` at the end of every request ("Goal: n of m automatic goal turns used."). Masking moves in batches of
  `min(4, keep)` results; the stub is `[<tool> result, <size>, exit <code> — hidden to save context; full output in
  <path>]` (never "re-run").
- **Rehydration** is `FenceData(context_after_compaction, …)` with the plan, skill texts (`<skill_text>`), deliverable
  statuses (`<deliverable_status>`) and the user's request (`<user_request>`) each fenced as data.
- **Prompts.** The deepseek variant keeps the memory, git and web parts (each when enabled); the `view_file` sentence of
  `<deliverables>` is only rendered for vision models (`prompts.Prompt.Vision`).
- **Goal judge** evidence covers every turn since the goal was set (`agent.DeliverablesSince`, `goals.Hook.Evidence(ctx,
  run, goal)`); the judge prompt heading is "DELIVERABLES OF THE GOAL'S TURNS".
- **Background jobs** are listed in Redis (`work:sbx:jobs`, `sandboxtools.RedisJobs`); a runtime process start resumes the
  keep-alive of every listed job still running in its live sandbox (`Provider.ResumeJobs`, never creates one).
- **Memory tools (A-26):** no `runs/`/`handoff/` in descriptions; `memory_delete_batch` takes `ids`, `paths` or both
  (nothing required in its schema; "ids or paths is required" when both are empty).
- **Tests:** the strict AG-UI order (`orderError`, now also "a result follows its call's end") is checked for every run of
  the verification, approval and Responses rigs.
<!-- review round 1 (rj/fix-agent): end -->

<!-- review round 2 (rj/r2-skills): begin -->
### Review round 2 — skills, rendering and verification (`rj/r2-skills`)

Where it differs from the text above, this block wins.

- **view_file renders (A4).** Root cause of "LibreOffice could not render … failed to launch javaldx": two
  `view_file` calls of one file in the same step (read-only tools run in parallel) shared its render directory and
  raced on `<dir>/<name>.pdf`; the javaldx line is a harmless warning (no Java in the image; profiles, HOME, fonts and
  timeouts were not the cause). The render script now holds a `flock` on the render directory (a second view of the
  same file version waits and reuses the PDF and PNGs), converts a private copy (`--nolockcheck`, fresh profile) in a
  private directory next to the PDF, retries a failed conversion once, and reports `soffice exit N: <its output>`
  (javaldx line removed). Pages past the end: only past-the-end pages → error `FILE has N pages|slides[ when
  printed]: page(s) X past the end (valid pages: 1-N)`; a range running past the end shows the valid pages with
  `note: "pages X-Y do not exist (FILE has N slides)"`. `read_file` gives the same error for PDF/PPTX pages.
- **Finish gates (A3, A10, A9).** The docx, pptx, xlsx and pdf gates: the runtime's post-write verdict counts (no
  re-run), one `view_file` per file after its last change, never a new check or view of an unchanged file, "could
  not view: <reason>" when view_file cannot render; cross-check with `cross_check.py DELIVERABLE SOURCE…` (same file
  in the four skills' `scripts/`; a helper, not a checker: no `checks` key, exit 0) and say in the answer what was
  cross-checked; `check_citations.py` when pages of a PDF are cited (pdf: CITATION GATE, chat answers through
  `/workspace/.work/answer.md`).
- **Citations (A9).** `pdf/scripts/check_citations.py SOURCE.pdf [MORE.pdf] DELIVERABLE` follows the checker
  contract (checks `citations`, `pages_exist`, `numbers_on_page`, `quotes_on_page`; extra `citations` list).
  `verify.Options.Citations` (default `DefaultCitations()`, pdf skill; none when `Checkers` is set explicitly) runs
  after the format checker of a `.docx/.pptx/.pdf` deliverable when the thread has uploaded PDFs (artifact
  kind=upload, present in the sandbox, ≤ 5) and merges into the same `CheckResult` (checker name unchanged; `ok` =
  both; summary `<format>; check_citations: …`; next step of the failing one). No citation → result unchanged; a
  citation checker that cannot run leaves the verdict and adds "citations could not be checked (…)".
- **Deck baselines (N8).** `verify.Checker.Baseline` (pptx: `--baseline {baseline}`) is appended when the deliverable
  has an original in the artifact store (`verify.Options.Artifacts`, `*artifacts.Service`, new delegating methods
  `ListArtifacts` / `ListArtifactVersions`): (1) the latest version of that path from no agent run (`RunID == ""`),
  else (2) an upload of the same file name as uploaded, else (3) the upload the deliverable's first version is a byte
  copy of. The original is written to `/workspace/.work/baselines/<sha16>.<ext>`.
- **Skill CLI drift (A7).** `inspect_xlsx.py --formulas` (every formula, `--sheet` for one; with `--range` the
  default dump; `--values` without `--range` is an error). `work/skills/docs_flags_test.go` (go test) checks every
  `--flag` of every SKILL.md/reference against its script's `--help` (argparse strings without python3), and that the
  shared helpers (`cross_check.py`, `lo.py`, `blocks.py`) stay identical across skills.
<!-- review round 2 (rj/r2-skills): end -->

<!-- round 3 (G1): begin -->
### Round 3 (G1) — loop, verification, approvals, jobs (`rj/r2-loop`)

Fixes of review round 2 (N2–N6) and of the A/B (`evals/results/AB-v3.md`: A1, A2, A3 harness side, A5, A6, A8,
A11). Where it differs from the text above (§1, §2, §4, §5, §6, the C1, fix-sec and fix-agent blocks), this block wins.

- **Masking only under context pressure (A1).** `COMPACTION_MASK_FROM_FRACTION` (default `0.5`; `agent.Harness.MaskFromFraction`,
  `agent.MaskFromSetting`): nothing is masked while the next prompt (the last reported prompt tokens + the messages
  added since, else an estimate) stays below fraction × window, so the request prefix is byte-identical from step to
  step. Once it passes, every tool result but the newest `COMPACTION_KEEP_RECENT_TOOL_RESULTS` is masked in one batch and
  stays masked for the rest of the run (by tool call id); the boundary moves again only the next time the prompt passes
  the fraction. `0` keeps the v3 always-on batches of `min(4, keep)`. Stub format and the never-masked tools unchanged;
  compaction unchanged.
- **Step budget (A2).** The run log of `long-synthesis-12` k2 (`wt-rj-eval-ab/…/runs/long-synthesis-12/k2/calls.json`;
  per-run logs are not committed) shows a period-7 cycle: the same 7 reports re-extracted 8 times (53 identical
  commands) at 21–24k tokens, because masking (keep 6) always hid the oldest one. Besides A1: a call that returns the
  same result for the 3rd time in a turn gets `agent.RepeatedResultNote` (never for a running job's poll); a run with a
  sandbox and `AGENT_MAX_STEPS` ≥ 10 that has written no deliverable gets `agent.PartialDeliverableReminder` once,
  max(3, 10 %) steps before the limit; a forced last-step answer of a run that called tools but wrote no deliverable
  ends with `RUN_ERROR max_steps` (`agent.MsgStepLimitNoDeliverable`; the answer is persisted and streamed; only for
  a run that was writing a file: F3 in the final-review block). A run that wrote a deliverable, or answered without
  tools, still finishes. `AGENT_MAX_STEPS` stays 60: the loop, not the budget,
  was the cause (legitimate runs used ≤ 43 steps).
- **Deferred checks are persisted (N2).** The checks of files past the 3-per-call cap (run before the next model call)
  are appended to `metadata.verification` of the origin tool message (`store.HarnessStore.AppendToolVerification(ctx,
  threadID, toolCallID, items)`, optional `agent.VerificationStore` on `Agent.Store`): same item shape as the call's own
  checks (`{path, checker, ok, summary, nextStep?, failed}`), so `GET /threads/{id}`, `agent.TurnDeliverables`, the goal
  judge and rehydration see them, and a resumed run neither re-checks nor re-announces them. Live, they keep their
  `CUSTOM work.verification {toolCallId: <origin call>, path, checker, ok, summary}` (frontend shape (a) live, (b) stored).
- **Stall watchdog (N3).** Only provider event lines (`data:`, `event:`) count as stream activity; SSE comment lines
  (`: keep-alive`) do not. Every model call has an absolute ceiling `LLM_CALL_MAX_SECONDS` (default 1200; ×2 at high,
  ×3 at xhigh effort; `Agent.CallCeiling`), after which the attempt is a timeout (retried, then `model_timeout`).
  `llmtest.Turn.Pings` is a comment-only silence; `KeepAlive` sends progress events.
- **Background jobs (N4).** `sandboxtools.JobList` is `Add(ctx, thread, job) (started time.Time, error)` (the first start
  is kept: `HSETNX`, unix ms; older entries in seconds are read), `Remove`, `List(ctx) ([]ListedJob, error)`,
  `Claim(ctx, thread, job, owner, ttl) (bool, error)` and `Release`. A keep-alive lasts until start + `JobKeepAliveMax`
  (a resumed one gets what is left); each job is leased to one process (`work:sbx:job-lease:<thread>/<job>`, TTL 3 ×
  `JobPollEvery`, renewed at every poll; a lease lost to another process ends the local keep-alive and keeps the job
  listed). `Provider.KeepResumingJobs(ctx, every, done)` runs `ResumeJobs` at start and every 2 min, so a dead owner's
  jobs are taken over.
- **Approval gate (N5).** `agent.BeforeToolHook.NeedsApproval(ctx, run, toolName, arguments)` gets the run's context.
  `approvals.SandboxResolver` only uses the thread's live sandbox (`Get`); without one the lexical paths are the real
  ones (the gate never starts a sandbox; the write itself refuses symlinks, N6). Resolution is bounded by 10 s (the per-run
  cache is gone: F6 in the final-review block).
- **No-follow writes (N6).** `write_file` and `edit_file` write through `sandbox.WriteFileNoFollow(ctx, sb, path, data)`:
  upload to `/workspace/.work/.staging/<uuid>`, then one script (marker `# work:place`) refuses a target that is a
  symbolic link or whose directory resolves elsewhere (`/workspace` itself may resolve to another name) and moves the
  file into place (`mv -f -T`). A refusal is `*sandbox.SymlinkError{RealDir}` (`sandbox.ErrSymlinkTarget`) → tool error
  "… is a symbolic link or inside one (its directory is really …); not written: write to the real path instead".
- **No duplicate checks (A3, harness side).** When a command's last step (after `&&`, `||`, `;` or a newline) is exactly
  the file's checker command (`agent.CheckCommander.CheckCommand`; words in any order, quotes removed, no pipe,
  redirection, `$` or background job), its contract result stands for the **format part** of the runtime's check after
  the change: the runtime does not run that checker again without a reason (one `work.verification`). The baseline
  (N8) and citation (A9) parts of the r2-skills block still run (`agent.CheckCompleter`, see the final-review block
  below, which also lists the commands that never get the shortcut). Any other model-run check still counts only before
  the call's changes. Requiring one `view_file` per deliverable after its last change is unchanged.
- **Failed views (A5).** A `view_file` that fails on a deliverable after its last change gets `agent.ViewFailedNote`; the
  StopGuard no longer refuses the stop for that file, and `metadata.verification.unverified` lists it with reason
  `render failed: <error>` (`agent.RenderFailedPrefix`), also for a resumed run.
- **Edits of unread or stale files (A6).** The refusal carries the file's numbered content (or, past one `read_file`
  page, the 3 regions closest to `oldText`) and counts as a read: the retried edit works in the next step. Non-UTF-8
  files keep the plain refusal.
- **Language (A8).** Every prompt variant (default, gpt-5.6, deepseek, legacy) carries `prompts.LanguageRule`: "Answer in
  the language of the user's latest message, whatever the language of the files, file names and earlier messages."
  (deepseek: a `<language>` part).
- **Deleted job files (A11).** The A/B's "process ended without an exit status" came from a command that ran
  `rm -rf /workspace/.work`, deleting its own job directory. Jobs also keep `pid`, `exit` and `killed` in
  `/tmp/work-jobs/<jobId>/`; a job whose directory is gone reports `status removed` with its exit code, and the result's
  stderr is `sandboxtools.RemovedJobNote` (the output is lost; do not delete `/workspace/.work/jobs/`). `missing` (no
  directory, no shadow) still means a recreated sandbox; a sandbox that dies during a command is retried once on a new
  one with the recreation notice (`sandbox.Retry`), as before. `execute_command`'s description says not to delete
  `/workspace/.work/jobs/`.
- **Release:** rebuild the sandbox image from `sandbox/Dockerfile` with every release (README, *Release checklist*).
<!-- round 3 (G1): end -->

<!-- final review of round 3 (rj/r3-fix): begin -->
### Final review of round 3 (`rj/r3-fix`)

Fixes of the final review of `3e3888374..22390e23e` (F1–F8, A10 in part). Where it differs from the text above (the
r2-skills and round 3 blocks included), this block wins.

- **The A3 shortcut keeps the baseline and citation checks (F1).** The r2-skills rule (the runtime checks a deck against
  its original, N8, and a document's citations against the uploaded PDFs, A9) and the round-3 A3 rule (no second run of
  a checker the command just ran) are reconciled: the model's result is accepted for the **format check only**.
  `agent.CheckCompleter.Complete(ctx, run, path, base)` (implemented by `verify.Verifier`) gets that result: a file type
  without a baseline checker or citation check keeps it (no sandbox call); a deck with an original is checked again with
  `--baseline` (the model's command did not pass it); a `.docx/.pptx/.pdf` then gets the citation check merged in, as
  in `Verifier.Check`. A Checker without `CheckCompleter` keeps the model's result.
- **No forged shortcut (F5).** The shortcut is refused — the runtime runs its own full check, which wins — when the
  command anywhere defines a shell function (`f() {`, `function`) or alias, assigns `PATH`, `PYTHON*`, `BASH_ENV`, `ENV`,
  `LD_*`, `IFS`, `SHELLOPTS`/`BASHOPTS`, uses `source`, `.`, `eval`, `exec`, `enable`, `hash`, `builtin`, `command`,
  `declare`, `typeset`, `trap`, `shopt`, mentions `sitecustomize`/`usercustomize`/`.pth`, names the checker script more
  than once or its interpreter as a path (`…/python3`) (`agent.unsafeShell`). When in doubt, the runtime checks.
- **`max_steps` only for a missing requested file (F3).** A forced last-step answer ends with `RUN_ERROR max_steps`
  (`agent.MsgStepLimitNoDeliverable`) only when the run called tools, produced no file — no deliverable under
  `/workspace/outputs/` and no in-place change (`updated`) of an existing workspace file outside it and outside
  `/workspace/.work/` — and was writing one: its text or a tool call's arguments named `/workspace/outputs` (a plan
  item, a write, a command that began writing there; also replayed for a resumed run). A question or an analysis
  completes. `agent.PartialDeliverableReminder` uses the same notion of "produced" and now says such a run is reported
  as incomplete; it still tells a run that was not asked for a file to answer with what it found.
- **Citation checker speed (F4).** `check_citations.py` reads each source page once (pdftotext when it yields one text per
  page, else pdfplumber) and computes its figures once; 200 citations against a 60-page dense PDF take 0.27 s (10.8 s
  before; the skills suite bounds it at 8 s, target < 5 s).
- **`cross_check.py` (F2).** A numeric date followed by the sentence's full stop is read (`DATE_NUM` lookahead
  `(?![\d/-]|\.\d)`); a source's abbreviated figures also count in full ("1 232,50 millions" ↔ "1 232 500 000 €", both
  ways); figures ending a capitalised group or a table cell are not part of a name ("Effectif 4512"). One source of truth:
  `work/skills/shared/{cross_check,lo,blocks}.py`, copied into the skills by `work/skills/sync_shared.sh`;
  `TestSharedScriptCopiesMatchTheirSource` fails on a drifted or missing copy.
- **Approval gate (F6).** The per-run realpath cache of `approvals.Gate` is removed (invalidating it on every write,
  edit, delete, move and command would leave it no hits: the gate resolves once per gated write): every decision runs
  one realpath exec in the live sandbox (measured ~70 ms with `docker exec`, the script itself ~1 ms).
- **Leftovers (F7).** `sandbox.WriteFileNoFollow` removes its staged upload (`# work:unstage`) when the upload or the move
  fails without the place script cleaning up. A run's first acquisition of a sandbox runs, best effort (≤ 10 s,
  errors ignored), `# work:sweep`: staged uploads older than 10 min, and the `/tmp/work-jobs/<jobId>` records (root
  `WORK_JOBS_SHADOW_ROOT`, for tests) of ended jobs (exit or killed, or a dead pid) older than 24 h or past the 200
  newest; a running job's record stays.
- **Docs ↔ CLI checks (F8).** `work/skills/docs_flags_test.go` reads the scripts statically: the option strings that open
  each `add_argument(` call (groups and subcommands included) and `--help`; shell scripts their `--flag)` case patterns
  and `== --flag` tests — no interpreter runs, so nothing is written into the skill tree. The skills suite
  (`test_packages.test_documented_flags_are_in_the_scripts_help`) checks the same docs against each script's `--help`
  inside the sandbox image (`PYTHONDONTWRITEBYTECODE=1`, `/skills` read-only). `test_package_limits` skips `__pycache__`
  and `.pyc` like the packager (`skills.LoadFS`).
- **Cross-check rule in the prompt (A10, prompt side).** The `<deliverables>` section of `default`, `gpt-5.6` and
  `deepseek` (not `legacy`): "Before you finish, cross-check every date, figure and named item in the deliverable against
  the sources, and say in your answer what you cross-checked." (replaces "Check numbers against their source.").
<!-- final review of round 3 (rj/r3-fix): end -->

<!-- Lazy skills (rj/lazy-skills): begin -->
### Lazy skills (`rj/lazy-skills`)

Skill text never requires a sandbox; a sandbox is created only when something executes, and gets only the skills
its run needs.

- **`load_skill`** never calls `Manager.Acquire` and installs nothing into a sandbox it would have to create. It
  records the skill as loaded by the run (`Installer.MarkNeeded`) and returns the text: "Installed at
  /skills/<name>/ (read-only)." when the thread's sandbox is running (`Manager.Get`; the skill and its dependencies
  are installed at once), else "Loading it started nothing: its files are at /skills/<name>/ (read-only) from your
  first command." Both keep the run-from-`/workspace` sentence. It works at the per-user sandbox quota.
- **What a sandbox gets** (`Installer.EnsureRunSkills` on `OnAcquire`, `Installer.EnsureCommandSkills` on the new
  `sandboxtools.Options.BeforeCommand`, before every `execute_command`, foreground or background job):
  (a) skills loaded in this run or in the thread's history (successful `load_skill` calls of the stored messages,
  read once per run, so later and resumed runs get them back); (b) skills requested for the run (`skills`, mentions,
  selected versions, the creation session's skill-creator); (c) known skills whose folder the command names
  (`/skills/<name>/`, or `/skills/<name>` ending a word; `/home/x/skills/…` does not count). Defaults are no longer
  installed with every sandbox. Installs stay idempotent per sandbox and revision (`work:skills:installed:<id>`).
  A `BeforeCommand` failure only stops the command when the sandbox is gone (retried on a new one).
- **Dependencies.** SKILL.md frontmatter `requires: [name, …]` (`Package.Requires`): installing a skill installs
  them. docx, pdf, pptx, charts and xlsx require `studio-brand` (their builders import `brand.py`). Checked by
  `work/skills/requires_test.go` (every `/skills/<other>/` path, sibling skill folder or documented
  `sys.path.insert` import of another skill must be declared) and by `test_isolated.py`, which `run_tests.sh` runs
  once per skill with only that skill and its dependencies mounted at `/skills`.
- **Model-written code.** `sandboxtools.Options.OnWrite` (`Installer.NoteContent`): `write_file` / `edit_file`
  content naming `/skills/<name>/` (known skills) marks the skill needed by the run and installs it now. Before a
  command, the `/workspace` scripts it runs (`python3|node|bash|sh path`, `./path`, also inside `bash -lc "…"`; never
  `-m`, `-c`, `-mpip`, `node -e`; ≤ 4 files, ≤ 1 MiB in all) are scanned as well, with one exec
  (`sandbox.ReadRegularFiles`, `# work:read-files`: regular files reached without a symlink only, so no symlink,
  FIFO or device is read), each downloaded again only when its (size, mtime) changed and never when the run wrote it. `Options.AfterFailedCommand` (`Installer.AfterFailedCommand`):
  a foreground command exiting non-zero whose output shows a missing file/module ("No such file or directory",
  "No module named", "Cannot find module", …) and names a skill not yet installed (a `/skills/<name>/` path, or a
  Python module only one skill's `scripts/` holds) installs it and sets the result's `note`: "Skill <name> was
  installed; run the command again." No automatic re-run.
- **Checkers.** The runtime's post-write checkers install their skill and its dependencies before they run
  (`verify.Options.EnsureSkill` = `Installer.EnsureSkill`; skill and dependencies resolved in the user's catalog,
  disabled ones included, cached per run), so they do not depend on the defaults.
- **Review fixes.** Implicit dependencies: every package (built-in, user, global, Anthropic) gets `Package.Uses`, the
  `/skills/<name>/` paths of its non-markdown files (computed once per parsed revision, no sandbox call), installed
  with it when known. A mentioned or selected default is installed at acquire (`Explicit || Reason != default`). A
  bare `/skills` path argument (`ls /skills`, `find /skills/ …`; not a quoted `"/skills"`) installs the run's index
  first (at most 8 skills). Install tracking: the marker `/skills/<name>/.installed-<revision>` is written after
  every file; an installation counts only when Redis records it and the marker exists (one Stat per runtime, then
  remembered), so a partial install is retried from scratch; installations of a skill in a sandbox take the Redis
  lock `work:skills:lock:<sandboxId>:<name>` (3 min TTL, re-checked once held). The run state is refreshed on use
  (an hour after its last use); skills found in written files (`write_file` content, `edit_file` newText) and
  install notes of failed commands are read back from the thread history (`skills.LoadedIn`) by later runs.
- **Prompt.** The skills index (`prompts.SkillsIntro`: default, gpt-5.6, deepseek), the legacy skills section and the
  `load_skill` description say that loading a skill starts nothing and the scripts are at `/skills/<name>/` from the
  first command; goldens regenerated.
- **Effect.** Runs that only load skills (advice, planning, "how do I…") create no sandbox; a new sandbox installs 0
  files until a skill is loaded or used (before: all 6 built-ins, 53 files, 2 execs, on its first tool call); a docx
  run installs docx + studio-brand (17 files).
<!-- Lazy skills (rj/lazy-skills): end -->
