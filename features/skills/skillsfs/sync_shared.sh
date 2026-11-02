#!/usr/bin/env bash
# Copies the shared skill helpers (work/skills/shared/*, the one source of truth) over their
# copies in the built-in skills (builtin/<skill>/scripts/<name>): a skill never depends on
# another one being loaded, so each ships its own copy. Edit shared/<name>, then run this;
# docs_flags_test.go (go test ./skills/) fails when a copy drifts.
#
#   bash work/skills/sync_shared.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
for src in "$here"/shared/*; do
  name="$(basename "$src")"
  for dst in "$here"/builtin/*/scripts/"$name"; do
    [ -e "$dst" ] && install -m 0644 "$src" "$dst"
  done
done
