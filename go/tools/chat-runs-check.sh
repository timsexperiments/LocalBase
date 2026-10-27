#!/bin/sh
# Detached chat runs in headless Chrome (DESIGN §22.7) against a --no-auth
# scratch gateway whose chat model is the fake llama-server, so no weights
# load (and, for the Lab steps, the same fake as LOCALBASE_SD_SERVER with
# sparse files for the active image model). One way (scratch root and port only):
#   go build -o /tmp/fake/llama-server ./internal/runtime/fakebackend
#   mkfile -n <size> <root>/models/llm/<the active model's file>   (sparse)
#   LOCALBASE_LLAMA_SERVER=/tmp/fake/llama-server local-base --root <root> \
#     serve --port <port> --no-auth --llm --no-stt --no-tts --no-image --no-video
#   tools/chat-runs-check.sh http://127.0.0.1:<port>
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
base=${1:?usage: chat-runs-check.sh <base-url>}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile=$(mktemp -d)
port=$((24000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 6000))
"$chrome" --headless=new --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" --no-first-run about:blank >/dev/null 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null; sleep 1; rm -rf "$profile"' EXIT
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
bun run "$here/tools/chat-runs-check/run.ts" "http://127.0.0.1:$port" "$base"
