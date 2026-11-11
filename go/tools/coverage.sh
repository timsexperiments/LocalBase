#!/bin/bash
# Statement coverage per package: unit tests plus the contract suite, which
# drives the Go binary from outside, so neither alone is the true figure.
#
#   tools/coverage.sh            # unit tests + contract -impl=go
#   tools/coverage.sh both       # + the TS differential and CLI tests; needs
#                                # LOCALBASE_CONTRACT_MODELS for the TS gateway
#
# Runs no real models unless LOCALBASE_CONTRACT_RUNTIMES is set. Prints the table
# and leaves merged.out in $COVERAGE_DIR (default: a new temp dir).
set -uo pipefail
cd "$(dirname "$0")/.."
impl=${1:-go}
dir=${COVERAGE_DIR:-$(mktemp -d)}
mkdir -p "$dir/bin"
go=$(go env GOROOT)/bin/go
# Packages with no tests fail to build under -coverprofile with a switched
# toolchain, so only packages with tests run; -coverpkg still counts the rest. The suites that need
# their own setup are left out of the unit pass: contract and system run through contract.sh below,
# and test/chaos (chaos.yml) here would run without its setup.
tested=$("$go" list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' ./... | grep -v -E '/test/contract$|/test/system$|/test/chaos$|third_party')
[ -n "$tested" ] || { echo "coverage: no packages with tests to run" >&2; exit 1; }
"$go" test $tested -count=1 -coverpkg=github.com/timsexperiments/LocalBase/go/... \
  -coverprofile="$dir/unit.out" -covermode=atomic >"$dir/unit.log" 2>&1 || { tail -30 "$dir/unit.log"; exit 1; }
LOCALBASE_CONTRACT_COVERDIR="$dir/bin" LOCALBASE_CONTRACT_LOGS="$dir/runs" \
  tools/contract.sh -args -impl="$impl" >"$dir/contract.log" 2>&1 || { tail -30 "$dir/contract.log"; exit 1; }
"$go" tool covdata textfmt -i="$dir/bin" -o="$dir/contract.out" || exit 1
"$go" run ./tools/covmerge -o "$dir/merged.out" "$dir/unit.out" "$dir/contract.out"
echo "profiles: $dir"
