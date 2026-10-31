#!/usr/bin/env bash
# Release the Go modules of this repository, bottom-up.
#
#   scripts/release-modules.sh check            what is tagged, and which modules still point at local replaces
#   scripts/release-modules.sh prepare VERSION  rewrite go.mod files to require VERSION of the sibling modules and drop
#                                               the local replace lines (go.work keeps working in the checkout)
#   scripts/release-modules.sh verify           build, vet and test every module alone (GOWORK=off) against the
#                                               versions it requires - run after "prepare", before tagging
#   scripts/release-modules.sh tag VERSION      create the tags (modules/v... style: <dir>/vVERSION, root: vVERSION)
#   scripts/release-modules.sh push             push the tags
#
# Order matters: a module can only be verified once everything it requires is tagged and pushed, so release in layers
# (see LAYERS) and run prepare/verify/tag/push for each layer. "release LAYER VERSION" does one layer end to end.
set -euo pipefail
cd "$(dirname "$0")/.."

PREFIX=github.com/7ar-chat/harness
# Layers, lowest first. Each module may require only modules of lower layers.
LAYERS=(
  "ai core modelinfo"
  "hosts/client adapters/sandbox"
  "hosts/tui"
  "."
)
all_modules() { for l in "${LAYERS[@]}"; do for m in $l; do echo "$m"; done; done; }
modpath() { [ "$1" = . ] && echo "$PREFIX" || echo "$PREFIX/$1"; }
tagname() { [ "$1" = . ] && echo "v$2" || echo "$1/v$2"; }

cmd=${1:-}; shift || true
case "$cmd" in
check)
  git fetch -q --tags origin 2>/dev/null || true
  for m in $(all_modules); do
    last=$(git tag -l "$([ "$m" = . ] && echo 'v*' || echo "$m/v*")" | sed "s#^$m/##" | sort -V | tail -1)
    reps=$(grep -c '^replace .*=> \.' "$([ "$m" = . ] && echo go.mod || echo "$m/go.mod")" || true)
    printf '%-18s latest tag: %-10s local replaces: %s\n' "$m" "${last:--}" "$reps"
  done ;;
prepare)
  v=${1:?version}
  for m in $(all_modules); do
    f=$([ "$m" = . ] && echo go.mod || echo "$m/go.mod")
    for dep in $(all_modules); do
      [ "$dep" = "$m" ] && continue
      p=$(modpath "$dep")
      if grep -q "^[[:space:]]*$p " "$f" 2>/dev/null || grep -q "^require $p " "$f" 2>/dev/null; then
        # a dependency tagged in this release uses VERSION; one tagged earlier keeps the version its own tag has
        tagv=$(git tag -l "$([ "$dep" = . ] && echo 'v*' || echo "$dep/v*")" | sed "s#^$dep/##" | sort -V | tail -1)
        use=${tagv:-v$v}
        [ -n "${RELEASING:-}" ] && case " $RELEASING " in *" $dep "*) use=v$v;; esac
        (cd "$(dirname "$f")" && go mod edit -droprequire="$p" -dropreplace="$p" -require="$p@$use")
      fi
    done
  done ;;
verify)
  for m in $(all_modules); do
    echo "== $m"; (cd "$m" && GOWORK=off GOFLAGS=-mod=mod go mod tidy >/dev/null && GOWORK=off go vet ./... && GOWORK=off go test ./... 2>&1 | grep -vE '^(ok|\?)' || true)
  done ;;
tag)
  v=${1:?version}
  for m in ${MODULES:-$(all_modules)}; do git tag -a "$(tagname "$m" "$v")" -m "$m v$v"; done ;;
push) git push origin --tags ;;
*) sed -n '2,15p' "$0"; exit 2 ;;
esac
