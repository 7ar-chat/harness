---
title: Sandbox backends
description: "Choose where the agent's commands run: the local machine, an OpenSandbox container per thread, none, or a backend you register."
---

A sandbox backend decides where the agent's `execute_command` and file tools run. The harness ships three, selected by `SANDBOX_BACKEND`: `local`, `opensandbox` and `none`. A host can register more by name. For the idea of a sandbox per thread, see [Sandboxes](/docs/concepts/sandboxes/); to write a new backend, see [Write a sandbox backend](/docs/guides/sandbox-backend/).

| Backend | Where commands run | Isolation | Use it for |
|---|---|---|---|
| `local` | on this machine, in a directory shown to the agent as `/workspace` | none beyond path confinement of the file API | your own terminal, or inside a container or VM |
| `opensandbox` | one container per thread on an [OpenSandbox](https://github.com/alibaba/OpenSandbox) server | resource limits, egress policy, optional credential proxy | servers and teams |
| `none` | nowhere | not applicable | agents with host-defined tools only |
| your own | wherever your adapter runs | yours | E2B, Firecracker, Kubernetes jobs, ... |

## How the backend is selected

`SANDBOX_BACKEND` (or `sandbox.backend` in `config.yml`) names the backend, case-insensitively. When it is empty, the backend is `opensandbox` if `OPENSANDBOX_URL` is set and `none` otherwise. In `config.yml`, `sandbox.backend` accepts only `local`, `opensandbox` or `none`; the environment variable also takes a name you registered. `none` also clears `OPENSANDBOX_URL`.

```bash
SANDBOX_BACKEND=local LOCAL_WORKSPACE=$PWD harness
```

In Go, `Options.Backend` wins over everything: pass a `sandbox.Backend` and no registry lookup happens. A run can also opt out of the sandbox per request; see [Sandboxes](/docs/concepts/sandboxes/#sandbox-per-run).

## Local

The `local` backend runs commands with `bash` on the host, with the rights of the user who runs the process. The agent sees `/workspace`, which maps to a host directory. It exists only on Unix; elsewhere `SANDBOX_BACKEND=local` fails with `SANDBOX_BACKEND=local is not supported on this platform`. macOS needs no GNU coreutils: the protocol scripts are answered natively.

| Variable | Default | Meaning |
|---|---|---|
| `LOCAL_WORKSPACE` | the current directory | Host directory shown as `/workspace` and shared by every sandbox. Created if missing. |
| `LOCAL_STATE_DIR` | see below | Where the backend keeps what is not your project: `/skills`, job records, and (without `LOCAL_WORKSPACE`) one workspace per sandbox. |

The backend's own default for the state directory is `<user cache dir>/harness`. The same variable also locates the durable session store (`harness.OpenLocalStore`): when `LOCAL_STATE_DIR` is empty the CLI and `harness-web -local` use `~/.har/sessions` for it. See [Stores](/docs/integrations/stores/#filestore).

<div class="callout warning">

**Local is not isolation.** Commands run as you. The file API refuses `..` and symlink escapes, but a shell command can read and write anything you can. Use it on your own machine with the permission modes of the coding profile (`AGENT_PERMISSIONS`), or inside a container or VM. `harness-web` refuses it in team mode or on a non-loopback address unless `HARNESS_ALLOW_LOCAL_SANDBOX=1`: `SANDBOX_BACKEND=local runs the agent's commands on this machine without isolation: not allowed in team mode or on a non-loopback address; use SANDBOX_BACKEND=opensandbox, or set HARNESS_ALLOW_LOCAL_SANDBOX=1 to accept the risk`.

</div>

Library use:

```go
package main

import (
	"log"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
)

func main() {
	dir, _ := os.MkdirTemp("", "workspace")
	os.Setenv("SANDBOX_BACKEND", "local")
	os.Setenv("LOCAL_WORKSPACE", dir)
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("ok")}) // scripted: no network
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}})
	if err != nil {
		log.Fatal(err)
	}
	_ = h
}
```

## None

With no backend the agent has no sandbox tools and gets a prompt that says so. Host-defined tools (`Options.Tools`) and the non-sandbox features still work. This is the default when neither `SANDBOX_BACKEND` nor `OPENSANDBOX_URL` is set. See [Add a custom tool](/docs/guides/custom-tool/).

## OpenSandbox

`opensandbox` creates one container per thread on an OpenSandbox lifecycle server, lazily, at the first command that needs it. The harness is the client; you run the server.

<div class="callout breaking">

**v0.3.x: OpenSandbox is opt-in.** Package `harness` no longer links the OpenSandbox SDK. A program that selects `SANDBOX_BACKEND=opensandbox` (or sets only `OPENSANDBOX_URL`) must blank-import the adapter. Without it, `harness.New` fails with:

`unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: local, none; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it`

The stock `harness`, `harness-web` and `harness-worker` binaries already import it.

</div>

```go
import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" // registers SANDBOX_BACKEND=opensandbox
```

With that import in place, the same program builds the harness from the environment:

```bash
export SANDBOX_BACKEND=opensandbox
export OPENSANDBOX_URL=http://127.0.0.1:18090
export OPENSANDBOX_IMAGE=harness-sandbox:local
export SANDBOX_EGRESS_ALLOWLIST=pypi.org,files.pythonhosted.org
```

Or skip the registry and hand the harness a backend:

```go
backend, err := opensandbox.NewOpenSandbox(opensandbox.OpenSandboxOptions{
	URL:             "http://127.0.0.1:18090",
	APIKey:          os.Getenv("OPENSANDBOX_API_KEY"),
	Image:           "harness-sandbox:local",
	CPU:             "1",
	Memory:          "2Gi",
	TimeoutSeconds:  1800,
	EgressAllowlist: []string{"pypi.org", "files.pythonhosted.org"},
})
if err != nil {
	log.Fatal(err)
}
h, err := harness.New(harness.Options{Config: cfg, Backend: backend})
```

### Settings

| Variable | Default | Meaning |
|---|---|---|
| `OPENSANDBOX_URL` | none | Server address. Required; an empty URL fails with `SANDBOX_BACKEND=opensandbox needs OPENSANDBOX_URL`. |
| `OPENSANDBOX_API_KEY` | none | Server API key (`sandbox.api_key_env` in `config.yml` names the variable that holds it). |
| `OPENSANDBOX_IMAGE` | `python:3.11-slim` | Container image of every sandbox. |
| `OPENSANDBOX_USE_SERVER_PROXY` | `false` | Send all `execd` traffic through the server instead of connecting to the sandbox directly. Needed when the harness is not on the Docker host's network. |
| `OPENSANDBOX_CREDENTIAL_PROXY` | `false` | Create sandboxes with the egress sidecar's Credential Vault; see below. |
| `SANDBOX_TIMEOUT_SECONDS` | `1800` | Idle lifetime of a sandbox; renewed while it is in use. |
| `SANDBOX_COMMAND_TIMEOUT_SECONDS` | `120` | Default `execute_command` timeout. |
| `SANDBOX_CPU`, `SANDBOX_MEMORY` | `1`, `2Gi` | Resource limits passed to the server. |
| `MAX_SANDBOXES_PER_USER` | `5` | Live sandboxes per login. Over the limit the manager first reclaims sandboxes of dead threads, then fails with `ErrQuota`. |
| `SANDBOX_UNHEALTHY_RECREATE_SECONDS` | `300` | A sandbox unhealthy for longer, across at least three checks, is killed and recreated. |

A recreated sandbox gets the thread's git history and saved artifacts restored; the model is told that `/workspace` was reset.

### Egress policy

Every sandbox is created with a network policy; one created without it can reach other sandboxes and the host.

| Setting | Default | Mode |
|---|---|---|
| `SANDBOX_EGRESS_ALLOWLIST` | empty | Comma-separated host names (`*.example.com` allowed), IPs or CIDRs. When set, egress is deny by default and only these are allowed. |
| `SANDBOX_EGRESS_DENY_CIDRS` | RFC 1918, link-local, CGNAT, `0.0.0.0/8`, IPv6 ULA and link-local, IPv4-mapped IPv6 | Denied in unrestricted mode (no allow-list), so a sandbox cannot reach other sandboxes, the host or the cloud metadata service. |
| `SANDBOX_EGRESS_UNRESTRICTED` | `false` | Explicitly accepts unrestricted egress in production. |

An entry that is not a valid host name, IP or CIDR stops the process at load (`SANDBOX_EGRESS_ALLOWLIST: "x" is not a host name, IP address or CIDR (no scheme, port or path)`), so a typo cannot silently open the network.

<div class="callout warning">

**Production needs a decision.** With `HARNESS_PRODUCTION` set and an empty allow-list, startup fails with `SANDBOX_EGRESS_ALLOWLIST is empty in production: set it, or set SANDBOX_EGRESS_UNRESTRICTED=true to accept unrestricted sandbox egress`. See [Production checklist](/docs/operations/production/).

</div>

### Credential proxy

With `OPENSANDBOX_CREDENTIAL_PROXY=true`, the egress sidecar adds authentication to outbound HTTPS requests outside the sandbox. A project's GitHub installation token is bound to that repository's paths on `github.com` and `api.github.com`, so plain `git clone`, `git push` and `curl https://api.github.com/repos/OWNER/REPO/pulls` work and no token enters the sandbox. It needs `opensandbox-server` 0.2.0 or later, egress 1.1.1 or later in `dns+nft` mode and a default-deny allow-list that includes the bound hosts. Without a credential broker the token is written to `/workspace/.work/git-credentials`, readable by anything in the sandbox. See [GitHub and Telegram](/docs/integrations/github-telegram/#clone-and-credentials).

### The sandbox image

`OPENSANDBOX_IMAGE` must provide `bash` 4+, GNU coreutils (`realpath -m -z`, `mv -T`, `stat -c`, `base64 -w0`), `findutils`, `setsid`, and `python3`, with a writable `/workspace`. BusyBox and Alpine images fail those checks. The default `python:3.11-slim` has this required tier, but not `rg`, `git`, poppler or LibreOffice: enough for a tools-only agent, with no document work and no workspace checkpoints.

`sandbox-image/` in the repository holds a complete image and a lifecycle-server configuration:

| File | Purpose |
|---|---|
| `Dockerfile` | Debian bookworm with Python 3.11, Node 22, LibreOffice, poppler, pandoc, ripgrep, git and the Office and data libraries. Extra CA certificates go in `certs/`; mirrors through build args. |
| `opensandbox.toml` | Lifecycle server configuration for a local Docker runtime: loopback bind, dropped capabilities, `no-new-privileges`, a pids limit and the `dns+nft` egress sidecar. |

```bash
docker build -t harness-sandbox:local sandbox-image/
# run the OpenSandbox server with sandbox-image/opensandbox.toml, then:
export OPENSANDBOX_URL=http://127.0.0.1:18090
export OPENSANDBOX_IMAGE=harness-sandbox:local
```

## Custom backends

A backend implements `sandbox.Backend` (`Create`, `Connect`, `Kill`) and returns `sandbox.Instance` values. Register it by name so `SANDBOX_BACKEND=<name>` selects it in every binary that links your package:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/ai/llm/llmtest"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"
)

func init() {
	// Selected by SANDBOX_BACKEND=mine. Settings carries the parsed SANDBOX_* and OPENSANDBOX_* values.
	backends.Register("mine", func(s backends.Settings) (sandbox.Backend, error) {
		return sandboxtest.NewBackend(), nil // replace with your adapter
	})
}

func main() {
	os.Setenv("LLM_MODELS", "m")
	os.Setenv("LLM_DEFAULT_MODEL", "m")
	os.Setenv("SANDBOX_BACKEND", "mine")
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	model := llmtest.New(llmtest.Turn{Chunks: llmtest.Text("ok")})
	h, err := harness.New(harness.Options{Config: cfg, Models: llmtest.Factory{M: model}})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("backends:", backends.Registered(), "sandbox enabled:", cfg.SandboxEnabled(), h != nil)
}
```

This prints `backends: [local mine none] sandbox enabled: true true`.

Rules of the registry:

- Names are case-insensitive. `backends.Register` panics on an empty name, a nil factory or a name registered twice; it is a programming error.
- An unknown name fails at start with the list of registered backends.
- A factory that returns `nil, nil` means "no sandbox".
- `backends.Settings` carries `OpenSandbox.EgressAllowlist`, `EgressDenyCIDRs`, `CPU`, `Memory` and `TimeoutSeconds` for you to honour. Any other setting of your own, read it from the environment.
- `Connect` must return `sandbox.ErrNoSandbox` only when the sandbox definitively no longer exists. Any other error is treated as transient; a wrong `ErrNoSandbox` makes the manager delete a user's workspace.

Prove the adapter with `sandboxtest.RunConformance`. The contract, the image requirements, the egress rules and a worked REST example are in [`docs/sandbox-adapters.md`](https://github.com/7ar-chat/harness/blob/main/docs/sandbox-adapters.md).
