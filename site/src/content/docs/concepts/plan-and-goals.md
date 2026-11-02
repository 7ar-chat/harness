---
title: "Plan mode and goals"
description: "Plan mode makes a run read-only so the agent investigates and proposes a plan; goals keep a thread working, run after run, until a judge says the goal is met."
---

Two features steer how much the agent does on its own. **Plan mode** (`RunOptions.Mode`) removes every tool that could change something, so a run can only investigate and propose. **Goals** let a thread carry an objective that is checked after each run and, while unmet, continues with another run. Both are part of the `work` and `coding` [profiles](/docs/concepts/features-profiles/); the `minimal` profile has the plan tools only.

## Plan mode is enforced by the tool set

A run has a mode, `build` (the default) or `plan`. In a plan run the engine drops every tool that is not read-only before the model is called, so the restriction is the tool set, not a sentence in the prompt. A plan run keeps:

- tools that declare themselves read-only (`tools.ReadOnly`): file reads, searches, listings, `view_file`, `job_output`, `load_skill`, the `explore` sub-agent and MCP tools annotated `readOnlyHint`;
- `update_plan` and `ask_user`, which only touch the run's own plan and questions.

Command execution, file writes, memory writes and every tool that does not say it is read-only are not offered. The run's prompt gets an extra section asking for an investigation and a concrete plan the user can approve.

This program sends the same message in both modes and prints the tools the model was offered. It runs offline with the scripted `llmtest` model:

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
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/agui"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	cfg, err := config.Load()
	check(err)

	// The scripted model records the tool names each request offers.
	var offered []string
	model := llmtest.NewFunc(func(c llmtest.Call) llmtest.Turn {
		if len(c.Tools) > 0 {
			offered = offered[:0]
			for _, t := range c.Tools {
				offered = append(offered, t.Name)
			}
			sort.Strings(offered)
		}
		return llmtest.Turn{Chunks: llmtest.Text("Here is the plan.")}
	})

	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}, Backend: sandboxtest.NewBackend()})
	check(err)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = h.Run(ctx) }()

	th, err := h.NewThread(ctx, "alice", "")
	check(err)
	for _, mode := range []harness.RunMode{harness.ModeBuild, harness.ModePlan} {
		run, err := h.Send(ctx, "alice", th.ID, "what would you change?", harness.RunOptions{Model: "m", Mode: mode})
		check(err)
		for ev := range h.Events(ctx, run.ID, 0) {
			if agui.IsTerminal(ev.Type) {
				break
			}
		}
		fmt.Printf("%s run offers: %s\n", mode, strings.Join(offered, " "))
	}
}

