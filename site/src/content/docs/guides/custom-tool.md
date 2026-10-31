---
title: Add a custom tool
description: "Give the agent a function of your application as a tool: a tools.Provider built per run, a schema inferred from a Go struct, errors the model can act on, and approval for risky tools."
---

A tool is a function the model can call. In the harness a tool belongs to a `tools.Provider`, which is asked for its tools once per run, so a tool can close over the run's user and never see another tenant's data. This page builds one tool, runs it offline with the scripted model, and covers errors, risky tools and output limits. For the model of tools in general see [Tools](/docs/concepts/tools/).

## A complete example

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/engine/tools"
)

// Orders is the application's data. Every method takes the user.
type Orders interface {
	Status(ctx context.Context, user, id string) (string, bool)
}

type memOrders map[string]string

func (m memOrders) Status(_ context.Context, user, id string) (string, bool) {
	s, ok := m[user+"/"+id]
	return s, ok
}

type lookupArgs struct {
	OrderID string `json:"orderId" jsonschema_description:"The order number, e.g. A-1042"`
}

// orderTools is a tools.Provider: Tools is called once per run.
type orderTools struct{ orders Orders }

func (orderTools) Name() string { return "orders" }

func (p orderTools) Tools(_ context.Context, rc tools.RunContext) ([]ai.BaseTool, func(), error) {
	lookup, err := ai.InferTool("lookup_order",
		"Look up the status of one of the signed-in customer's orders.",
		func(ctx context.Context, in lookupArgs) (string, error) {
			if in.OrderID == "" {
				return "", tools.Errorf("orderId is required, for example A-1042")
			}
			status, ok := p.orders.Status(ctx, rc.Login, in.OrderID) // rc.Login: the run's user
			if !ok {
				return "no such order for this customer", nil // same answer for "missing" and "not yours"
			}
			return status, nil
		})
	if err != nil {
		return nil, nil, err
	}
	return []ai.BaseTool{lookup}, func() {}, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) // silence the run logs
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	check(err)

	// Scripted model: call the tool, then answer with its result.
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if last := c.Messages[len(c.Messages)-1]; last.Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("Result: ", last.Content)}
		}
		return llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "c1", "lookup_order", `{"orderId":"A-1042"}`), "tool_calls")}}
	})

	h, err := harness.New(harness.Options{
		Config:   cfg,
		Models:   llmtest.Factory{M: model},
		Features: harness.ProfileMinimal(),
		Tools:    []tools.Provider{orderTools{memOrders{"alice/A-1042": "shipped 2026-10-20"}}},
	})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	for _, user := range []string{"alice", "bob"} {
		th, err := h.NewThread(ctx, user, "")
		check(err)
		run, err := h.Send(ctx, user, th.ID, "Where is order A-1042?",
			harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
		check(err)
		for ev := range h.Events(ctx, run.ID, 0) {
			switch ev.Type {
			case agui.ToolCallResult, agui.TextMessageContent:
				var e map[string]any
				_ = json.Unmarshal(ev.Data, &e)
				fmt.Printf("%-5s %-22s %v\n", user, ev.Type, firstNonEmpty(e["content"], e["delta"]))
			}
			if agui.IsTerminal(ev.Type) {
				break
			}
		}
	}
}

