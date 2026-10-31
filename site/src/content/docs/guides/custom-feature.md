---
title: Add a custom feature
description: "Package tools, prompt sections and hooks that belong together as a Feature, see what the Builder offers, and share services between features."
---

A feature is the unit the harness uses to add a capability: a set of tools, prompt sections, hooks and background jobs that belong together (memory, skills, permissions and goals are all features). Write your own when a tool needs a prompt section, a hook around tool calls or a service shared with another feature. For a single tool, `Options.Tools` is enough ([Add a custom tool](/docs/guides/custom-tool/)). The built-in set and profiles are described in [Features and profiles](/docs/concepts/features-profiles/).

## A complete example: an audit feature

The feature below records who started a run, which tools it called and how it ended, adds one sentence to the system prompt, and publishes its log as a service that a second feature picks up. It runs offline with the scripted model.

```go
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/store"
	"github.com/7ar-chat/harness/engine/agent"
	"github.com/7ar-chat/harness/engine/runtime"
	"github.com/7ar-chat/harness/engine/tools"
)

// Log is the service the audit feature publishes for features installed after it.
type Log struct {
	mu      sync.Mutex
	entries []string
}

func (l *Log) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, fmt.Sprintf(format, args...))
}

func (l *Log) Entries() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

// runStart implements agent.RunStartHook.
type runStart struct{ log *Log }

func (h runStart) BeforeRun(_ context.Context, run *store.Run) {
	h.log.add("run started for %s", run.UserLogin)
}

// toolHook implements agent.ToolHook.
type toolHook struct{ log *Log }

func (h toolHook) AfterTool(_ context.Context, run *store.Run, _, toolName string) map[string]any {
	h.log.add("%s called %s", run.UserLogin, toolName)
	return nil
}

// endOfRun implements runtime.Hook.
type endOfRun struct{ log *Log }

func (h endOfRun) Finishing(_ context.Context, f *runtime.Finish) func(context.Context) {
	h.log.add("run finished: %s", f.Status)
	return nil
}

// Audit builds the feature: a Feature made from a function.
func Audit() harness.Feature {
	return harness.FeatureFunc("audit", func(b *harness.Builder) error {
		if err := b.Require(harness.FeatureSandbox); err != nil { // built-in features it builds on
			return err
		}
		log := &Log{}
		if err := harness.Provide(b, log); err != nil { // one service per type
			return err
		}
		b.Section(func(_ context.Context, in agent.Input) string {
			return "Every tool call of this conversation is recorded for audit."
		})
		b.RunStart(runStart{log})
		b.ToolHook(toolHook{log})
		b.EndOfRun(endOfRun{log})
		return nil
	})
}

// Report is the interface variant: a type with Name and Install.
// It is installed after Audit and finds its Log.
type Report struct{ Log *Log }

func (Report) Name() string { return "audit-report" }

func (r *Report) Install(b *harness.Builder) error {
	if err := b.Require("audit"); err != nil {
		return err
	}
	log, ok := harness.Lookup[*Log](b)
	if !ok {
		return fmt.Errorf("no audit log published")
	}
	r.Log = log
	return nil
}

type pingArgs struct {
	Host string `json:"host" jsonschema_description:"host to ping"`
}

type pingTools struct{}

func (pingTools) Name() string { return "ping" }
func (pingTools) Tools(context.Context, tools.RunContext) ([]ai.BaseTool, func(), error) {
	t, err := ai.InferTool("ping", "Ping a host.", func(_ context.Context, in pingArgs) (string, error) {
		return in.Host + " is up", nil
	})
	return []ai.BaseTool{t}, nil, err
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	check(err)

	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if c.Messages[len(c.Messages)-1].Role == ai.RoleTool {
			return llmtest.Turn{Chunks: llmtest.Text("done")}
		}
		return llmtest.Turn{Chunks: []*ai.Message{
			llmtest.Finish(llmtest.ToolCall(0, "c1", "ping", `{"host":"example.com"}`), "tool_calls")}}
	})

	// Report must come after Audit; swapping them fails harness.New.
	_, err = harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model},
		Features: append(harness.ProfileMinimal(), &Report{}, Audit())})
	fmt.Println("wrong order:", err)

	report := &Report{}
	h, err := harness.New(harness.Options{
		Config: cfg, Models: llmtest.Factory{M: model},
		Features: append(harness.ProfileMinimal(), Audit(), report),
		Tools:    []tools.Provider{pingTools{}},
	})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	th, _ := h.NewThread(ctx, "alice", "")
	run, err := h.Send(ctx, "alice", th.ID, "ping example.com",
		harness.RunOptions{Model: "m", Sandbox: harness.SandboxOff})
	check(err)
	for ev := range h.Events(ctx, run.ID, 0) {
		if agui.IsTerminal(ev.Type) {
			break
		}
	}
	fmt.Println(strings.Join(report.Log.Entries(), "\n"))
	system := model.Calls()[0].Messages[0].Content
	fmt.Println("prompt has the audit section:", strings.Contains(system, "recorded for audit"))
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

Output (observed):

```text
wrong order: harness: feature "audit-report": feature "audit-report" requires feature "audit" (install it earlier in Options.Features)
run started for alice
alice called ping
run finished: completed
prompt has the audit section: true
```

## The Feature interface

```go
type Feature interface {
	Name() string            // unique in a harness
	Install(*harness.Builder) error
}
```

`harness.FeatureFunc(name, fn)` adapts a function; implement the interface on a type (like `Report` above) when the feature carries state or needs a constructor with arguments. `Install` runs once, in the order of `Options.Features`, while `harness.New` builds the harness. Returning an error aborts `harness.New` and names the feature. A feature without a name, or two features with the same name, also fail `New`.

Compose the list from a profile and your features: `append(harness.ProfileWork(), myFeature)`. `ProfileMinimal` is the sandbox tools and the plan tools only; `ProfileWork` is the full set; `ProfileCoding` adds the explore sub-agent, code checks and the permission layer. Put a feature after the built-ins it uses.

## What the Builder offers

The `Builder` gives a feature the environment to read and the places to contribute.

### Read the environment

| Field | Meaning |
|---|---|
| `b.Config` | the environment-driven configuration |
| `b.Store` | the persistence (`harness.StoreAs[T](b)` returns it as an interface your feature needs, or an error naming your feature) |
| `b.Models`, `b.Sandboxes`, `b.Blobs`, `b.Infra`, `b.Options` | models, sandbox manager (`Sandboxes.Enabled()` reports a configured backend), artifact blobs, shared infrastructure, the host's options |
| `b.Artifacts`, `b.Workspace`, `b.Skills`, `b.Memory`, `b.Verifier`... | services of the built-in features; `nil` until their feature is installed, so install it earlier |

### Contribute

| Method | Contributes |
|---|---|
| `AddTools(providers...)` | tool providers, before `Options.Tools` |
| `WrapTools(func(tools.Provider) tools.Provider)` | wraps the whole tool registry (filters, auditing); the first wrapper is innermost |
| `Section(s)` | a static system-prompt section (`func(ctx, agent.Input) string`; `""` adds nothing) |
| `TailSection(s)`, `ContextSection(s)` | text in the reminder at the end of each request (keeps the system prompt a cacheable prefix), or in the final context block |
| `RunStart(h)` | a hook run before the first step of every run (not compactions); it must not fail the run |
| `ToolHook(h)` | an observer of every tool result; it may return metadata stored on the tool message |
| `BeforeTool(h)` | a gate before a tool call executes (the interactive approvals use this) |
| `EndOfRun(h)` | a `runtime.Hook` run at the end of every run, with the run's status and usage |
| `Job(j)`, `Background(f)` | a periodic maintenance job, a goroutine started once `harness.New` succeeds |
| `StepNote(n)` | a note source asked before every model call |
| `OnSandboxCreate(f)`, `PrepareSandbox(f)`, `SandboxTools(f)` | sandbox lifecycle hooks and options of the sandbox tools |
| `Configure(f)` | an escape hatch: change the agent's configuration after every feature contributed |
| `After(f)` | run `f` once every feature is installed |

`SetAnswers`, `SetChecker`, `SetImages` and `SetPurger` each take one implementation per harness and return an error if another feature already set it.

### Order, requirements and shared services

- `b.Has(name)` reports whether a feature is installed so far. `b.Require(names...)` fails `New` unless the named features are installed earlier; the error names both: `feature "audit-report" requires feature "audit" (install it earlier in Options.Features)`.
- `b.RequireSandbox()` fails unless a backend is configured. Skip it, and test `b.Sandboxes.Enabled()`, if your feature should simply do less without one.
- `harness.Provide(b, v)` publishes a service by its Go type for features installed after it; only one service per type. `harness.Lookup[T](b)` returns it. Wrap the type in your own named type when two features need two values of the same underlying type.
- The built-in feature names are the `harness.Feature*` constants (`FeatureSandbox`, `FeatureMemory`, `FeaturePermissions`...).

<div class="callout note">

**Hooks run where the run runs.** In a web + workers deployment the agent, and so your tool hooks, run hooks and end-of-run hooks, run on the worker. Compile the same features into both tiers (see [Choose a deployment shape](/docs/guides/deployment-shapes/)). The end-of-run hook is also the right place for server-side metering.

</div>

<div class="callout tip">

**Keep hooks cheap and safe.** `RunStart` and `ToolHook` run on the run's path; do slow or fallible work in `Background`/`Job`, and log your own errors, since a run-start hook cannot fail the run.

</div>

## Next steps

- Prove the feature in a test with a scripted model: [Test your agent](/docs/guides/testing/).
- A feature that needs approvals or per-user data: [Approvals and permissions](/docs/concepts/approvals-permissions/), [Memory](/docs/concepts/memory/).
- The facade API: [The Go facade](/docs/reference/harness-api/).
