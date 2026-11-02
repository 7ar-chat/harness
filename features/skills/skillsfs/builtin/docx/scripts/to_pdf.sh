#!/usr/bin/env bash
# Convert an office document (docx, doc, odt, rtf, pptx, xlsx, html...) to PDF with
# LibreOffice headless, using a private throw-away profile (safe to run in parallel with
# other soffice processes). Prints {"pdf": "...", "pages": N} on stdout.
#
# Usage: bash to_pdf.sh INPUT [OUTDIR]
#   OUTDIR defaults to the input's directory. Env SOFFICE_TIMEOUT (seconds, default 180).
set -euo pipefail

if [[ $# -lt 1 || "$1" == "-h" || "$1" == "--help" ]]; then
  sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
  exit 0
fi

in="$1"
[[ -f "$in" ]] || { echo "error: input not found: $in" >&2; exit 1; }
outdir="${2:-$(dirname "$in")}"
mkdir -p "$outdir"

profile="$(mktemp -d /tmp/lo_profile.XXXXXX)"
log="$(mktemp /tmp/soffice_log.XXXXXX)"
trap 'rm -rf "$profile" "$log"' EXIT

if ! timeout "${SOFFICE_TIMEOUT:-180}" soffice "-env:UserInstallation=file://$profile" \
     --headless --norestore --nolockcheck --nodefault \
     --convert-to pdf --outdir "$outdir" "$in" >"$log" 2>&1; then
  echo "error: soffice failed or timed out:" >&2
  cat "$log" >&2
  exit 1
fi

base="$(basename "$in")"
pdf="$outdir/${base%.*}.pdf"
if [[ ! -s "$pdf" ]]; then
  echo "error: no PDF produced for $in" >&2
  cat "$log" >&2
  exit 1
fi
pages="$(pdfinfo "$pdf" 2>/dev/null | awk '/^Pages:/{print $2}')"
python3 -c 'import json,sys; print(json.dumps({"pdf": sys.argv[1], "pages": int(sys.argv[2] or 0)}))' "$pdf" "${pages:-0}"
