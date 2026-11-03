# Sourced by the scripts that start a browser. One at a time per script and worktree, and none
# outlives its run, however the run ends. Needs $here. Nothing here deletes: the profile is at a
# fixed path each script clears as it starts, and an empty pid file means no run. Nothing here ends
# a process by name: only this run's browser group, and the check it started.

# browser_guard <name>: refuses only while a live run owns the pid file. A file left by a run
# that's gone doesn't block. After a kill -9 the watcher clears up within a few seconds.
browser_guard() {
  browser_pidfile="$here/.cache/$1.browser.pid"
  mkdir -p "$here/.cache"
  if [ -s "$browser_pidfile" ]; then
    browser_old=$(cat "$browser_pidfile")
    if ps -o command= -p "$browser_old" 2>/dev/null | grep -q -- --remote-debugging-port; then
      echo "$1: a browser from an earlier run is still alive (pid $browser_old). Stop it first." >&2
      exit 3
    fi
  fi
}

# browser_group <command...>: run in the background, as the start of a browser. Makes it the leader
# of its own process group, so its helpers can be ended together and nothing else can be.
browser_group() {
  exec perl -MPOSIX -e 'POSIX::setpgid(0, 0); exec @ARGV or die "cannot start $ARGV[0]: $!\n"' "$@"
}

# browser_started <pid>: from here on the browser and the check are cleaned up.
browser_started() {
  browser_pid=$1
  browser_child=
  echo "$browser_pid" > "$browser_pidfile"
  trap browser_cleanup EXIT
  trap 'exit 143' HUP INT TERM ALRM
  browser_watch
}

# browser_watch: a kill -9 of this script runs none of its cleanup, so a small process does it
# then. The same way as watchOwner in tools/lab/stats.go: its own session, so killing this
# script's whole group doesn't take it too, and none of this script's settings (set -e would
# stop it at the first thing already gone). It ends by itself once the browser is gone.
browser_watch() {
  perl -MPOSIX -e 'POSIX::setsid(); exec @ARGV' sh -c '
    owner=$1 browser=$2 pidfile=$3
    while kill -0 "$owner" 2>/dev/null && kill -0 "$browser" 2>/dev/null; do sleep 1; done
    kill -0 "$owner" 2>/dev/null && exit 0
    child=$(cat "$pidfile.child" 2>/dev/null)
    [ -z "$child" ] || kill "$child" 2>/dev/null
    kill "$browser" 2>/dev/null
    sleep 2
    [ -z "$child" ] || kill -9 "$child" 2>/dev/null
    kill -9 -- "-$browser" 2>/dev/null
    # Only this run'"'"'s: a new run may already have written its own.
    if [ "$(cat "$pidfile" 2>/dev/null)" = "$browser" ]; then : > "$pidfile"; : > "$pidfile.child"; fi
  ' sh "$$" "$browser_pid" "$browser_pidfile" </dev/null >/dev/null 2>&1 &
}

# browser_run <command...>: runs the check as a child, so a signal can stop it too.
browser_run() {
  "$@" &
  browser_child=$!
  echo "$browser_child" > "$browser_pidfile.child"
  wait "$browser_child"
}

browser_cleanup() {
  browser_status=$?
  # Things here are often already gone. With set -e that would end the cleanup early.
  set +e
  # A signal now can't stop this partway, and this runs once.
  trap '' HUP INT TERM ALRM
  trap - EXIT
  [ -z "$browser_child" ] || kill "$browser_child" 2>/dev/null
  kill "$browser_pid" 2>/dev/null
  sleep 1
  # The browser leads its own group, and its helpers are in it.
  kill -9 -- "-$browser_pid" 2>/dev/null
  : > "$browser_pidfile"
  : > "$browser_pidfile.child"
  exit "$browser_status"
}
