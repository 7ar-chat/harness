---
title: "Install"
description: "Add the harness to a Go program, install the command-line tools, or build every binary from a checkout."
---

There are two ways to get the harness: as a Go library you import (`go get`), or as the programs `harness`, `harness-web` and `harness-worker` that live in the `app` module. This page covers both, and what your build links depending on the packages you import.

## As a library

```bash
go get github.com/7ar-chat/harness@latest
```

The root module is the engine: `harness.New`, the features, the HTTP API and the stores. It requires the smaller modules (`ai`, `core`, `modelinfo`, `hosts/client`, `adapters/sandbox`), which `go get` resolves for you from their own tags. See the [module map](/docs/introduction/modules/) for what each is for.

**Go version.** Every `go.mod` of the repository says `go 1.26.0`, so you need Go 1.26 or newer. The `Dockerfile` builds with `golang:1.26-bookworm`.

Then continue with the [smallest embedding](/docs/quickstart/embed/), which runs one conversation offline with a scripted model.

### What your build links

A build links the packages you import and what they import, not the whole repository. Measured on the program from [Smallest embedding](/docs/quickstart/embed/) (scripted model, memory store), the dependency list holds only the harness modules and a handful of small libraries (`uuid`, a JSON-schema reflector, `yaml.v3`, `env`, `x/text`), and the binary is about 16.5 MB on macOS arm64 without `-ldflags='-s -w'`.

Three parts that carry heavy SDKs are opt-in, so they are not in that binary:

| You use | Import | Not linked otherwise |
|---|---|---|
| Redis shared by several processes | `github.com/7ar-chat/harness/redisinfra` | go-redis |
| MCP servers | `_ "github.com/7ar-chat/harness/mcpfeature"` | the MCP SDK |
| `SANDBOX_BACKEND=opensandbox` | `_ "github.com/7ar-chat/harness/adapters/sandbox/opensandbox"` | the OpenSandbox SDK |

Postgres (`adapters/store/pgstore`), S3 (`adapters/blobs/s3blobs`) and Sentry (`adapters/errtrack`) are likewise separate packages that are linked only when you import them. The product (OIDC, projects) and the terminal UI libraries are in the `app` and `hosts/tui` modules, so an embedding does not carry them.

<div class="callout breaking">

**v0.3.x.** `Options.Redis`, `harness.NewRedisClient` and `harness.RedisInfra` were removed. Use `redisinfra.NewClient(cfg)` and `redisinfra.Infra(rdb, store)`, passed as `Options.Infra`. See [Upgrade notes](/docs/operations/upgrade-notes/).

</div>

## The command-line tools

The programs are in the `app` module. `go install` builds one of them at the latest `app` tag:

```bash
go install github.com/7ar-chat/harness/app/cmd/harness@latest
go install github.com/7ar-chat/harness/app/cmd/harness-web@latest
```

<div class="callout warning">

**`harness-web` from `go install` has no browser UI.** The React app in `web/` is built by `make web` into `hosts/httpapi/ui`, which is not committed. A binary installed this way serves the API, and `GET /` answers with a 404 and the text "The web UI is not built into this binary: run make web, then rebuild." To get the UI, build from a checkout as shown below.

</div>

## Build from a checkout

```bash
git clone https://github.com/7ar-chat/harness
cd harness
make build
```

`make build` runs, from the `app` directory, `go build -trimpath -ldflags='-s -w'` (smaller binaries: the symbol table and DWARF are dropped) for three commands:

| Output | Source | What it is |
|---|---|---|
| `bin/harness` | `app/cmd/harness` | Terminal UI and print mode: [Run the CLI and TUI](/docs/quickstart/cli/) |
| `bin/harness-web` | `app/cmd/harness-web` | The HTTP API and browser UI: [Run the web product](/docs/quickstart/web/) |
| `bin/bench` | `app/cmd/bench` | The coding benchmark runner |

`bin/` is ignored by git. `make build` does not build `harness-worker`; build it yourself when you split web and workers:

```bash
cd app && go build -trimpath -ldflags='-s -w' -o ../bin/harness-worker ./cmd/harness-worker
```

To include the browser UI in `bin/harness-web`, build the React app first. It needs Node and npm:

```bash
make web      # npm ci && npm run build, into hosts/httpapi/ui
make build
```

Other targets you may want: `make test` (root module, then every module alone with the workspace off), `make vet`, and `make check-embed`, which builds the embedding program against the published root module from an empty directory.

## Check the install

```bash
bin/harness -h        # lists the flags
bin/harness-web -h
```

Next: [the smallest embedding](/docs/quickstart/embed/), or [the CLI](/docs/quickstart/cli/) if you want to try the agent first.
