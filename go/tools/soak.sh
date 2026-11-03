#!/bin/bash
# Runs tools/soak on a shared machine: its own session and process group, lower
# priority, a stop on any signal and a hard watchdog, so no client or scratch
# gateway can outlive a run. Arguments go to the soak (see tools/soak/main.go):
#   tools/soak.sh -smoke            about 2 minutes
#   tools/soak.sh -duration 30m     the full soak
#   tools/soak.sh -smoke -race      against a race-detector gateway
# LOCALBASE_SOAK_HARD_SECONDS overrides the watchdog (default 2700: 45 minutes).
# LOCALBASE_TOOLS_CACHE says where the binary goes (default ~/.cache/localbase).
set -u
cd "$(dirname "$0")/.." || exit 2
# Built to a fixed place and replaced by the next run, so there is nothing to clean up.
bin=${LOCALBASE_TOOLS_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/localbase}/soak
mkdir -p "${bin%/*}" || exit 2
go build -o "$bin" ./tools/soak || exit 2
GOMAXPROCS=2 perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' nice -n 10 "$bin" "$@" &
pid=$!
stop() { kill -TERM -- "-$pid" 2>/dev/null; }
trap stop INT TERM HUP
(
  sleep "${LOCALBASE_SOAK_HARD_SECONDS:-2700}" &
  nap=$!
  trap 'kill "$nap" 2>/dev/null; exit 0' TERM
  wait "$nap"
  echo "soak.sh: the watchdog fired" >&2
  kill -TERM -- "-$pid" 2>/dev/null
  sleep 20
  kill -KILL -- "-$pid" 2>/dev/null
) &
watchdog=$!
status=0
# wait returns early when a trapped signal arrives; keep waiting for the soak.
while kill -0 "$pid" 2>/dev/null; do
  wait "$pid"
  status=$?
done
kill "$watchdog" 2>/dev/null
wait "$watchdog" 2>/dev/null
exit "$status"
