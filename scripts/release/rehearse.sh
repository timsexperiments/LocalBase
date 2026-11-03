#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Runs the release workflow's local steps against local artifacts: version and
# changelog checks, notices, cross-compile, archives, SBOM, checksums, the Homebrew
# formula, release notes, and the installer test on this machine. Publishes nothing.
#   scripts/release/rehearse.sh [OUTPUT] [PORT]      needs `make bootstrap` for syft
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
export PATH="$repo/.cache/tools/bin:$PATH"
output=${1:-}
port=${2:-26010}
keep=1
if [ -z "$output" ]; then
  output="$(mktemp -d "${TMPDIR:-/tmp}/lb-rehearsal.XXXXXX")"
  keep=0
fi
mkdir -p "$output"
output="$(cd "$output" && pwd)"
cleanup() { [ "$keep" = 1 ] || rm -rf "$output"; }
trap cleanup EXIT
step() { printf '\n== %s\n' "$1"; }
fail() { printf 'REHEARSAL FAILED: %s\n' "$1" >&2; exit 1; }
warnings=0
warn() { printf 'warning: %s\n' "$1"; warnings=$((warnings + 1)); }
cd "$repo"
command -v syft > /dev/null 2>&1 || fail "syft is missing; run make bootstrap"

step "version and changelog, as the tag would check them"
version="$(scripts/release/build.sh version)"
[ -n "$version" ] || fail "no version in the source"
printf 'source version %s, so the tag would be v%s\n' "$version" "$version"
heading="$(grep -m1 "^## \[$version\]" CHANGELOG.md || true)"
[ -n "$heading" ] || fail "CHANGELOG.md has no entry for $version"
case "$heading" in *Unreleased*) warn "the changelog entry has no date yet: $heading" ;; esac
git rev-parse -q --verify "refs/tags/v$version" > /dev/null && warn "tag v$version already exists locally"

step "notices are current"
scripts/release/generate-notices.sh --check

step "cross-compile"
scripts/release/build.sh binaries "$output"

step "archives"
scripts/release/build.sh package "$output"
for archive in "$output"/dist/local-base-*; do
  case "$archive" in
    *.zip) listing="$(unzip -Z1 "$archive")" ;;
    *) listing="$(tar -tzf "$archive" | sed 's#^\./##')" ;;
  esac
  for want in LICENSE README.md THIRD_PARTY_NOTICES.txt; do
    printf '%s\n' "$listing" | grep -qx "$want" || fail "$want is missing from $(basename "$archive")"
  done
  name="$(basename "$archive")"
  name=${name%.zip}
  name=${name%.tar.gz}
  printf '%s\n' "$listing" | grep -qx "$name" || fail "$name is missing from $(basename "$archive")"
done
echo "4 archives hold the binary, LICENSE, README.md and notices"

step "binary architectures"
for pair in "macos-arm64:arm64" "macos-x64:x86_64" "linux-x64:x86-64" "linux-arm64:aarch64"; do
  target=${pair%%:*}
  description="$(file "$output/bin/local-base-$target")"
  printf '%s\n' "$description" | grep -q "${pair##*:}" || fail "local-base-$target is the wrong architecture: $description"
done
echo "all four match their names"

step "stamping"
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) native=macos-arm64 ;;
  Darwin-x86_64) native=macos-x64 ;;
  Linux-x86_64) native=linux-x64 ;;
  Linux-aarch64) native=linux-arm64 ;;
  *) fail "no native binary for $(uname -s)-$(uname -m)" ;;
esac
binary=$output/bin/local-base-$native
chmod 755 "$binary"
[ "$("$binary" --version)" = "$version" ] || fail "--version is not exactly $version"
if stamped="$("$binary" version 2> /dev/null)"; then
  printf '%s\n' "$stamped"
  case "$stamped" in
    *"$(git rev-parse --short=12 HEAD)"*) ;;
    *) fail "the build isn't stamped with this commit" ;;
  esac
else
  warn "this tree has no \`version\` command, so commit stamping wasn't checked"
fi

step "SBOM"
syft scan "file:$output/bin/local-base-linux-x64" --source-name local-base --source-version "$version" \
  -o "spdx-json=$output/dist/local-base.spdx.json" > /dev/null 2>&1
jq -e '.packages | map(.name) | index("modernc.org/sqlite")' "$output/dist/local-base.spdx.json" > /dev/null ||
  fail "the SBOM doesn't list modernc.org/sqlite"
echo "$(jq '.packages | length' "$output/dist/local-base.spdx.json") packages, including modernc.org/sqlite"

step "installer, checksums and Homebrew formula"
cp scripts/install.sh "$output/dist/install.sh"
scripts/release/build.sh checksums "$output"
scripts/release/render-formula.sh "$version" "$output/dist/checksums.txt" > "$output/dist/local-base.rb"
if command -v ruby > /dev/null 2>&1; then
  ruby -c "$output/dist/local-base.rb" > /dev/null
else
  warn "no ruby here, so the formula's syntax wasn't checked"
fi
if grep -q '@' "$output/dist/local-base.rb"; then fail "the formula has an unfilled placeholder"; fi
scripts/release/build.sh checksums "$output"
(
  cd "$output/dist"
  if command -v sha256sum > /dev/null 2>&1; then sha256sum -c checksums.txt > /dev/null; else shasum -a 256 -c checksums.txt > /dev/null; fi
)
echo "checksums verify for $(wc -l < "$output/dist/checksums.txt" | tr -d ' ') files"

step "release notes"
notes="$(awk -v version="$version" '
  index($0, "## [" version "]") == 1 { found = 1; next }
  found && /^## \[/ { exit }
  found { print }
' CHANGELOG.md)"
[ -n "$notes" ] || fail "the release notes are empty"
echo "$(printf '%s\n' "$notes" | wc -l | tr -d ' ') lines from CHANGELOG.md"

step "installer test on this machine's archive"
LOCALBASE_EXPECT_VERSION=$version scripts/release/test-installer.sh "$output/dist" "$port"

cat <<'NOTE'

== passed, with these still only provable by a real tag run

- macOS signing, notarization and the Gatekeeper assessment of the signed binary
- provenance and SBOM attestations, and `gh attestation verify` against them
- creating the draft release, and publishing it as "latest"
- the workflow's tag and permission wiring, including its secrets
- the installer downloading from the real release URLs
- smoke tests on the platforms other than this machine, which PR CI covers unsigned
NOTE
printf '\nWarnings: %s\n' "$warnings"
