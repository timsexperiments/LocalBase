#!/bin/sh
# Builds internal/web/static/app.css with the pinned Tailwind standalone CLI.
# No Node or npm. The CLI is cached in go/.cache and verified by sha256.
#   tools/tailwind.sh          build
#   tools/tailwind.sh --check  build, then fail if app.css changed (CI)
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
pins="$here/tools/tailwind.pins"
version=$(awk '$1=="version"{print $2}' "$pins")
case "$(uname -s)-$(uname -m)" in
  Darwin-arm64) target=macos-arm64 ;;
  Linux-x86_64) target=linux-x64 ;;
  *) echo "tailwind.sh: no pinned CLI for $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac
want=$(awk -v t="$target" '$1==t{print $2}' "$pins")
bin="$here/.cache/tailwindcss-$version-$target"
if [ ! -x "$bin" ]; then
  mkdir -p "$here/.cache"
  curl -fsSL -o "$bin.tmp" "https://github.com/tailwindlabs/tailwindcss/releases/download/$version/tailwindcss-$target"
  got=$(shasum -a 256 "$bin.tmp" 2>/dev/null | cut -d' ' -f1 || sha256sum "$bin.tmp" | cut -d' ' -f1)
  if [ "$got" != "$want" ]; then
    rm -f "$bin.tmp"; echo "tailwind.sh: checksum mismatch for $target ($got)" >&2; exit 1
  fi
  chmod +x "$bin.tmp" && mv "$bin.tmp" "$bin"
fi
out="$here/internal/web/static/app.css"
before=$(shasum -a 256 < "$out" 2>/dev/null || true)
"$bin" --input "$here/internal/web/css/input.css" --output "$out" --minify 2>/dev/null
if [ "${1:-}" = "--check" ]; then
  after=$(shasum -a 256 < "$out")
  [ "$before" = "$after" ] || { echo "app.css is stale; run tools/tailwind.sh and commit it" >&2; exit 1; }
fi
