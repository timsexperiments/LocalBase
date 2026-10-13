#!/bin/sh
set -eu

if [ "${1:-}" = --help ] || [ "${1:-}" = -h ]; then
  cat <<'HELP'
Install LocalBase from its GitHub release archive.

LOCALBASE_VERSION pins the release (default: latest).
LOCALBASE_INSTALL_DIR selects the CLI destination (default: ~/.local/bin).
LOCALBASE_LICENSE_DIR selects the license destination (default: ~/.local/share/local-base/licenses).
HELP
  exit 0
fi

repo=timsexperiments/LocalBase
case "$(uname -s)" in
  Darwin) os=macos ;;
  Linux) os=linux ;;
  *) echo "LocalBase installer: unsupported operating system: $(uname -s)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=x64 ;;
  *) echo "LocalBase installer: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
case "$os-$arch" in
  macos-arm64|macos-x64|linux-x64|linux-arm64) ;;
  *) echo "LocalBase installer: unsupported platform: $os-$arch" >&2; exit 1 ;;
esac

version=${LOCALBASE_VERSION:-latest}
if [ "$version" = latest ]; then
  release_url="https://github.com/$repo/releases/latest/download"
else
  case "$version" in v*) tag=$version ;; *) tag="v$version" ;; esac
  release_url="https://github.com/$repo/releases/download/$tag"
fi
archive="local-base-$os-$arch"
if [ "$os" = macos ]; then archive="$archive.zip"; else archive="$archive.tar.gz"; fi
install_dir=${LOCALBASE_INSTALL_DIR:-"$HOME/.local/bin"}
license_dir=${LOCALBASE_LICENSE_DIR:-"$HOME/.local/share/local-base/licenses"}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/local-base-install.XXXXXX")
trap 'rm -rf "$tmp"' 0 HUP INT TERM

curl -fsSL "$release_url/$archive" -o "$tmp/$archive"
curl -fsSL "$release_url/checksums.txt" -o "$tmp/checksums.txt"
awk -v name="$archive" '$2 == name { print $1 "  " $2 }' "$tmp/checksums.txt" > "$tmp/archive.sha256"
if [ ! -s "$tmp/archive.sha256" ]; then echo "LocalBase installer: checksum entry missing for $archive" >&2; exit 1; fi
if command -v shasum >/dev/null 2>&1; then
  (cd "$tmp" && shasum -a 256 -c archive.sha256)
elif command -v sha256sum >/dev/null 2>&1; then
  (cd "$tmp" && sha256sum -c archive.sha256)
else
  echo "LocalBase installer: install shasum or sha256sum to verify downloads" >&2
  exit 1
fi

mkdir -p "$install_dir"
mkdir -p "$license_dir"
if [ "$os" = macos ]; then
  unzip -p "$tmp/$archive" "local-base-$os-$arch" > "$tmp/local-base"
  unzip -p "$tmp/$archive" LICENSE > "$tmp/LICENSE"
  unzip -p "$tmp/$archive" THIRD_PARTY_NOTICES.txt > "$tmp/THIRD_PARTY_NOTICES.txt"
else
  tar -xOzf "$tmp/$archive" "local-base-$os-$arch" > "$tmp/local-base"
  tar -xOzf "$tmp/$archive" LICENSE > "$tmp/LICENSE"
  tar -xOzf "$tmp/$archive" THIRD_PARTY_NOTICES.txt > "$tmp/THIRD_PARTY_NOTICES.txt"
fi
chmod 755 "$tmp/local-base"
mv "$tmp/local-base" "$install_dir/local-base"
mv "$tmp/LICENSE" "$license_dir/LICENSE"
mv "$tmp/THIRD_PARTY_NOTICES.txt" "$license_dir/THIRD_PARTY_NOTICES.txt"
printf 'Installed LocalBase to %s/local-base\n' "$install_dir"
printf 'Installed license texts to %s\n' "$license_dir"
case ":${PATH:-}:" in *":$install_dir:"*) ;; *) printf 'Add %s to your PATH, then run: local-base init\n' "$install_dir" ;; esac
