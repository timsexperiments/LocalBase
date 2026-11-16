#!/bin/sh
. "$(dirname "$0")/../../scripts/test-ports.sh"
# DOM-level phone checks in headless Chrome against a running --no-auth
# LocalBase UI (DESIGN §22.6): at 360, 390 and 430 px (touch) and 1280 px,
# every page has no horizontal overflow, controls are at least 44 px, the
# tab bar or sidebar shows as it should, the More sheet opens, and a text
# field stays visible and hides the tab bar when the keyboard takes space.
#   test/browser/phone-check.sh http://127.0.0.1:<port>
set -eu
here=$(cd "$(dirname "$0")/../.." && pwd)
base=${1:?usage: phone-check.sh <base-url>}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile="$here/.cache/browser-profile-phone-check"
port=$(test_port_pick)
. "$here/test/browser/browser-run.sh"
browser_guard phone-check
(cd "$here" && rm -rf .cache/browser-profile-phone-check)
browser_group "$chrome" --headless=new --use-mock-keychain --password-store=basic --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" --no-first-run about:blank >/dev/null 2>&1 &
browser_started $!
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
browser_run bun run "$here/test/browser/phone-check/run.ts" "http://127.0.0.1:$port" "$base"
