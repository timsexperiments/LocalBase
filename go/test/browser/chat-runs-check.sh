#!/bin/sh
. "$(dirname "$0")/../../tools/test-ports.sh"
# Detached chat runs in headless Chrome (DESIGN §22.7) against a --no-auth
# scratch gateway whose chat model is the fake llama-server, so no weights
# load (and, for the Lab steps, the same fake as LOCALBASE_SD_SERVER with
# sparse files for the active image model). One way (scratch root and port only):
#   go build -o /tmp/fake/llama-server ./internal/runtime/fakebackend
#   mkfile -n <size> <root>/models/llm/<the active model's file>   (sparse)
#   LOCALBASE_LLAMA_SERVER=/tmp/fake/llama-server local-base --root <root> \
#     serve --port <port> --runtime-port-base <port+1> --no-auth --llm --no-stt --no-tts --no-image --no-video
#   test/browser/chat-runs-check.sh http://127.0.0.1:<port>
# Start a new gateway for each run. On one this script has already used, it fails at
# "announces the change once" and "another tool's tray does not", in any browser.
set -eu
here=$(cd "$(dirname "$0")/../.." && pwd)
base=${1:?usage: chat-runs-check.sh <base-url>}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile="$here/.cache/browser-profile-chat-runs-check"
port=$(test_port_pick)
. "$here/test/browser/browser-run.sh"
browser_guard chat-runs-check
(cd "$here" && rm -rf .cache/browser-profile-chat-runs-check)
"$chrome" --headless=new --use-mock-keychain --password-store=basic --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" --no-first-run about:blank >/dev/null 2>&1 &
browser_started $!
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
browser_run bun run "$here/test/browser/chat-runs-check/run.ts" "http://127.0.0.1:$port" "$base"
