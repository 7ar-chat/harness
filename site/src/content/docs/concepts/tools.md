---
title: Tools
description: "What a tool is, how the harness builds the tool set of each run, the built-in tools, how to select them with AGENT_TOOLS, and how large outputs are bounded."
---

A tool is a function the model can call during a [run](/docs/concepts/agent-loop/): it receives JSON arguments and returns text. The harness assembles the tool set **per run** from `tools.Provider`s; built-in features contribute the sandbox and plan tools, and you add your own with `Options.Tools` or from a [feature](/docs/concepts/features-profiles/). To write one step by step, see [Add a custom tool](/docs/guides/custom-tool/).

## A custom tool

`ai.InferTool` builds a tool from a Go function: the parameter schema is reflected from the input struct (`json` and `jsonschema_description` tags), and the result is rendered as JSON (a string is returned as is). A `tools.Provider` is asked for its tools once per run and receives the run's identity, so a tool can close over the user:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/tools"
)

type orderArgs struct {
	ID string `json:"id" jsonschema_description:"the order id"`
}

// ordersTools is a tools.Provider: it is asked for its tools once per run, with the run's identity.
type ordersTools struct{}

func (ordersTools) Name() string { return "orders" }
func (ordersTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	lookup, err := ai.InferTool("lookup_order", "Look up an order by id.", func(_ context.Context, in orderArgs) (string, error) {
		if in.ID == "" {
			return "", tools.Errorf("id must not be empty") // the model sees this message and can fix its call
		}
		return "order " + in.ID + " shipped (for " + rc.Login + ")", nil
	})
	return []ai.BaseTool{readOnly{lookup}}, nil, err
}

// readOnly marks a tool as free of side effects: consecutive read-only calls of one step run in parallel.
type readOnly struct{ ai.InvokableTool }

func (readOnly) ReadOnly() bool { return true }

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("AGENT_TOOLS", "-job_kill,-view_file") // remove two built-in tools
	cfg, err := config.Load()
	check(err)

	var offered []string
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if offered == nil {
			for _, t := range c.Tools {
				offered = append(offered, t.Name)
			}
		}
		return llmtest.Turn{Chunks: llmtest.Text("ok")}
	})

	h, err := harness.New(harness.Options{
		Config:   cfg,
		Models:   llmtest.Factory{M: model},
		Backend:  sandboxtest.NewBackend(),
		Features: append(harness.ProfileMinimal(), harness.ToolProfile(false)),
		Tools:    []tools.Provider{ordersTools{}},
	})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "m"})
	check(err)
	for ev := range h.Events(ctx, run.ID, 0) {
		if agui.IsTerminal(ev.Type) {
			break
		}
	}
	sort.Strings(offered)
	fmt.Println(strings.Join(offered, "\n"))
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

The scripted model records the tools it was offered. The sandbox tools of `ProfileMinimal`, `update_plan`/`ask_user`, and `lookup_order` are there; `job_kill` and `view_file` were removed by `AGENT_TOOLS`:

```text
ask_user
edit_file
execute_command
job_output
list_files
lookup_order
read_file
search_files
update_plan
write_file
```

### What a provider receives

```go
type Provider interface {
	Name() string
	Tools(ctx context.Context, rc RunContext) ([]ai.BaseTool, func(), error)
}
```

`RunContext` carries `ThreadID`, `RunID`, `Login` (the owner of the run) and `Sandbox` (`""`, `auto`, `on` or `off`). The second return value is a cleanup called at the end of the run (may be nil). A provider that returns an error is logged and skipped; the run goes on without its tools. If two providers offer the same tool name, the first one wins and the duplicate is dropped with a warning.

<div class="callout tip">

**Close over `rc.Login`.** Build tools per run and let them capture the user. A tool that can only reach the caller's data cannot be talked into reading someone else's, whatever the model or an injected document asks for.

</div>

### Errors the model can act on

Return `tools.Errorf("...")` for a failure the model should read and correct (bad arguments, "not found"). Its message is passed to the model as `Error: <message>`. Any other error is also reported to the model, as its text. A panic is caught and reported as an error; it does not end the run. Return the same answer for "missing" and "not yours".

### Marking a tool read-only

