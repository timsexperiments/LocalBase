#!/bin/sh
# Local replacement for the real-iPhone gate (DESIGN §17.9): real iOS Safari
# on the iOS Simulator, driven by XCUITest (ios-check/LBCheck.xcodeproj),
# against a running --no-auth LocalBase UI. Checks no zoom on focus, the
# composer above the software keyboard, Return adding a line while Send
# submits, and runs Apple's accessibility audit (the Accessibility Inspector
# engine) on chat, the model picker, the history drawer, Admin and the Lab.
#   tools/ios-check.sh http://127.0.0.1:<port> [simulator-udid]
# The default device is the first booted iPhone. Results: $out (xcresult,
# screenshots, audit reports).
set -eu
here=$(cd "$(dirname "$0")" && pwd)
base=${1:?usage: ios-check.sh <base-url> [udid]}
udid=${2:-$(xcrun simctl list devices booted | sed -n 's/.*iPhone.*(\([0-9A-F-]\{36\}\)) (Booted).*/\1/p' | head -1)}
[ -n "$udid" ] || { echo "ios-check.sh: boot an iPhone simulator first (xcrun simctl boot <udid>)" >&2; exit 1; }
out=${LBGO_IOS_OUT:-/tmp/lbgo-ios-check}
rm -rf "$out/result.xcresult" "$out/attachments"; mkdir -p "$out"
rc=0
TEST_RUNNER_LB_BASE_URL="$base" xcodebuild test -project "$here/ios-check/LBCheck.xcodeproj" -scheme LBSafariUITests \
  -destination "id=$udid" -derivedDataPath "$out/derived" -resultBundlePath "$out/result.xcresult" \
  -only-testing:LBSafariUITests/LBSafariUITests CODE_SIGNING_ALLOWED=NO > "$out/xcodebuild.log" 2>&1 || rc=$?
grep -E "Test Case .*(passed|failed)|error:" "$out/xcodebuild.log" || true
xcrun xcresulttool export attachments --path "$out/result.xcresult" --output-path "$out/attachments" >/dev/null
jq -r '.[].attachments[] | "\(.suggestedHumanReadableName) \(.exportedFileName)"' "$out/attachments/manifest.json" |
  while read -r name file; do
    case $file in *.txt) echo "== ${name%%_0_*}"; cat "$out/attachments/$file"; echo ;; esac
  done
echo "screenshots and reports: $out/attachments"
exit $rc
