#!/bin/sh
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Installs LocalBase from a GitHub release archive after verifying its checksum.
# It never uses sudo and writes only to the install and license directories.
set -eu

if [ "${1:-}" = --help ] || [ "${1:-}" = -h ]; then
  cat <<'HELP'
Install LocalBase from its GitHub release archive.

LOCALBASE_VERSION      release to install (default: latest)
LOCALBASE_INSTALL_DIR  CLI destination (default: ~/.local/bin)
LOCALBASE_LICENSE_DIR  license destination (default: ~/.local/share/local-base/licenses)
LOCALBASE_RELEASE_URL  download from this base URL instead of GitHub

The previous binary, if any, is kept as local-base.previous for rollback.
HELP
  exit 0
fi

die() {
  echo "LocalBase installer: $1" >&2
  exit 1
}

repo=timsexperiments/LocalBase
case "$(uname -s)" in
  Darwin) os=macos ;;
  Linux) os=linux ;;
  *) die "unsupported operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=x64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac

version=${LOCALBASE_VERSION:-latest}
if [ -n "${LOCALBASE_RELEASE_URL:-}" ]; then
  release_url=$LOCALBASE_RELEASE_URL
elif [ "$version" = latest ]; then
  release_url="https://github.com/$repo/releases/latest/download"
else
  case "$version" in v*) tag=$version ;; *) tag="v$version" ;; esac
  release_url="https://github.com/$repo/releases/download/$tag"
fi

name="local-base-$os-$arch"
if [ "$os" = macos ]; then archive="$name.zip"; else archive="$name.tar.gz"; fi
install_dir=${LOCALBASE_INSTALL_DIR:-"$HOME/.local/bin"}
license_dir=${LOCALBASE_LICENSE_DIR:-"$HOME/.local/share/local-base/licenses"}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/local-base-install.XXXXXX")
trap 'rm -rf "$tmp"' 0 HUP INT TERM

curl -fsSL "$release_url/$archive" -o "$tmp/$archive" || die "could not download $release_url/$archive"
curl -fsSL "$release_url/checksums.txt" -o "$tmp/checksums.txt" || die "could not download checksums.txt"
awk -v name="$archive" '$2 == name { print $1 "  " $2 }' "$tmp/checksums.txt" > "$tmp/archive.sha256"
[ -s "$tmp/archive.sha256" ] || die "checksum entry missing for $archive"
if command -v shasum > /dev/null 2>&1; then
  (cd "$tmp" && shasum -a 256 -c archive.sha256 > /dev/null) || die "checksum mismatch for $archive"
elif command -v sha256sum > /dev/null 2>&1; then
  (cd "$tmp" && sha256sum -c archive.sha256 > /dev/null) || die "checksum mismatch for $archive"
else
  die "install shasum or sha256sum to verify downloads"
fi

mkdir "$tmp/unpacked"
if [ "$os" = macos ]; then
  unzip -q "$tmp/$archive" -d "$tmp/unpacked"
else
  tar -xzf "$tmp/$archive" -C "$tmp/unpacked"
fi
chmod 755 "$tmp/unpacked/$name"
new_version=$("$tmp/unpacked/$name" --version) || die "the downloaded binary does not run on this machine"

mkdir -p "$install_dir" "$license_dir"
# Replace by rename within one directory. Copying over a binary in place can
# make macOS kill it, because the kernel caches code signatures per inode.
cp "$tmp/unpacked/$name" "$install_dir/.local-base.new"
if [ -e "$install_dir/local-base" ]; then
  cp -p "$install_dir/local-base" "$install_dir/.local-base.previous.new"
  mv -f "$install_dir/.local-base.previous.new" "$install_dir/local-base.previous"
  upgraded=1
else
  upgraded=0
fi
mv -f "$install_dir/.local-base.new" "$install_dir/local-base"
cp "$tmp/unpacked/LICENSE" "$tmp/unpacked/THIRD_PARTY_NOTICES.txt" "$license_dir/"

printf 'Installed LocalBase %s to %s/local-base\n' "$new_version" "$install_dir"
case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to your PATH.\n' "$install_dir" ;;
esac
if [ "$upgraded" = 1 ]; then
  printf 'Previous binary kept as %s/local-base.previous. If the service is running, apply the upgrade with: local-base restart\n' "$install_dir"
else
  printf 'Next, start the gateway as a user service with: local-base start\n'
fi
