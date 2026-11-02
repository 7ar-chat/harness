---
title: "Upgrade notes"
description: "What changed between v0.1.0 and v0.3.1, with before and after code for the breaking changes, and the current tag of every module."
---

Upgrade one minor version at a time and let the compiler find what moved. This page lists every release of the root module, the breaking changes with before and after code, and the tag each module is at today. The harness is pre-1.0: a minor version may break source compatibility.

## Summary

| Release | Date | Breaking for you? | Change |
|---|---|---|---|
| `v0.1.0` | 2026-10-31 | first tag | The root module requires tagged sibling modules instead of local `replace` lines. |
| `v0.2.0` | 2026-10-31 | **yes, if you imported `product` or ran `cmd/...`** | The product and the commands move to the `app` module. |
| `v0.2.1` | 2026-10-31 | no | A run's first liveness signal is written before its heartbeat goroutine starts (`engine/runtime`). |
| `v0.3.0` | 2026-10-31 | **yes** | Redis, MCP and OpenSandbox become opt-in imports. |
| `v0.3.1` | 2026-10-31 | no | `app` requires root `v0.3.0`; documentation only in the root module. |

<div class="callout breaking">

**v0.3.0 removed `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra`.** MCP servers and the OpenSandbox backend now need a blank import. Both break at run time or compile time, with messages that name the fix. Details below.

</div>

## v0.3.0: Redis, MCP and OpenSandbox are opt-in

Package `harness` no longer imports go-redis, the MCP SDK or the OpenSandbox SDK, so an application that does not use them does not link them. `TestTheFacadeLinksNoOptionalSDKs` keeps it that way. The minimal embedding built by `make check-embed` went from 22.1 MB and 28 linked modules to 16.5 MB and 14 (`docs/embedding.md`, measured on the published `v0.3.0`).

### Redis: `Options.Redis` becomes `redisinfra`

`Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` are **removed**. The Redis client and the Redis-backed `Infra` now live in package `github.com/7ar-chat/harness/redisinfra`, and you pass the result as `Options.Infra`.

<div class="tabs">

<div class="tab" data-label="Before (v0.2.x)">

```go
rdb := harness.NewRedisClient(cfg)
h, err := harness.New(harness.Options{Config: cfg, Store: store, Redis: rdb})
// or, building the Infra yourself:
infra := harness.RedisInfra(rdb, store)
h, err = harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
```

</div>

<div class="tab" data-label="After (v0.3.x)">

```go
import "github.com/7ar-chat/harness/redisinfra"

rdb := redisinfra.NewClient(cfg)
infra := redisinfra.Infra(rdb, store)
h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
```

</div>

</div>

Notes:

- `redisinfra.NewClient(cfg)` reads `REDIS_*` and `USE_SENTINEL` exactly as `harness.NewRedisClient` did.
- `redisinfra.Infra(rdb, store)` returns a `harness.Infra` (event log, queue and bus, locks, job list, skill installs). It takes the same store you pass in `Options.Store`, because the event log is built over it.
- The old "set `Options.Infra` or `Options.Redis`, not both" error is gone with `Options.Redis`.
- `rdb` is a go-redis `redis.UniversalClient`; close it yourself.

On `v0.3.1` the "before" code stops compiling with `undefined: harness.NewRedisClient`. See [Redis](/docs/integrations/redis/) and [Postgres and Redis for several processes](/docs/guides/postgres-redis/).

### MCP: blank-import `mcpfeature`

MCP is now an implementation registered by package `mcpfeature`. If the configuration names MCP servers (`MCP_SERVERS`, `.mcp.json`, `mcp.servers` in `config.yml`) and the package is not linked, `harness.New` fails instead of ignoring them:

```text
harness: feature "mcp": harness: MCP servers are configured but MCP support is not linked: import _ "github.com/7ar-chat/harness/mcpfeature"
```

A configuration with no MCP servers installs nothing and needs no import. The fix is one line:

```go
import _ "github.com/7ar-chat/harness/mcpfeature"
```

`harness`, `harness-web` and `harness-worker` already import it. A project server that is waiting for approval (`harness trust mcp`) counts as configured for this check.

### OpenSandbox: blank-import the adapter

`SANDBOX_BACKEND=opensandbox` (or just `OPENSANDBOX_URL`) selects a backend that registers itself when its package is imported. Without the import:

```text
unknown sandbox backend "opensandbox" (SANDBOX_BACKEND); registered: local, none; import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" to register it
```

```go
import _ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"
```

The `local` and `none` backends need no import. Your own backend still registers with `backends.Register` or goes in `Options.Backend` ([Write a sandbox backend](/docs/guides/sandbox-backend/)).

### All three together

