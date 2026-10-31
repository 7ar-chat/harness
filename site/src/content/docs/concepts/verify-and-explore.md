---
title: "Verify and explore"
description: "Three features that give the agent feedback: deliverable checkers, a read-only explore sub-agent for open-ended questions, and syntax checks after code edits."
---

These features keep the agent honest and its context small. **Verify** runs a checker on every deliverable the agent writes and stops it from finishing on an unchecked one. **Explore** hands an open-ended repository question to a read-only sub-agent so the files it reads never enter the main conversation. **Codecheck** appends a syntax error to the result of `write_file` and `edit_file`. Verify is in the `work` and `coding` [profiles](/docs/concepts/features-profiles/); Explore and Codecheck(true) are in `coding`; `Codecheck(false)` in `work` only installs when `CODE_CHECKS` is set.

## Verify: check what the agent produces

Verification applies to **deliverables**: files under `/workspace/outputs/`. After a tool call creates or changes one, the checker for its extension runs in the thread's sandbox (60 seconds at most per check) and its summary is appended to the tool result. The same result is sent as a `work.verification` custom event and stored in the tool message's `metadata.verification`.

The feature requires the Artifacts feature and a sandbox. With `Sandbox: off` runs, or no sandbox, a deliverable's check reports `could not run (no sandbox)`.

### What is checked

| Extension | Checker | Skill it comes from |
|---|---|---|
| `.docx` | `inspect_docx` | `docx` |
| `.pptx` | `fit_text` (only overlaps the agent added to an uploaded deck fail, by comparing with the original) | `pptx` |
| `.xlsx`, `.xlsm` | `recalc` | `xlsx` |
| `.pdf` | `extract` | `pdf` |
| `.png`, `.jpg`, `.jpeg`, `.svg` | `make_chart` | `charts` |
| `.py`, `.json`, `.csv` | `py_compile`, `json`, `csv` (a generic validator shipped with the `xlsx` skill) | `xlsx` |

Documents (`.docx`, `.pptx`, `.pdf`) that cite pages as `(p. N)` are also checked against the PDFs the user uploaded to the thread (`check_citations`, up to 5 sources); its verdict is merged into the format checker's. Other file types have no checker.

A checker is a command that prints a JSON object `{ok, file, checks: [{name, ok, detail}], next_step?}` and exits `0` (ok), `2` (not ok) or `1` (could not run). A checker that could not run, timed out or gave no result leaves the file **unverified**, which is different from known bad. The skills are installed into the sandbox on first use (see [Skills](/docs/concepts/skills/)).

Custom checkers: `verify.New(verify.Options{Checkers: map[string]verify.Checker{...}})` replaces the defaults, keyed by lower-case extension. A `verify.Checker` has a `Name`, an optional `Skill`, and a `Command` in which `{file}` is replaced by the shell-quoted path. The `Verifier` becomes the run's checker through `Builder.SetChecker`; see [Add a custom feature](/docs/guides/custom-feature/).

### The stop guard

The loop tracks every deliverable changed in the turn. If the model tries to answer while one is unverified, it is refused **once** with a reminder: `Before you finish: these deliverables changed in this task are not verified yet: ...` followed by a reason per file (its checker failed, no checker ran after its last change, the checker could not run). If it still cannot fix them, it must say in its answer which file could not be verified and why.

Two details worth knowing:

- At most 3 checkers run after one tool call; the other changed files are checked before the next model call and the model is told so.
- For models marked as vision models (`LLM_VISION_MODELS`), `.docx`, `.pptx` and `.pdf` deliverables must also have been looked at with `view_file` after their last change. A file `view_file` cannot render is reported as `render failed: ...` and the model is told to say that it could not view it.

Every answer from a run that changed deliverables carries `metadata.verification` with three lists: `verified`, `failed` (path, checker, summary) and `unverified` (path, reason). A `work.verification` event announces it live.

### Verify and goals

