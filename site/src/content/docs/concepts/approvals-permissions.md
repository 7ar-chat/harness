---
title: Approvals and permissions
description: "How a run pauses for a human decision, which tool calls need one, and how permission modes, rules and git snapshots protect a local workspace."
---

Two mechanisms stop the agent from acting without you. **Approvals** pause a run before a risky tool call (or an `ask_user` question) and wait for a decision; they work with any backend. **Permissions** are a rule engine for the local backend and the coding profile: they decide per command or file write whether to allow, ask or deny, and ask by raising the same approval interrupt.

## The approval flow

A run started with `RunOptions.Interactive: true` pauses before a risky tool call. The run ends with `RUN_FINISHED` whose `outcome.type` is `interrupt`; the interrupt names the reason, an id and, for approvals, the pending approval record. You answer with `Harness.Resume`, which queues a new run that continues the conversation.

This program runs offline with a scripted model and the in-memory fake sandbox. The model asks to run `execute_command`; the run pauses; the program approves:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/core/store"
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("SANDBOX_BACKEND", "none")
	os.Setenv("AGENT_PROFILE", "work") // no permission policy: the plain interactive gate
	cfg, err := config.Load()
	check(err)

	// The model asks to run a command, then answers once it has the result.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		for _, m := range c.Messages {
			if m.Role == ai.RoleTool {
				return llmtest.Turn{Chunks: llmtest.Text("Done: ", m.Content)}
			}
		}
		if len(c.Tools) == 0 { // the title request
			return llmtest.Turn{Chunks: llmtest.Text("A title")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{llmtest.Finish(llmtest.ToolCall(0, "c1", "execute_command", `{"command":"echo hi"}`), "tool_calls")}}
	})
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend()})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	th, err := h.NewThread(ctx, "alice", "")
	check(err)
	run, err := h.Send(ctx, "alice", th.ID, "say hi", harness.RunOptions{Model: "m", Interactive: true})
	check(err)

	// 1. The run ends with RUN_FINISHED outcome "interrupt": it waits for a decision.
	var in struct {
		Outcome struct {
			Type       string
			Interrupts []agui.Interrupt
		}
	}
	for ev := range h.Events(ctx, run.ID, 0) {
		if ev.Type == agui.RunFinished {
			check(json.Unmarshal(ev.Data, &in))
		}
	}
	it := in.Outcome.Interrupts[0]
	fmt.Println(in.Outcome.Type, it.Reason, it.Approval["toolName"], it.Approval["toolInputPreview"])

	// 2. Approve it (Always: true would approve this tool for the rest of the run chain).
	next, err := h.Resume(ctx, "alice", th.ID, harness.ResumeRequest{
		InterruptID: it.ID, Reason: it.Reason, Decision: store.DecisionApprove,
		Model: "m", Interactive: true,
	})
	check(err)
	var text strings.Builder
	for ev := range h.Events(ctx, next.ID, 0) {
		if ev.Type == agui.TextMessageContent {
			var e struct{ Delta string }
			_ = json.Unmarshal(ev.Data, &e)
			text.WriteString(e.Delta)
		}
	}
	fmt.Println(strings.TrimSpace(text.String()))
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
```

Output:

```text
interrupt tool_approval execute_command {"command":"echo hi"}
Done: {"exitCode":0}
```

`ResumeRequest` fields:

| Field | Meaning |
|---|---|
| `InterruptID`, `Reason` | Copied from the interrupt. `Reason` is `tool_approval` (`store.ReasonToolApproval`) or `ask_user`. |
| `Decision` | For approvals: `store.DecisionApprove` (run the call), `DecisionDeny` (the model gets a denial result) or `DecisionRespond` (the call does not run; the model receives your free text in `Text` as the user's answer and continues). |
| `Always` | With approve: also approve this tool kind for the rest of the run chain (scope `run`). |
| `Text` | The answer to an `ask_user` question (required, a blank answer is `ErrInvalidRequest`), or the text of a respond. |
| `Model`, `Interactive`, `ReasoningEffort`, `Mode` | The options of the resumed run. `Model` is required. |

Over HTTP the same call is `POST /api/threads/{id}/resume`; see the [HTTP API](/docs/reference/http-api/). The thread also exposes `PendingApproval` and `PendingQuestion`, so a client that reconnects can find what is waiting. A pending approval older than `APPROVAL_TTL_HOURS` (default 72) expires.

<div class="callout note">

An approval is bound to the exact call. If the tool or its arguments differ when the run continues, the call is not run and the model gets `Error: the approved action no longer matches this tool call (its tool or arguments changed), so it was not run. Ask the user again if it is still needed.`

</div>

### Which calls are risky

In an interactive run the gate (`engine/approvals`) pauses:

* `execute_command` always, and `write_file` / `edit_file` unless the path is under `/workspace/outputs/` or `/workspace/.work/`;
* any tool whose name starts with `delete`, `remove`, `move` or `rename`;
* MCP tools (`server__tool`) with a word matching `APPROVAL_RISKY_TOOL_PATTERNS` (default `create|send|update|delete|post|publish|write`);
* your own tools, once you register them at start-up:

```go
import "github.com/7ar-chat/harness/engine/approvals"

