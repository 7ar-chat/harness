---
title: Features and profiles
description: "How harness.New builds an agent from features (tools, prompt sections, hooks and jobs that belong together), what the work, coding and minimal profiles contain, and how to compose your own."
---

`harness.New` builds a small core (store, models, sandboxes, infrastructure, the [agent loop](/docs/concepts/agent-loop/), the runtime) and installs **features** on top of it. A feature is a set of tools, prompt sections, hooks and background jobs that belong together: permissions, memory, skills, goals. A **profile** is just a list of features. Pick one with `AGENT_PROFILE`, or pass `Options.Features` to compose your own. To write a feature, see [Add a custom feature](/docs/guides/custom-feature/).

## Compose a profile

`Options.Features` is a slice. Start from a profile, append, drop:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/tools"
)

type none struct{}

// support is a feature of our own: a tool, a prompt section and a dependency on another feature.
func support() harness.Feature {
	return harness.FeatureFunc("support", func(b *harness.Builder) error {
		if err := b.Require(harness.FeaturePlan); err != nil { // plan must be installed earlier in the list
			return err
		}
		fmt.Println("installing support; memory installed so far:", b.Has(harness.FeatureMemory))
		b.AddTools(ticketTools{})
		b.Section(func(context.Context, agent.Input) string { return "Support: use open_ticket for problems you cannot fix." })
		return nil
	})
}

type ticketTools struct{}

func (ticketTools) Name() string { return "tickets" }
func (ticketTools) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("open_ticket", "Open a support ticket.", func(context.Context, none) (string, error) { return "T-1", nil })
	return []ai.BaseTool{t}, nil, err
}

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
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

	// A profile is a list; extend it like any slice.
	features := append(harness.ProfileMinimal(), harness.Memory(), harness.Titles(), support())
	for _, f := range features {
		fmt.Print(f.Name(), " ")
	}
	fmt.Println()

	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend(), Features: features,
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
	fmt.Println("tools offered:", offered)

	// A missing dependency, a duplicate and an unmet Require are reported by harness.New.
	_, err = harness.New(harness.Options{Config: cfg, Features: []harness.Feature{harness.Workspace()}})
	fmt.Println(err)
	_, err = harness.New(harness.Options{Config: cfg, Features: []harness.Feature{harness.Sandbox(), harness.Sandbox()}})
	fmt.Println(err)
	_, err = harness.New(harness.Options{Config: cfg, Features: []harness.Feature{support()}})
	fmt.Println(err)
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

Output:

```text
sandbox plan memory titles support 
installing support; memory installed so far: true
tools offered: [execute_command job_output job_kill read_file view_file search_files write_file edit_file list_files update_plan ask_user memory_search memory_get memory_create memory_update memory_delete memory_create_batch memory_delete_batch memory_capabilities open_ticket]
harness: feature "workspace": feature "workspace" requires feature "artifacts" (install it earlier in Options.Features)
harness: feature "sandbox" is listed twice
harness: feature "support": feature "support" requires feature "plan" (install it earlier in Options.Features)
```

The order of `tools offered` is the order of installation: features install in list order, and that is the order of tools and prompt sections in the model's request. Keep the list stable across deployments if you rely on [prompt caching](/docs/integrations/llm-providers/).

<div class="callout note">

**`nil` is not empty.** `Options.Features == nil` means "use the profile of `AGENT_PROFILE`". A non-nil empty slice (`[]harness.Feature{}`) installs no feature at all: no tools, no sandbox, no plan.

</div>

## The profiles

`AGENT_PROFILE=work|coding|minimal` (config.yml `profile`) picks the profile when `Options.Features` is nil. Any other value fails `config.Load()` with `AGENT_PROFILE must be work, coding or minimal`. The constructors are `harness.ProfileWork()`, `ProfileCoding()`, `ProfileMinimal()` and `ProfileNamed(name)` (an unknown name gives the work profile).

| Profile | Features, in installation order |
|---|---|
| `work` (default) | `sandbox`, `codecheck`, `skills`, `mcp`, `plan`, `projectdocs`, `artifacts`, `workspace`, `titles`, `verify`, `goals`, `memory`, `toolprofile`, `permissions` |
| `coding` | `sandbox`, `codecheck`, `explore`, `skills`, `mcp`, `plan`, `projectdocs`, `artifacts`, `workspace`, `titles`, `verify`, `goals`, `memory`, `toolprofile`, `permissions` |
| `minimal` | `sandbox`, `plan` |

The work and coding lists differ in three constructor arguments and one feature: coding adds `explore`, and passes `true` to `Codecheck`, `ToolProfile` and `Permissions`. In effect:

