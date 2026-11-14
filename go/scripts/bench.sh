#!/bin/bash
# Runs tools/bench on a shared machine, as scripts/soak.sh does the soak: its own
# session and process group, lower priority, and a stop on any signal. Arguments go
# to the benchmark (see tools/bench/main.go). LOCALBASE_TOOLS_CACHE says where the
# binary goes (default ~/.cache/localbase).
set -u
cd "$(dirname "$0")/.." || exit 2
. scripts/disk-guard.sh
disk_guard bench
# Built to a fixed place and replaced by the next run, so there is nothing to clean up.
bin=${LOCALBASE_TOOLS_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/localbase}/bench
mkdir -p "${bin%/*}" || exit 2
go build -o "$bin" ./tools/bench || exit 2
perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' nice -n 10 "$bin" "$@" &
pid=$!
stop() { kill -TERM -- "-$pid" 2>/dev/null; }
trap stop INT TERM HUP
status=0
while kill -0 "$pid" 2>/dev/null; do
  wait "$pid"
  status=$?
done
exit "$status"