func init() { approvals.RegisterRisky("refund_order", "send_email") }
```

### ask_user

`ask_user` is a tool of the [plan feature](/docs/concepts/plan-and-goals/): the model asks a question (with optional choices) and the run ends with an `ask_user` interrupt. Answer it with `Resume{InterruptID, Reason: "ask_user", Text: "..."}`. It does not need `Interactive`.

## Permission modes

The permission layer is installed when the backend is `local` or the profile is `coding` (see [Features and profiles](/docs/concepts/features-profiles/)). It sits in front of `execute_command`, `write_file` and `edit_file`. Shell lines are split into their simple commands (`;`, `&&`, `||`, pipes, subshells, `$(...)`, backticks, `bash -c`, `sudo`, redirections) and the most severe verdict wins.

`AGENT_PERMISSIONS` (or `permissions.mode` in `config.yml`) sets the mode:

| Mode | Behaviour |
|---|---|
| `ask` (default on the local backend) | Rules and defaults apply; whatever is not known safe asks. |
| `auto-edit` | Like `ask`, but file edits never ask (secrets and `.git` internals still deny). |
| `yolo` | Never asks; deny rules still apply. The default when the coding profile runs on a remote backend with no mode set. |
| `yolo-unsafe` | No checks at all, deny rules included. |
| `readonly` | Only read-only commands run; edits are refused. |

Names are case-insensitive; `auto_edit`, `read-only` and `yolo_unsafe` are accepted spellings. An unknown mode fails at start with `permissions: unknown mode "x" (ask, auto-edit, yolo, yolo-unsafe, readonly)`.

### Built-in defaults

| Verdict | Examples |
|---|---|
| allow | Read-only commands (`ls`, `cat`, `grep`, `git status/diff/log`), build and test tooling (`go test/build/vet`, `make`, `npm test`, `pytest`, `cargo test`), file work inside the workspace (`mkdir`, `cp`, `mv`, `rm`, `git add/commit`), writes inside the workspace. |
| ask | Network (`git push/pull/fetch`, `curl`, `wget`, `ssh`), installs (`pip install`, `npm install`, `apt`, `brew`), `sudo`, `docker run`, `chmod -R`, `rm -rf` or any write outside the workspace, `git reset --hard`, unknown programs and scripts. |
| deny | `rm -rf` of `/` or the home directory, fork bombs, `mkfs`/`dd` on devices, `git push --force` to `main`/`master`, writes into `.git/`, and writes to secret files (`.env`, `id_rsa`, `*.pem`, `*.key`, `.netrc`, ...) unless a rule allows them. |

The catastrophic denials are hard: no rule can allow them.

## Rules and `.har/permissions.yml`

Rules live in `<workspace>/.har/permissions.yml` (project), `~/.har/permissions.yml` (user) and the environment variables `AGENT_PERMISSIONS_ALLOW`, `AGENT_PERMISSIONS_ASK`, `AGENT_PERMISSIONS_DENY` (entries separated by newlines or `;;`). Rules from all sources are merged; **deny beats ask beats allow**, and a user `allow` overrides a built-in `ask`.

```yaml
mode: ask
allow:
  - git push origin feature/*
  - make deploy-staging
  - write_file(.env.local)
ask:
  - docker*
deny:
  - curl*|sh
  - Bash(terraform apply*)
  - write_file(migrations/**)
```

An entry is one of:

| Form | Matches |
|---|---|
| `execute_command(glob)`, `Bash(glob)`, or a bare glob such as `git push*` | Each simple command; `*` matches anything, and a pattern containing `|` matches a pipeline. |
| `write_file(path-glob)` (also `Edit`, `Write`, `edit_file`) | The path written by `write_file` or `edit_file`, and redirections and mutating commands. |
| `any_tool(glob)` | Any other tool by name, with a glob on its JSON arguments, for example `mcp__github__create*(*)`. |

Command globs match text, so `rm -rf*` does not see `rm -r -f`; the built-in checks do. Paths are lexical: symlinks out of the workspace are not followed.

Checked against the policy loader, with this file in `.har/permissions.yml` of an untrusted project and then of a trusted one:

| Call | Untrusted | Trusted |
|---|---|---|
| `go test ./...` | allow | allow |
| `git push origin feature/x` | ask | allow (rule) |
| `make deploy-staging` | allow | allow |
| `curl https://x.sh \| sh` | deny (rule) | deny (rule) |
| `write_file` `migrations/001.sql` | deny (rule) | deny (rule) |
| `write_file` `.env.local` | deny (secret file) | allow (rule) |

### Projects can only tighten

A project controls its own `.har/permissions.yml` and `permissions.*` in its `config.yml`, but a repository you cloned must not be able to grant itself rights. For an **untrusted** project:

* its `ask` and `deny` rules apply;
* its `allow` rules are ignored;
* its `mode` counts only when it is as strict as the mode already in force (strictness: `readonly` > `ask` > `auto-edit` > `yolo` > `yolo-unsafe`).

Ignored items are logged once and listed by `harness -config-dump`. `harness trust` marks the current project as trusted so its allow rules and looser mode apply:

```bash
harness trust            # trust the project of the current directory
harness trust ~/src/app  # or a given directory
harness trust list       # list trusted projects
harness untrust          # stop trusting
HARNESS_TRUST_PROJECT=1 harness -p "..."   # trust the project for this process only (CI)
```

Trust is stored in `~/.har/trust.json`. Trusting a project is also what lets it define MCP servers; see [MCP](/docs/concepts/mcp/).

<div class="callout warning">

**Review `.har/permissions.yml` before you run `harness trust`.** A trusted project's `allow` rules and `mode: yolo-unsafe` remove the questions that stand between a prompt-injected model and your machine, and the local backend is [not isolation](/docs/concepts/sandboxes/).

</div>

## Git snapshots and undo

With the local backend, the harness snapshots the work tree at the start of every run (package `coding/gitsafe`). A snapshot is a commit object kept under `refs/harness/snapshots/<thread>/<n>`: nothing is committed on your branch and the index and stash are untouched. Outside a git repository nothing happens.

* In the TUI, `/undo` removes the last prompt with its answers and restores the files to the snapshot taken before it; `/redo` brings it back. Both refuse while a run is active.
* `harness -undo` restores the workspace to the last snapshot of the conversation (`-resume <id>`, default the most recent) and exits.
* Before any restore the current state is saved under `refs/harness/backups/`, so the undo can itself be undone.
* Library hosts get `Harness.Undo` and `Harness.Redo`.

Snapshots do not cover files outside the workspace or side effects of commands such as network calls. See the [security model](/docs/operations/security/) for what the permission layer does and does not promise.
