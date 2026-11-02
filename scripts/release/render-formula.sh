#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Prints the Homebrew formula for a release from its checksums.txt.
#   scripts/release/render-formula.sh VERSION CHECKSUMS
set -euo pipefail

version=$1
checksums=$2
template="$(cd "$(dirname "$0")/../.." && pwd)/packaging/homebrew/local-base.rb.tmpl"

sum() {
  local value
  value="$(awk -v name="$1" '$2 == name { print $1 }' "$checksums")"
  [ -n "$value" ] || { echo "render-formula: no checksum for $1" >&2; exit 1; }
  printf '%s' "$value"
}

sed -e "s/@VERSION@/$version/g" \
  -e "s/@SHA_MACOS_ARM64@/$(sum local-base-macos-arm64.zip)/" \
  -e "s/@SHA_MACOS_X64@/$(sum local-base-macos-x64.zip)/" \
  -e "s/@SHA_LINUX_ARM64@/$(sum local-base-linux-arm64.tar.gz)/" \
  -e "s/@SHA_LINUX_X64@/$(sum local-base-linux-x64.tar.gz)/" \
  "$template"
