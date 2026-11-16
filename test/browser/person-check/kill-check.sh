#!/bin/bash
# Shows a killed person-check leaves nothing running. It kills the driver with -9, then in a second run
# the script itself, and waits up to 25 s for everything the run started to be gone and for its ports
# to be free. Exits 1 if anything is still there, or if a run couldn't be exercised.
#   bash test/browser/person-check/kill-check.sh [first port]   (5 ports from there, default 24650)
#
# It only ever touches what the run recorded about itself:
#  - the script, which this starts;
#  - the driver and the browser, from browser-run.sh's pid files;
#  - the gateway's process group, whose leader (supervise.sh) person-check/run.ts writes to
#    .cache/person-check/gateway.group.
# Each one's command is checked before anything is sent to it. Ports are only read, to see whether
# anything still listens; they're never used to find a process.
here=$(cd "$(dirname "$0")/../../.." && pwd)
cd "$here" || exit 2
first=${1:-${PERSON_CHECK_PORT:-24650}}
log=$(mktemp)
work="$here/.cache/person-check"
pidfile="$here/.cache/person-check.browser.pid"
ports="$first $((first + 1)) $((first + 2)) $((first + 3)) $((first + 4))"
held() { for p in $ports; do lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1 && printf '%s ' "$p"; done; }
command_of() { ps -ww -o command= -p "$1" 2>/dev/null; }
# The members of a process group, by pid, from the process table.
members() { ps -A -o pid= -o pgid= | awk -v g="$1" '$2 == g { print $1 }' | tr '\n' ' '; }
alive() { n=0; for x in $1; do kill -0 "$x" 2>/dev/null && n=$((n + 1)); done; echo $n; }
# Whether a pid is still the process the run recorded, by what its command must contain.
is() { case "$(command_of "$1")" in *"$2"*) return 0 ;; esac; return 1; }
bad=0
# tidy ends whatever a failed try left, with the same records and command checks.
tidy() {
  is "$drv" "person-check/run.ts" && kill -9 "$drv" 2>/dev/null
  is "$brw" "browser-profile-person-check" && kill -9 "$brw" 2>/dev/null
  for x in $(members "$grp"); do
    is "$x" "$work/" || is "$x" "person-check/supervise.sh" || { echo "  left $x alone: not this run's ($(command_of "$x" | cut -c1-80))"; continue; }
    kill -9 "$x" 2>/dev/null
  done
}
try() {
  what=$1
  # A listener here before the run isn't the run's, and nothing below may touch it.
  if [ -n "$(held)" ]; then
    echo "FAIL $what: NOT EXERCISED: these ports are already held: [$(held)]"
    bad=1
    return
  fi
  sh test/browser/person-check.sh "$first" >"$log" 2>&1 </dev/null &
  s=$!
  # Wait for the backend, so there's something to orphan. No backend means this proved nothing.
  for _ in $(seq 1 90); do lsof -nP -iTCP:$((first + 3)) -sTCP:LISTEN >/dev/null 2>&1 && break; sleep 1; done
  sleep 1
  drv=$(cat "$pidfile.child" 2>/dev/null)
  brw=$(cat "$pidfile" 2>/dev/null)
  grp=$(cat "$work/gateway.group" 2>/dev/null)
  if [ -z "$drv" ] || [ -z "$brw" ] || [ -z "$grp" ]; then
    echo "FAIL $what: NOT EXERCISED: driver [$drv], browser [$brw], gateway group [$grp]"
    bad=1; kill -9 "$s" 2>/dev/null; wait "$s" 2>/dev/null; return
  fi
  # Each record must still be what it says, and the group must be led by supervise.sh.
  if ! is "$s" "person-check.sh" || ! is "$drv" "person-check/run.ts" || ! is "$brw" "browser-profile-person-check" ||
    ! is "$grp" "person-check/supervise.sh" || [ "$(ps -o pgid= -p "$grp" | tr -d ' ')" != "$grp" ]; then
    echo "FAIL $what: NOT EXERCISED: a record doesn't match its process; nothing was killed"
    bad=1; return
  fi
  group=$(members "$grp")
  list="$s $drv $brw $group"
  echo "$what: before: script $s, driver $drv, browser $brw, gateway group $grp [$group], listening [$(held)]"
  case "$what" in driver) kill -9 "$drv" ;; script) kill -9 "$s" ;; esac
  t=0
  while [ "$(alive "$list")" != 0 ] || [ -n "$(members "$grp")" ] || [ -n "$(held)" ]; do
    [ $t -ge 25 ] && break
    sleep 1
    t=$((t + 1))
  done
  left=$(alive "$list")
  still=$(members "$grp")
  if [ "$left" = 0 ] && [ -z "$still" ] && [ -z "$(held)" ]; then echo "ok   $what: nothing left after ${t}s"; else
    echo "FAIL $what: after ${t}s, $left of the run's processes left, gateway group [$still], listening [$(held)]"
    bad=1
    tidy
  fi
  wait "$s" 2>/dev/null
  sleep 2
}
try driver
try script
exit $bad