- **work** is the full agent for documents and data: every tool, with `codecheck` only installing when `AGENT_CODE_CHECKS` is set, and `permissions` only active on the local backend.
- **coding** is lean for software work: the `explore` sub-agent, syntax checks on every written file, the permission layer on the local backend and on remote sandboxes, and a tool allow-list that leaves out `view_file`, `load_skill` (unless the user has skills) and the memory tools. See [Tools](/docs/concepts/tools/#selecting-tools-with-agent_tools).
- **minimal** is the sandbox tools and the plan tools: no artifacts, memory, skills, goals or titles.

## The built-in features

| Constructor | Name | Contributes | Needs |
|---|---|---|---|
| `Sandbox()` | `sandbox` | `execute_command`, `job_output`, `job_kill`, `read_file`, `view_file`, `search_files`, `write_file`, `edit_file`, `list_files`. Nothing without a backend. | |
| `Codecheck(coding)` | `codecheck` | A syntax check in the notice of `write_file` and `edit_file`. With `coding` false only when `AGENT_CODE_CHECKS` is set. | `sandbox` |
| `Explore()` | `explore` | The `explore` read-only sub-agent (`LLM_EXPLORE_MODEL`, else the default model). | `sandbox` |
| `Skills()` | `skills` | `load_skill`, the skills index, installation of skills into sandboxes. Publishes `b.Skills` and `b.Installer`. | |
| `MCP()` | `mcp` | Tools of the configured MCP servers. | the `mcpfeature` import |
| `Plan()` | `plan` | `update_plan`, `ask_user`. | |
| `ProjectDocs()` | `projectdocs` | `AGENTS.md`, `CLAUDE.md` and similar instructions in the prompt. | |
| `Artifacts()` | `artifacts` | Tracking of produced files, previews, retention purge. Publishes `b.Artifacts`. | |
| `Workspace()` | `workspace` | Git checkpoints of `/workspace` before each turn. Publishes `b.Workspace`. | `artifacts` |
| `Titles()` | `titles` | Names new threads from their first exchange. | |
| `Verify()` | `verify` | Checks deliverables before the agent may finish. Publishes `b.Verifier`. | `artifacts` |
| `Goals()` | `goals` | A thread goal judged after each run. Uses `artifacts` and `verify` when present. | |
| `Memory()` | `memory` | The memory tools, prompt and recall, and a consolidation job. Publishes `b.Memory`. | |
| `ToolProfile(coding)` | `toolprofile` | The `AGENT_TOOLS` and coding allow-list filter. Works on `skills` and `memory` whatever the order. | |
| `Permissions(coding)` | `permissions` | The allow/ask/deny permission layer in front of the tools. | |

The `Feature*` constants (`harness.FeatureSandbox`, `FeaturePlan`, ...) are these names, for `Builder.Has` and `Builder.Require`.

<div class="callout warning">

**MCP is opt-in since v0.3.** `MCP()` is part of the `work` and `coding` profiles, but the MCP implementation is linked only by a blank import: `import _ "github.com/7ar-chat/harness/mcpfeature"`. Without it, `harness.New` fails when MCP servers are configured, with `harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"`; with none configured it installs nothing. See [MCP](/docs/concepts/mcp/).

</div>

## Writing a feature

```go
type Feature interface {
	Name() string
	Install(*harness.Builder) error
}
```

`harness.FeatureFunc(name, install)` turns a function into one. `Install` is called once, in list order, while `New` builds the harness. It reads the environment from the builder (`b.Config`, `b.Store`, `b.Models`, `b.Sandboxes`, `b.Blobs`, `b.Infra`, `b.Options`) and contributes through its methods:

| Builder method | Contributes |
|---|---|
| `AddTools(providers...)`, `WrapTools(f)` | Tool providers (before `Options.Tools`); a wrapper over the whole registry (filters, auditing). |
| `Section(f)`, `ContextSection(f)`, `TailSection(f)` | A static system-prompt section; a dynamic section in the final context block; a per-run reminder at the end of each request. |
| `BeforeTool(h)`, `ToolHook(h)`, `RunStart(h)`, `StepNote(f)` | Gate tool calls; observe tool results; run before a run's first step; add a note to the next model call when news appears mid-run (not persisted). |
| `SetAnswers`, `SetImages`, `SetChecker`, `Configure(f)` | Final-answer review, image attachment, deliverable checker (one feature each, a second call is an error); a free-form mutator of `agent.Harness`. |
| `EndOfRun(h)`, `Job(j)`, `SetPurger(p)`, `Background(f)` | An end-of-run hook; a periodic job (run by one runtime at a time); the retention job; a goroutine started once `New` succeeded. |
| `SandboxTools(f)`, `PrepareSandbox(f)`, `OnSandboxCreate(f)` | Chain onto the sandbox tools' options; run when a sandbox is created. |
| `RequireSandbox()`, `Require(names...)`, `StoreAs[T](b)` | Fail with a clear error when no backend is configured, a feature is missing, or the store lacks an extension. |
| `Provide[T](b, v)`, `Lookup[T](b)`, `b.Has(name)` | Publish a service for later features (one per type); look one up; ask if a feature is installed. |
| `After(f)` | Run once every feature is installed. |

The errors are specific: a name listed twice is `harness: feature "sandbox" is listed twice`; a feature with an empty name is `harness: a feature has no name`; `Require` says `feature "x" requires feature "y" (install it earlier in Options.Features)`; `RequireSandbox` says `feature "x" needs a sandbox: set Options.Backend, SANDBOX_BACKEND or OPENSANDBOX_URL`; `StoreAs` says `feature "x" needs a store implementing <type>`. An `Install` error aborts `New`, wrapped as `harness: feature "x": ...`.

<div class="callout tip">

**Order is the dependency mechanism.** A feature sees only what earlier features have published: `b.Artifacts`, `b.Workspace`, `b.Skills`, `b.Installer`, `b.Memory` and `b.Verifier` are nil until their feature is installed. Put yours after the ones it builds on, and use `b.After` for anything that needs the final set.

</div>

The wiring of the built-ins is in [`features_core.go`](https://github.com/7ar-chat/harness/blob/main/features_core.go) and [`features_state.go`](https://github.com/7ar-chat/harness/blob/main/features_state.go); they use the same `Builder` as your feature does.

## Related

- [Add a custom feature](/docs/guides/custom-feature/): a feature with tools, a prompt section and a hook, end to end.
- [Tools](/docs/concepts/tools/): `AGENT_TOOLS` and the built-in tool names.
- [The Go facade](/docs/reference/harness-api/): every exported function of package `harness`.
