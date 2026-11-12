#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Proves the repo guard rejects what it exists to reject, on a throwaway repo.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/check-repo-hygiene.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/lb-guard-test.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q
mkdir -p .github/workflows go
printf 'name: ok\npermissions:\n  contents: read\n' > .github/workflows/ok.yml
printf 'text\n' > ok.txt
git add -A

sha=0123456789abcdef0123456789abcdef01234567
pass=0

expect() {
  local want=$1 label=$2 got=0
  "$script" > out.log 2>&1 || got=$?
  if { [ "$want" = pass ] && [ "$got" -ne 0 ]; } || { [ "$want" = fail ] && [ "$got" -eq 0 ]; }; then
    printf 'FAILED: %s (exit %s)\n' "$label" "$got" >&2
    cat out.log >&2
    exit 1
  fi
  rm -f out.log
  pass=$((pass + 1))
}


expect pass "clean tree"

printf 'stub\n' > go/fakebackend
git add -A
expect fail "named build output"
git rm -qf --cached go/fakebackend && rm go/fakebackend

printf '\000\001\002binary' > tool.dat
git add -A
expect fail "binary file"
printf 'tool.dat\n' > .github/repo-guard-allowlist.txt
git add -A
expect pass "allowlisted binary"
git rm -qf --cached tool.dat .github/repo-guard-allowlist.txt && rm -f tool.dat .github/repo-guard-allowlist.txt

head -c 1048577 /dev/zero | tr '\0' 'a' > big.txt
git add -A
expect fail "large text file"
git rm -qf --cached big.txt && rm big.txt

printf 'name: bad\njobs: {}\n' > .github/workflows/bad.yml
git add -A
expect fail "workflow without permissions"
printf 'name: bad\npermissions: {}\njobs:\n  a:\n    steps:\n      - uses: actions/checkout@v4\n' > .github/workflows/bad.yml
expect fail "action pinned by tag"
printf 'name: ok\npermissions: {}\njobs:\n  a:\n    steps:\n      - uses: actions/checkout@%s # v4\n      - uses: ./.github/actions/local\n' "$sha" > .github/workflows/bad.yml
expect pass "action pinned by SHA and a local action"
git rm -qf --cached .github/workflows/bad.yml && rm .github/workflows/bad.yml

mkdir -p .github/actions/local
printf 'name: local\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@v6\n' > .github/actions/local/action.yml
git add -A
expect fail "local action using an action pinned by tag"
printf 'name: local\nruns:\n  using: composite\n  steps:\n    - uses: actions/setup-go@%s # v6\n' "$sha" > .github/actions/local/action.yml
expect pass "local action pinned by SHA, with no permissions block"

printf 'repo guard test: %s cases pass\n' "$pass"
