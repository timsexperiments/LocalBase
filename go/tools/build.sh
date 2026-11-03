#!/bin/bash
# The reproducible release build: the same commit and Go version give the same
# bytes. Run from anywhere; the binary goes to $1 (default ./local-base).
#
#   tools/build.sh [output]            # version from internal/version
#   VERSION=0.3.0 tools/build.sh dist/local-base
#
# Commit and date come from git, so nothing depends on when or where it is built.
# The path is the one in go.mod's module, which is why a clone under
# $GOPATH/src/github.com/timsexperiments/LocalBase and any other checkout agree.
set -euo pipefail
out=$(cd "$(dirname "${1:-local-base}")" && pwd)/$(basename "${1:-local-base}")
cd "$(dirname "$0")/.."
pkg=github.com/timsexperiments/LocalBase/go/internal/version
flags="-X $pkg.Commit=$(git rev-parse HEAD) -X $pkg.Date=$(git show -s --format=%cI HEAD)"
if [ -n "${VERSION:-}" ]; then flags="$flags -X $pkg.Version=$VERSION"; fi
CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$flags" -o "$out" ./cmd/localbase
echo "$out"
