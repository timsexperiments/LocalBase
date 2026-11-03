#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Proves the signing job guard rejects what it exists to reject, on throwaway workflows.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/check-signing-jobs.sh"
work="$(mktemp -d "${TMPDIR:-/tmp}/lb-signing-guard.XXXXXX")"
trap 'rm -rf "$work"' EXIT
cd "$work"
mkdir -p .github/workflows

expect() { # pass|fail name body
  printf '%s\n' "$3" > .github/workflows/case.yml
  if "$script" >/dev/null 2>&1; then got=pass; else got=fail; fi
  [ "$got" = "$1" ] || { echo "$2: expected $1, got $got" >&2; exit 1; }
}

signing='  sign:
    runs-on: macos-14
    steps:
      - uses: actions/download-artifact@0000000000000000000000000000000000000000 # v1
      - env:
          KEY: ${{ secrets.APPLE_CERTIFICATE }}
        run: security import key.p12'

expect pass "a job with the key and only Apple's tools" "jobs:
$signing"
expect pass "a job with no key can build" "jobs:
  build:
    runs-on: macos-14
    steps:
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v1
      - run: bun run build
$signing"
expect pass "a comment about bun isn't running it" "jobs:
$signing
      # never bun install here"
expect pass "a word that contains one isn't one" "jobs:
$signing
      - run: security create-keychain -p x y"
expect pass "the team id alone isn't the key" "jobs:
  qualify:
    runs-on: macos-14
    steps:
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v1
      - env:
          TEAM_ID: \${{ secrets.APPLE_TEAM_ID }}
        run: bun run qualify"
expect fail "a checkout beside the key" "jobs:
$signing
      - uses: actions/checkout@0000000000000000000000000000000000000000 # v1"
expect fail "a toolchain beside the key" "jobs:
$signing
      - uses: oven-sh/setup-bun@0000000000000000000000000000000000000000 # v1"
expect fail "a build beside the key" "jobs:
$signing
      - run: bun run build"
expect fail "a make beside the key" "jobs:
$signing
      - run: make all"
expect fail "the notarizing password beside a build" "jobs:
  notarize:
    runs-on: macos-14
    steps:
      - env:
          PASSWORD: \${{ secrets.APPLE_ID_PASSWORD }}
        run: go build ./..."
expect fail "the key's passphrase beside a build" "jobs:
  sign:
    runs-on: macos-14
    steps:
      - run: npm ci
      - env:
          PASS: \${{ secrets.APPLE_CERTIFICATE_PASS }}
        run: security import key.p12"
expect fail "a later job in the file" "jobs:
  first:
    runs-on: ubuntu-24.04
    steps:
      - run: echo fine
  sign:
    runs-on: macos-14
    steps:
      - run: cmake --build .
      - env:
          KEY: \${{ secrets.APPLE_CERTIFICATE }}
        run: security import key.p12"
echo "signing job guard: all cases behaved"
