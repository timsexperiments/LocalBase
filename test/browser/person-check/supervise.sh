#!/bin/sh
# Runs the check's gateway in this script's own process group, and stops the group when the driver
# is gone, however it went: a kill -9 of the driver runs none of its cleanup.
#   supervise.sh <driver pid> <gateway> [args...]   (started in a new session, so this is the group's leader)
driver=$1
shift
"$@" &
gateway=$!
trap 'kill -KILL "$gateway" 2>/dev/null' EXIT
while kill -0 "$driver" 2>/dev/null && kill -0 "$gateway" 2>/dev/null; do sleep 1; done
# The gateway stops its own backends when asked; anything left in the group after that goes hard.
kill -TERM "$gateway" 2>/dev/null
i=0
while kill -0 "$gateway" 2>/dev/null && [ "$i" -lt 8 ]; do sleep 1; i=$((i + 1)); done
trap - EXIT
trap '' TERM
kill -KILL -- "-$$" 2>/dev/null
