#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Writes THIRD_PARTY_NOTICES.md from packaging/notices-header.md and the license
# files of every Go module linked into any release target.
#   scripts/release/generate-notices.sh          write the file
#   scripts/release/generate-notices.sh --check  fail if the file is out of date
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
target=$repo/THIRD_PARTY_NOTICES.md
output="$(mktemp "${TMPDIR:-/tmp}/lb-notices.XXXXXX")"
modules="$(mktemp "${TMPDIR:-/tmp}/lb-modules.XXXXXX")"
trap 'rm -f "$output" "$modules"' EXIT

cd "$repo/go"
for platform in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  GOOS=${platform%/*} GOARCH=${platform#*/} CGO_ENABLED=0 go list -deps \
    -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/localbase
done | LC_ALL=C sort -u > "$modules"

cat "$repo/packaging/notices-header.md" > "$output"
while read -r path version dir; do
  found=0
  printf '\n### %s %s\n' "$path" "$version" >> "$output"
  while IFS= read -r file; do
    found=1
    printf '\n`%s`:\n\n````text\n' "$(basename "$file")" >> "$output"
    cat "$file" >> "$output"
    printf '\n````\n' >> "$output"
  done < <(find "$dir" -maxdepth 1 -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'PATENTS*' \) | LC_ALL=C sort)
  if [ "$found" -eq 0 ]; then
    echo "generate-notices: no license file for $path" >&2
    exit 1
  fi
done < "$modules"

if [ "${1:-}" = --check ]; then
  cmp -s "$output" "$target" || { echo "THIRD_PARTY_NOTICES.md is out of date; run scripts/release/generate-notices.sh" >&2; exit 1; }
else
  cp "$output" "$target"
fi
