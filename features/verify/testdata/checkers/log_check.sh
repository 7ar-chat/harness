#!/usr/bin/env bash
# Fixture checker (checker contract) that logs its call: one line "args: …" in
# $CHECK_LOG and, for --baseline FILE, the line "baseline: <FILE's content>".
# Prints {"ok": true} with one check and exits 0.
log="${CHECK_LOG:?}"
echo "args: $*" >> "$log"
file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) echo "baseline: $(cat "$2")" >> "$log"; shift 2 ;;
    *) file="$1"; shift ;;
  esac
done
echo '{"ok": true, "file": "'"$file"'", "checks": [{"name": "overlap", "ok": true, "detail": ""}]}'
