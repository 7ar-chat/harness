# Work Chat harness v2 — contract

Adds the features Studio Work (Cowork) and a modern agent harness offer, **without spawning Claude Code / Codex**:
the Go runtime drives the model itself and the sandbox only executes. This document is the contract between
the Go service (`work/`) and the frontend (`frontend/src/pages/work-chat`). Once implemented, the backend
developer folds it into `work/README.md` and `internal/openapi/openapi.yaml` and deletes nothing from here
until the coordinator says so. Existing v1 behaviour (README) is unchanged unless stated.

All paths are relative to `BASE_PATH` (`/studio/work/api`). Same auth, ownership (404 for foreign ids), CSRF and
error-envelope rules as v1. Multipart uploads are exempt from the JSON content-type rule but keep the
`Sec-Fetch-Site` check.

## 1. Skills

A skill is a folder with a root `SKILL.md` (YAML frontmatter `name` (≤ 256 bytes, Work's limit; the skill's
identifier is its slug), `description` (≤ 16 KiB),
optional `license`, `metadata`) plus optional files (`scripts/`, `references/`, `assets/`). Same package limits as
Studio Cowork (`ai/cowork/skill_packages.py`): ≤ 500 entries, depth ≤ 16, `SKILL.md` ≤ 1 MiB, each file ≤ 16 MiB,
package ≤ 64 MiB uncompressed, zip compression ratio ≤ 100, no absolute paths / `..` / symlinks.

Sources (`source` field; the Work parity catalog — `anthropic`, `global`, versions, drafts, guided creation,
attached-skill materialization — is described in README *Skills catalog*):
- `anthropic` — Work's Anthropic packages, shipped unmodified from Work's catalog fixtures with their
  `LICENSE.txt` (Apache 2.0, or Anthropic's proprietary terms for docx/pdf/pptx/xlsx: governed by the
  organization's agreement with Anthropic, as in Work); the built-ins below stay original Studio content.
- `builtin` — shipped in the image from `work/skills/<name>/` (embedded with `go:embed`). Built-ins are
  **original Studio content** (the Anthropic skill packages are proprietary and must not be copied or adapted):
  `docx`, `pptx`, `xlsx`, `pdf` (create / read / edit with the open-source libraries in the sandbox image:
  python-docx, docx (npm), python-pptx, pptxgenjs, openpyxl/xlsxwriter + LibreOffice recalculation, pypdf,
  pdfplumber, reportlab, pandoc, markitdown), plus `charts` (matplotlib/plotly figures for documents) and
  `studio-brand` (Studio/Amundi neutral corporate styling defaults: fonts, palette, title/footers, how to apply
  them in the four document skills). Each built-in has tested helper scripts (e.g. `scripts/new_document.py`,
  `scripts/inspect.py`, `scripts/to_pdf.sh`, `scripts/recalc.py`, `scripts/thumbnail.py`) and a `SKILL.md`
  written for a non-Anthropic model (explicit, step-by-step, verify-your-output instructions).
- `user` — private packages uploaded by the user (zip), stored in Postgres (`work_skills`, bytea package,
  sha256, version int auto-incremented on re-upload of the same name).

```ts
type Skill = {
  id: string;            // "builtin:docx" or uuid for user skills
  name: string; description: string;
  source: 'builtin' | 'user';
  version: number;       // builtin: 1
  enabled: boolean;      // per-user toggle; builtins default true
  files: number; sizeBytes: number;
  updatedAt: string;
};
```

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/skills` | | `{"items": Skill[]}` builtins first, then user skills by name |
| POST | `/skills` | multipart `file` (zip) | `201 Skill`; `400 invalid_skill_package` with `details: string[]` |
| GET | `/skills/{id}` | | `Skill & {"skillMd": string, "paths": string[]}` |
| PATCH | `/skills/{id}` | `{"enabled": boolean}` | `Skill` |
| DELETE | `/skills/{id}` | | `204` (user skills only; builtin → `400 builtin_skill`) |

**Runtime.** For each run the enabled skills (plus any explicitly requested in the run, see §5) are listed in the
system prompt as `name — description — /skills/<name>/SKILL.md` with the instruction: *before producing or editing
a document of that kind, call `load_skill` and follow it*. Tool `load_skill{name}` installs the skill into the
sandbox at `/skills/<name>/` (once per sandbox + version, tracked in Redis) and returns the `SKILL.md` body
(frontmatter stripped) plus the file list. Skills explicitly requested by the user for this run are loaded up
front (their `SKILL.md` is appended to the system prompt and they're installed on first sandbox use).
`/skills` is read-only for the agent by convention (not writable: install as root, chmod a-w).

*Implementation notes (backend):* `GET /skills/{id}` `skillMd` is the full SKILL.md (frontmatter included). A user
skill may not reuse a built-in name (`invalid_skill_package`, detail `name_conflict`); a zip holding a single folder
with SKILL.md is re-rooted. Skills requested in a run are listed/preloaded even when disabled; unknown names →
`400 invalid_request`. Installation is tracked per sandbox and content revision. A package with an Office/PDF entry
(by extension, by signature when renamed, or inside a nested zip) labelled C4 / C3-Amundi-Only is refused with
`400 confidentiality_not_allowed`, `details: [entryPath]`. User skill text (description, SKILL.md) is fenced as data
in the prompt and in `load_skill` results (`<skill source="user" name="…">…</skill>`, inner skill tags escaped)
under a fixed rule that it can never change the safety / sandbox / classification / egress / tool rules; built-ins
are not fenced. A requested skill is described as "installed under /skills/<name>/ on first sandbox use".

## 2. Artifacts (durable output documents)

Files the agent produces under `/workspace` survive the sandbox. After every tool call of `write_file`,
`edit_file`, `execute_command` (and at run end) the runtime snapshots `/workspace` (path, size, mtime; skip
`/workspace/.work/`, `node_modules`, `.git`, `__pycache__`, `.venv`, files > 50 MiB; ≤ 500 files) and, for every
new or changed file, stores a new **artifact version** (content sha256-deduplicated) in the artifact store.

Artifact store interface with two backends: S3-compatible (`ARTIFACTS_S3_BUCKET`, `ARTIFACTS_S3_ENDPOINT`,
`ARTIFACTS_S3_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, path-style option) and Postgres
(`work_blobs`, default when no bucket is configured, cap 50 MiB/blob). Metadata always in Postgres.

