  commit="$(git -C "$repo" rev-parse HEAD)"
  date="$(git -C "$repo" show -s --format=%cI HEAD)"
#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Builds and packages Go release archives. Used by .github/workflows/release-go.yml
# and runnable locally; it writes only under OUTPUT.
#
#   scripts/release/build.sh binaries  OUTPUT   cross-compile every target
#   scripts/release/build.sh package   OUTPUT   archive OUTPUT/bin into OUTPUT/dist
#   scripts/release/build.sh checksums OUTPUT   write OUTPUT/dist/checksums.txt
#   scripts/release/build.sh version            print the version in the source
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
# Archive names match v0.1.0, so older copies of the installer keep working.
targets="macos-arm64:darwin:arm64 macos-x64:darwin:amd64 linux-x64:linux:amd64 linux-arm64:linux:arm64"
version_package=github.com/timsexperiments/LocalBase/go/internal/version

source_version() {
  local file=$repo/go/internal/version/version.go
  [ -f "$file" ] || file=$repo/go/internal/gateway/server.go
  sed -nE 's/^[[:space:]]*(const |var )?Version[[:space:]]*= "([^"]+)"$/\2/p' "$file"
}

binaries() {
  local output=$1 version commit date entry name goos goarch
  version="${LOCALBASE_VERSION:-$(source_version)}"
  [ -n "$version" ] || { echo "build: cannot read the version from the source" >&2; exit 1; }
  commit="$(git -C "$repo" rev-parse HEAD)"
  date="$(git -C "$repo" show -s --format=%cI HEAD)"
  mkdir -p "$output/bin"
  for entry in $targets; do
    IFS=: read -r name goos goarch <<< "$entry"
    (
      cd "$repo/go"
      CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch go build -trimpath -buildvcs=false \
        -ldflags "-s -w -X $version_package.Version=$version -X $version_package.Commit=$commit -X $version_package.Date=$date" \
        -o "$output/bin/local-base-$name" ./cmd/localbase
    )
    printf 'built %s\n' "local-base-$name"
  done
}

package() {
  local output=$1 entry name goos goarch staging archive
  mkdir -p "$output/dist"
  for entry in $targets; do
    IFS=: read -r name goos goarch <<< "$entry"
    staging="$(mktemp -d "${TMPDIR:-/tmp}/lb-package.XXXXXX")"
    cp "$output/bin/local-base-$name" "$staging/"
    chmod 755 "$staging/local-base-$name"
    cp "$repo/LICENSE" "$repo/README.md" "$staging/"
    cp "$repo/THIRD_PARTY_NOTICES.md" "$staging/THIRD_PARTY_NOTICES.txt"
    if [ "$goos" = darwin ]; then
      # Notarization accepts zip, not tar.gz.
      archive="$output/dist/local-base-$name.zip"
      rm -f "$archive"
      (cd "$staging" && zip -qX "$archive" ./*)
    else
      archive="$output/dist/local-base-$name.tar.gz"
      (cd "$staging" && COPYFILE_DISABLE=1 tar -czf "$archive" ./*)
    fi
    rm -rf "$staging"
    printf 'packaged %s\n' "$(basename "$archive")"
  done
}

checksums() {
  local dist=$1/dist
  (
    cd "$dist"
    rm -f checksums.txt
    if command -v sha256sum > /dev/null 2>&1; then
      sha256sum ./* | sed 's# \*\{0,1\}\./# #; s#  *# #; s# #  #' > "$TMPDIR_CHECKSUMS"
    else
      shasum -a 256 ./* | sed 's# \*\{0,1\}\./# #; s#  *# #; s# #  #' > "$TMPDIR_CHECKSUMS"
    fi
    mv "$TMPDIR_CHECKSUMS" checksums.txt
  )
}

command=${1:-}
case "$command" in
  version) source_version ;;
  binaries | package | checksums)
    [ $# -eq 2 ] || { echo "usage: build.sh $command OUTPUT" >&2; exit 2; }
    mkdir -p "$2"
    output="$(cd "$2" && pwd)"
    TMPDIR_CHECKSUMS="$output/.checksums.tmp"
    "$command" "$output"
    ;;
  *)
    echo "usage: build.sh binaries|package|checksums OUTPUT, or build.sh version" >&2
    exit 2
    ;;
esac
