#!/usr/bin/env bash
#
# coverage.sh runs the tests and prints what they cover, per package.
#
# Two profiles are merged. `go test` counts what runs in its own process.
# cmd/gophper's tests drive a binary built with -cover, which writes its
# counters to GOPHPER_TEST_COVERDIR instead (see cmd/gophper/cli_test.go).
# Read alone, the first profile reports cmd/gophper as untested.
#
# It writes coverage.txt (go test), coverage-bin.txt (the binary) and
# coverage-merged.txt (both). GOTESTFLAGS adds flags to `go test`. The exit
# status is go test's.

set -euo pipefail

cd "$(dirname "$0")"

covdir="$(mktemp -d)"
trap 'rm -rf "$covdir"' EXIT
# Git Bash on Windows: Go wants C:/..., not the /tmp/... that mktemp gives.
if command -v cygpath > /dev/null; then
    covdir="$(cygpath -m "$covdir")"
fi

# -count=1: a cached package runs no test, so its binary writes no counters.
#
# shellcheck disable=SC2086 # GOTESTFLAGS is a list of flags
GOPHPER_TEST_COVERDIR="$covdir" go test -count=1 ${GOTESTFLAGS:-} \
    -covermode=atomic -coverpkg=./... -coverprofile=coverage.txt ./...
go tool covdata textfmt -i="$covdir" -o=coverage-bin.txt

# The generator runs under go generate, not in the binary. Its output is
# checked by cmd/gophper's TestLicensesModules instead.
exclude='/cmd/gophper/internal/genlicenses/'

# A block is listed once per test binary that links it, so the counts are
# summed per block. A block counts as covered when any run reached it.
{
    echo "mode: atomic"
    tail -q -n +2 coverage.txt coverage-bin.txt | grep -v "$exclude" |
        awk '{ count[$1 " " $2] += $3 } END { for (k in count) print k, count[k] }' |
        sort
} > coverage-merged.txt

echo ""
echo "Statement coverage, both profiles merged:"
awk '
    NR == 1 { next }
    {
        split($1, loc, ":")
        pkg = loc[1]
        sub(/\/[^\/]*$/, "", pkg)
        all[pkg] += $2
        total += $2
        if ($3 > 0) { hit[pkg] += $2; covered += $2 }
    }
    END {
        for (p in all) printf "  %-45s %6.1f%%  (%d/%d)\n", p, 100 * hit[p] / all[p], hit[p], all[p] | "sort"
        close("sort")
        printf "  %-45s %6.1f%%  (%d/%d)\n", "total", 100 * covered / total, covered, total
    }
' coverage-merged.txt
