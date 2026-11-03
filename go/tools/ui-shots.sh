#!/bin/sh
# Every page at 360, 390, 430, 768 and 1280, in light and dark, with contact sheets
# (DESIGN §25). Run against a scratch --no-auth gateway with fake backends, and set
# LOCALBASE_COMPUTER_NAME on it, so no real host name reaches a picture.
#   tools/ui-shots.sh http://127.0.0.1:<port> <out-dir> [baseline-dir]
# With a baseline folder it also says which pictures changed by more than 0.5%. Pictures
# stay out of the repository; keep baselines beside the screenshots of a PR.
# UI_SHOTS_PAGES=chat,studio limits the pages.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
base=${1:?usage: ui-shots.sh <base-url> <out-dir> [baseline-dir]}
out=${2:?out-dir}
baseline=${3:-}
mkdir -p "$out"
out=$(cd "$out" && pwd)
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile=$(mktemp -d)
port=$((24000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 6000))
"$chrome" --headless=new --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" \
  --no-first-run --hide-scrollbars --allow-file-access-from-files about:blank >/dev/null 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null; sleep 1; rm -rf "$profile"' EXIT
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
bun run "$here/tools/ui-shots/run.ts" "http://127.0.0.1:$port" "$base" "$out" $baseline