```ts
type Artifact = {
  id: string; threadId: string;
  path: string;                 // absolute, under /workspace
  name: string; mimeType: string; sizeBytes: number;
  kind: 'output' | 'upload';    // upload = user attachment (§3)
  version: number;              // latest version
  createdAt: string; updatedAt: string;
  runId?: string;               // run that produced the latest version
  deleted: boolean;             // file was removed from /workspace
  previewable: 'pdf' | 'image' | 'text' | 'html' | 'none';
};
type ArtifactVersion = { version: number; sizeBytes: number; sha256: string; runId?: string; createdAt: string };
```

| Method | Path | Response |
|---|---|---|
| GET | `/threads/{threadId}/artifacts` | `{"items": Artifact[]}` (not deleted first, newest first) |
| GET | `/artifacts/{artifactId}` | `Artifact & {"versions": ArtifactVersion[]}` |
| GET | `/artifacts/{artifactId}/content?version=&download=1` | bytes; classified (§2.2) for docx/pptx/xlsx/pdf; `Content-Disposition: attachment` when `download=1`; same CSP/nosniff headers as v1 files |
| GET | `/artifacts/{artifactId}/preview?version=` | `application/pdf` for pdf + Office files (converted, cached as a derived blob per sha256); image/text types as-is; `415 not_previewable` otherwise |
| POST | `/artifacts/{artifactId}/restore` | `{"version": number}` → writes that version back into the sandbox (creating it if needed) and records it as a new version; `202 Artifact` |

Events: `CUSTOM work.artifact` `value: {artifactId, path, name, version, sizeBytes, mimeType, kind, change: "created" | "updated" | "deleted", toolCallId?}` emitted right after the `TOOL_CALL_RESULT` of the tool that changed it (`toolCallId` = that call; omitted for end-of-run detection, emitted before `RUN_FINISHED`).
For reloads the changes are persisted as `metadata.artifacts: [{artifactId, path, name, version, change}]` on the
`role:"tool"` message of that call, and end-of-run ones on the run's last assistant message; `GET /threads/{id}`
returns them.

