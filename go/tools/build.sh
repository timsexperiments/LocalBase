#!/bin/bash
# The one build recipe. scripts/release/build.sh and `make build` both run this, so a binary
# built here has the same bytes as the one CI ships, given the same commit, Go version and
# target. Run from anywhere; the binary goes to $1 (default ./local-base).
#
#   tools/build.sh [output]                     for this machine
#   GOOS=linux GOARCH=arm64 tools/build.sh out  for another target
#   VERSION=0.3.0 tools/build.sh out            a version other than the source's
#   tools/build.sh --version                    print the version in the source
#
# The version comes from internal/version unless VERSION says otherwise; commit and date come
# from git, so nothing depends on when or where it is built. The path is the one in go.mod's
# module, which is why a clone under $GOPATH/src/github.com/timsexperiments/LocalBase and any
# other checkout agree.
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)

source_version() {
  sed -nE 's/^[[:space:]]*(const |var )?Version[[:space:]]*= "([^"]+)"$/\2/p' "$here/internal/version/version.go"
}

if [ "${1:-}" = "--version" ]; then
  source_version
  exit 0
fi

out=$(cd "$(dirname "${1:-local-base}")" && pwd)/$(basename "${1:-local-base}")
cd "$here"
version=${VERSION:-$(source_version)}
[ -n "$version" ] || { echo "build: cannot read the version from internal/version/version.go" >&2; exit 1; }
pkg=github.com/timsexperiments/LocalBase/go/internal/version
flags="-s -w -X $pkg.Version=$version -X $pkg.Commit=$(git rev-parse HEAD) -X $pkg.Date=$(git show -s --format=%cI HEAD)"
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$flags" -o "$out" ./cmd/localbase
echo "$out"
