---
title: "Releasing and versioning"
description: "How the repository's eight Go modules are tagged and released bottom-up, how to verify a release as a user would, and what versions you get when you import the harness."
---

The repository is a root Go module plus seven modules in subdirectories. Each module has its own version, so a program that imports one needs a **tag per module**. This page is for maintainers cutting a release and for users who want to know which versions they get. The tag for the root module is `vX.Y.Z`; for any other it is the directory plus the version, and Go resolves `github.com/7ar-chat/harness/core@v0.1.0` from the git tag `core/v0.1.0`.

## The modules

| Module | Tag | Requires |
|---|---|---|
| `ai` | `ai/vX.Y.Z` | third-party only |
| `core` | `core/vX.Y.Z` | |
| `modelinfo` | `modelinfo/vX.Y.Z` | |
| `hosts/client` | `hosts/client/vX.Y.Z` | core, modelinfo |
| `adapters/sandbox` | `adapters/sandbox/vX.Y.Z` | core |
| `hosts/tui` | `hosts/tui/vX.Y.Z` | core, modelinfo, hosts/client |
| root: engine, features, facade, HTTP API, stores | `vX.Y.Z` | all of the above |
| `app`: the product and the commands | `app/vX.Y.Z` | the root and all of the above |

Which module to import for what is on the [Module map](/docs/introduction/modules/).

Inside the checkout the modules reference each other through `go.work`, so `go build ./...` always uses the local source. **On `main`, the `go.mod` files require tagged versions of each other and have no `replace` lines**, so a module resolves the same way for you and for someone importing it. After changing a lower module, release it before the modules above it can use the change outside the workspace. The dependency direction is fixed: `layers_test.go` pins which module may require which, and a module that requires a sibling it did not declare fails the test.

## What you get as a user

Versions are independent, so the latest tags differ per module. At v0.3.1 of the root (checked with `scripts/release-modules.sh check`):

```text
ai                 latest tag: v0.1.1
core               latest tag: v0.1.0
modelinfo          latest tag: v0.1.0
hosts/client       latest tag: v0.1.0
adapters/sandbox   latest tag: v0.1.0
hosts/tui          latest tag: v0.1.0
.                  latest tag: v0.3.1
app                latest tag: v0.2.0
```

Asking for the root module pulls the matching versions of the others. From an empty module with no workspace, `go get github.com/7ar-chat/harness@v0.3.1` resolves to:

```text
github.com/7ar-chat/harness v0.3.1
github.com/7ar-chat/harness/adapters/sandbox v0.1.0
github.com/7ar-chat/harness/ai v0.1.1
github.com/7ar-chat/harness/core v0.1.0
github.com/7ar-chat/harness/hosts/client v0.1.0
github.com/7ar-chat/harness/modelinfo v0.1.0
```

The root and `ai` also have older tags; `ai/v0.1.0` is retracted in `ai/go.mod` because it was published under the previous module path and cannot be fetched under the current one. The upgrade story for the root (what v0.3 removed: `Options.Redis`, `harness.NewRedisClient`, `harness.RedisInfra`; MCP and OpenSandbox now opt-in by blank import) is in [Upgrade notes](/docs/operations/upgrade-notes/).

<div class="callout tip">

**The `ai` module on its own** is the lightest import: it needs only third-party modules. See [Use the ai module on its own](/docs/guides/use-ai/). `core` and `modelinfo` are small too: [Use core and modelinfo on their own](/docs/guides/use-core-modelinfo/).

</div>

## Releasing a set of changes

Go bottom-up, one layer at a time. A module can be verified only once everything it requires is tagged **and visible to Go**.

1. **Layer 1: `ai`, `core`, `modelinfo`.** From the repository root run `make test-modules`, then tag and push:

   ```bash
   git tag -a core/v0.2.0 -m "core v0.2.0" && git push origin core/v0.2.0
   ```

2. **Layer 2: `hosts/client`, `adapters/sandbox`.** In each, require the new tag, then verify against the published code with the workspace off:

   ```bash
   cd hosts/client
   go mod edit -require=github.com/7ar-chat/harness/core@v0.2.0
   GOWORK=off GOFLAGS=-mod=mod go mod tidy && GOWORK=off go vet ./... && GOWORK=off go test ./...
   ```

   Commit, push, tag.

3. **Layer 3: `hosts/tui`.** The same, requiring the layer 1 and 2 tags.
4. **Root.** Require every tag, run `GOWORK=off go build ./... && GOWORK=off go test ./...`, commit, push, tag `vX.Y.Z`.
5. **`app`.** Require the root tag and the rest, verify with the workspace off, commit, push, tag `app/vX.Y.Z`. Nothing imports `app`, so it can be released whenever its own code changes.

`scripts/release-modules.sh` automates the mechanical steps and works layer by layer:

| Subcommand | What it does |
|---|---|
| `check` | the latest tag of each module and how many local `replace` lines it still has |
| `prepare VERSION` | rewrite `go.mod` files to require tagged versions of the siblings and drop local `replace` lines |
| `verify` | `go mod tidy`, `go vet` and `go test` of every module alone with `GOWORK=off` |
| `tag VERSION` | create the tags (`<dir>/vVERSION`, the root `vVERSION`); `MODULES="..."` limits it |
| `push` | `git push origin --tags` |

<div class="callout note">

**CI is red between a root API change and the matching `app` commit.** `app` requires the root by version, and `make test-modules` builds it with the workspace off, so a commit that removes or renames something `app` uses cannot pass that step until the root is tagged and `app` requires the new tag. Expect the `go` job to fail on the commit that changes the root's API and on its release tag, and to pass on the `app: require the root vX.Y.Z` commit that follows. Land them close together; a change that does not touch what `app` imports is unaffected. The `embed` job of the tag, which builds only from published versions, is the check that matters for the release itself.

</div>

## Rules

- **A tag is permanent.** The module proxy caches it, so never move or delete a published tag. To fix a bad release publish the next patch version and `retract` the bad one in that module's `go.mod`.
- **Verify against the published code.** `GOWORK=off` is the point: with the workspace on, a module can build against a sibling's unreleased source and pass while being unusable for everyone else. `make test-modules` does this for the modules, and the `embed` CI job does it for the root.
- **Dependency direction is fixed** (see above).
- **A new tag can take a minute to resolve.** Right after a push `go get` may say `unknown revision` while GitHub's caches catch up. Wait and retry; do not re-tag.

## Check a release as a user would

From an empty directory, with a clean module cache and no `replace`:

```bash
mkdir /tmp/try && cd /tmp/try && go mod init example.com/try
GOMODCACHE=$(mktemp -d) GOWORK=off go get github.com/7ar-chat/harness@vX.Y.Z
```

`make check-embed` does this for the root module (`scripts/check-embed.sh [VERSION]`, default the latest `v*` tag; `make check-embed VERSION=main` checks the branch head). It builds a small program, `scripts/embedcheck/main.go.txt`, in an empty directory with a clean module cache, `GOWORK=off` and no `replace`; the program creates a `harness.Harness` and runs one conversation through it using only published versions, then lists the 7ar-chat modules it depends on. CI runs the same script on every `v*` tag (the `embed` job).