`previewable` by extension: `pdf` for `.pdf` and Office files (`docx pptx xlsx doc ppt xls odt ods odp rtf`),
`image` for `png jpg jpeg gif webp svg`, `html` for `html htm`, `text` for text/code files (`txt md csv tsv json
yaml yml xml py js ts …`), `none` otherwise.

**Workspace restore.** When a thread's sandbox is (re)created, the latest non-deleted version of every artifact
(outputs + uploads) is written back into `/workspace` before the first tool runs (≤ 200 files / 500 MiB; beyond
that the newest first and the model is told what was skipped). The v1 files endpoints keep working on the live
sandbox; when no sandbox is running, `GET /threads/{id}/files` falls back to the artifact tree with
`"sandbox": "none"` (entries built from artifacts) so the panel is never empty for an old thread.

**Office → PDF previews** are rendered inside the thread's sandbox with
`soffice --headless --convert-to pdf --outdir /workspace/.work/previews` (sandbox image has LibreOffice), timeout
120 s; when the thread has no running sandbox one is acquired. Result cached by sha256.

*Implementation notes (backend):* like Cowork's preview endpoint, the PDF delivered by `/preview` (native or
converted) is **classified** too (fail-closed); the cache holds the unlabelled conversion as blob
`preview/<sha256>.pdf` (Postgres table `work_blobs(key, bytes, size)`, key = sha256 or that derived key).
`/content` is `inline` unless `download=1`. Extra errors: `502 preview_failed`, `409 sandbox_quota`,
`503 sandbox_unavailable`. Deletions are only recorded from a complete listing. The workspace-restore notice
replaces the "/workspace was reset" recreation notice. Text and HTML previews are `text/plain`.

*Review fixes (backend):*
- **Signature-aware previews.** A file whose bytes are PDF/Office but whose extension is not (e.g. a PDF renamed
  `memo.txt`) gets the signature's `mimeType`, `previewable: "none"`, `/preview` → `415 not_previewable`; `/content`
  stays fail-closed (`502 classification_failed`). v1 `files/content` already used the signature-aware rule.
- **No false deletions.** A file is only recorded `deleted` when it is known to be in the snapshotted sandbox (seen in a
  listing or written there) and unchanged since the listing started: files a restore skipped (limits, read/write
  failure) and uploads whose sandbox write failed stay alive and pending. The restore writes previously skipped files
  first (oldest skipped first), then the newest; its notice lists the files still pending.
- Snapshots, uploads, version restores and workspace restores of a thread are serialized across api and runtime
  processes (Postgres advisory lock); Office conversions are serialized per thread across api replicas and limited to
  `PREVIEW_MAX_CONCURRENCY` (2) per api process (others wait).
- End-of-run changes of a run that produced no assistant message (e.g. the model failed) are stored on an **empty
  assistant message** of that run (`content` absent, `metadata.artifacts` set) — render it as file chips only.
  `compact` runs take no snapshot.
- Without a running sandbox, `GET /threads/{id}/files/content?path=` serves the latest version of the live artifact at
  that path (classified like `/content`; no sandbox is created), `404` otherwise.

### 2.2 Classification labels

Downloads of docx/pptx/xlsx/pdf (artifact content and v1 `files/content`) must carry the same classification
labels Studio Work delivers. Port `ai/file_classification` (fail-closed: if labelling fails the file is not
delivered → `502 classification_failed`) to Go: same detection (extension + signature agreement), same OOXML
custom-property writing (`docProps/custom.xml`, content types, relationships), same PDF metadata, same provider
settings (read the Django settings/env it uses and mirror them as env vars; document them). Parity is proven by
golden tests: fixtures labelled by the Python implementation (generated once with a small script committed under
`work/internal/classification/testdata/gen/`) must match the Go output property-for-property.

