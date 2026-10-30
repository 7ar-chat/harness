# Small, stripped binaries: -s -w drops the symbol table and DWARF (44 MB -> 30 MB for cmd/harness).
GOFLAGS_BUILD = -trimpath -ldflags='-s -w'

.PHONY: build test bench-offline vet web ci gen
build:
	go build $(GOFLAGS_BUILD) -o bin/harness ./cmd/harness
	go build $(GOFLAGS_BUILD) -o bin/harness-web ./cmd/harness-web
	go build $(GOFLAGS_BUILD) -o bin/bench ./cmd/bench

vet:
	gofmt -l . | (! grep .) && go vet ./...

test:
	go test ./...

# What .github/workflows/ci.yml runs; pgstore tests run when HARNESS_TEST_DATABASE_URL points at a Postgres.
ci: vet test
	cd web && npm ci && npm run typecheck && npm test && npm run build

# Everything the benchmark proves without a model: fixtures fail untouched and pass with their solutions.
bench-offline:
	go test ./bench ./longtest

# web/src/api/generated.ts holds the TypeScript types of the wire format, written from the Go types (cmd/gen-ts).
# Run this after changing a DTO of hosts/client, a view of product or a type they embed; `go test ./cmd/gen-ts`
# (part of `make test` and CI) fails while the committed file is stale.
gen:
	go run ./cmd/gen-ts

# The browser UI (web/) builds into hosts/httpapi/ui, which the Go binary embeds.
web:
	find hosts/httpapi/ui -mindepth 1 -not -name .gitkeep -delete
	cd web && npm ci && npm run build