func firstNonEmpty(vs ...any) any {
	for _, v := range vs {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

Running it prints (observed):

```text
alice TOOL_CALL_RESULT       shipped 2026-10-20
alice TEXT_MESSAGE_CONTENT   Result: 
alice TEXT_MESSAGE_CONTENT   shipped 2026-10-20
bob   TOOL_CALL_RESULT       no such order for this customer
bob   TEXT_MESSAGE_CONTENT   Result: 
bob   TEXT_MESSAGE_CONTENT   no such order for this customer
```

Both users asked for the same order id; the tool answered from the run's `rc.Login`. The scripted model is [`llmtest`](/docs/guides/testing/#script-the-model), so this needs no network or key.

## How it fits together

| Piece | What it is |
|---|---|
| `tools.Provider` | `Name() string` and `Tools(ctx, rc) ([]ai.BaseTool, func(), error)`. Called once per run; the returned func is a cleanup called at run end. |
| `ai.InferTool(name, desc, fn)` | Builds an `ai.InvokableTool` whose parameter schema is reflected from the input struct. A `string` result is returned as is; any other result type is rendered as JSON. |
| `Options.Tools` | Providers added to every run. A feature can add its own with `Builder.AddTools` (see [Add a custom feature](/docs/guides/custom-feature/)); features' tools come first. |

Register providers either way: `Options.Tools: []tools.Provider{...}` for one-offs, a feature when the tool comes with a prompt section or a hook.

### Describing the arguments

`InferTool` reflects the schema from the `json` and `jsonschema` struct tags of the input type. Use `jsonschema_description:"..."` on each field: the description is what tells the model what to pass. A field without `omitempty` is required. The model's tool description (the second argument) should say what the tool does and when to prefer it.

### The run context

`tools.RunContext` identifies the run the tool set is built for:

| Field | Meaning |
|---|---|
| `Login` | the user the run belongs to: the id you passed to `Send` |
| `ThreadID` | the conversation |
| `RunID` | this run |
| `Sandbox` | the run's sandbox mode (`""`, `"auto"`, `"on"` or `"off"`; `tools.SandboxOff` is `"off"`) |

Close over `rc.Login` and pass it to your data layer. Do not accept a user id as a tool argument: the model, or text injected into a document it read, would control it.

## Return errors the model can act on

`tools.Errorf(format, ...)` builds a `*tools.UserError`. The agent gives its message to the model as the tool result, prefixed with `Error: `, and the run continues, so the model can correct the call. In the example, an empty `orderId` returns `Error: orderId is required, for example A-1042`.

Any other error returned from the function is also reported to the model as `Error: ` plus the error text, but that text then includes the wrapping the tool builder adds (`[LocalFunc] failed to invoke tool, toolName=..., err=...`). Use `tools.Errorf` for what the model should read, and keep internal detail (SQL errors, hostnames) out of it.

- Return the same answer for "missing" and "not yours", as the example does, so the tool is no oracle for another tenant's data.
- A panic in a tool is recovered and reported as an error (`the tool crashed: ...`); do not rely on it.
- A tool that must ask the user returns a `*tools.Interrupt` (the built-in `ask_user` does); most tools never need to.

<div class="callout warning">

**Tool results are data, not instructions.** A document or web page a tool returns can contain text that tries to steer the model. Keep tools from returning secrets or other users' content in the first place; do not rely on the model to ignore it.

</div>

## Risky tools and approvals

A run started with `Interactive: true` pauses before a risky tool call and ends with an `interrupt` outcome; the user approves or denies through `Resume` (see [Approvals and permissions](/docs/concepts/approvals-permissions/)). Which of your tools count as risky:

1. Register them by name, once at start-up and before any run:

```go
import "github.com/7ar-chat/harness/engine/approvals"

approvals.RegisterRisky("refund_order") // names are compared with case, "-" and "_" ignored
```

2. Or name them so that they start with `delete`, `remove`, `move` or `rename`: any tool whose normalized name starts with one of those is risky.

A `refund_order` tool run both ways with the scripted model (observed). Without `Interactive` the call ran and returned its result; with `Interactive: true` the run ended with outcome `interrupt` and the tool did not execute:

```text
interactive=false result: Error: orderId is required
interactive=false outcome: success
interactive=true outcome: interrupt
```

(The model called `refund_order` with `{}`, which is why the non-interactive result is the `tools.Errorf` message.)

<div class="callout note">

**`APPROVAL_RISKY_TOOL_PATTERNS` does not apply to your own tools.** The pattern (default `create|send|update|delete|post|publish|write`) is matched against the words of MCP tool names (`{server}__{tool}`) only. A host tool named `send_email` is not risky unless you register it with `approvals.RegisterRisky` or its name starts with `delete`, `remove`, `move` or `rename`. Register every tool that acts on the world.

</div>

## Read-only tools run in parallel

A tool may implement `ReadOnly() bool` (the `tools.ReadOnly` interface). When it returns true, the tool has no side effects and may run in parallel with the other read-only calls of a step. Plan mode also allows only read-only tools. Declare it on lookup and search tools; leave it off for anything that writes.

## Output limits

What the model sees of a tool result is bounded, whatever the tool returned:

| Setting | Default | Effect |
|---|---|---|
| `AGENT_TOOL_OUTPUT_SPILL_CHARS` | 20000 | a longer output is written in full to `/workspace/.work/tool-output/` in the thread's running sandbox; the model gets the head, the tail and the path, and pages the file with `read_file` |
| `AGENT_MAX_TOOL_OUTPUT_CHARS` | 30000 | hard cap on what is kept; the spill threshold is never above it (a configuration with the spill above the cap fails at start) |

Spilling only uses a sandbox that is already running; it never creates one. For a run without one (`Sandbox: off`, or a thread with no sandbox yet) a long output is cut instead, with a note that the full text was not kept. A tool that returned 50000 characters in a run without a sandbox reached the model as 19996 characters (observed), with this note in the middle:

```text
[… 30103 of 50000 characters not shown; the full output could not be kept (no running sandbox) …]
```

Return less in the first place: paginate, accept a `limit` argument, summarize. A JSON object result stays valid JSON after cutting.

## Next steps

- Group the tool with a prompt section or hook in a [custom feature](/docs/guides/custom-feature/).
- Test the tool with a scripted model and a fake sandbox: [Test your agent](/docs/guides/testing/).
- Tools of MCP servers need no code: [Connect MCP servers](/docs/guides/connect-mcp/).
