# Sourced by the scripts that start a browser. One at a time per script and worktree, and none
# outlives its run, however the run ends. Needs $here and $profile.

# browser_guard <name>: refuses to start while a browser from this script's last run is alive.
browser_guard() {
  browser_pidfile="$here/.cache/$1.browser.pid"
  mkdir -p "$here/.cache"
  if [ -s "$browser_pidfile" ]; then
    browser_old=$(cat "$browser_pidfile")
    if ps -o command= -p "$browser_old" 2>/dev/null | grep -q -- --remote-debugging-port; then
      echo "$1: a browser from an earlier run is still alive (pid $browser_old). Stop it first." >&2
      rm -rf "$profile"
      exit 3
    fi
  fi
}

# browser_started <pid>: from here on the browser and the check are cleaned up.
browser_started() {
  browser_pid=$1
  browser_child=
  echo "$browser_pid" > "$browser_pidfile"
  trap browser_cleanup EXIT
  trap 'exit 143' HUP INT TERM ALRM
}

# browser_run <command...>: runs the check as a child, so a signal can stop it too.
browser_run() {
  "$@" &
  browser_child=$!
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
  kill -9 "$browser_pid" 2>/dev/null
  # Its helpers carry the profile's path, which belongs to this run alone.
  pkill -9 -f -- "$profile" 2>/dev/null
  sleep 0.2
  rm -rf "$profile"
  rm -f "$browser_pidfile"
  exit "$browser_status"
}
