# Small, stripped binaries: -s -w drops the symbol table and DWARF (44 MB -> 30 MB for cmd/harness).
GOFLAGS_BUILD = -trimpath -ldflags='-s -w'

.PHONY: build test bench-offline vet web
build:
	go build $(GOFLAGS_BUILD) -o bin/harness ./cmd/harness
	go build $(GOFLAGS_BUILD) -o bin/harness-web ./cmd/harness-web
	go build $(GOFLAGS_BUILD) -o bin/bench ./cmd/bench

vet:
	gofmt -l . | (! grep .) && go vet ./...

test:
	go test ./...

# Everything the benchmark proves without a model: fixtures fail untouched and pass with their solutions.
bench-offline:
	go test ./bench ./longtest

# The browser UI (web/) builds into hosts/web/ui, which the Go binary embeds.
web:
	cd web && npm ci && npm run build
