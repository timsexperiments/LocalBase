#!/bin/sh
. "$(dirname "$0")/../../scripts/test-ports.sh"
# Search in Studio (DESIGN §35) in headless Chrome, against a --no-auth scratch gateway whose models
# are the fake llama-server, so no weights load. Two embedding models are selected, so the page offers
# a choice. One way (scratch root and port only):
#   go build -o /tmp/fake/llama-server ./internal/runtime/fakebackend
#   mkfile -n <size> <root>/models/llm/<each model's file>   (sparse: a chat model,
#     qwen3-embedding-0.6b-q8_0 and qwen3-embedding-4b-q4_k_m)
#   select all three as llm models, then:
#   LOCALBASE_LLAMA_SERVER=/tmp/fake/llama-server local-base --root <root> \
#     serve --port <port> --runtime-port-base <port+1> --no-auth --llm --no-stt --no-tts --no-image --no-video
#   test/browser/search-check.sh http://127.0.0.1:<port>
# The embeddings requests are held and answered in the browser where a step needs an error, so the
# gateway sees only the ones passed on to it.
set -eu
here=$(cd "$(dirname "$0")/../.." && pwd)
base=${1:?usage: search-check.sh <base-url>}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile="$here/.cache/browser-profile-search-check"
port=$(test_port_pick)
. "$here/test/browser/browser-run.sh"
browser_guard search-check
(cd "$here" && rm -rf .cache/browser-profile-search-check)
browser_group "$chrome" --headless=new --use-mock-keychain --password-store=basic --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" --no-first-run about:blank >/dev/null 2>&1 &
browser_started $!
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
browser_run bun run "$here/test/browser/search-check/run.ts" "http://127.0.0.1:$port" "$base"
