#!/bin/bash
# Runs the contract suite (go/test/contract and go/test/system) and keeps the evidence of a failing run: the full
# `go test -json` stream is written to a timestamped file, kept (and its path
# printed) when the run fails, and removed when it passes. A failure without
# output then still leaves its events behind.
#
#   tools/contract.sh [go test flags...] [-args -impl=both]
#
# LOCALBASE_CONTRACT_LOGS chooses the directory (default: $TMPDIR/localbase-contract-runs).
set -uo pipefail
cd "$(dirname "$0")/.."
. tools/disk-guard.sh
disk_guard contract
dir=${LOCALBASE_CONTRACT_LOGS:-${TMPDIR:-/tmp}/localbase-contract-runs}
mkdir -p "$dir"
log="$dir/contract-$(date -u +%Y%m%dT%H%M%SZ)-$$.jsonl"
# -p 1: one package at a time. Each launches its own shared targets (Go, and TS under -impl=both),
# and two at once could load real models side by side on a small runner.
go test -json -p 1 ./test/contract ./test/system -count=1 "$@" > "$log"
code=$?
# On CI nothing else shares the temp directory, so the run must leave none of the suites' own
# scratch directories in it: the harness's gateways, the CLI builds and the upgrade kit's binaries.
# Left on a shared machine, they filled its disk (33 GB by 2026-11-13); here a leak fails the run.
if [ -n "${CI:-}" ]; then
  left=$(find "${TMPDIR:-/tmp}" -maxdepth 1 \( -name 'lbgo-contract-*' -o -name 'lbgo-cli-bin-*' -o -name 'lbgo-cli-lab-bin-*' -o -name 'lbgo-upgrade-*' \) 2>/dev/null)
  if [ -n "$left" ]; then
    echo "contract run left its scratch directories in ${TMPDIR:-/tmp}:" >&2
    echo "$left" >&2
    [ "$code" -eq 0 ] && code=1
  fi
fi
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
