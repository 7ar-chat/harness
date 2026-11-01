# Contributing

Thanks for looking. The project is a Go monorepo (several Go modules joined by `go.work`) with a React web UI in `web/`.
`README.md` explains what it is; `docs/architecture.md` explains how it is layered.

## Setup

- Go 1.26 and Node 22 (the web UI). Docker is only needed for the Postgres-backed tests.
- `make build` builds `bin/harness`, `bin/harness-web` and `bin/bench`. `make web` builds the browser UI into `hosts/httpapi/ui`.

## Before you open a pull request

```sh
make vet     # gofmt, go vet, in every module
make test    # go test ./... in the root, then each module on its own with the workspace off
make ci      # what CI runs, including the web UI's typecheck, tests and build
```

- **Tests for behaviour.** A change to behaviour comes with a test that fails without it. Tests check what a caller can see:
  results, boundaries, errors. They do not pin wording, wiring or incidental defaults.
- **Coverage is gated** at 80% over all modules (`make coverage`). The Postgres tests run when `HARNESS_TEST_DATABASE_URL` points at a
  database and skip otherwise; CI runs them.
- **Layers are enforced.** `layers_test.go` fixes which package may import which (`core/` imports only `core/`, and so on), and which
  module may require which. A change that crosses a layer fails that test.
- **Wire types.** After changing a DTO of `hosts/client`, a view in `app/product` or a type they embed, run `make gen`; a stale
  `web/src/api/generated.ts` fails `go test ./cmd/gen-ts`.
- **Docs.** A change to behaviour, an environment variable or an API updates the doc that describes it (`README.md`, `docs/`).

## Pull requests

Keep a pull request to one change, and say in the description what it does and how you checked it. Describe a new dependency and why
the standard library or an existing one will not do. Changes to the sandbox, secrets, authentication, the MCP guard or the agent
factory's policy get the closest review: say what threat you considered.

CI runs on every pull request. Because the repository is public, a first pull request from a new contributor needs a maintainer to
approve the workflow run.

## Reporting problems

Bugs and ideas: open an issue and fill in the template. **Security problems: do not open an issue**, follow `SECURITY.md`.

## Releases

Maintainers only: each module is tagged separately, bottom-up, as described in `docs/releasing.md`.

## License

By contributing you agree that your contribution is licensed under the MIT license of the repository.
