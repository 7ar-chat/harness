---
title: Sandboxes
description: "Where the agent's commands and files live, how one sandbox per thread is created, kept alive and replaced, and how to choose or disable a backend."
---

A sandbox is the place the agent's commands run and its files live: a container elsewhere, or a directory on your machine. The `core/sandbox` package defines the contract; you pick a backend by name, and the harness gives every conversation thread its own sandbox, created on the first tool call that needs one.

The smallest way to see the per-run switch is an in-process harness with the in-memory fake backend and a scripted model. It runs offline:

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func main() {
	os.Setenv("LLM_PROVIDER", "openai")
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("SANDBOX_BACKEND", "none")
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	model := llmtest.Factory{M: llmtest.NewFunc(func(llmtest.Call) llmtest.Turn {
		return llmtest.Turn{Chunks: llmtest.Text("ok")}
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// No backend: HasSandbox is false and a run that demands a sandbox is refused.
	bare, err := harness.New(harness.Options{Config: cfg, Models: model})
	if err != nil {
		panic(err)
	}
	th, _ := bare.NewThread(ctx, "alice", "")
	_, err = bare.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOn})
	fmt.Println(bare.HasSandbox(), errors.Is(err, harness.ErrNoSandbox))
	fmt.Println(err)

	// The in-memory fake backend of core/sandbox/sandboxtest.
	h, err := harness.New(harness.Options{Config: cfg, Models: model, Backend: sandboxtest.NewBackend()})
	if err != nil {
		panic(err)
	}
	th, _ = h.NewThread(ctx, "alice", "")
	_, err = h.Send(ctx, "alice", th.ID, "hi", harness.RunOptions{Model: "m", Sandbox: harness.SandboxOn})
	fmt.Println(h.HasSandbox(), err)
}
```

Output:

```text
false true
invalid request: this run requires a sandbox but the harness has none: set Options.Backend or SANDBOX_BACKEND
true <nil>
```

## The contract

Everything is in [`core/sandbox`](https://github.com/7ar-chat/harness/tree/main/core/sandbox). Three layers:

| Type | Role |
|---|---|
| `Backend` | Talks to the provider: `Create(ctx, CreateOptions) (Instance, error)`, `Connect(ctx, id)` and `Kill(ctx, id)`. `Connect` returns `ErrNoSandbox` only when the sandbox definitively no longer exists (404, Terminated, Failed); every other error is treated as transient. |
| `Instance` | A live sandbox with lifecycle calls on top of `Sandbox`: `Healthy`, `Gone` (definitive end of life) and `Renew(ctx, d)`. |
| `Sandbox` | What tools and the files API use: `ID`, `Exec(ExecRequest)`, `ReadFile`, `WriteFile`, `MkdirAll`, `List`, `Stat`. A non-zero exit code is a result (`ExecResult.ExitCode`), not an error. |
| `Manager` | The lifecycle for a conversation: `Acquire(ctx, threadID, login)` creates when needed, `Get` never creates, `Kill`, `Invalidate`, `Enabled`. `sandbox.NewManager` builds the default one; `harness.New` does it for you and exposes it as `Harness.Sandboxes`. |

The agent sees a fixed layout: `/workspace` (the working tree), `/workspace/outputs` (final deliverables, saved as [artifacts](/docs/concepts/artifacts-checkpoints/)), `/workspace/.work` (scratch, never saved) and `/skills/<name>/` for installed [skills](/docs/concepts/skills/).

To write your own backend and prove it against the conformance suite, see [Write a sandbox backend](/docs/guides/sandbox-backend/). Backend settings per provider are on [Sandbox backends](/docs/integrations/sandbox-backends/).

## Choose a backend

`SANDBOX_BACKEND` selects the backend by name (case-insensitive). When it is empty, the harness uses `opensandbox` if `OPENSANDBOX_URL` is set and `none` otherwise.

| Name | What it does |
|---|---|
| `none` | No sandbox. The agent has no sandbox tools and a prompt that says so. |
| `local` | Commands run on this machine with bash, inside `LOCAL_WORKSPACE` (default: the current directory), seen by the agent as `/workspace`. Unix only. |
| `opensandbox` | One container per thread on an [OpenSandbox](https://github.com/alibaba/OpenSandbox) server (`OPENSANDBOX_URL`, `OPENSANDBOX_IMAGE`, `SANDBOX_CPU`, `SANDBOX_MEMORY`). |

<div class="callout breaking">

**`opensandbox` is opt-in since v0.3.** The facade no longer links the OpenSandbox SDK. Import it for its side effect in your `main`, otherwise `SANDBOX_BACKEND=opensandbox` fails at start with `unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: ...; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it`. The `harness`, `harness-web` and `harness-worker` binaries already import it.

```go
import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
```

</div>

<div class="callout warning">

**The local backend is not isolation.** Commands run with your user's rights on your machine; only the file API is confined to the workspace directory. Use it for a terminal on your own machine or inside a container or VM, and use `opensandbox` (or another isolated backend) for anything multi-user or untrusted. The [permission policy](/docs/concepts/approvals-permissions/) is what protects you with `local`.

</div>

`harness-web` enforces this: with `SANDBOX_BACKEND=local` it refuses to start in team mode or on a non-loopback address, and the error reads `SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk`. Only the exact value `1` lifts the check.

### Register your own backend

A backend that is not built in is selected by name once a package registers it. `backends.Register` panics on an empty name, a nil factory or a duplicate name; names are lower-cased.

```go
import (
	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

func init() {
	backends.Register("rest", func(s backends.Settings) (sandbox.Backend, error) {
		return &myBackend{}, nil
	})
}
```

Blank-import the package in your `main` and run with `SANDBOX_BACKEND=rest`. A library host can skip registration and pass the backend directly as `harness.Options.Backend`. The factory receives `backends.Settings`, which carries the OpenSandbox and local settings parsed from the environment; anything else your backend needs it reads itself.

## Sandbox per run

`RunOptions.Sandbox` (JSON field `sandbox` on `POST .../runs`) decides whether a single run may use a sandbox:

| Value | Constant | Effect |
|---|---|---|
| `auto` (default) | `harness.SandboxAuto` | Sandbox tools and the sandbox prompt when a backend is configured; the sandbox is created on the first command, never before. |
| `off` | `harness.SandboxOff` | No sandbox tools, the no-sandbox prompt, `Backend.Create` is never called. |
| `on` | `harness.SandboxOn` | Like `auto`, but `Send` fails with `ErrInvalidRequest` wrapping `ErrNoSandbox` when there is no backend. |

The mode is stored on the run. A resumed run, a goal turn and a [compaction](/docs/concepts/compaction/) keep the mode of the thread's latest run. Features that need a sandbox (command permissions, workspace checkpoints, project docs, verification) switch themselves off for `off` runs. Decide the mode on the server; do not forward a browser-supplied value.

## Lifecycle

`DefaultManager` builds the same behaviour around every backend:

* **One sandbox per thread**, created lazily by `Acquire` and recorded on the thread row, so any replica or worker can `Connect` to it.
* **Quota.** `MAX_SANDBOXES_PER_USER` (default 5) live sandboxes per login. Over the limit the manager first reclaims sandboxes of dead threads, then fails with `ErrQuota`. Deleting a thread kills its sandbox.
* **Keep-alive.** The manager calls `Renew` with the idle timeout (`SANDBOX_TIMEOUT_SECONDS`, default 1800) at most about once a minute while the sandbox is used. `sandbox.KeepAlive(ctx, sb)` keeps one alive for work that outlives a single `Exec`, which is how background jobs survive.
* **Health.** Each acquisition checks `Healthy` (cached for 30 seconds). A sandbox unhealthy for longer than `SANDBOX_UNHEALTHY_RECREATE_SECONDS` (default 300) across at least three checks is killed and recreated.
* **Recreation.** A sandbox that is gone is replaced. Before anyone can use the new one, the `OnCreate` hook restores saved files from the thread's [artifacts and checkpoints](/docs/concepts/artifacts-checkpoints/). The model receives a notice with its next tool result, for example `Notice: the sandbox stopped or stayed unresponsive and has been recreated. 3 saved file(s) were restored into /workspace ... (other files, installed packages and running processes are gone).`

<div class="callout note">

Anything the agent wrote outside the saved set, including installed packages and running processes, does not survive a recreation. Put deliverables under `/workspace/outputs`.

</div>

### Coordinator

The manager takes creation locks, health-streak counters and one-shot markers from a `sandbox.Coordinator`. Without one, it uses `sandbox.NewLocalCoordinator()`, which is correct for a single process (a TUI, a desktop app, tests). With several processes, pass a shared coordinator through `harness.Options.Infra`: the Redis one comes from `redisinfra.Infra(rdb, store)`; see [Infrastructure](/docs/concepts/infra/) and [Redis](/docs/integrations/redis/).

## Egress policy

The OpenSandbox backend applies the network policy when it creates a sandbox:

| Variable | Default | Meaning |
|---|---|---|
| `SANDBOX_EGRESS_ALLOWLIST` | empty | Comma-separated host names (`*.example.com` allowed), IPs or CIDRs. When set, egress is deny-by-default and only these are allowed. |
| `SANDBOX_EGRESS_DENY_CIDRS` | private, link-local, CGNAT, ULA and IPv4-mapped ranges | Blocked when no allowlist is set, so a sandbox cannot reach other sandboxes, the host or the cloud metadata service. |
| `SANDBOX_EGRESS_UNRESTRICTED` | `false` | Required to accept an empty allowlist in production. |

With `HARNESS_PRODUCTION` set (any non-empty value), starting with `opensandbox` and no allowlist fails with `SANDBOX_EGRESS_ALLOWLIST is empty in production: set it, or set SANDBOX_EGRESS_UNRESTRICTED=true to accept unrestricted sandbox egress`. Invalid entries are rejected when the configuration loads. `none`, `local` and a host's own backend report egress as `disabled`: their isolation is their own. See the [production checklist](/docs/operations/production/) and the [security model](/docs/operations/security/).

## Test with the fake

`core/sandbox/sandboxtest` provides `NewBackend()`, an in-memory `sandbox.Backend` for tests, plus `HostBackend` (a real directory) and the conformance suite `RunConformance`. The fake has no shell: scripted protocol commands are answered natively and you can script `Exec` with `SetExec`. See [Test your agent](/docs/guides/testing/).
