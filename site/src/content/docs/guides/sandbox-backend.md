---
title: "Write a sandbox backend"
description: "Implement sandbox.Backend for a new provider (E2B, Firecracker, Kubernetes jobs, a REST exec service), prove it with the conformance suite and select it with SANDBOX_BACKEND."
---

The harness runs commands and touches files through one small contract, `sandbox.Backend`. Write a backend when none of the shipped ones ([`local`, `opensandbox`](/docs/integrations/sandbox-backends/)) fits your provider. This page gives the contract, what the image must contain, what the Manager does for you, a compiling skeleton with its conformance test, and how to register it. For what a sandbox is and when a run gets one, read [Sandboxes](/docs/concepts/sandboxes/).

```text
agent tools (execute_command, read_file, ...)
        │
 sandbox.Manager   one sandbox per thread, quota, health, recreate
        │
 sandbox.Backend   Create / Connect / Kill  ──►  sandbox.Instance   (your adapter)
```

## The contract

A backend has three methods and returns an `Instance`, which is a `Sandbox` plus three lifecycle calls (package `github.com/7ar-chat/harness/core/sandbox`).

```go
type Backend interface {
    Create(ctx context.Context, opts CreateOptions) (Instance, error) // a new, empty sandbox
    Connect(ctx context.Context, id string) (Instance, error)         // reattach
    Kill(ctx context.Context, id string) error                        // destroy (best effort)
}

type Instance interface {
    Sandbox                                      // ID, Exec, ReadFile, WriteFile, MkdirAll, List, Stat
    Healthy(ctx context.Context) bool            // answers and can run commands now
    Gone(ctx context.Context) bool               // definitively over (terminated, failed, 404)
    Renew(ctx context.Context, d time.Duration) error // extend the idle lifetime by d
}
```

