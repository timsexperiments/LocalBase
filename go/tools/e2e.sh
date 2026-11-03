#!/bin/bash
# Runs the browser suite (tools/e2e): finds Chrome, fetches the pinned axe-core, and gives
# the run its own session, lower priority and a stop on any signal.
#   tools/e2e.sh [-out dir]
# Chrome comes from LBGO_CHROME, then the Chrome for Testing pinned in tools/e2e/pins, fetched
# into LOCALBASE_E2E_CACHE (default ~/.cache/localbase-e2e) and never installed
# system-wide, so that every run checks the same browser. Only a platform with no pin falls
# back to an installed Chrome.
# E2E_PART=journeys|matrix, E2E_PAGES, E2E_WIDTHS and E2E_THEMES narrow a run.
set -u
cd "$(dirname "$0")/.." || exit 2
cache=${LOCALBASE_E2E_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/localbase-e2e}
mkdir -p "$cache" || exit 2
pin() { awk -v k="$1" '$1 == k { print $2 }' "$2"; }

axeversion=$(pin version tools/axe.pins)
axe=$cache/axe-core-$axeversion.min.js
if [ ! -s "$axe" ]; then
  tmp=$(mktemp -d "$cache/axe-XXXXXX") || exit 2
  curl -fsSL -o "$tmp/axe.tgz" "$(pin tarball tools/axe.pins)" || { echo "e2e.sh: could not fetch axe-core" >&2; exit 2; }
  got=$(shasum -a 256 "$tmp/axe.tgz" | cut -d' ' -f1)
  if [ "$got" != "$(pin sha256 tools/axe.pins)" ]; then
    echo "e2e.sh: axe-core checksum mismatch ($got)" >&2
    exit 2
  fi
  tar -xzf "$tmp/axe.tgz" -C "$tmp" package/axe.min.js && mv "$tmp/package/axe.min.js" "$axe" || exit 2
fi

platform=$(go env GOOS)-$(go env GOARCH)
url=$(pin "chrome-$platform-url" tools/e2e/pins)
chrome=${LBGO_CHROME:-}
if [ -z "$chrome" ] && [ -z "$url" ]; then
  # No pin for this platform: an installed Chrome will do, though its version floats.
  for c in google-chrome google-chrome-stable chromium chromium-browser; do
    if command -v "$c" >/dev/null 2>&1; then chrome=$c; break; fi
  done
  if [ -z "$chrome" ]; then
    echo "e2e.sh: no Chrome pinned for $platform and none installed; set LBGO_CHROME" >&2
    exit 2
  fi
  echo "e2e.sh: no Chrome pinned for $platform; using $chrome" >&2
fi
if [ -z "$chrome" ]; then
  sum=$(pin "chrome-$platform-sha256" tools/e2e/pins)
  version=$(pin chrome-version tools/e2e/pins)
  dir=$cache/chrome-$version-$platform
  chrome=$dir/$(pin "chrome-$platform-path" tools/e2e/pins | tr '+' ' ')
  if [ ! -x "$chrome" ]; then
    if [ -e "$dir" ]; then
      echo "e2e.sh: $dir has no Chrome in it; remove it and run again" >&2
      exit 2
    fi
    echo "e2e.sh: fetching Chrome for Testing $version into $dir" >&2
    # Nothing is unpacked until the checksum matches, and it only gets its final name whole.
    tmp=$(mktemp -d "$cache/chrome-XXXXXX") || exit 2
    curl -fsSL -o "$tmp/chrome.zip" "$url" || { echo "e2e.sh: could not fetch Chrome" >&2; exit 2; }
    got=$(shasum -a 256 "$tmp/chrome.zip" | cut -d' ' -f1)
    if [ "$got" != "$sum" ]; then
      echo "e2e.sh: Chrome checksum mismatch ($got)" >&2
      exit 2
    fi
    (cd "$tmp" && unzip -q chrome.zip) || exit 2
    mv "$tmp" "$dir" || exit 2
  fi
fi
command -v bun >/dev/null 2>&1 || { echo "e2e.sh: needs bun" >&2; exit 2; }

bin=$cache/e2e-$platform
go build -o "$bin" ./tools/e2e || exit 2
perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' nice -n 10 "$bin" -chrome "$chrome" -axe "$axe" "$@" &
pid=$!
stop() { kill -TERM -- "-$pid" 2>/dev/null; }
trap stop INT TERM HUP
status=0
while kill -0 "$pid" 2>/dev/null; do
  wait "$pid"
  status=$?
done
exit "$status"
