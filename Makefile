# Small, stripped binaries: -s -w drops the symbol table and DWARF (44 MB -> 30 MB for app/cmd/harness).
GOFLAGS_BUILD = -trimpath -ldflags='-s -w'

# The standalone Go modules next to the root one (see go.work); each is also built and tested alone, with GOWORK=off.
MODULES = ai core modelinfo hosts/client hosts/tui adapters/sandbox app

.PHONY: build test test-modules coverage e2e check-embed bench-offline vet web ci gen
build:
	cd app && go build $(GOFLAGS_BUILD) -o ../bin/harness ./cmd/harness
	cd app && go build $(GOFLAGS_BUILD) -o ../bin/harness-web ./cmd/harness-web
	cd app && go build $(GOFLAGS_BUILD) -o ../bin/bench ./cmd/bench

vet:
	gofmt -l . | (! grep .) && go vet ./... && for m in $(MODULES); do (cd $$m && go vet ./...) || exit 1; done

test:
	go test ./...
	$(MAKE) test-modules   # includes app/ (the product, the commands), run alone

# Every module on its own, workspace off: proves it builds with only the modules it declares.
test-modules:
	for m in $(MODULES); do (cd $$m && GOWORK=off go vet ./... && GOWORK=off go test ./...) || exit 1; done

# What .github/workflows/ci.yml runs; pgstore tests run when HARNESS_TEST_DATABASE_URL points at a Postgres.
ci: vet test
	cd web && npm ci && npm run typecheck && npm test && npm run build

# Statement coverage of the production code over every module, failing below COVERAGE_MIN (default 80). Postgres tests count
# when HARNESS_TEST_DATABASE_URL is set (CI sets it). Writes coverage.out; `go tool cover -html=coverage.out` shows the gaps.
coverage:
	bash scripts/coverage.sh $(COVERAGE_MIN)

# End-to-end scenarios on the real stack with a scripted model: the HTTP conversation, tool runs and artifact versions, approvals,
# cancel and steer, user isolation and auth, restart, the web+worker split over Postgres and Redis, the CLI, and the browser UI.
# The Postgres suites run when HARNESS_TEST_DATABASE_URL is set, the browser run needs the built UI (make web) and Chromium.
e2e:
	cd app && go test -race -count=1 -timeout 5m ./integration/...
	cd web && npm run e2e

# Embeds the harness from an empty directory with a clean module cache and no replace directives, against the PUBLISHED
# root module (default: the latest v* tag; make check-embed VERSION=main for the branch head), and runs a conversation.
check-embed:
	bash scripts/check-embed.sh $(VERSION)

# Everything the benchmark proves without a model: fixtures fail untouched and pass with their solutions.
bench-offline:
	go test ./bench ./longtest

# web/src/api/generated.ts holds the TypeScript types of the wire format, written from the Go types (app/cmd/gen-ts).
# Run this after changing a DTO of hosts/client, a view of product or a type they embed; `go test ./cmd/gen-ts` in app/
# (part of `make test` and CI) fails while the committed file is stale.
gen:
	cd app && go run ./cmd/gen-ts -o ../web/src/api/generated.ts

# The browser UI (web/) builds into hosts/httpapi/ui, which the Go binary embeds.
web:
	find hosts/httpapi/ui -mindepth 1 -not -name .gitkeep -delete
	cd web && npm ci && npm run build
