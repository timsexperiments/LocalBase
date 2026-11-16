#!/bin/bash
# Runs the browser suite (test/browser/e2e): finds Chrome, fetches the pinned axe-core, and gives
# the run its own session, lower priority and a stop on any signal.
#   test/browser/e2e.sh [-out dir]
# Chrome comes from LBGO_CHROME, then the Chrome for Testing pinned in test/browser/e2e/pins, fetched
# into LOCALBASE_E2E_CACHE (default ~/.cache/localbase-e2e) and never installed
# system-wide, so that every run checks the same browser. Only a platform with no pin falls
# back to an installed Chrome.
# Every run checks what it is about to use, before any of it runs: the fetched archives against
# their pins, then what was unpacked against the archive (test/browser/e2e/verify.go). A changed or
# half-written file in the cache stops the run at the first mismatch, with exit 2.
# A failed or stopped fetch leaves nothing behind. Its working directories have fixed names so
# that the trap can remove them by name, with nothing behind a variable. So two runs fetching
# into one cache at the same moment can collide. That fails closed: nothing gets its final name,
# or is used, until its checksum matches. Runs that only use the cache do not collide.
# LOCALBASE_E2E_SCRATCH puts the run's scratch and default results there, instead of $TMPDIR.
# E2E_PART=journeys|matrix, E2E_PAGES, E2E_WIDTHS and E2E_THEMES narrow a run.
set -u
cd "$(dirname "$0")/../.." || exit 2
. scripts/disk-guard.sh
disk_guard e2e
cache=${LOCALBASE_E2E_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/localbase-e2e}
mkdir -p "$cache" || exit 2
pin() { awk -v k="$1" '$1 == k { print $2 }' "$2"; }

clear_fetches() { (cd "$cache" && rm -rf fetching-axe fetching-chrome); }
# Only a run that fetches ever clears them, so a run that only uses the cache leaves another's be.
fetching=""
begin_fetch() {
  fetching=1
  trap clear_fetches EXIT
  trap '[ -n "$fetcher" ] && kill "$fetcher" 2>/dev/null; exit 2' INT TERM HUP
  clear_fetches
}
fetcher=""
fetch() { # url, file; in the background so that a signal is heard while it runs
  curl -fsSL --max-time 900 -o "$2" "$1" &
  fetcher=$!
  wait "$fetcher"
  local status=$?
  fetcher=""
  return "$status"
}

axeversion=$(pin version test/browser/axe.pins)
axe=$cache/axe-core-$axeversion.min.js
axearchive=$cache/axe-core-$axeversion.tgz
axesum=$(pin sha256 test/browser/axe.pins)
if [ ! -s "$axe" ] || [ ! -s "$axearchive" ]; then
  begin_fetch
  mkdir -p "$cache/fetching-axe" || exit 2
  fetch "$(pin tarball test/browser/axe.pins)" "$cache/fetching-axe/axe.tgz" || { echo "e2e.sh: could not fetch axe-core" >&2; exit 2; }
  got=$(shasum -a 256 "$cache/fetching-axe/axe.tgz" | cut -d' ' -f1)
  if [ "$got" != "$axesum" ]; then
    echo "e2e.sh: axe-core checksum mismatch ($got)" >&2
    exit 2
  fi
  tar -xzf "$cache/fetching-axe/axe.tgz" -C "$cache/fetching-axe" package/axe.min.js || exit 2
  mv "$cache/fetching-axe/package/axe.min.js" "$axe" && mv "$cache/fetching-axe/axe.tgz" "$axearchive" || exit 2
fi

platform=$(go env GOOS)-$(go env GOARCH)
url=$(pin "chrome-$platform-url" test/browser/e2e/pins)
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
pinned=()
if [ -z "$chrome" ]; then
  sum=$(pin "chrome-$platform-sha256" test/browser/e2e/pins)
  version=$(pin chrome-version test/browser/e2e/pins)
  dir=$cache/chrome-$version-$platform
  chrome=$dir/$(pin "chrome-$platform-path" test/browser/e2e/pins | tr '+' ' ')
  if [ ! -x "$chrome" ]; then
    if [ -e "$dir" ]; then
      echo "e2e.sh: $dir has no Chrome in it; remove it and run again" >&2
      exit 2
    fi
    echo "e2e.sh: fetching Chrome for Testing $version into $dir" >&2
    # Nothing is unpacked until the checksum matches, and it only gets its final name whole.
    begin_fetch
    mkdir -p "$cache/fetching-chrome" || exit 2
    fetch "$url" "$cache/fetching-chrome/chrome.zip" || { echo "e2e.sh: could not fetch Chrome" >&2; exit 2; }
    got=$(shasum -a 256 "$cache/fetching-chrome/chrome.zip" | cut -d' ' -f1)
    if [ "$got" != "$sum" ]; then
      echo "e2e.sh: Chrome checksum mismatch ($got)" >&2
      exit 2
    fi
    (cd "$cache/fetching-chrome" && unzip -q chrome.zip) || exit 2
    mv "$cache/fetching-chrome" "$dir" || exit 2
  fi
  pinned=(-chrome-dir "$dir" -chrome-archive "$dir/chrome.zip" -chrome-sha256 "$sum" -chrome-version "$version")
fi
if [ -n "$fetching" ]; then
  clear_fetches
  trap - EXIT INT TERM HUP
fi
command -v bun >/dev/null 2>&1 || { echo "e2e.sh: needs bun" >&2; exit 2; }

bin=$cache/e2e-$platform
go build -o "$bin" ./test/browser/e2e || exit 2
perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' nice -n 10 "$bin" -chrome "$chrome" -axe "$axe" -axe-archive "$axearchive" -axe-sha256 "$axesum" ${pinned[@]+"${pinned[@]}"} "$@" &
pid=$!
stop() { kill -TERM -- "-$pid" 2>/dev/null; }
trap stop INT TERM HUP
status=0
while kill -0 "$pid" 2>/dev/null; do
  wait "$pid"
  status=$?
done
exit "$status"
