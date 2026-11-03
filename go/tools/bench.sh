#!/bin/bash
# Runs tools/bench on a shared machine, as tools/soak.sh does the soak: its own
# session and process group, lower priority, and a stop on any signal. Arguments go
# to the benchmark (see tools/bench/main.go).
set -u
cd "$(dirname "$0")/.." || exit 2
work=$(mktemp -d "${TMPDIR:-/tmp}/lbbench-XXXXXX") || exit 2
if ! go build -o "$work/bench" ./tools/bench; then
  rm -rf "$work"
  exit 2
fi
perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' nice -n 10 "$work/bench" "$@" &
pid=$!
stop() { kill -TERM -- "-$pid" 2>/dev/null; }
trap stop INT TERM HUP
status=0
while kill -0 "$pid" 2>/dev/null; do
  wait "$pid"
  status=$?
done
rm -rf "$work"
exit "$status"
