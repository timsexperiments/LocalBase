#!/bin/bash
# The one build recipe. scripts/release/build.sh and `make build` both run this, so a binary
# built here has the same bytes as the one CI ships, given the same commit, Go version and
# target. Run from anywhere; the binary goes to $1 (default ./local-base).
#
#   scripts/build.sh [output]                     for this machine
#   GOOS=linux GOARCH=arm64 scripts/build.sh out  for another target
#   VERSION=0.3.0 scripts/build.sh out            a version other than the source's
#   scripts/build.sh --version                    print the version in the source
#   scripts/build.sh --goflags | --ldflags | --env
#                                               print what a build passes to go build: the flags,
#                                               -ldflags (for VERSION, if set) and environment.
#                                               tools/upgrade-rehearsal builds from these.
#
# The version comes from internal/version unless VERSION says otherwise; commit and date come
# from git, so nothing depends on when or where it is built. The path is the one in go.mod's
# module, which is why a clone under $GOPATH/src/github.com/timsexperiments/LocalBase and any
# other checkout agree.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
goflags="-trimpath -buildvcs=false"
env="CGO_ENABLED=0"

source_version() {
  sed -nE 's/^[[:space:]]*(const |var )?Version[[:space:]]*= "([^"]+)"$/\2/p' "$here/internal/version/version.go"
}

ldflags() {
  local version pkg
  version=${VERSION:-$(source_version)}
  [ -n "$version" ] || { echo "build: cannot read the version from internal/version/version.go" >&2; exit 1; }
  pkg=github.com/timsexperiments/LocalBase/go/internal/version
  echo "-s -w -X $pkg.Version=$version -X $pkg.Commit=$(git -C "$here" rev-parse HEAD) -X $pkg.Date=$(git -C "$here" show -s --format=%cI HEAD)"
}

case "${1:-}" in
  --version) source_version; exit 0 ;;
  --goflags) echo "$goflags"; exit 0 ;;
  --ldflags) ldflags; exit 0 ;;
  --env) echo "$env"; exit 0 ;;
esac

out=$(cd "$(dirname "${1:-local-base}")" && pwd)/$(basename "${1:-local-base}")
flags=$(ldflags)
cd "$here"
# shellcheck disable=SC2086 # goflags is a list of separate flags.
env $env go build $goflags -ldflags "$flags" -o "$out" ./cmd/localbase
echo "$out"