When both features are installed, the [goal judge](/docs/concepts/plan-and-goals/#goals) receives, for each deliverable produced since the goal was set (at most 8), the latest checker result and the first 2000 characters of its extracted text. A failed checker prevents the goal from being met.

## Explore: a read-only sub-agent

`explore` is a tool the main agent can call with a self-contained `task`, such as "where is X handled" or "which files would change for Z". It starts a short separate conversation, with its own model, that can only call `read_file`, `search_files` and `list_files`. It returns a report of at most 300 words (paths and line numbers rather than file contents; the report is capped at 6000 characters). Because the files it reads stay out of the main context, every later step is cheaper; with a cheaper model it is cheaper still.

The loop is `ai/agent.Run` with these limits:

| Limit | Value |
|---|---|
| Steps | 12 (`DefaultMaxSteps`); at the limit the explorer is told to write its report |
| Parallel read-only calls per step | 4 |
| One tool result | 8000 characters, then cut |
| `work.subagent` events per call | at most 200; the `end` event always goes out |

The `explore` tool is itself read-only, so [plan mode](/docs/concepts/plan-and-goals/) keeps it. It needs the sandbox: without file tools the call fails with `no file tools are available (no sandbox)`.

### Choosing the model

The explorer uses `EXPLORE_MODEL`, falling back to `LLM_DEFAULT_MODEL`. In `config.yml` it is `explore_model` or `agents.explore.model`, and a project file may set it (it must be a model you configured). If neither model is set the tool is not installed. Its token usage is added to the run's totals.

```bash
EXPLORE_MODEL=claude-haiku-4-5 AGENT_PROFILE=coding harness
```

### Watching it

The explorer reports through `work.subagent` custom events: `toolCallId` (the id of the `explore` call), `phase` (`start`, `step`, `tool`, `text`, `end`), `agent` (`explore`), `step`, `model`, cumulative `usage` and, depending on the phase, `task`, `tool` (`name`, `args` summary, `result` first line, `isError`), `text`, `reportChars` and `failed`. See [Event types](/docs/reference/events/).

In the terminal UI the steps appear live under the `explore` tool call (expand with `ctrl+o`), and `/children` (alias `/subagents`) lists the sub-agent runs of the conversation and opens one. See [Run the CLI and TUI](/docs/quickstart/cli/).

## Codecheck: syntax feedback after an edit

After `write_file` or `edit_file`, a cheap per-extension command checks the file. Only a failure is reported, appended to the tool result so the next step can fix it:

```text
Syntax check failed (python3 -m py_compile app.py, exit 1); fix it before going on:
...
```

It is deliberately not a language server: no build, no type check. A command whose first word is not installed in the sandbox is skipped silently, and one check is bounded by 20 seconds and 1500 characters of output.

| Extension | Default command |
|---|---|
| `.go` | `gofmt -e {path} >/dev/null` |
| `.py` | `python3 -m py_compile {path}` |
| `.json` | `python3 -m json.tool {path} >/dev/null` |
| `.js`, `.mjs` | `node --check {path}` |
| `.sh` | `bash -n {path}` |
| `.rb` | `ruby -c {path} >/dev/null` |
| `.php` | `php -l {path} >/dev/null` |

`CODE_CHECKS` adds or overrides entries as `.ext=command` separated by `;`, with `{path}` standing for the shell-quoted file. An entry with an empty command disables an extension. In `config.yml` the key is `code_checks`.

```bash
CODE_CHECKS='.cjs=node --check {path};.mjs=' harness
```

<div class="callout note">

**When it is on.** The checks are always on with `AGENT_PROFILE=coding`, and in other profiles whenever `CODE_CHECKS` is set. A malformed entry fails at startup: `CODE_CHECKS: "go=gofmt" is not .ext=command`. The Codecheck feature requires the Sandbox feature. The `docs/sandbox-adapters.md` file of the repository names the variable `AGENT_CODE_CHECKS`; the code reads `CODE_CHECKS`.

</div>

Parsing is available on its own: `codecheck.Parse(".cjs=node --check {path};.mjs=")` returns the map, and `codecheck.New(extra)` merges it over the defaults.

Related pages: [Artifacts and checkpoints](/docs/concepts/artifacts-checkpoints/), [Sandboxes](/docs/concepts/sandboxes/), [Tools](/docs/concepts/tools/).
