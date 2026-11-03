#!/bin/bash
# Shows a killed person-check leaves nothing running: kill -9 of the driver, then of the script, watching
# exact pids and the check's ports. Exits 1 if anything is still there after 25 s.
#   tools/person-check/kill-check.sh [first port]
here=$(cd "$(dirname "$0")/../.." && pwd)
cd "$here" || exit 2
first=${1:-${PERSON_CHECK_PORT:-24650}}
log=$(mktemp)
ports="$first $((first + 1)) $((first + 2)) $((first + 3)) $((first + 4))"
listeners() { for p in $ports; do lsof -nP -iTCP:"$p" -sTCP:LISTEN -t 2>/dev/null; done | sort -u | tr '\n' ' '; }
alive() { n=0; for x in $1; do kill -0 "$x" 2>/dev/null && n=$((n + 1)); done; echo $n; }
bad=0
try() {
  what=$1
  # A listener here before the run isn't the run's, and nothing below may touch it.
  if [ -n "$(listeners)" ]; then
    echo "FAIL $what: NOT EXERCISED: these ports are already held: [$(listeners)]"
    bad=1
    return
  fi
  sh tools/person-check.sh "$first" >"$log" 2>&1 </dev/null &
  s=$!
  # Wait for a backend, so there's something to orphan. No backend means this proved nothing.
  for _ in $(seq 1 90); do lsof -nP -iTCP:$((first + 3)) -sTCP:LISTEN -t >/dev/null 2>&1 && break; sleep 1; done
  sleep 1
  gw=$(lsof -nP -iTCP:$((first + 2)) -sTCP:LISTEN -t 2>/dev/null | head -1)
  be=$(lsof -nP -iTCP:$((first + 3)) -sTCP:LISTEN -t 2>/dev/null | head -1)
  # The script records the check it started.
  drv=$(cat "$here/.cache/person-check.browser.pid.child" 2>/dev/null)
  if [ -z "$gw" ] || [ -z "$be" ] || [ -z "$drv" ]; then
    echo "FAIL $what: NOT EXERCISED: driver [$drv], gateway [$gw], backend [$be]"
    bad=1
  fi
  list="$drv $gw $be"
  echo "$what: before: driver $drv, script $s, gateway $gw, backend $be"
  case "$what" in driver) kill -9 "$drv" ;; script) kill -9 "$s" ;; esac
  t=0
  while [ "$(alive "$list")" != 0 ] || [ -n "$(listeners)" ]; do
    [ $t -ge 25 ] && break
    sleep 1
    t=$((t + 1))
  done
  left=$(alive "$list")
  held=$(listeners)
  if [ "$left" = 0 ] && [ -z "$held" ]; then echo "ok   $what: nothing left after ${t}s"; else
    echo "FAIL $what: after ${t}s, $left processes left, listeners [$held]"
    bad=1
  fi
  wait "$s" 2>/dev/null
  for x in $list; do kill -9 "$x" 2>/dev/null; done
  sleep 2
}
try driver
try script
exit $bad
