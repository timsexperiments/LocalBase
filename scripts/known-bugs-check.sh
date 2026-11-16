#!/bin/sh
# Fails if a test of a known bug still skips for a pull request that has merged (its
# fix should have removed the skip), or skips without naming one. Needs gh, and
# GH_TOKEN in CI. Run from anywhere in the repository.
set -eu
cd "$(dirname "$0")/.."
dirs="test/chaos internal/runtime"
calls=$(grep -rhoE 'knownBug\(t, [A-Za-z0-9]+,' $dirs | sed -E 's/knownBug\(t, ([A-Za-z0-9]+),/\1/' | sort -u)
status=0
for pr in $calls; do
  case "$pr" in
    pr) continue ;; # the helper's own signature
    noPRYet)
      echo "a skip names no pull request yet:" >&2
      grep -rn 'knownBug(t, noPRYet' $dirs >&2
      status=1
      continue
      ;;
  esac
  state=$(gh pr view "$pr" --json state --jq .state)
  case "$state" in
    MERGED)
      echo "#$pr has merged, so this skip should be gone:" >&2
      grep -rn "knownBug(t, $pr," $dirs >&2
      status=1
      ;;
    CLOSED)
      echo "#$pr was closed without merging; these skips need a new pull request:" >&2
      grep -rn "knownBug(t, $pr," $dirs >&2
      status=1
      ;;
    *) echo "#$pr: $state" ;;
  esac
done
exit "$status"
