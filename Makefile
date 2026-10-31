# Small, stripped binaries: -s -w drops the symbol table and DWARF (44 MB -> 30 MB for cmd/harness).
GOFLAGS_BUILD = -trimpath -ldflags='-s -w'

# The standalone Go modules next to the root one (see go.work); each is also built and tested alone, with GOWORK=off.
MODULES = ai core modelinfo hosts/client hosts/tui adapters/sandbox

.PHONY: build test test-modules bench-offline vet web ci gen
build:
	go build $(GOFLAGS_BUILD) -o bin/harness ./cmd/harness
	go build $(GOFLAGS_BUILD) -o bin/harness-web ./cmd/harness-web
	go build $(GOFLAGS_BUILD) -o bin/bench ./cmd/bench

vet:
	gofmt -l . | (! grep .) && go vet ./... && for m in $(MODULES); do (cd $$m && go vet ./...) || exit 1; done

test:
	go test ./...
	$(MAKE) test-modules

# Every module on its own, workspace off: proves it builds with only the modules it declares.
test-modules:
	for m in $(MODULES); do (cd $$m && GOWORK=off go vet ./... && GOWORK=off go test ./...) || exit 1; done

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
