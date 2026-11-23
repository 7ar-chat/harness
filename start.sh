#!/usr/bin/env bash
# One command to run the whole team app: ./start.sh (see docs/quickstart.md). Subcommands: up logs stop down restart pull dev.
exec "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/scripts/install.sh" "$@"