func check(err error) {
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
```

Output (the default work profile):

```text
build run offers: ask_user edit_file execute_command job_kill job_output list_files load_skill memory_capabilities memory_create memory_create_batch memory_delete memory_delete_batch memory_get memory_search memory_update read_file search_files update_plan view_file write_file
plan run offers: ask_user job_output list_files load_skill read_file search_files update_plan view_file
```

<div class="callout note">

**Custom tools are dropped too.** A tool you add with `Options.Tools` stays out of plan runs unless it implements `ReadOnly() bool` and returns true. Mark pure lookups read-only; leave anything with a side effect unmarked. See [Tools](/docs/concepts/tools/).

</div>

### Choosing the mode

| Where | How |
|---|---|
| Library | `harness.RunOptions{Mode: harness.ModePlan}` on `Send`, or `ResumeRequest.Mode` on `Resume` |
| HTTP | `"mode": "build"` or `"plan"` in `POST /api/threads/{id}/runs` (and in `.../resume`); see the [HTTP API](/docs/reference/http-api/) |
| Terminal UI | `/plan` and `/build`; `tab` toggles |

An empty mode keeps the mode of the thread's previous run. The mode is stored on the run record, so a worker process honours it, and resumed runs, goal runs and compactions inherit it. Any other value fails with an `ErrInvalidRequest` error: `mode must be build or plan, not "..."`.

## The plan tools

`update_plan` and `ask_user` come from the `plan` feature.

| Tool | What it does |
|---|---|
| `update_plan` | Replaces the thread's todo list. Items have `id`, `title` and `status` (`pending`, `in_progress`, `completed`); at most 50 items, titles up to 300 characters, ids unique. The new list is saved with the thread and sent as a state snapshot (`{"plan": [...]}`) so a UI can show it. The result says how many are completed and warns when more or fewer than one item is in progress. |
| `ask_user` | Pauses the run with a question for the user (up to 8 options, optional free text). The answer arrives with a `Resume` and becomes the tool's result. |

`update_plan` is also a stop guard: if the agent tries to finish with unfinished items in a plan it wrote or updated *in that run*, it is reminded once or twice to continue or to mark the items done or removed. A plan left by an earlier run never blocks an answer.

## Goals

A goal is a sentence stored on the thread. While it is active:

1. The goal text is part of every run's system prompt, and a counter ("Goal: 2 of 8 automatic goal turns used.") is added after the history, so the system prompt stays cacheable.
2. When a run completes, the `goals` hook asks the run's model to judge it. The judge sees the goal, the agent's last answer, the tools used in the latest turn, the files produced and, when the [verify](/docs/concepts/verify-and-explore/) feature is installed, the checker result and start of the text of each deliverable since the goal was set.
3. The judge answers with JSON `{"met": bool, "reason": "...", "blockers": [...]}`. An unusable answer is retried once; the whole evaluation is bounded by 20 seconds.
4. A met goal ends. An unmet goal with turns left gets a new run whose user message is `Continue working toward the goal. Last check: <reason>` plus a `Still missing:` list of blockers. The run is queued like any other.
5. The hook emits a `work.goal` custom event just before the run's terminal event, carrying the goal, its status, `turnsUsed`, `maxTurns`, the last verdict and, when one was started, `nextRunId`.

| Status | Meaning |
|---|---|
| `active` | still being worked on |
| `met` | the judge said so |
| `exhausted` | the turns ran out before the judge was satisfied |
| `cleared` | removed by the user |

Each goal run claims one of the goal's turns (`MaxTurns`, default 8, at most 20). Runs that failed, were cancelled, only compacted or are waiting for an `ask_user` answer are not judged. If the judge itself fails twice in a row (no model, timeout, invalid JSON), automatic continuation stops with the reason `evaluation failed`.

<div class="callout note">

**A failing checker overrides the judge.** If a deliverable's checker reports a failure, the goal cannot be met in that evaluation: the failure is added to the blockers and `met` is forced to false.

</div>

The goal judge uses the model of the run. A separate `agents.goal_judge` role is not available yet; the config loader rejects it with `agents.goal_judge is not available yet: that role uses the model of the run`.

### Setting a goal from Go

Goals are stored through the `store.HarnessStore` methods `PutGoal`, `GetGoal` and `ClearGoal`; the first run is created with `goals.NewRun`. This fragment continues the program above (with a scripted model that also answers the judge and title calls) and prints the final goal state:

```go
g, err := h.Store.PutGoal(ctx, "alice", th.ID, "Explain the plan", 3)
check(err)
run, _, err := h.Store.CreateRun(ctx, goals.NewRun(g, "alice", "m", "", "", goals.StartMessage(g.Text)))
check(err)
check(h.Queue.Enqueue(ctx, run.ID))
// ... read h.Events(ctx, run.ID, 0) until the terminal event, then:
g, _ = h.Store.GetGoal(ctx, "alice", th.ID)
fmt.Printf("goal: status=%s turns=%d/%d verdict=%q\n", g.Status, g.TurnsUsed, g.MaxTurns, g.LastVerdict.Reason)
```

```text
goal: status=met turns=1/3 verdict="The answer states the plan."
```

The scripted model returns `{"met": true, "reason": "The answer states the plan.", "blockers": []}` when its first message equals `goals.JudgeInstructions`. `PutGoal` replaces an earlier goal with a new active one. If a run is active when the goal is replaced, the new goal's first run starts when that run ends.

<div class="callout tip">

**Combine the two.** Run a goal in plan mode first to get a reviewed plan, then switch the thread to build mode: goal runs inherit the mode of the thread's latest run.

</div>

Related pages: [Threads, runs and events](/docs/concepts/threads-runs-events/), [Approvals and permissions](/docs/concepts/approvals-permissions/), [Event types](/docs/reference/events/).

<div class="callout note">

**No HTTP route or UI command sets a goal in v0.3.1.** The goal machinery (store methods, prompt sections, judge, continuation) is complete and installed by the `Goals()` feature, but `hosts/httpapi`, `hosts/client` and the terminal UI expose no goal endpoint or command. Set goals from Go as shown above, or add a route in your own host.

</div>
