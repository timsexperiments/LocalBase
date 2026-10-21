#!/bin/sh
# axe-core in desktop Chrome (headless) against a running LocalBase UI
# (DESIGN §17.9): chat, the model picker, the history drawer, generation
# controls, Admin, the Lab and the other pages, at 1280 and 390 px, light
# and dark. The pinned axe-core is cached in go/.cache, verified by sha256,
# and injected over the DevTools protocol only for the test.
#   tools/axe-check.sh http://127.0.0.1:<port>   (a --no-auth gateway)
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
base=${1:?usage: axe-check.sh <base-url>}
pins="$here/tools/axe.pins"
version=$(awk '$1=="version"{print $2}' "$pins")
axe="$here/.cache/axe-core-$version.min.js"
if [ ! -s "$axe" ]; then
  mkdir -p "$here/.cache"
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/axe.tgz" "$(awk '$1=="tarball"{print $2}' "$pins")"
  got=$(shasum -a 256 "$tmp/axe.tgz" | cut -d' ' -f1)
  [ "$got" = "$(awk '$1=="sha256"{print $2}' "$pins")" ] || { rm -rf "$tmp"; echo "axe-check.sh: checksum mismatch ($got)" >&2; exit 1; }
  tar -xzf "$tmp/axe.tgz" -C "$tmp" package/axe.min.js && mv "$tmp/package/axe.min.js" "$axe"
  rm -rf "$tmp"
fi
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile=$(mktemp -d)
port=$((24000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 6000))
"$chrome" --headless=new --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" --no-first-run about:blank >/dev/null 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null; sleep 1; rm -rf "$profile"' EXIT
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
bun run "$here/tools/axe-check/run.ts" "http://127.0.0.1:$port" "$base" "$axe"
