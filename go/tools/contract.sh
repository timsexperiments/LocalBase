#!/bin/bash
# Runs the contract suite and keeps the evidence of a failing run: the full
# `go test -json` stream is written to a timestamped file, kept (and its path
# printed) when the run fails, and removed when it passes. A failure without
# output then still leaves its events behind.
#
#   tools/contract.sh [go test flags...] [-args -impl=both]
#
# LOCALBASE_CONTRACT_LOGS chooses the directory (default: $TMPDIR/localbase-contract-runs).
set -uo pipefail
cd "$(dirname "$0")/.."
dir=${LOCALBASE_CONTRACT_LOGS:-${TMPDIR:-/tmp}/localbase-contract-runs}
mkdir -p "$dir"
log="$dir/contract-$(date -u +%Y%m%dT%H%M%SZ)-$$.jsonl"
go test -json ./contract -count=1 "$@" > "$log"
code=$?
# Show the run as go test would (package results and failures).
if command -v jq >/dev/null; then
  jq -r 'select(.Action=="output") | .Output' "$log" | grep -E '^(--- |FAIL|ok |PASS|panic|\s+\S+_test\.go:)' || true
else
  grep -o '"Output":"[^"]*"' "$log" | sed 's/^"Output":"//; s/"$//; s/\\n$//; s/\\t/\t/g' | grep -E '^(--- |FAIL|ok |PASS|panic|\s+\S+_test\.go:)' || true
fi
if [ "$code" -ne 0 ]; then
  echo "contract run failed (exit $code); full go test -json output kept at $log" >&2
else
  rm -f "$log"
fi
exit "$code"
