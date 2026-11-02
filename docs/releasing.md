# Releasing

The repository is a root Go module plus seven modules in subdirectories (`go.work` joins them for local work). A program
that imports any of them needs a **tag per module**: Go resolves `github.com/7ar-chat/harness/core@v0.1.0` from the git tag
`core/v0.1.0`, and the root module from `v0.1.0`.

| Module | Tag | Requires |
|---|---|---|
| `ai` | `ai/vX.Y.Z` | (third-party only) |
| `core` | `core/vX.Y.Z` | |
| `modelinfo` | `modelinfo/vX.Y.Z` | |
| `hosts/client` | `hosts/client/vX.Y.Z` | core, modelinfo |
| `adapters/sandbox` | `adapters/sandbox/vX.Y.Z` | core |
| `hosts/tui` | `hosts/tui/vX.Y.Z` | core, modelinfo, hosts/client |
| root (the engine, features, facade, HTTP API, stores) | `vX.Y.Z` | all of the above |
| `app` (the product and the commands) | `app/vX.Y.Z` | the root and all of the above |

Inside the checkout the modules reference each other through `go.work`, so `go build ./...` always uses the local source.
**On `main`, the `go.mod` files require tagged versions of each other and have no `replace` lines**, so a module resolves
the same way for you and for someone importing it. After changing a lower module, release it (below) before the modules
above it can use the change outside the workspace.

## Releasing a set of changes

Go bottom-up, one layer at a time: a module can be verified only once everything it requires is tagged **and visible to Go**.

1. **Layer 1: `ai`, `core`, `modelinfo`.** From the repository root: `make test-modules`, then tag and push:

   ```sh
   git tag -a core/v0.2.0 -m "core v0.2.0" && git push origin core/v0.2.0
   ```
2. **Layer 2: `hosts/client`, `adapters/sandbox`.** In each, require the new tag, then verify against the published code
   with the workspace off:

   ```sh
   cd hosts/client
   go mod edit -require=github.com/7ar-chat/harness/core@v0.2.0
   GOWORK=off GOFLAGS=-mod=mod go mod tidy && GOWORK=off go vet ./... && GOWORK=off go test ./...
   ```
   Commit, push, tag.
3. **Layer 3: `hosts/tui`.** Same, requiring the layer 1 and 2 tags.
4. **Root.** Require every tag, `GOWORK=off go build ./... && GOWORK=off go test ./...`, commit, push, tag `vX.Y.Z`.
5. **`app`.** Require the root tag and the rest, verify with the workspace off, commit, push, tag `app/vX.Y.Z`. Nothing imports `app`, so it can be released whenever its own code changes.

`scripts/release-modules.sh check` lists the latest tag of each module and how many local `replace` lines it still has.

**Release the root and `app` in the order that keeps the tag's `go` job green.** The `go` job runs `app` with the workspace off,
against the root version `app/go.mod` names. A root tag is cut from a commit where `app` still names the previous root tag, so a
root tag whose release changes anything `app`'s tests exercise is red on its own `go` job until `app` is moved onto it.
If a root release carries test-only or app-invisible changes, that does not happen. Otherwise tag the root, move `app` onto the
tag in the next commit, tag `app/vX.Y.Z` there, and expect the root tag's `go` job to show the older `app` result; the
`app` tag's run is the one that proves the pair.

**CI is red between a root API change and the matching `app` commit.** `app` requires the root by version, and `make test-modules`
builds it with the workspace off, so a commit that removes or renames something `app` uses cannot pass that step until the
root is tagged and `app` requires the new tag. Expect the `go` job to fail on the commit that changes the root's API and on
its release tag, and to pass on the `app: require the root vX.Y.Z` commit that follows. Land them close together; a change
that does not touch what `app` imports is unaffected. (The tag's `embed` job, which builds only from published
versions, is the check that matters for the release itself.)

Rules:

- **A tag is permanent.** The module proxy caches it, so never move or delete a published tag. To fix a bad release, publish
  the next patch version and `retract` the bad one in that module's `go.mod`.
- **Verify against the published code.** `GOWORK=off` is the whole point: with the workspace on, a module can build against
  a sibling's unreleased source and pass while being unusable for everyone else. `make test-modules` does this for the
  modules, and the `embed` CI job does it for the root.
- **Dependency direction is fixed.** `layers_test.go` pins which module may require which; a module that requires a sibling it
  did not declare fails the test.
- **A new tag can take a minute to resolve.** Right after a push, `go get` may say `unknown revision` while GitHub's caches
  catch up. Wait and retry; do not re-tag.

## Checking a release as a user would

From an empty directory, with a clean module cache and no `replace`:

```sh
mkdir /tmp/try && cd /tmp/try && go mod init example.com/try
GOMODCACHE=$(mktemp -d) GOWORK=off go get github.com/7ar-chat/harness@vX.Y.Z
```

`make check-embed` does this for the root module: it builds a small program that creates a `harness.Harness` and runs one
conversation through it, using only published versions.
