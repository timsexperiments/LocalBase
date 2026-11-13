#!/bin/sh
. "$(dirname "$0")/../../tools/test-ports.sh"
# Where each <details> really lands, in one browser, for comparing browsers (DESIGN §27.5). Browsers
# box a <details> differently, so a layout can pass in one and be wrong in another. Run it once per
# browser, then compare. See test/browser/details-check/README.md.
#   LBGO_CHROME=<browser> test/browser/details-check.sh http://127.0.0.1:<port> <out.json>
#   bun test/browser/details-check/compare.ts <a.json> <b.json>
set -eu
here=$(cd "$(dirname "$0")/../.." && pwd)
base=${1:?usage: details-check.sh <base-url> <out.json>}
out=${2:?usage: details-check.sh <base-url> <out.json>}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile="$here/.cache/browser-profile-details-check"
port=$(test_port_pick)
. "$here/test/browser/browser-run.sh"
browser_guard details-check
(cd "$here" && rm -rf .cache/browser-profile-details-check)
browser_group "$chrome" --headless=new --use-mock-keychain --password-store=basic --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" --no-first-run --no-default-browser-check --disable-extensions about:blank >/dev/null 2>&1 &
browser_started $!
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
browser_run bun run "$here/test/browser/details-check/run.ts" "http://127.0.0.1:$port" "$base" "$out"
