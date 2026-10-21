#!/bin/sh
# Microphone dictation end to end (DESIGN §17.9): desktop Chrome with a fake
# capture device that plays a speech clip, so the page records through
# getUserMedia, encodes the browser WAV and sends it to the gateway's
# transcription route, which runs the real whisper-server. The audio service
# runs in process so the fake device can read the clip (a plain PCM WAV).
#   tools/mic-check.sh http://127.0.0.1:<port> <clip.wav> "<expected words>"
# A --no-auth gateway with an installed STT model.
set -eu
here=$(cd "$(dirname "$0")/.." && pwd)
base=${1:?usage: mic-check.sh <base-url> <clip.wav> <expected>}
clip=${2:?clip}
expect=${3:?expected words}
chrome=${LBGO_CHROME:-"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
profile=$(mktemp -d)
port=$((24000 + $(od -An -N2 -tu2 /dev/urandom | tr -d ' ') % 6000))
"$chrome" --headless=new --remote-debugging-address=127.0.0.1 --remote-debugging-port=$port --user-data-dir="$profile" \
  --no-first-run --use-fake-ui-for-media-stream --use-fake-device-for-media-stream --use-file-for-fake-audio-capture="$clip" \
  --disable-features=AudioServiceOutOfProcess --autoplay-policy=no-user-gesture-required about:blank >/dev/null 2>&1 &
pid=$!
trap 'kill $pid 2>/dev/null; sleep 1; rm -rf "$profile"' EXIT
for i in $(seq 1 30); do curl -s "http://127.0.0.1:$port/json/version" >/dev/null && break; sleep 0.5; done
bun run "$here/tools/mic-check/run.ts" "http://127.0.0.1:$port" "$base" "$expect"