This program compiles against the workspace and uses the three opt-ins:

```go
package main

import (
	"fmt"
	"log"

	"github.com/7ar-chat/harness"
	"github.com/7ar-chat/harness/adapters/store/memstore"
	"github.com/7ar-chat/harness/config"
	"github.com/7ar-chat/harness/redisinfra"

	_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox" // SANDBOX_BACKEND=opensandbox
	_ "github.com/7ar-chat/harness/mcpfeature"                   // MCP servers
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	store := memstore.New()
	rdb := redisinfra.NewClient(cfg)
	defer rdb.Close()
	infra := redisinfra.Infra(rdb, store)
	h, err := harness.New(harness.Options{Config: cfg, Store: store, Infra: &infra})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("built %T\n", h)
}
```

## v0.2.0: `product` and the commands move to `app`

The team product and the three commands left the root module for a new module, `github.com/7ar-chat/harness/app`. The root's `go.mod` lost 26 modules (OIDC, JOSE, the terminal UI libraries, Sentry) that only they used, so embedding the harness no longer pulls them in. Nothing imports `app`.

| Was (root module) | Now (`app` module) |
|---|---|
| `github.com/7ar-chat/harness/product` | `github.com/7ar-chat/harness/app/product` |
| `cmd/harness`, `cmd/harness-web`, `cmd/harness-worker`, `cmd/bench` | `app/cmd/harness`, `app/cmd/harness-web`, `app/cmd/harness-worker`, `app/cmd/bench` |

```bash
# before
go install github.com/7ar-chat/harness/cmd/harness@latest
# after
go install github.com/7ar-chat/harness/app/cmd/harness@latest
```

In a checkout, build with `cd app && go build ./cmd/harness-web` (the Makefile and the Dockerfile do). Package `hosts/worker` (the worker library) stays in the root module; only its command moved. `app` requires the root by version, so importing `app/product` pulls in its dependencies.

## v0.1.0: tagged modules and the module path

- Before the first tag the repository moved to `github.com/7ar-chat/harness`. `ai/v0.1.0` was published under the old path (`github.com/GhaziBenDahmane/harness/ai`) and cannot be fetched under the new one, so `ai/go.mod` retracts it: use `ai/v0.1.1`.
- Each module in a subdirectory has its own tag (`core/v0.1.0`, ...). A tag is permanent; a bad release is fixed by a new patch version and a `retract`. See [Releasing and versioning](/docs/guides/releasing/).

## Module tags

The latest tag of each module at HEAD (`git tag`, and `scripts/release-modules.sh check`):

| Module | Import path | Latest tag |
|---|---|---|
| root (engine, features, facade, HTTP API, stores) | `github.com/7ar-chat/harness` | `v0.3.1` |
| `ai` | `github.com/7ar-chat/harness/ai` | `ai/v0.1.1` (`ai/v0.1.0` is retracted) |
| `core` | `github.com/7ar-chat/harness/core` | `core/v0.1.0` |
| `modelinfo` | `github.com/7ar-chat/harness/modelinfo` | `modelinfo/v0.1.0` |
| `hosts/client` | `github.com/7ar-chat/harness/hosts/client` | `hosts/client/v0.1.0` |
| `hosts/tui` | `github.com/7ar-chat/harness/hosts/tui` | `hosts/tui/v0.1.0` |
| `adapters/sandbox` | `github.com/7ar-chat/harness/adapters/sandbox` | `adapters/sandbox/v0.1.0` |
| `app` (product and commands) | `github.com/7ar-chat/harness/app` | `app/v0.2.0` (requires root `v0.3.0`) |

`go list -m -versions github.com/7ar-chat/harness` lists `v0.1.0 v0.2.0 v0.2.1 v0.3.0 v0.3.1`. The module map is in [Module map](/docs/introduction/modules/).

## How to upgrade

1. `go get github.com/7ar-chat/harness@v0.3.1` (and each standalone module you import directly).
2. Run `go build ./...`. Replace `harness.NewRedisClient`, `harness.RedisInfra` and `Options.Redis` as shown above; change `product` imports to `app/product`.
3. If you use MCP or OpenSandbox, add the blank imports now, or `harness.New` fails at start.
4. Run your tests. Both v0.3.0 failures happen in `harness.New`, not at the first request, so a start-up smoke test catches them.
5. Pin `go.mod` and deploy a tag, not a branch. See [Production checklist](/docs/operations/production/#9-version-pinning).

<div class="callout note">

**`go get` can say `unknown revision` right after a release.** The module proxy needs a minute to see a new tag. Wait and retry; do not re-tag. See [Troubleshooting](/docs/operations/troubleshooting/#go-get-invalid-version-unknown-revision).

</div>
