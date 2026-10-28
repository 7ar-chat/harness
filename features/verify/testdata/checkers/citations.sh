#!/usr/bin/env bash
# Fixture of check_citations.py SOURCE.pdf... DELIVERABLE (checker contract):
# logs "citations: <args>" in $CHECK_LOG; a deliverable containing NOCITE has
# no citation, INVENTED fails numbers_on_page, CRASH exits 1; else ok.
echo "citations: $*" >> "${CHECK_LOG:?}"
f="${@: -1}"
if grep -q CRASH "$f"; then echo "Traceback: boom" >&2; exit 1; fi
if grep -q NOCITE "$f"; then
  echo '{"ok": true, "file": "'"$f"'", "checks": [{"name": "citations", "ok": true, "detail": "no (p. N) citation found: nothing to check"}], "citations": []}'
  exit 0
fi
if grep -q INVENTED "$f"; then
  echo '{"ok": false, "file": "'"$f"'", "checks": [{"name": "citations", "ok": true, "detail": "2 page citations checked"}, {"name": "numbers_on_page", "ok": false, "detail": "38 (p. 19): not on p. 19 (on no page of the source)"}], "next_step": "use the figure printed on the cited page", "citations": [{"ok": false}, {"ok": true}]}'
  exit 2
fi
echo '{"ok": true, "file": "'"$f"'", "checks": [{"name": "citations", "ok": true, "detail": "2 page citations checked"}, {"name": "numbers_on_page", "ok": true, "detail": ""}], "citations": [{"ok": true}, {"ok": true}]}'
