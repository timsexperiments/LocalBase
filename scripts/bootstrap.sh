#!/usr/bin/env bash
# SPDX-FileCopyrightText: TimsExperiments
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Installs the pinned tools into .cache/tools/bin, verified by SHA-256. scripts/tools.pins is
# the one place their versions are pinned: CI fetches its tools through this script too.
# Nothing is installed outside this checkout unless LOCALBASE_TOOLS_DIR names another directory,
# and no sudo is used. Safe to re-run.
#   scripts/bootstrap.sh [tool ...]      default: every tool in scripts/tools.pins
#   scripts/bootstrap.sh --version tool  print the pinned version of one tool
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
pins=$repo/scripts/tools.pins

if [ "${1:-}" = "--version" ]; then
  [ $# -eq 2 ] || { echo "usage: scripts/bootstrap.sh --version tool" >&2; exit 2; }
  version="$(awk -v t="$2" '$1 == t { print $2; exit }' "$pins")"
  [ -n "$version" ] || { echo "bootstrap: no pin for $2" >&2; exit 1; }
  echo "$version"
  exit 0
fi

tools=${LOCALBASE_TOOLS_DIR:-$repo/.cache/tools}
mkdir -p "$tools/bin"

die() {
  printf 'bootstrap: %s\n' "$1" >&2
  exit 1
}

command -v git > /dev/null 2>&1 || die "install git first"
# A full contributor setup needs Go; fetching named tools, as CI does, doesn't.
[ $# -gt 0 ] || command -v go > /dev/null 2>&1 || die "install Go 1.21 or newer first: https://go.dev/dl/ (it fetches the exact toolchain in go/go.mod by itself)"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) die "unsupported operating system: $(uname -s)" ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) arch=arm64 ;;
  x86_64 | amd64) arch=amd64 ;;
  *) die "unsupported architecture: $(uname -m)" ;;
esac
platform=$os-$arch

sha256() {
  if command -v sha256sum > /dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  else
    shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

install_tool() {
  local tool=$1 version=$2 want=$3 url=$4 marker=$tools/.$1-$2 work archive
  if [ -f "$marker" ] && [ -x "$tools/bin/$tool" ]; then
    printf '%s %s: already installed\n' "$tool" "$version"
    return
  fi
  work="$(mktemp -d "${TMPDIR:-/tmp}/lb-bootstrap.XXXXXX")"
  archive=$work/${url##*/}
  curl -fsSL --retry 3 -o "$archive" "$url" || { rm -rf "$work"; die "could not download $url"; }
  if [ "$(sha256 "$archive")" != "$want" ]; then
    rm -rf "$work"
    die "checksum mismatch for $tool $version"
  fi
  mkdir "$work/unpacked"
  case "$archive" in
    *.zip) unzip -q "$archive" -d "$work/unpacked" ;;
    *) tar -xzf "$archive" -C "$work/unpacked" ;;
  esac
  local found
  found="$(find "$work/unpacked" -type f -name "$tool" | head -n 1)"
  [ -n "$found" ] || { rm -rf "$work"; die "$tool not found in its archive"; }
  chmod 755 "$found"
  mv -f "$found" "$tools/bin/$tool"
  rm -rf "$work"
  rm -f "$tools/.$tool"-*
  touch "$marker"
  printf '%s %s: installed\n' "$tool" "$version"
}

wanted=" $* "
while read -r tool version pin_platform want url; do
  case "$tool" in '' | \#*) continue ;; esac
  [ "$pin_platform" = "$platform" ] || continue
  if [ $# -gt 0 ] && [ "${wanted#* "$tool" }" = "$wanted" ]; then continue; fi
  install_tool "$tool" "$version" "$want" "$url"
done < "$pins"

printf 'Tools are in %s. `make` finds them by itself.\n' "${tools#"$repo"/}/bin"