A tool that has no side effects can implement `ReadOnly() bool`. Consecutive read-only calls of one step then run in parallel (up to 4), see [tool scheduling](/docs/concepts/agent-loop/#tool-scheduling). Read-only tools are also the only custom tools kept in a **plan-mode** run (`RunOptions.Mode: harness.ModePlan`), which removes every tool that does not declare itself read-only, so the model cannot change anything.

## Built-in tools

Which tools a run has depends on the [features](/docs/concepts/features-profiles/) installed and on the sandbox mode:

| Tool | Feature | Read-only | What it does |
|---|---|---|---|
| `execute_command` | `sandbox` | no | Run a shell command in the sandbox (long ones continue as background jobs). |
| `job_output` | `sandbox` | yes | Read the output of a background job. |
| `job_kill` | `sandbox` | no | Stop a background job. |
| `read_file` | `sandbox` | yes | Read a text file. |
| `view_file` | `sandbox` | yes | View a document or image. |
| `search_files` | `sandbox` | yes | Search text in files. |
| `write_file` | `sandbox` | no | Create or overwrite a file. |
| `edit_file` | `sandbox` | no | Edit a file in place. |
| `list_files` | `sandbox` | yes | List files and directories. |
| `update_plan` | `plan` | no | Keep a plan of items with a status. |
| `ask_user` | `plan` | no | Ask the user a question; the run ends with an [interrupt](/docs/concepts/threads-runs-events/#interrupts). |
| `explore` | `explore` | yes | A read-only sub-agent that searches the workspace and reports back. |
| `load_skill` | `skills` | yes | Load the instructions of a [skill](/docs/concepts/skills/). |
| `memory_search`, `memory_get`, `memory_create`, `memory_update`, `memory_delete`, `memory_create_batch`, `memory_delete_batch`, `memory_capabilities` | `memory` | no | Long-term [memory](/docs/concepts/memory/). |
| `{server}__{tool}` | `mcp` | by the server's `readOnlyHint` | Tools of [MCP servers](/docs/concepts/mcp/). Characters outside `[a-zA-Z0-9_-]` become `_`; names are cut to 64 characters. |

The sandbox tools are offered only when a backend is configured and the run's sandbox mode is not `off`; without a backend a run has no sandbox tools and a prompt that says so. The sandbox itself is created lazily, on the first tool call that needs it. See [Sandboxes](/docs/concepts/sandboxes/).

## Selecting tools with AGENT_TOOLS

`AGENT_TOOLS` (config.yml `tools`) is a comma list applied over the profile's default tools by the `toolprofile` feature, which is part of the `work` and `coding` profiles:

| Entry | Meaning |
|---|---|
| `-name` | Remove a tool. Also removes tools that are not built in (MCP, host tools) by exact name. |
| `+name` | Add a built-in tool the profile leaves out. |
| `name` (bare) | Replace the default set: only the bare names are offered. |
| `memory` | Stands for all `memory_*` tools, and hides the memory prompt section. |
| `skills` | Stands for `load_skill`, and hides the skills index. |

```bash
AGENT_TOOLS="-execute_command,-memory"        # work profile minus shell access and memory
AGENT_PROFILE=coding AGENT_TOOLS="+memory"    # coding profile with the memory tools back
```

Tools that are not built in (yours, MCP) are always offered unless you remove them by name; a bare list does not remove them. The `coding` profile starts without `view_file`, `load_skill` (unless the user has skills of their own, or `SKILLS_IN_CODING` is set) and the memory tools (unless `MEMORY_IN_CODING` is set). In the coding profile `view_file` also needs a vision model in `LLM_VISION_MODELS`, even when `+view_file` adds it.

<div class="callout note">

**`AGENT_TOOLS` needs the `toolprofile` feature.** `ProfileMinimal()` does not include it, so with the minimal profile the variable has no effect. Add `harness.ToolProfile(false)` to your feature list, as the program above does, or use `ProfileWork()` or `ProfileCoding()`.

</div>

## Output caps and spill

The model never reads an unbounded tool result:

- A result longer than `AGENT_TOOL_OUTPUT_SPILL_CHARS` (default 20000) is written in full to a file under `/workspace/.work/tool-output/` in the sandbox. The model gets the head and tail, the path, and a hint to page it with `read_file` or `search_files`.
- Without a running sandbox, the output is cut and the model is told the full text was not kept.
- The final result is capped at `AGENT_MAX_TOOL_OUTPUT_CHARS` (default 30000), keeping the head and the tail.

The spill threshold must not exceed the cap; `config.Load` rejects that combination. Details, including how a JSON-object result stays valid JSON, are in [The agent loop](/docs/concepts/agent-loop/#output-caps-and-spill).

## Risky tools and approvals

In an interactive run (`RunOptions.Interactive: true`), risky tool calls pause for the user's approval before they execute. The harness treats as risky: `execute_command`; `write_file` and `edit_file` outside `/workspace/outputs/` and `/workspace/.work/`; any tool whose name starts with `delete`, `remove`, `move` or `rename`; and MCP tools whose name contains a word matching `APPROVAL_RISKY_TOOL_PATTERNS` (default `create|send|update|delete|post|publish|write`). The run ends with an `interrupt` outcome and continues with `Resume`. Permissions rules and modes are described in [Approvals and permissions](/docs/concepts/approvals-permissions/).

To make a tool of your own risky, name it so one of those rules matches, or register it at start-up with `approvals.RegisterRisky("my_tool")` (package `engine/approvals`).

<div class="callout warning">

**Tool results are data, not instructions.** Keep your tools from returning secrets or other users' content in the first place; a result flows into the model's context, and an injected document can try to steer the model with it. The harness marks a run as having untrusted content once such a result is in the context; see the [security model](/docs/operations/security/).

</div>
