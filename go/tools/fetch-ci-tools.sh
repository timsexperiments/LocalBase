#!/bin/bash
# Fetches the lint and secret-scan tools CI runs, at CI's versions, so that a local run
# sees what CI will. Into the directory you name, and nowhere else:
#   tools/fetch-ci-tools.sh <dir>
# Then, from go/:
#   <dir>/golangci-lint run --config .golangci.yml ./...
#   <dir>/gitleaks dir .. --config ../.gitleaks.toml
#
# Nothing is unpacked, let alone run, until its archive matches the sha256 pinned in
# tools/ci-tools.pins, and the first mismatch stops the script. This script never runs
# what it fetches. The archives stay in <dir>/.fetch, and the next run replaces them.
set -euo pipefail
cd "$(dirname "$0")/.."
if [ $# -ne 1 ]; then
  echo "usage: tools/fetch-ci-tools.sh <dir>" >&2
  exit 2
fi
pins=${LOCALBASE_CI_TOOLS_PINS:-tools/ci-tools.pins}
pin() { awk -v k="$1" '$1 == k { print $2 }' "$pins"; }
platform=$(go env GOOS)-$(go env GOARCH)
mkdir -p "$1/.fetch"
dest=$(cd "$1" && pwd)
work=$dest/.fetch
for tool in golangci-lint gitleaks; do
  url=$(pin "$tool-$platform-url")
  want=$(pin "$tool-$platform-sha256")
  member=$(pin "$tool-$platform-member")
  if [ -z "$url" ] || [ -z "$want" ] || [ -z "$member" ]; then
    echo "fetch-ci-tools: no pin for $tool on $platform" >&2
    exit 1
  fi
  curl -fsSL -o "$work/$tool.tar.gz" "$url"
  got=$(shasum -a 256 "$work/$tool.tar.gz" | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    echo "fetch-ci-tools: $tool checksum mismatch: got $got, pinned $want" >&2
    exit 1
  fi
  tar -xzf "$work/$tool.tar.gz" -C "$work" "$member"
  mv "$work/$member" "$dest/$tool"
  echo "$dest/$tool"
done
