# Sandbox adapters

The harness runs commands and touches files in a sandbox, always through one
small contract (`sandbox.Backend`). OpenSandbox is the adapter that ships for
remote isolation, `adapters/sandbox/local` runs on the developer's machine, and
`core/sandbox/sandboxtest` fakes one for tests. This page is for whoever writes the
next adapter (E2B, Firecracker, Kubernetes jobs, a REST exec service...): the
contract, what the image must contain, how sandboxes live and die, the security
rules, a worked example, and how to register it.

```
agent tools (execute_command, read_file, ...)
        │
 sandbox.Manager   one sandbox per thread, quota, health, recreate   (sandbox/manager.go)
        │
 sandbox.Backend   Create / Connect / Kill  ──►  sandbox.Instance  (your adapter)
```

A run may also have **no** sandbox (`RunOptions.Sandbox: off`) or a harness with no
backend at all; nothing in an adapter needs to know. See "Sandbox per run" in
[embedding.md](embedding.md).

## 1. The contract

Defined in `sandbox/sandbox.go`.

```go
type Backend interface {
    Create(ctx, CreateOptions) (Instance, error)  // a new, empty sandbox
    Connect(ctx, id string) (Instance, error)     // reattach to a sandbox of this backend
    Kill(ctx, id string) error                    // destroy it (best effort)
}

type Instance interface {
    Sandbox                                       // ID, Exec, ReadFile, WriteFile, MkdirAll, List, Stat
    Healthy(ctx) bool                             // answers and can run commands now
    Gone(ctx) bool                                // definitively over (terminated, failed, 404)
    Renew(ctx, d time.Duration) error             // extend the idle lifetime by d
}
```

Rules, each checked by the conformance suite (section 6):

