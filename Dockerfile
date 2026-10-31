# harness-web: the team web app (API + embedded React UI).
FROM node:22-bookworm-slim AS web
WORKDIR /src
COPY web/package.json web/package-lock.json web/
RUN cd web && npm ci
COPY web web
# vite builds into ../hosts/httpapi/ui, which the Go binary embeds
RUN mkdir -p hosts/httpapi && cd web && npm run build

FROM golang:1.26-bookworm AS build
WORKDIR /src
# The module and the standalone modules it replaces (go.mod: replace ... => ./ai, ./core, ...) are needed to resolve dependencies.
COPY go.mod go.sum go.work go.work.sum ./
COPY ai/go.mod ai/go.sum* ai/
COPY core/go.mod core/go.sum* core/
COPY modelinfo/go.mod modelinfo/go.sum* modelinfo/
COPY hosts/client/go.mod hosts/client/go.sum* hosts/client/
COPY hosts/tui/go.mod hosts/tui/go.sum* hosts/tui/
COPY adapters/sandbox/go.mod adapters/sandbox/go.sum* adapters/sandbox/
RUN go mod download
COPY . .
COPY --from=web /src/hosts/httpapi/ui hosts/httpapi/ui
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/harness-web ./cmd/harness-web

FROM debian:bookworm-slim
# What the coding agent's local sandbox needs: a shell, git (project clones) and the usual tools.
RUN apt-get update && apt-get install -y --no-install-recommends \
      bash ca-certificates git ripgrep python3 curl openssh-client \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/harness-web /usr/local/bin/harness-web
ENV HOME=/root LOCAL_STATE_DIR=/data/state
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["harness-web"]
CMD ["-local", "-addr", "0.0.0.0:8080"]
