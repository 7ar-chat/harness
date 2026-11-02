#!/usr/bin/env bash
# Statement coverage of the production code, over every module of the workspace, with a floor.
#
#   scripts/coverage.sh [MIN_PERCENT]      MIN_PERCENT defaults to $COVERAGE_MIN, else 80
#
# A statement counts as covered when ANY test of ANY module executes it (-coverpkg names every production package, so the
# engine is credited for what the product's and the hosts' tests drive through it, as a user of the code would see it).
# Test support packages (fakes, conformance suites, examples, generators, benchmarks) are not production code and are
# left out. The profiles are merged by taking, per statement block, the highest count.
#
# Postgres tests run when HARNESS_TEST_DATABASE_URL is set, as in CI; without it they skip and the total is lower.
# Writes coverage.out (merged profile) and prints the packages with the most uncovered statements.
set -euo pipefail
cd "$(dirname "$0")/.."
min=${1:-${COVERAGE_MIN:-80}}
out=${COVERAGE_OUT:-coverage.out}
modules="${COVERAGE_MODULES:-. ai core modelinfo hosts/client hosts/tui adapters/sandbox app}"
# Not production code: fakes and conformance suites used by tests, runnable examples, generators and the benchmark/long-run drivers.
skip='/(clienttest|llmtest|sandboxtest|conformance|testdata)(/|$)|/examples/|/cmd/gen-ts|/cmd/bench|/longtest(/|$)'

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pkgs=$(for m in $modules; do (cd "$m" && go list ./... ); done | sort -u | grep -Ev "$skip" | paste -sd, -)
[ -n "$pkgs" ] || { echo "coverage: no packages found" >&2; exit 2; }

i=0
for m in $modules; do
  i=$((i + 1))
  echo "== $m"
  (cd "$m" && go test ./... -count=1 -covermode=atomic -coverpkg="$pkgs" -coverprofile="$tmp/$i.out")
done

# Merge: one line per statement block, with the highest count any test reached ("mode:" first, as go tool cover requires).
{
  echo "mode: atomic"
  awk '
    /^mode:/ { next }
    {
      key = $1; n = $2; c = $3
      if (!(key in stmts)) { stmts[key] = n; count[key] = c } else if (c > count[key]) count[key] = c
    }
    END { for (k in stmts) print k, stmts[k], count[k] }
  ' "$tmp"/*.out | sort
} > "$out"

awk -v min="$min" '
  /^mode:/ { next }
  {
    split($1, a, ":"); file = a[1]; n = $2 + 0; covered = ($3 + 0 > 0) ? n : 0
    sub(/[^\/]*$/, "", file); pkg = file; sub(/^github.com\/7ar-chat\/harness\//, "", pkg); sub(/\/$/, "", pkg)
    if (pkg == "") pkg = "(root)"
    total[pkg] += n; cov[pkg] += covered; T += n; C += covered
  }
  END {
    printf "\n%-46s %7s %7s %6s\n", "package (most uncovered first)", "stmts", "uncov", "cov%"
    for (p in total) printf "%d\t%s\t%d\t%.0f\n", total[p] - cov[p], p, total[p], 100 * cov[p] / total[p] | "sort -rn | head -25"
    close("sort -rn | head -25")
    pct = 100 * C / T
    printf "\ntotal: %d/%d statements = %.1f%% (minimum %s%%)\n", C, T, pct, min
    if (pct + 0 < min + 0) { print "FAIL: coverage is below the minimum"; exit 1 }
  }
' "$out" | awk -F'\t' 'NF == 4 { printf "%-46s %7d %7d %5d%%\n", $2, $3, $1, $4; next } { print }'
exit "${PIPESTATUS[0]}"