| Operation | Contract |
|---|---|
| `Create` | Returns a ready sandbox with an empty `/workspace` (the manager creates the standard sub-directories). `CreateOptions.Metadata` carries `work-thread` and `work-user` (a hash, never the login): attach them to the provider object for debugging and orphan sweeping. Must apply the egress policy (section 5). |
| `Connect` | Returns `sandbox.ErrNoSandbox` **only** when the sandbox definitively no longer exists (404, terminated, failed). Every other error (timeout, 5xx, network) is transient: the manager then keeps the sandbox and retries later. Returning `ErrNoSandbox` for a blip makes the manager delete a user's workspace. |
| `Kill` | Idempotent. Killing an unknown id is not an error worth failing for; the manager ignores the result. May complete asynchronously, but `Connect` must report `ErrNoSandbox` soon after. |
| `Healthy` | A cheap probe (run `true`, or the provider's ping). Must return `false` when the context ends. |
| `Gone` | `true` only for a definitive end of life. A timeout is **not** gone. |
| `Renew` | Extend the provider-side TTL. Called at most once per minute while a sandbox is in use. |
| `Exec` | Runs `Command` with **bash** (`bash -c`, a script may be many lines and contain heredocs), in `Cwd` (the tools pass `/workspace`; the protocol scripts pass `/`, which a remote adapter must accept), with `Env` added to the environment (values are data: never re-parsed by a shell). Streams stdout and stderr to `OnStdout`/`OnStderr` **as produced**, in order within each stream. A non-zero exit is a result (`ExecResult.ExitCode`), not an error; an error means the command could not be run or its result is unknown. `Timeout` (0 = none) kills the **whole process group** and returns an error or a non-zero exit; cancelling `ctx` does the same promptly. Output of several MB must not be dropped; a transport that delivers message by message may add a newline per message (the tools never depend on where newlines fall). |
| `ReadFile` | At most `maxBytes` of the file (0 = no cap) **and** its full size. A missing file is `sandbox.ErrNotFound` (wrapped is fine). Binary safe. |
| `WriteFile` | Creates parent directories, replaces the file (mode 0644). Binary safe, at least 4 MiB. |
| `MkdirAll` | Idempotent. |
| `List(path, depth)` | Entries below `path` down to `depth` (1 = direct children), as **absolute sandbox paths**, `Type` `"file"` or `"directory"`, `ModifiedAt` set. Missing path: `ErrNotFound`. |
| `Stat` | Same `FileInfo` for one path; missing: `ErrNotFound`. |

Errors the manager and tools understand (all in `sandbox`): `ErrNotFound` (file),
`ErrNoSandbox` (from `Connect`), `ErrGone` (an operation found its sandbox gone:
`sandbox.IsGone` also matches the provider string `SANDBOX_NOT_FOUND`; the
manager then drops the handle and recreates), `ErrQuota`, `ErrUnavailable`
(transient). Wrap with `%w`.

Optional: implement `WriteFileFrom(ctx, path, io.Reader)` (see `sandbox.WriteFrom`)
for streamed uploads of large files.

### File API paths and symlinks

Tools address files as absolute paths under `/workspace` (also `/skills` for
installed skills, `/tmp` for job records). Two things matter:

* The file API is an implementation detail of your provider and may follow
  symlinks inside the sandbox: a container is the boundary. A **host-level**
  adapter (like `adapters/sandbox/local`, no container) must confine the file API to its
  roots itself and refuse `..` and symlink escapes (`ConformanceOptions.Confined`
  checks it).
* Anything that must be safe against a symlink planted by the agent does not use
  the file API alone. It goes through the protocol helpers below, which run a
  script in the sandbox and decide on the real path.

## 2. The protocol scripts the tools run through `Exec`

Besides plain user commands, the harness runs fixed scripts (first line
`# work:<name>`) through `Exec`. They are the real requirement on the image.
`adapters/sandbox/local` and `sandboxtest` recognise the markers and answer natively;
a remote adapter just executes them.

| Script (marker) | Source | Used by | Needs |
|---|---|---|---|
| `place` / `unstage` | `sandbox/place.go` | `write_file`, `edit_file`, uploads: move a staged upload onto the target unless the target or a parent is a symlink (exit 4) | bash, `realpath -m`, `dirname`, `mkdir -p`, `mv -f -T`, `rm` |
| `realpath` | `sandbox/realpath.go` | the files API and the approval gate: resolve real paths | bash, `realpath -m -z`, `tr` |
| `read-files` | `sandbox/readfiles.go` | read-before-edit cache, checkpoint diffs | bash, `realpath -e`, `stat -c '%s %Y'`, `head -c`, `base64 -w0` |
| `job-start` / `job-wait` / `job-status` / `job-kill` | `coding/sandboxtools/exec.go` | background jobs (`execute_command` past its timeout, `background:true`, `job_output`, `job_kill`) | bash, `setsid`, `base64 -d`/`-w0`, `tail -F -s`, `stat -c`, `head -c`/`tail -c`, `kill`, `sleep` with fractions; optional `pkill` |
| `sweep` | `coding/sandboxtools/sandboxtools.go` | removes stale staged uploads and job records | `find -mmin -delete`, `ls -1dt`, `tail -n +N` |
| `search` | `coding/sandboxtools/search.go` | `search_files` | `rg` (ripgrep) preferred, else `python3` |
| `extract` / `render` | `read.go`, `view.go` | `read_file`/`view_file` on PDF and Office files | `python3` with `pypdf`, `markitdown`; `pdftotext`, `pdftoppm` (poppler); `soffice` (LibreOffice) for legacy formats and rendering |
| workspace checkpoints | `features/artifacts/workspace` | per-thread git history, archive and restore | `git`, `tar`, `gzip` |
| code checks (coding profile) | `coding/codecheck` | syntax check after `write_file` | `python3`, `node` as configured by `AGENT_CODE_CHECKS` |

## 3. Image requirements

What an image (`OPENSANDBOX_IMAGE`, or whatever your provider boots) must
provide. `sandbox-image/Dockerfile` satisfies all three tiers (built and smoke-tested on linux/arm64 and linux/amd64, 2026-10-30; the image runs as root, so `/workspace` is owned by the command user).

**Required** (every adapter; without these file writes, jobs or path checks break):

* a POSIX filesystem with a writable `/workspace` (the exact path), `/tmp` and
  `/skills` (created on demand), owned by the user commands run as;
* `bash` (4+; scripts use arrays, `PIPESTATUS`, `[[ ]]`) as `bash` on `PATH`;
* GNU coreutils: `realpath` (with `-m`, `-e`, `-z`), `dirname`, `basename`,
  `mv` (with `-T`), `mkdir`, `rm`, `cat`, `stat -c`, `head -c`, `tail`
  (`-c`, `-n +N`, `-F -s`), `base64` (`-w0`, `-d`), `tr`, `touch`, `sleep` (fractions), `ls`;
* `findutils` (`find -mmin -delete`), `util-linux` `setsid`, `kill` (builtin);
* `python3` (3.8+) with the standard library (the search fallback and the
  extract/render wrappers run in it).

BusyBox/Alpine images fail the `realpath -m -z`, `stat -c`, `base64 -w0` and
`mv -T` checks: install GNU coreutils there.

**Recommended** (degrade gracefully without):

* `ripgrep` (`rg`): faster `search_files` (the python fallback is used without it);
* `git` and `tar`: workspace checkpoints, history restore (`no_git` is reported without);
* `procps` (`pkill`): `job_kill` also kills processes that left the group.

**Optional, for documents**: LibreOffice (`soffice`), `poppler-utils`
(`pdftotext`, `pdftoppm`), `pandoc`, Python packages `pypdf`, `markitdown`,
`python-docx`, `openpyxl`, `python-pptx`, fonts. `read_file`/`view_file` on
Office and PDF files, previews and document skills need them.

Compared with `sandbox-image/Dockerfile`: it is the third tier (Debian
bookworm + everything above, plus node and the Office libraries). The
config default `python:3.11-slim` has the required tier but **not** `rg`,
`git`, poppler or LibreOffice: fine for a tools-only agent, no document work and
no checkpoints. There is no `flock` or `timeout` dependency.

`go test ./sandbox -run TestOpenSandboxConformance` (opt-in) and
`sandboxtest.RunConformance` fail with a message naming the missing tool.

## 4. Lifecycle, quotas, recreate (Manager)

Adapters implement three calls; `sandbox.Manager` (`sandbox/manager.go`) builds
the behaviour around them. It never changes per adapter, so do not duplicate it.

* **One sandbox per thread**, created **lazily** on the first tool call that
  needs one (`Manager.Acquire`). A run with `Sandbox: off`, or one that only
  uses non-sandbox tools, never calls `Create`. The thread row stores the
  sandbox id; any process (API replica, worker) reaches it with `Connect`.
* **Quota**: `MAX_SANDBOXES_PER_USER` (default 5) live sandboxes per login,
  counted in the store under a per-user lock (cluster wide with Redis). Over the
  limit, the manager first reclaims sandboxes of dead threads, then fails with
  `ErrQuota` ("ask the user to delete an old conversation"). Deleting a thread
  kills its sandbox.
* **Health**: each acquisition checks `Healthy` (cached for 30 s). Unhealthy
  for longer than `SANDBOX_UNHEALTHY_RECREATE_SECONDS` (default 300) across at
  least three checks: killed and recreated. A definitive `Connect` ->
  `ErrNoSandbox` (or `Gone`) also recreates.
* **Recreate**: a new sandbox replaces the lost one and `OnCreate` hooks run
  before anyone can use it: the workspace feature restores the thread's git
  history and the artifact feature restores the saved files; the model gets a
  notice that `/workspace` was reset (processes and files outside the saved set
  are gone). Adapters must not try to preserve state themselves.
* **Renew**: the manager calls `Renew(Timeout)` (default 1800 s,
  `SANDBOX_TIMEOUT_SECONDS`) at most every minute while used and keeps running
  commands and background jobs alive. Idle sandboxes expire on the provider.
* **Concurrency**: tools of one run may call `Exec`/file methods concurrently on
  the same `Instance`; the instance must be safe for that.
* **Retries**: an operation failing with a "gone" error is retried once on a new
  sandbox (`sandbox.Retry`); return `ErrGone` (or an error containing
  `SANDBOX_NOT_FOUND`) for that case and nothing else.

## 5. Security expectations

The agent runs untrusted, model-written code, and reads untrusted files.

* **One sandbox per thread, never shared across users.** Do not pool sandboxes
  between threads or reuse one after a user's thread is deleted.
* **Egress policy at `Create`.** The default for a remote adapter is
  *deny all, allow a list* (`SANDBOX_EGRESS_ALLOWLIST`). Without a list, deny at
  least private, link-local, CGNAT and metadata ranges
  (`SANDBOX_EGRESS_DENY_CIDRS`, default in `sandbox.DefaultEgressDenyCIDRs`), so a
  sandbox cannot reach other sandboxes, the host, the orchestrator or the cloud
  metadata service. `config.EgressMode()` (checked by `harness.NewSandboxBackend`) refuses unrestricted egress in
  production unless `SANDBOX_EGRESS_UNRESTRICTED=true`. Your adapter must apply
  it or document why the provider already does.
* **No credentials in the sandbox.** Pass none via `Env`/`Metadata` beyond what a
  command needs; the harness never puts model keys there. The tools' `Env` is
  for `HARNESS_*` protocol variables.
* **Credential brokering (optional).** A backend whose egress layer can add
  authentication to outbound HTTPS requests implements `sandbox.CredentialBroker`
  (`SetCredentials`: the complete set of credentials and host/path bindings,
  idempotent, called again to renew). A project's GitHub installation token is
  then handed to the broker, limited to that repository's paths on `github.com`
  and `api.github.com`, and never enters the sandbox: plain `git clone`, `git push`
  and `curl https://api.github.com/repos/OWNER/REPO/pulls` just work. Without a
  broker the token is written to `/workspace/.work/git-credentials`, where
  anything running in the sandbox can read it. If a broker refuses the token, the
  checkout does **not** fall back to the file: a private repository then fails
  to clone. The OpenSandbox adapter implements it with the egress sidecar's
  Credential Vault (`OPENSANDBOX_CREDENTIAL_PROXY=true`; needs
  `opensandbox-server` >= 0.2.0, egress >= 1.1.1 in `dns+nft` mode, and a
  default-deny `SANDBOX_EGRESS_ALLOWLIST` that includes the bound hosts). The
  sidecar decrypts HTTPS for the bound hosts with its own CA, which the sandbox
  image trusts at start.
* **File contents are data.** An adapter never interprets, executes, renders or
  logs file contents; it does not follow a `ReadFile` result into another path.
  Sizes are capped by the caller (`maxBytes`); enforce your provider's own caps
  and return an error rather than truncating silently on writes.
* **Symlinks** planted by the agent must not turn a file API call into a read or
  write outside the sandbox (container adapters: the container; host-level
  adapters: confine). The protocol helpers refuse symlinked targets themselves.
* **Control plane auth.** The exec/files endpoints of a sandbox must not be
  reachable from other sandboxes (hence the egress deny list) and need the
  provider's token.
* **Resource limits** (CPU, memory, pids, disk, wall clock) are the provider's
  job; `SANDBOX_CPU`/`SANDBOX_MEMORY` are passed to OpenSandbox. Pass equivalents.
* **Log without secrets**: sandbox ids, thread ids and hashed user tags are
  fine; commands and file names can hold private data.

## 6. Conformance suite

```go
func TestConformance(t *testing.T) {
    sandboxtest.RunConformance(t, func(t *testing.T) sandbox.Backend { return myadapter.New(...) })
}
```

`sandboxtest.RunConformanceWith` takes options: `Skip` (subtest -> reason, for
test doubles and known gaps), `Confined` (host-level adapters), `KillGrace`.
It creates real sandboxes (a few), so gate it behind your provider's env var like
`sandbox/opensandbox_live_test.go` does (`OPENSANDBOX_URL`). What it checks:

* **Lifecycle**: Create, Healthy, Gone, Renew, Connect returns the same
  sandbox with its files, Kill then Connect -> `ErrNoSandbox`, unknown id ->
  `ErrNoSandbox`, two sandboxes do not share files.
* **Exec**: stdout/stderr separate; output streams *while* the command runs and
  in order; exit codes (including 127) are results; `Cwd`; `Env` with quotes and
  `$` round-trips; a timeout kills the command *and its children*; cancelling the
  context returns promptly; 3 MB of output arrives whole; files written by
  commands are visible to the file API and vice versa.
* **Files**: binary round trip with parents created, `maxBytes` cut with full
  size, missing -> `ErrNotFound` for Read/Stat/List, replace and empty files,
  MkdirAll/List(depth)/Stat shapes, unusual names, 4 MiB file,
  `PrepareWorkspace` layout, and (Confined) no escape through `..` or symlinks.
* **Protocol**: `RealPaths` resolves symlinks and missing tails and refuses
  relative paths; `WriteFileNoFollow` writes, cleans the staging directory and
  refuses a symlinked directory or target without touching what it points to;
  `ReadRegularFiles` skips symlinks, directories and missing paths, honours the
  known-stamp and the byte budget.

A second suite runs the real tools on your adapter:
`coding/sandboxtools/conformance.Run(t, newBackend, memstore.New())`
(execute_command, write/read/edit/list, search_files, a background job to
completion and `job_kill` of its process tree).

Run both against `adapters/sandbox/local` (unix; protocol scripts answered natively), the
in-memory fake (`TestFakeBackendConformance`: Exec subtests skipped, it has no
shell) and `examples/sandbox-adapter` (through a real HTTP service). The live OpenSandbox
run is the reference for a remote adapter.

## 7. Worked example: a generic REST exec service

`examples/sandbox-adapter/rest.go` (about 265 lines with comments, 190 of code) adapts a service with
these endpoints (see the package comment for the exact JSON):

```
POST   /sandboxes                 create            -> {"id"}
GET    /sandboxes/{id}            health / gone (404 + X-Not-Found: sandbox)
DELETE /sandboxes/{id}            kill
POST   /sandboxes/{id}/exec       run -> NDJSON stream {"o"} {"e"} ... {"exit":N}
GET|PUT /sandboxes/{id}/file      read (max, X-File-Size) / write
POST   /sandboxes/{id}/mkdir, GET /list, GET /stat
```

The parts worth copying:

* `do` is the single place errors are classified: network error -> returned as
  is (transient); 404 with the "sandbox" marker -> `sandbox.ErrGone`; other
  404 -> `sandbox.ErrNotFound`; 429 -> `ErrQuota`; other 4xx/5xx ->
  `ErrUnavailable`. `Connect` turns `IsGone` into `ErrNoSandbox`, nothing else.
* `Exec` decodes the NDJSON stream with a `json.Decoder` and calls the callbacks
  as events arrive, finishing on the `exit` event. A stream that ends without one
  is an error (never a guessed exit code).
* `ReadFile` returns the data and the size from a header; the service applies `max`.

`examples/sandbox-adapter/rest_test.go` starts a small in-process fake of that service
(about 130 lines, backed by `adapters/sandbox/local` for the shell and files) and runs
`sandboxtest.RunConformanceWith` against the adapter, plus a test of the error
mapping. It is also the template for your own test: replace the fake with your
provider's test account behind an env var.

## 8. Registering the adapter

Two ways, both ending in `harness.Options.Backend`:

1. **Directly** (library hosts): `harness.New(harness.Options{Backend: myadapter.New(...)})`.
   No registration; the website example does this.
2. **By name**, so `SANDBOX_BACKEND=<name>` selects it for every binary that
   links your package (`harness-web`, `harness`, a worker built by you):

```go
package myadapter

import (
    "github.com/7ar-chat/harness/adapters/sandbox/backends"
    "github.com/7ar-chat/harness/core/sandbox"
)

func init() {
    backends.Register("rest", func(s backends.Settings) (sandbox.Backend, error) {
        return &Backend{BaseURL: os.Getenv("REST_SANDBOX_URL")}, nil
    })
}
```

then, in your own `main` (or the module's `cmd`), blank-import the package:
`import _ "example.com/you/myadapter"`, and run with `SANDBOX_BACKEND=rest`.
Built-ins: `local` (unix only: registered from the facade behind a build tag),
`opensandbox` (also selected when only `OPENSANDBOX_URL` is set) and `none`.
An unknown name fails at start with the list of registered ones;
`backends.Register` panics on a duplicate name. Names are case-insensitive.

A registered adapter receives `backends.Settings`: `OpenSandbox.EgressAllowlist`,
`OpenSandbox.EgressDenyCIDRs`, `OpenSandbox.CPU`, `OpenSandbox.Memory` and
`OpenSandbox.TimeoutSeconds` (from `SANDBOX_EGRESS_ALLOWLIST`, `SANDBOX_EGRESS_DENY_CIDRS`,
`SANDBOX_CPU`, `SANDBOX_MEMORY`, `SANDBOX_TIMEOUT_SECONDS`) are there for you to honour; any
other setting of your own it reads from the environment.

## 9. Checklist before you ship an adapter

1. `RunConformance` and `conformance.Run` pass against the real provider, in CI behind an env var.
2. `Connect` returns `ErrNoSandbox` only on a definitive absence (test with the provider down).
3. A killed or expired sandbox is reported by `Gone`, and operations on it return `ErrGone`.
4. Egress policy applied and verified from inside (cannot reach metadata or another sandbox).
5. Image checked against section 3 (the conformance failures name the missing tool).
6. `Exec` streams, kills the process group on timeout and cancel, and never blocks on a full pipe.