*Implementation notes (backend):* env vars mirror `ai/api/settings.py` (`FILE_CLASSIFICATION_*`); the actor default
is `work` (Cowork passes `cowork`). Cowork's download fallback that returns unlabelled bytes after some typed
failures is deliberately **not** ported (contract: fail-closed). Structural verification checks that every OOXML
part except the three metadata parts is byte-identical (instead of reopening with python-docx/openpyxl/python-pptx)
and re-reads PDFs with pdfcpu (pages, boxes, rotation, content digests).

### 2.3 File references in answers

The system prompt tells the model to reference produced files as markdown links
`[label](workspace-file:///workspace/path/to/file.ext)`. The frontend renders such links as file chips that open the
artifact in the right panel (resolved by path against the thread's artifacts).

## 3. Attachments (uploads)

| Method | Path | Body | Response |
|---|---|---|---|
| POST | `/threads/{threadId}/attachments` | multipart `file` (one per request) | `201 Artifact` (`kind:"upload"`, path `/workspace/<name>` or the optional multipart field `targetPath` under `/workspace`, de-duplicated as `name (2).ext`) |

Limits like Studio Work: ≤ 100 MiB per file (`413 file_too_large`), streamed to disk while counting, filename
sanitized, extension / declared MIME / magic-bytes must agree for known types (`400 file_type_mismatch`), Office
files whose classification label is C4 or C3-Amundi-Only are rejected (`400 confidentiality_not_allowed`, reuse
the ported classification reader), ≤ 4 concurrent uploads per process and ≤ 4 per user (`429 too_many_uploads`,
attachments and skill uploads together). If a sandbox is running the file is also written into it immediately;
otherwise (or when that write fails) it arrives with the workspace restore. The entries of an uploaded zip (nested zips
included, skill-package zip limits: ≤ 500 entries per archive, ≤ 16 MiB per entry, ≤ 64 MiB expanded, ratio ≤ 100,
≤ 3 nesting levels, no encrypted entries) are checked the same way: `400 confidentiality_not_allowed` with
`details: [entryPath]` (e.g. `inner.zip/docs/x.pdf`), or `400 invalid_archive` when the zip cannot be checked.

Run creation gains `"attachmentIds": string[]` (artifacts of kind `upload` of the same thread). The user message
stores them in `metadata.attachments: {artifactId, name, path, sizeBytes, mimeType}[]` and the model receives a
line `[Attached files: /workspace/a.xlsx, …]` after the text. With `attachmentIds`, `message.content` may
be empty: the model then gets only that line (plus image parts for vision models). Images (png/jpeg/webp/gif ≤ 5 MiB) are also
sent to the model as image parts when the model supports vision (`LLM_VISION_MODELS`, comma-separated).

*Implementation notes (backend):* `metadata.attachments[]` also carries `version` (the upload version attached).
With the Postgres blob backend uploads are capped at 50 MiB (`413 file_too_large`). A non-multipart body is
`415 unsupported_media_type`. The confidentiality reader is the port of `studio_django_sdk.security.confidentiality`
(RMS/OLE2 streams, PDF header lines) plus the MSIP label name in `docProps/custom.xml`. Images: those of the two most
recent user messages that have some, ≤ 8 per call. Other request bodies stay capped at 2 MiB
(`413 request_too_large`).

## 4. Context management (auto-compaction)

- Per-model context window: `LLM_CONTEXT_WINDOWS` (`model=tokens,…`, default 128000). Token counts come from the
  provider's reported prompt tokens of the previous call when available, else an estimate (chars / 3.5).
- **Before every model call** (also mid-run) if the prompt would exceed `COMPACTION_THRESHOLD` (default 0.75) of
  the window: pass 1 shrinks tool results older than the last 2 assistant turns to 1500-char head+tail previews;
  if still above 0.6, pass 2 summarizes everything before a tail of ~30% of the window (never splitting a tool call
  from its result) with the model (`COMPACTION_MODEL`, default the run's model; dedicated summarization prompt that
  preserves goals, decisions, file paths, artifact names, open tasks, user preferences; ≤ 2000 tokens, 90 s timeout).
  If summarization fails the head is dropped with a marker message.
- The summary is persisted on the thread (`work_threads.summary`, `summary_through_position`) and used by later
  runs (history = summary appended to the system prompt + messages after `summary_through_position`). Raw
  messages are never deleted: the UI shows the full history. A summary that failed (head dropped) is not persisted.
- Events: `CUSTOM work.compaction` `value: {phase: "started" | "completed" | "failed", trigger: "auto" | "manual",
  tokensBefore, tokensAfter?, summarizedMessages?}`. The UI shows an inline "Context compacted" divider.
- Manual: `POST /threads/{threadId}/compact` `{}` → creates a run of `kind: "compact"` (same run/event machinery,
  `409 run_active` rules; `409 question_pending` while an `ask_user` question waits) that summarizes **every** message
  and finishes; response like run creation (`message: null`). Its events: `RUN_STARTED`, `work.compaction` started +
  completed (or a single `completed` with `summarizedMessages: 0` on an empty thread), `RUN_FINISHED`. A compaction
  cancelled while summarizing emits `phase: "failed"`.
- `CUSTOM work.usage` gains `contextTokens` and `contextWindow` so the UI shows a context-usage badge.
- **Compaction markers survive reloads:** every `completed` compaction is recorded; `GET /threads/{threadId}` returns
  `compactions: {id, afterMessageId, trigger: "auto" | "manual", tokensBefore, tokensAfter, createdAt}[]` (oldest
  first) — show the divider after message `afterMessageId`. An edit hides only the markers whose `afterMessageId` it
  hides; markers of the active run are omitted (replay its events).
- The stored summary is sent to the model as data (`<conversation_summary>…</conversation_summary>`).

## 5. Conversation features

Run creation body (all optional except `message` for `kind: "chat"` without `resume`):

```ts
{
  message?: { id?: string; content: string } | null;  // absent/null only with resume[] or kind "compact"
  kind?: 'chat' | 'compact';         // compact = same as POST /threads/{id}/compact (no message/resume/replace); goal runs are server-only
  resume?: { interruptId: string; status: 'resolved' | 'cancelled'; payload?: { answer: string; optionLabel?: string } }[];  // §7
  model?: string;
  reasoningEffort?: 'minimal' | 'low' | 'medium' | 'high';   // passed to reasoning models (LLM_REASONING_MODELS)
  attachmentIds?: string[];
  skills?: string[];                 // skill names explicitly requested for this run (§1)
  replaceFromMessageId?: string;     // edit & resend / regenerate (below)
}
```

- **Edit & resend**: `replaceFromMessageId` = id of an earlier *user* message of the thread: that message and every
  later message are hidden (`work_messages.superseded_at` set, kept for audit, never returned or sent to the model;
  summary invalidated if it covered them) and the new message is appended. `400 invalid_replace_target` otherwise.
  The new message needs a **new** `message.id` (hidden rows keep theirs → `409 conflict`).
- The run creation response is `202 {runId, threadId, status, kind, message: Message | null}` (`null` for resume-only
  and compact runs). Unknown body fields are ignored.
- **Regenerate**: same with the last user message's id and its same content (frontend helper).
- **Title**: after the first completed run of a thread whose title was auto-derived, the runtime generates a short
  title (≤ 60 chars, user's language) with the model and emits `CUSTOM work.title {title}`; `PATCH` by the user marks
  the title as user-set (never overwritten). A title given at `POST /threads` is user-set too; threads that existed
  before the migration keep their titles (untitled ones are `auto`). A failed title call is retried after the next
  completed run. The run waits at most 3 s for the title: a slower title is saved after the run without `work.title`
  (refetch threads periodically / on open). `Thread.titleSource: "auto" | "generated" | "user"` exposes the source.
- `resume` and `replaceFromMessageId` cannot be combined (`400 invalid_request`). An edit while a question is pending
  cancels it like a plain message. `Thread.plan` is recomputed after an edit from the last visible `update_plan` call
  (`[]` when none).
- **Search**: `GET /threads?search=` (case-insensitive on title and message content, trigram-free ILIKE, ≤ 200 chars).
- **Feedback**: `PUT /messages/{messageId}/feedback` `{"sentiment": "like" | "dislike", "comment"?: string}`
  (comment required for dislike, ≤ 2000) → `200 {sentiment, comment}`; `DELETE` → `204`. `Message.feedback` is
  returned on assistant messages. `404` for user/tool messages, hidden messages and other users' messages.
- **Export**: `GET /threads/{threadId}/export?format=md|json` → attachment; md renders user/assistant text, tool
  calls as collapsed summaries, artifact links (reasoning left out); json is `{thread, messages, exportedAt}` with every
  visible message.
- **Copy** is frontend-only.

## 6. Goals

A thread can have one active goal: the runtime keeps working across runs until the goal is judged met.

| Method | Path | Body | Response |
|---|---|---|---|
| PUT | `/threads/{threadId}/goal` | `{"text": string (≤ 4000), "maxTurns"?: number (1–20, default 8), "model"?: string, "reasoningEffort"?: string, "interactionMode"?: "interactive"\|"automatic"}` | `200 Goal & {runId?: string}`; if no run is active and no question is pending, immediately starts a run (`kind:"goal"`, user message "Work toward the goal: <text>", in `interactionMode` — omitted: the thread's last mode, else automatic) and returns its `runId`; automatic continuation runs keep the mode of the run they follow |
| GET | `/threads/{threadId}/goal` | | `Goal` (also when `met`/`exhausted`/`cleared`) or `404` |
| DELETE | `/threads/{threadId}/goal` | | `204` (status → `cleared`) |

```ts
type Goal = { text: string; status: 'active' | 'met' | 'exhausted' | 'cleared'; turnsUsed: number; maxTurns: number;
  lastVerdict?: { met: boolean; reason: string; at: string }; createdAt: string; updatedAt: string };
```

- While active, the goal is injected into every run's system prompt ("Current goal: … Work toward it; when you
  believe it is met, say so explicitly and explain").
- After each completed run of the thread (`chat` or `goal`; not cancelled/failed, not `compact`, not a run that ended
  waiting for an `ask_user` answer — the answer's run is checked) the runtime evaluates the goal with a short
  model call (strict JSON `{"met": bool, "reason": string}` from the last assistant answer + goal + the run's tool
  calls + files written; one retry on invalid JSON). Met → status `met`. Not met and `turnsUsed < maxTurns` →
  automatically enqueues a new run `kind:"goal"` whose user message (`metadata.source: "goal"`) is
  "Continue working toward the goal. Last check: <reason>". Otherwise `exhausted`. A user message sent meanwhile
  simply becomes the next run (the goal stays active). `turnsUsed` counts goal runs started (the first one included).
  An evaluation that fails twice in a row records `lastVerdict.reason: "evaluation failed"` and stops the automatic
  continuation (the goal stays active; the next completed run is checked again). Cancelling a goal run stops the loop
  too (goal stays active).
- Events (in the run that triggered the check, before its terminal event): `CUSTOM work.goal`
  `value: Goal & {nextRunId?: string}`.
- The judge has a 20 s budget (retry included); beyond it the check counts as an evaluation failure. A goal replaced
  (`PUT`) while a run was ending — the `PUT` then returns no `runId` — gets its first goal run as soon as that run is
  finished (same model / reasoning effort as that run); that run's `work.goal` carries the new goal and its `nextRunId`.
- `Thread` gains `goal: Goal | null` and `Run` gains `kind: 'chat' | 'goal' | 'compact'`.

## 7. Agent tools added

- `update_plan{items: {id, title, status: "pending" | "in_progress" | "completed"}[], explanation?}` — the model's
  todo list; persisted on the thread (`work_threads.plan`), emitted as `STATE_SNAPSHOT {snapshot: {plan: items}}`
  right after the call's `TOOL_CALL_RESULT` and returned in `Thread.plan` (`[]` when none). System prompt: use it for
  multi-step tasks, keep exactly one item in progress.
- `ask_user{question, options?: {label, description?}[], allowFreeText?: boolean (default true)}` — ends the run
  with `RUN_FINISHED{outcome: {type: "interrupt", interrupts: [{id, reason: "ask_user", message: question,
  toolCallId, responseSchema: {options, allowFreeText}}]}}` (AG-UI 1.0) after closing open steps/messages; the run
  status is `completed`, the thread gets `pendingQuestion: {interruptId, runId, toolCallId, question, options,
  allowFreeText}`. Answer: run creation with `resume: [{interruptId, status: "resolved", payload: {answer: string,
  optionLabel?: string}}]` (and no `message`, or a message which is then also appended) — the runtime stores the
  answer as the `ask_user` tool result ("The user answered: …" / "The user chose: …") and continues the loop; the
  new run's first event after `RUN_STARTED` is that `TOOL_CALL_RESULT`. `status: "cancelled"` → tool result "The user
  declined to answer." `409 no_pending_question` (no pending question or another `interruptId`) / `409
  question_pending` (`POST /compact` while a question is pending; a plain message while a question is pending is
  allowed and implicitly cancels the question with the result "The user did not answer the question and sent a new
  message instead."). One question per step: a second `ask_user` in the same step gets an error result.
- Approvals (interactive mode, Work parity): runs with `interactionMode: "interactive"` pause before risky calls
  (`execute_command`, `write_file`, `edit_file`, `*delete*/remove*/move*/rename*` tools incl. MCP) with
  `work.approval {phase: "required"}` and `RUN_FINISHED{outcome: interrupt, reason: "tool_approval"}`; decided with
  `POST /runs/{runId}/approvals/{approvalId}/resolve {decision: approve|deny|respond}` (README *Approvals*). MCP tools
  are judged on their original `{server}__{tool}` name (the exposed one may be truncated) and on every part after a
  `__`. The resume run keeps the paused run's model, effort, mode and attached skills and gets the caller's web search
  token; a decision cannot be combined with a message (`400`); a resume run that ended without running the decided
  call (failed, cancelled, interrupted) reopens the approval for a new decision; an approval cancelled by stop is
  announced (`work.approval` resolved, `cancelled`) by the next run. Guided skill creation sessions start interactive
  (Work's `interaction_mode=interactive`), and goal runs take the `interactionMode` of `PUT /goal` (continuations keep
  the mode of the run they follow).
- `memory_search`, `memory_get`, `memory_create`, `memory_update`, `memory_delete`, `memory_create_batch`,
  `memory_delete_batch`, `memory_capabilities` — user memory (§8; formerly `remember` / `forget`).
- `load_skill{name}` (§1).

## 8. Memory

*Memory parity with Work (supersedes the per-user facts and `remember` / `forget`):* per-user entries (path, title,
content, tags, importance, metadata, status active / pending / deleted) in `work_memory_entries` (migration `0010`, the
former facts become `memories/semantic/facts/` entries), with Work's memory MCP tools (same names, schemas and results),
hybrid search (0.45 keyword + 0.25 bag-of-words cosine + 0.15 path/tag + 0.10 importance + 0.05 recency), write
policy (triggers, `warn` / `strict`, secret redaction, soft / hard delete), Work's memory protocol and
`[Repository Memory Recall]` block in the system prompt instead of every fact, and a periodic consolidation job
(deduplication, purge of deleted entries). Details in the README, section *Memory*. Disabled per user with a setting.

| Method | Path | Body | Response |
|---|---|---|---|
| GET | `/memories?status=…` | | `{"enabled", "items": Memory[], "writeMode", "deleteMode", "limits"}` (default active and pending) |
| POST | `/memories` | `{"content" (or "text"), "title"?, "path"?, "tags"?, "importance"?, "metadata"?}` | `201` entry (`status: "active"`); `409 memory_limit` / `memory_path_exists` |
| PATCH | `/memories/{id}` | `title, content, path, tags, importance, metadata`, `status: "active"` | `200` entry |
| POST | `/memories/{id}/confirm` | `{}` | `200` entry (`status: "active"`); `404` otherwise |
| DELETE | `/memories/{id}?mode=hard\|soft` | | `204` (permanent by default; also rejects a pending proposal) |
| GET | `/memories/capabilities` | | `{"capabilities"}` |
| PUT | `/memories/settings` | `{"enabled": boolean}` | `200 {"enabled"}` |

`CUSTOM work.memory {action: "created" | "updated" | "proposed" | "deleted", id, path, title, text, status, mode?}` per
entry when the model uses the tools (right after their `TOOL_CALL_RESULT`). The memory tools are only offered to the
model while memory is enabled.

Untrusted context (output of `execute_command` / `read_file` / `edit_file` / `list_files` / MCP tools / `load_skill` of a
user skill — in this run or earlier in the visible history —, an attachment, or a compaction summary): `memory_create`
stores the entry `pending` and `memory_update` turns it `pending` (`action: "proposed"`); pending entries are neither
recalled, searched nor readable until the user confirms them (`POST /memories/{id}/confirm`) or deletes them.
`POST /memories` (user-initiated) is active immediately.

## 8b. Retention

`DELETE /threads/{id}` stays a soft delete. Threads soft-deleted more than `WORK_PURGE_AFTER_DAYS` (30) days ago are
hard-deleted by the runtime sweeper with their runs, events, messages, feedback, goal, compaction markers, artifacts
and versions; their content blobs (and cached previews) are deleted from the blob backend once no artifact version
references them and nothing has referenced them for `WORK_BLOB_GC_GRACE_HOURS` (24 h). Reusing stored content (upload,
snapshot) and recording a new version refresh the blob's last reference, and the deletion is conditional on both rules
still holding, so content a concurrent upload of identical bytes reuses is never collected. Memories (per user) and user
skills are kept.

## 8c. Ideas

Work's Ideas panel: `GET /ideas?featuredOnHome=` → `{"items": Idea[]}` (10 ideas, 4 featured), `GET /ideas/{ideaId}`,
`POST /ideas/{ideaId}/usage {action: "selected" | "submitted", surface?, threadId?, metadata?}` → `201` (table
`work_idea_usage`). `POST /threads/{id}/runs` accepts `sourceIdeaId` (unknown → `400 invalid_idea`) and records a
`submitted` usage (surface `composer`) when the run has a message. Demo PDF/Office files are real documents built from
Work's demo text (`demoFiles[].contentBase64`), so they pass the upload type check.

## 9. Models

`GET /models` items gain `{contextWindow, supportsReasoning, supportsVision}`.

## 10. Frontend (summary for the frontend developer)

- Composer: attachment button + drag & drop + paste (upload queue, 4 concurrent, progress, retry on 429, removable
  chips), model picker + reasoning effort (when supported), skills picker (popover with enabled skills, `@skill`
  mention in text), slash commands menu (`/goal <text>`, `/goal clear`, `/compact`, `/plan` (asks the agent to plan
  only), `/new`, `/model <id>`, `/skills`, `/memory`, `/export`, `/help`), context-usage badge, existing steer
  queue/stop.
- Messages: copy, edit (user, inline editor → `replaceFromMessageId`), regenerate (last assistant), thumbs up/down
  with comment dialog for dislike, attachment chips on user messages, `workspace-file:` links as file chips,
  "Context compacted" divider, goal-continuation messages labelled "Goal — sent automatically", `ask_user` card with
  options / free text that answers via `resume`, plan indicator above the composer (Step x/y + checklist).
- Approvals: interaction mode selector in the composer (interactive by default, remembered per thread), approval
  dock above the composer (Work's `CoworkApprovalDock`: tool name, input/command/diff preview, approve / deny /
  respond) wired to `POST /runs/{runId}/approvals/{approvalId}/resolve`; pending `ask_user` questions use the same dock.
- Header: goal bar (text, status, turns used, edit/clear), export menu.
- Right panel: **Artifacts** tab (default) listing artifacts with type icons, version history, download, restore
  version, and a preview pane: PDF (pdf.js — reuse Studio's existing PDF viewer used by Cowork
  `CoworkPdfPreviewViewer` if it is generic enough, else wrap it), images, markdown rendered, code highlighted,
  html as highlighted text only, Office files via `/preview` (PDF); the existing Files tab and Terminal tab stay.
- Settings dialog: Skills (list, enable/disable, upload zip, delete, view SKILL.md) and Memory (list with path,
  title, tags, importance and status; add, edit, keep a pending entry, delete / restore; toggle).
- Thread list: search box wired to `?search=`.
- Ideas: sidebar *Explore → Ideas* opens Work's Ideas panel in the work area (category icon, first 2 demo files,
  *Use idea*): new conversation, prompt prefilled, `selected` usage, demo files uploaded, `sourceIdeaId` on the first send.
