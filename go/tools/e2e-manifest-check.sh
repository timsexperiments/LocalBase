#!/bin/bash
# Shows the browser suite's table of what ran can fail: with each section guarded off the table
# must name it, and with nothing guarded off it must have no gaps. Also shows the exit status of
# what must not be tolerated: a failed journey is 5, and so is a key in the gateway's log, whose
# report names the file and line and holds no part of the key. Two pages at one width, so about
# two minutes.
#   tools/e2e-manifest-check.sh
# Chrome comes as for tools/e2e.sh. Results go to a fixed place the next run replaces; the exit
# status is read first, so an old file cannot answer for a run that never got that far.
# Exit 0: it held. Exit 1: a gap went unnoticed, or a clean run had one. Exit 2: could not run.
set -u
cd "$(dirname "$0")/.." || exit 2
. tools/disk-guard.sh
disk_guard e2e-manifest-check
command -v bun >/dev/null 2>&1 || { echo "e2e-manifest-check: needs bun" >&2; exit 2; }
out=${LOCALBASE_E2E_SCRATCH:-${TMPDIR:-/tmp}}/localbase-e2e/manifest-check
mkdir -p "$out" || exit 2
export E2E_PAGES=signin,models E2E_WIDTHS=390 E2E_THEMES=light

# Prints how many gaps of each kind a run's findings.json names, one "kind count" a line.
kinds() {
  bun -e '
    const { missing } = JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8")).manifest;
    const named = ["journey", "self-test", "overflow", "axe", "page errors", "keyboard", "sheets", "motion", "page models"];
    for (const k of named) console.log(`${k}\t${missing.filter((m) => m.startsWith(k + ":") || m.startsWith(k + " ")).length}`);
    console.log(`all\t${missing.length}`);
  ' "$1"
}

echo "e2e-manifest-check: nothing guarded off"
E2E_PLANT_OFF= tools/e2e.sh -out "$out/clean" > "$out/clean.log" 2>&1
status=$?
if [ "$status" -ne 0 ] && [ "$status" -ne 1 ]; then
  echo "e2e-manifest-check: FAIL: the clean run exited $status; its output is in $out/clean.log" >&2
  [ "$status" -eq 4 ] && exit 1
  exit 2
fi

echo "e2e-manifest-check: every section guarded off"
E2E_PLANT_OFF=studio,selftests,overflow,axe,errors,keyboard,sheets,motion,landing tools/e2e.sh -out "$out/planted" > "$out/planted.log" 2>&1
status=$?
if [ "$status" -ne 4 ]; then
  echo "e2e-manifest-check: FAIL: the planted run exited $status, not 4; its output is in $out/planted.log" >&2
  exit 1
fi
counts=$(kinds "$out/planted/findings.json") || exit 2
printf '%s\n' "$counts"
if printf '%s\n' "$counts" | awk -F'\t' '$2 == 0 { print "e2e-manifest-check: FAIL: nothing named " $1; bad = 1 } END { exit bad ? 0 : 1 }' >&2; then
  exit 1
fi

echo "e2e-manifest-check: a journey that fails"
E2E_PART=journeys E2E_PLANT_OFF=fail-a-journey tools/e2e.sh -out "$out/journey" > "$out/journey.log" 2>&1
status=$?
if [ "$status" -ne 5 ]; then
  echo "e2e-manifest-check: FAIL: a failed journey exited $status, not 5; its output is in $out/journey.log" >&2
  exit 1
fi

echo "e2e-manifest-check: a key in the gateway's log"
E2E_PART=journeys E2E_PLANT_OFF=logged-key tools/e2e.sh -out "$out/key" > "$out/key.log" 2>&1
status=$?
if [ "$status" -ne 5 ]; then
  echo "e2e-manifest-check: FAIL: a logged key exited $status, not 5; its output is in $out/key.log" >&2
  exit 1
fi
if ! grep -qE 'an API key is in the log: .+ line [0-9]+' "$out/key.log"; then
  echo "e2e-manifest-check: FAIL: the report of a logged key does not name the file and line" >&2
  exit 1
fi
# The plant is this repeated, and any one repeat is part of the key.
if grep -qF 'Zq9x' "$out/key.log" "$out/key/result.md" "$out/key/findings.json"; then
  echo "e2e-manifest-check: FAIL: part of the planted key is in the run's output or results" >&2
  exit 1
fi
if ! grep -q '^## Gateway log' "$out/key/result.md"; then
  echo "e2e-manifest-check: FAIL: result.md does not say the log failed" >&2
  exit 1
fi
echo "e2e-manifest-check: PASS"
