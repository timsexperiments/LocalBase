#!/bin/bash
# Shows that the browser suite leaves nothing running however it ends: stopped through the
# wrapper, killed outright, and at the last-resort exit past its hard deadline.
#   tools/e2e-kill-check.sh [sigterm] [sigkill] [deadline]       about a minute for all three
# Chrome comes as for tools/e2e.sh. It watches Chrome's process group, the driver, and
# everything running from the run's scratch directory, by exact pid, and exits 1 if anything
# stays. A stopped run leaves its scratch and results behind, and this prints where.
set -u
cd "$(dirname "$0")/.." || exit 2
. tools/disk-guard.sh
disk_guard e2e-kill-check
cases=${*:-sigterm sigkill deadline}
base=$(mktemp -d "${TMPDIR:-/tmp}/lbe2e-kill-XXXXXX") || exit 2
echo "results under $base"
failed=0

one() {
  local mode=$1 out=$base/$1 force="" wrapper suite chrome bunp gw scratch began i n
  mkdir -p "$out"
  [ "$mode" = deadline ] && force=30
  E2E_PART=matrix LOCALBASE_E2E_FORCE_EXIT_SECONDS=$force tools/e2e.sh -out "$out" > "$out/stdout.txt" 2> "$out/stderr.txt" &
  wrapper=$!
  for i in $(seq 1 480); do
    grep -q 'overflow self-test' "$out/stdout.txt" 2>/dev/null && break
    kill -0 "$wrapper" 2>/dev/null || break
    sleep 0.25
  done
  ps -A -o pid=,ppid=,pgid=,command= > "$out/before.txt"
  suite=$(awk -v p="$wrapper" '$2 == p { print $1; exit }' "$out/before.txt")
  chrome=$(awk -v p="$suite" '$2 == p && /remote-debugging-port=/ { print $1; exit }' "$out/before.txt")
  bunp=$(awk -v p="$suite" '$2 == p && /run\.ts/ { print $1; exit }' "$out/before.txt")
  gw=$(awk -v p="$suite" '$2 == p && /\/bin\/local-base/ { print $1; exit }' "$out/before.txt")
  scratch=$(awk -v p="$gw" '$1 == p { for (i = 4; i <= NF; i++) if ($i ~ /\/bin\/local-base$/) { sub(/\/bin\/local-base$/, "", $i); print $i; exit } }' "$out/before.txt")
  if [ -z "$suite" ] || [ -z "$chrome" ] || [ -z "$gw" ] || [ -z "$scratch" ]; then
    echo "$mode: FAILED: the suite never got going; see $out/stderr.txt"
    kill -TERM "$wrapper" 2>/dev/null
    failed=1
    return
  fi
  began=$(date +%s)
  case $mode in
    sigterm) kill -TERM "$wrapper" ;;
    sigkill) kill -KILL "$suite" ;;
    deadline) for i in $(seq 1 240); do kill -0 "$suite" 2>/dev/null || break; sleep 0.25; done ;;
  esac
  [ "$mode" = deadline ] && began=$(date +%s)
  for i in $(seq 1 200); do
    ps -A -o pid=,pgid=,command= > "$out/after.txt"
    n=$(awk -v c="$chrome" -v b="$bunp" -v s="$scratch/bin/" '$2 == c || $1 == b || index($0, s) { n++ } END { print n + 0 }' "$out/after.txt")
    [ "$n" = 0 ] && break
    sleep 0.25
  done
  if [ "$n" != 0 ]; then
    echo "$mode: FAILED: $n still running after $(( $(date +%s) - began )) s:"
    awk -v c="$chrome" -v b="$bunp" -v s="$scratch/bin/" '$2 == c || $1 == b || index($0, s)' "$out/after.txt" | cut -c1-120 | head -8
    failed=1
    awk -v c="$chrome" -v b="$bunp" -v s="$scratch/bin/" '$2 == c || $1 == b || index($0, s) { print $1 }' "$out/after.txt" | while read -r p; do kill -KILL "$p" 2>/dev/null; done
  else
    echo "$mode: nothing left after $(( $(date +%s) - began )) s"
  fi
  if [ "$mode" = deadline ] && ! grep -q 'hard deadline passed' "$out/stderr.txt"; then
    echo "deadline: FAILED: the last-resort exit never ran; see $out/stderr.txt"
    failed=1
  fi
  wait "$wrapper" 2>/dev/null
  echo "$mode: scratch left at $scratch"
}

for mode in $cases; do
  case $mode in
    sigterm | sigkill | deadline) one "$mode" ;;
    *) echo "unknown case $mode" >&2; exit 2 ;;
  esac
done
exit "$failed"
