# Building, CI and status

## Building

`make build` produces stripped binaries in `bin/` (`app/cmd/harness` is about 30 MB; a plain `go build` is 44 MB
because it keeps the symbol table and DWARF). Idle resident memory is about 20 MB. An application that embeds
the harness links none of go-redis, the MCP SDK or the OpenSandbox SDK unless it imports `redisinfra`, `mcpfeature` or
`adapters/sandbox/opensandbox` (a test pins this); the engine, sandbox, features and store packages do not import go-redis.

CI (`.github/workflows/ci.yml`, every push and pull request) runs `gofmt`, `go vet` and `go test ./...` against a
Postgres service, the web checks (`typecheck`, `test`, `build`) and a Docker build plus a `docker-compose.prod.yml`
validation. `make ci` runs the Go and web steps locally. After changing a DTO of `hosts/client` or a view of `product`, run
`make gen`: `go test ./cmd/gen-ts` (run in `app/`) fails while `web/src/api/generated.ts` is stale.

## Status / known gaps

* Requires Go 1.26 (`go test ./...`). `adapters/store/pgstore` tests need a Postgres (`HARNESS_TEST_DATABASE_URL`) and skip otherwise.
* A remote sandbox's project skills are read once per run, when the run first uses its sandbox (or at prompt time if
  one is already running), so a skill added to `/workspace` mid-run is seen by the next run. When the run itself
  starts the sandbox, the project skills it holds (fenced as untrusted, like the prompt's) are announced to the model
  in a note before its next model call, once per run, because the prompt never starts a sandbox just to list them.
* Web search is not built in: enable one from the MCP marketplace (Exa ships in the catalog and is on by default, see [docs/marketplace.md](marketplace.md)) or configure any web-search MCP server in `MCP_SERVERS`.
* The sandbox image (`sandbox-image/Dockerfile`) was built and smoke-tested on 2026-10-30 for linux/arm64 and linux/amd64 (see [sandbox-image/README.md](../sandbox-image/README.md)); it has not yet been run under a live OpenSandbox lifecycle server.

