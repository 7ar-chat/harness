#!/usr/bin/env bash
# Proves the harness can be embedded from outside this repository: builds a small program in an empty directory, with a clean
# module cache, GOWORK off and no replace directive, against the published root module, and runs one conversation through it.
#
#   scripts/check-embed.sh [VERSION]      VERSION defaults to the latest tag (v0.1.0, or "main" for the branch head)
set -euo pipefail
cd "$(dirname "$0")/.."
version=${1:-$(git tag -l 'v[0-9]*' | sort -V | tail -1)}
tmp=$(mktemp -d)
# The module cache is read-only (hence -modcacherw below); cleanup must not decide the result of the check.
trap 'chmod -R u+w "$tmp" 2>/dev/null || true; rm -rf "$tmp" 2>/dev/null || true' EXIT
mkdir -p "$tmp/app" && cp scripts/embedcheck/main.go.txt "$tmp/app/main.go"
cd "$tmp/app"
export GOMODCACHE="$tmp/modcache" GOFLAGS=-modcacherw GOWORK=off GOPROXY=${GOPROXY:-direct} GONOSUMDB='*'
go mod init example.com/embedcheck >/dev/null
echo "embedding github.com/7ar-chat/harness@$version"
go get "github.com/7ar-chat/harness@$version" >/dev/null 2>&1 || { sleep 20; go get "github.com/7ar-chat/harness@$version"; }
go mod tidy >/dev/null
if grep -q '^replace' go.mod; then echo "unexpected replace directive"; exit 1; fi
go run .
echo "modules the app depends on:"
go list -m all | grep 7ar-chat | sed 's/^/  /'
echo "third-party modules: $(go list -m all | grep -vc '7ar-chat\|^example.com')"
