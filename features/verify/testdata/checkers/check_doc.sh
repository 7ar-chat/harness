#!/usr/bin/env bash
# Fixture checker following the checker contract (harness v3 §4): prints
# {"ok", "file", "checks", "next_step"?} and exits 0 (ok), 2 (not ok) or
# 1 (usage error / missing file). A file containing BROKEN fails the
# "headings" check; CRASH makes the checker crash.
f="$1"
if [ -z "$f" ] || [ ! -f "$f" ]; then
  echo '{"ok": false, "file": "'"$f"'", "checks": []}'
  exit 1
fi
if grep -q CRASH "$f"; then
  echo "Traceback (most recent call last): boom" >&2
  exit 1
fi
if grep -q BROKEN "$f"; then
  echo '{"ok": false, "file": "'"$f"'", "checks": [{"name": "opens", "ok": true, "detail": ""}, {"name": "headings", "ok": false, "detail": "Heading 3 directly after Heading 1"}], "next_step": "fix the heading levels, then run the checker again"}'
  exit 2
fi
echo '{"ok": true, "file": "'"$f"'", "checks": [{"name": "opens", "ok": true, "detail": ""}, {"name": "headings", "ok": true, "detail": ""}]}'
exit 0