| Operation | What it must do |
|---|---|
| `Create` | Return a ready sandbox with an empty `/workspace` (the Manager creates the standard sub-directories). `CreateOptions.Metadata` carries `work-thread` and `work-user` (a hash, never the login): attach them to the provider object for debugging and orphan sweeping. Apply the [egress policy](#security-expectations). |
| `Connect` | Return `sandbox.ErrNoSandbox` **only** when the sandbox definitively no longer exists (404, terminated, failed). Every other error (timeout, 5xx, network) is transient and the Manager keeps the sandbox and retries. |
| `Kill` | Idempotent. Killing an unknown id is not a failure; the Manager ignores the result. May finish asynchronously, but `Connect` must report `ErrNoSandbox` soon after. |
| `Healthy` | A cheap probe (run `true`, or the provider's ping). Must return `false` when the context ends. |
| `Gone` | `true` only for a definitive end of life. A timeout is not gone. |
| `Renew` | Extend the provider-side TTL. Called at most once per minute while a sandbox is in use. |
| `Exec` | Run `Command` with **bash** (`bash -c`; a script may span many lines and contain heredocs) in `Cwd` (tools pass `/workspace`, the protocol scripts pass `/`), with `Env` added to the environment (values are data, never re-parsed by a shell). Stream stdout and stderr to `OnStdout`/`OnStderr` **as produced**, in order within each stream. A non-zero exit is a result (`ExecResult.ExitCode`), not an error; an error means the command could not be run or its result is unknown. `Timeout` (0 = none) kills the **whole process group**; cancelling `ctx` does the same promptly. Output of several MB must not be dropped. |
| `ReadFile` | At most `maxBytes` bytes (0 = no cap) **and** the full size. A missing file is `sandbox.ErrNotFound` (wrapping is fine). Binary safe. |
| `WriteFile` | Create parent directories, replace the file (mode 0644). Binary safe, at least 4 MiB. |
| `MkdirAll` | Idempotent. |
| `List(path, depth)` | Entries below `path` down to `depth` (1 = direct children) as **absolute sandbox paths**, `Type` `"file"` or `"directory"`, `ModifiedAt` set. Missing path: `ErrNotFound`. |
| `Stat` | The same `FileInfo` for one path; missing: `ErrNotFound`. |

<div class="callout warning">

**`ErrNoSandbox` for a network blip deletes a user's workspace.** The Manager treats it as "the sandbox is gone" and creates a new one. Classify errors in one place and return it only on a definitive 404/terminated answer.

</div>

The errors the Manager and the tools understand are all in `sandbox`; wrap them with `%w`:

| Error | Meaning |
|---|---|
| `ErrNotFound` | a file or directory is missing |
| `ErrNoSandbox` | returned by `Connect` only: the sandbox no longer exists |
| `ErrGone` | an operation found its sandbox gone; `sandbox.IsGone` also matches the provider string `SANDBOX_NOT_FOUND`. The Manager drops the handle and an operation is retried once on a fresh sandbox (`sandbox.Retry`) |
| `ErrQuota` | the provider refused for quota |
| `ErrUnavailable` | transient failure |

Optionally implement `WriteFileFrom(ctx, path, io.Reader)` (see `sandbox.WriteFrom`) for streamed uploads of large files.

### File API paths and symlinks

Tools address files as absolute paths under `/workspace` (also `/skills` for installed skills and `/tmp` for job records). For a container-based provider the file API may follow symlinks inside the sandbox: the container is the boundary. A **host-level** adapter (no container, like `adapters/sandbox/local`) must confine the file API to its roots itself and refuse `..` and symlink escapes; `ConformanceOptions.Confined` checks it. Anything that must be safe against a symlink planted by the agent does not use the file API alone: it goes through the protocol scripts below, which decide on the real path inside the sandbox.

## What the image must contain

Besides user commands, the harness runs fixed scripts (first line `# work:<name>`) through `Exec`. They are the real requirement on your image; `local` and `sandboxtest` recognise the markers and answer natively, a remote adapter just executes them.

| Script | Used by | Needs |
|---|---|---|
| `place` / `unstage` | `write_file`, `edit_file`, uploads | bash, `realpath -m`, `dirname`, `mkdir -p`, `mv -f -T`, `rm` |
| `realpath` | files API, approval gate | bash, `realpath -m -z`, `tr` |
| `read-files` | read-before-edit cache, checkpoint diffs | bash, `realpath -e`, `stat -c '%s %Y'`, `head -c`, `base64 -w0` |
| `job-start` / `job-wait` / `job-status` / `job-kill` | background jobs | bash, `setsid`, `base64`, `tail -F -s`, `stat -c`, `kill`, fractional `sleep`; optional `pkill` |
| `sweep` | stale staged uploads and job records | `find -mmin -delete`, `ls -1dt`, `tail -n +N` |
| `search` | `search_files` | `rg` preferred, else `python3` |
| `extract` / `render` | `read_file`/`view_file` on PDF and Office | `python3` with `pypdf`, `markitdown`; poppler; LibreOffice |
| workspace checkpoints | per-thread git history | `git`, `tar`, `gzip` |

Required in every image: a writable `/workspace` (that exact path), `/tmp` and `/skills`, owned by the user commands run as; `bash` 4+ on `PATH`; GNU coreutils (`realpath` with `-m -e -z`, `mv -T`, `stat -c`, `head`, `tail -F -s`, `base64 -w0 -d`, `tr`, `touch`, `sleep` with fractions, and the rest); `findutils`; `setsid`; `python3` 3.8+ with the standard library. Recommended: `ripgrep`, `git`, `tar`, `procps`. Optional for documents: LibreOffice, poppler-utils, pandoc and the Python packages.

<div class="callout note">

**BusyBox and Alpine fail.** They lack `realpath -m -z`, `stat -c`, `base64 -w0` and `mv -T`: install GNU coreutils there. The default `OPENSANDBOX_IMAGE` (`python:3.11-slim`) has the required tier but not `rg`, `git`, poppler or LibreOffice: fine for a tools-only agent, no document work and no checkpoints. `sandbox-image/Dockerfile` in the repository satisfies all three tiers. The conformance failures name the missing tool.

</div>

## What the Manager does for you

Adapters implement three calls; `sandbox.Manager` builds the behaviour around them and does not change per adapter. Do not duplicate it.

| Concern | Behaviour |
|---|---|
| One sandbox per thread | created lazily by `Manager.Acquire` on the first tool call that needs one. A run with `Sandbox: off` never calls `Create`. The thread row stores the id; any process reaches it with `Connect`. |
| Quota | `MAX_SANDBOXES_PER_USER` (default 5) live sandboxes per login, counted in the store under a per-user lock (cluster-wide with Redis). Over the limit it reclaims sandboxes of dead threads, then fails with `ErrQuota`. Deleting a thread kills its sandbox. |
| Health | each acquisition checks `Healthy` (cached 30 s). Unhealthy for longer than `SANDBOX_UNHEALTHY_RECREATE_SECONDS` (default 300) across at least three checks: killed and recreated. A definitive `ErrNoSandbox` or `Gone` also recreates. |
| Recreate | a new sandbox replaces the lost one, the workspace and artifact features restore the saved state, and the model gets a notice that `/workspace` was reset. Adapters must not preserve state themselves. |
| Renew | `Renew(Timeout)` (`SANDBOX_TIMEOUT_SECONDS`, default 1800) at most once a minute while used. |
| Concurrency | tools of one run call `Exec` and the file methods concurrently on the same `Instance`: it must be safe for that. |

## Security expectations

The agent runs untrusted, model-written code and reads untrusted files.

- **One sandbox per thread, never shared across users.** No pooling, no reuse after a thread is deleted.
- **Egress policy at `Create`.** For a remote adapter the default is deny-all with an allowlist (`SANDBOX_EGRESS_ALLOWLIST`). Without a list, deny at least private, link-local, CGNAT and metadata ranges (`SANDBOX_EGRESS_DENY_CIDRS`, whose default list is the same as `opensandbox.DefaultEgressDenyCIDRs`). Apply it or document why the provider already does.
- **No credentials in the sandbox** beyond what a command needs. The harness never puts model keys there. Optionally let your instances implement `sandbox.CredentialBroker` (`SetCredentials`) if your egress layer can add authentication to outbound HTTPS requests; then the GitHub installation token never enters the sandbox.
- **File contents are data.** Never interpret, execute, render or log them; enforce your provider's caps and fail rather than truncate silently on writes.
- **Symlinks** planted by the agent must not turn a file call into access outside the sandbox.
- **Resource limits** (CPU, memory, pids, disk, wall clock) are the provider's job; `SANDBOX_CPU` and `SANDBOX_MEMORY` are passed to OpenSandbox, pass equivalents.
- **Log without secrets**: sandbox ids, thread ids and hashed user tags are fine; commands and file names can hold private data.

## A skeleton backend

The package below separates the provider's wire API (`Client`, the only part that talks to the network) from the harness contract (`Backend` and `instance`). The classification rule is the whole job: every `Client` method returns an error wrapping `sandbox.ErrGone` when the sandbox does not exist and `sandbox.ErrNotFound` for a missing file, and `Connect` turns `IsGone` into `ErrNoSandbox` and nothing else. The worked reference is [`examples/sandbox-adapter`](https://github.com/7ar-chat/harness/tree/main/examples/sandbox-adapter), an adapter for a generic REST exec service.

```go
// Package mysandbox adapts a hypothetical sandbox provider to sandbox.Backend.
package mysandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
)

// Client is the provider's API, in the provider's terms. This is the only part
// that talks to the network; replace it with your SDK or an HTTP client.
// Contract: a sandbox that does not exist makes every method return an error
// wrapping sandbox.ErrGone; a missing file wraps sandbox.ErrNotFound.
type Client interface {
	Create(ctx context.Context, metadata map[string]string) (id string, err error)
	Delete(ctx context.Context, id string) error
	Ping(ctx context.Context, id string) error // nil: alive; ErrGone: over; other: transient
	Renew(ctx context.Context, id string, d time.Duration) error
	Exec(ctx context.Context, id string, req sandbox.ExecRequest) (sandbox.ExecResult, error)
	ReadFile(ctx context.Context, id, path string, maxBytes int64) ([]byte, int64, error)
	WriteFile(ctx context.Context, id, path string, data []byte) error
	MkdirAll(ctx context.Context, id, path string) error
	List(ctx context.Context, id, path string, depth int) ([]sandbox.FileInfo, error)
	Stat(ctx context.Context, id, path string) (sandbox.FileInfo, error)
}

// Backend is the sandbox.Backend of a provider.
type Backend struct{ Client Client }

var _ sandbox.Backend = (*Backend)(nil)

func (b *Backend) Create(ctx context.Context, opts sandbox.CreateOptions) (sandbox.Instance, error) {
	// opts.Metadata carries work-thread and work-user: attach it to the provider
	// object. Apply your egress policy here (deny private ranges / allowlist).
	id, err := b.Client.Create(ctx, opts.Metadata)
	if err != nil {
		return nil, fmt.Errorf("create sandbox: %w", err)
	}
	return &instance{c: b.Client, id: id}, nil
}

func (b *Backend) Connect(ctx context.Context, id string) (sandbox.Instance, error) {
	if err := b.Client.Ping(ctx, id); err != nil {
		if sandbox.IsGone(err) {
			return nil, sandbox.ErrNoSandbox // definitive absence, and only then
		}
		return nil, err // timeout, 5xx, network: transient, the manager retries later
	}
	return &instance{c: b.Client, id: id}, nil
}

func (b *Backend) Kill(ctx context.Context, id string) error {
	if err := b.Client.Delete(ctx, id); err != nil && !sandbox.IsGone(err) {
		return err
	}
	return nil // idempotent: an unknown id is not a failure
}

// instance is safe for concurrent use: it holds no state beyond the id.
type instance struct {
	c  Client
	id string
}

var _ sandbox.Instance = (*instance)(nil)

func (i *instance) ID() string { return i.id }

func (i *instance) Healthy(ctx context.Context) bool { return i.c.Ping(ctx, i.id) == nil }

func (i *instance) Gone(ctx context.Context) bool {
	return sandbox.IsGone(i.c.Ping(ctx, i.id)) // a timeout is not gone
}

func (i *instance) Renew(ctx context.Context, d time.Duration) error { return i.c.Renew(ctx, i.id, d) }

func (i *instance) Exec(ctx context.Context, req sandbox.ExecRequest) (sandbox.ExecResult, error) {
	return i.c.Exec(ctx, i.id, req)
}

func (i *instance) ReadFile(ctx context.Context, path string, maxBytes int64) ([]byte, int64, error) {
	return i.c.ReadFile(ctx, i.id, path, maxBytes)
}

func (i *instance) WriteFile(ctx context.Context, path string, data []byte) error {
	return i.c.WriteFile(ctx, i.id, path, data)
}

func (i *instance) MkdirAll(ctx context.Context, path string) error {
	return i.c.MkdirAll(ctx, i.id, path)
}

func (i *instance) List(ctx context.Context, path string, depth int) ([]sandbox.FileInfo, error) {
	return i.c.List(ctx, i.id, path, depth)
}

func (i *instance) Stat(ctx context.Context, path string) (sandbox.FileInfo, error) {
	return i.c.Stat(ctx, i.id, path)
}

// dial builds the provider client; replace it with the real one.
func dial(baseURL string) (Client, error) {
	return nil, errors.New("mysandbox: dial is the part you write: build your provider client for " + baseURL)
}

// Selected with SANDBOX_BACKEND=mysandbox in every binary that imports this package.
func init() {
	backends.Register("mysandbox", func(s backends.Settings) (sandbox.Backend, error) {
		url := os.Getenv("MYSANDBOX_URL")
		if url == "" {
			return nil, errors.New("SANDBOX_BACKEND=mysandbox needs MYSANDBOX_URL")
		}
		c, err := dial(url)
		if err != nil {
			return nil, err
		}
		return &Backend{Client: c}, nil
	})
}
```

`dial` is deliberately the one function you must write: it returns an error here so the skeleton never pretends to talk to a provider.

## Prove it with the conformance suite

`sandboxtest.RunConformance` creates real sandboxes (a few) and checks the contract; `RunConformanceWith` adds `ConformanceOptions` (`Skip`, `Confined`, `KillGrace`). Against a real provider, gate the test behind your provider's environment variable and run it in CI. What it checks:

- **Lifecycle**: `Create`, `Healthy`, `Gone`, `Renew`; `Connect` returns the same sandbox with its files; `Kill` then `Connect` gives `ErrNoSandbox`; an unknown id gives `ErrNoSandbox`; two sandboxes do not share files.
- **Exec**: stdout and stderr separate; output streams while the command runs and in order; exit codes (including 127) are results; `Cwd`; `Env` with quotes and `$`; a timeout kills the command and its children; cancelling the context returns promptly; 3 MB of output arrives whole.
- **Files**: binary round trip with parents created, the `maxBytes` cut with the full size, missing gives `ErrNotFound`, replace and empty files, `List` depth and `Stat` shapes, unusual names, a 4 MiB file, and with `Confined` no escape through `..` or symlinks.
- **Protocol**: `RealPaths`, `WriteFileNoFollow` and `ReadRegularFiles` behave on symlinks, directories and missing paths. These fail with a message naming the missing tool when the image lacks it.

A second suite runs the real tools on your adapter: `conformance.Run(t, newBackend, memstore.New())` from `github.com/7ar-chat/harness/coding/sandboxtools/conformance` (execute_command, the file tools, `search_files`, a background job to completion and `job_kill` of its process tree).

The test below runs the skeleton against a stand-in provider whose sandboxes are the host-level `local` backend (so it needs a unix machine and no network). In your repository replace `newFake` with a `Client` built from a test account.

```go
//go:build unix

package mysandbox_test

import (
	"testing"

	"github.com/7ar-chat/harness/adapters/sandbox/backends"
	"github.com/7ar-chat/harness/core/sandbox"
	"github.com/7ar-chat/harness/core/sandbox/sandboxtest"

	"example.com/mysandbox"
)

// newFake returns a Client whose "provider" is adapters/sandbox/local: it looks a
// sandbox up by id and forwards each call, answering sandbox.ErrGone for an
// unknown id (about 80 lines of forwarding code, omitted here; it is the `Client` interface of the skeleton implemented over `local.New(local.Options{StateDir: t.TempDir()})`).

func TestConformance(t *testing.T) {
	sandboxtest.RunConformanceWith(t, func(t *testing.T) sandbox.Backend {
		return &mysandbox.Backend{Client: newFake(t)}
	}, sandboxtest.ConformanceOptions{Skip: map[string]string{
		// only because this fake is the host-level local backend, which keeps /workspace/outputs out of the project
		"Files/PrepareWorkspaceBuildsTheLayout": "fake provider is adapters/sandbox/local",
	}})
}

func TestRegistered(t *testing.T) {
	found := false
	for _, n := range backends.Registered() {
		found = found || n == "mysandbox"
	}
	if !found {
		t.Fatal("mysandbox is not registered")
	}
}
```

With the stand-in provider all subtests pass (`go test ./...` ran in about 5 seconds). A real provider must pass the skip-free suite.

<div class="callout tip">

**Test the error mapping separately.** `examples/sandbox-adapter/rest_test.go` starts an in-process fake of the REST service and runs `RunConformanceWith` against the adapter, plus a test of each status-code mapping (404 with a sandbox marker is `ErrGone`, any other 404 is `ErrNotFound`, 429 is `ErrQuota`, other 4xx/5xx is `ErrUnavailable`). Copy that shape.

</div>

## Register it and select it

There are two ways, both ending in `harness.Options.Backend`.

1. **Directly** (library hosts): `harness.New(harness.Options{Backend: &mysandbox.Backend{Client: c}})`. No registration.
2. **By name**: `backends.Register("mysandbox", factory)` from an `init` (as in the skeleton), so `SANDBOX_BACKEND=mysandbox` selects it in every binary that links your package. Names are case-insensitive. `Register` panics on an empty name, a nil factory or a duplicate name.

In your own `main` blank-import the package and let the facade build the configured backend:

```go
package main

import (
	"fmt"
	"log"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/config"

	_ "example.com/mysandbox" // registers "mysandbox"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	be, err := harness.NewSandboxBackend(cfg) // the factory of SANDBOX_BACKEND
	if err != nil {
		log.Fatal(err)
	}
	h, err := harness.New(harness.Options{Config: cfg, Backend: be})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("backend %q ready: %T\n", cfg.SandboxBackendName(), h)
}
```

Run with `SANDBOX_BACKEND=mysandbox`. Without `MYSANDBOX_URL` this program stops at start with the factory's own error:

```text
SANDBOX_BACKEND=mysandbox needs MYSANDBOX_URL
```

A name nobody registered fails at start and lists the registered ones:

```text
unknown sandbox backend "nope" (SANDBOX_BACKEND); registered: local, mysandbox, none
```

A registered factory receives `backends.Settings`. `OpenSandbox.EgressAllowlist`, `OpenSandbox.EgressDenyCIDRs`, `OpenSandbox.CPU`, `OpenSandbox.Memory` and `OpenSandbox.TimeoutSeconds` (from `SANDBOX_EGRESS_ALLOWLIST`, `SANDBOX_EGRESS_DENY_CIDRS`, `SANDBOX_CPU`, `SANDBOX_MEMORY`, `SANDBOX_TIMEOUT_SECONDS`) are there for you to honour; any other setting of your own you read from the environment, as the skeleton does. Returning a nil `sandbox.Backend` with a nil error means "no sandbox".

<div class="callout breaking">

**OpenSandbox is opt-in since v0.3.** The built-in names are `local` (unix only), `none` and `opensandbox`, but `opensandbox` registers itself only when `github.com/7ar-chat/harness/adapters/sandbox/opensandbox` is imported with a blank import. Without it, `SANDBOX_BACKEND=opensandbox` fails with `unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: local, mysandbox, none; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it`. The stock `harness-web` and `harness` binaries already import it. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

When only `OPENSANDBOX_URL` is set and `SANDBOX_BACKEND` is not, the selected name is `opensandbox`; with neither it is `none`. `harness.NewSandboxBackend` also refuses unrestricted egress in production for `opensandbox` unless `SANDBOX_EGRESS_UNRESTRICTED=true`; for your own backend the egress policy is yours to apply.

## Checklist before you ship

1. `RunConformance` and `conformance.Run` pass against the real provider, in CI behind an env var.
2. `Connect` returns `ErrNoSandbox` only on a definitive absence (test with the provider down).
3. A killed or expired sandbox is reported by `Gone`, and operations on it return `ErrGone`.
4. Egress policy applied and verified from inside (cannot reach metadata or another sandbox).
5. Image checked against the tables above.
6. `Exec` streams, kills the process group on timeout and cancel, and never blocks on a full pipe.

Next: [Sandboxes](/docs/concepts/sandboxes/), [Sandbox backends](/docs/integrations/sandbox-backends/), [Choose a deployment shape](/docs/guides/deployment-shapes/).
