#!/bin/bash
# M7 shadow and rollback drill on a COPY of a LocalBase root (DESIGN §17.2,
# §17.3). Nothing here writes to the source root: the database is read
# through SQLite's online backup API on a read-only connection, everything
# else is an APFS clone (copy-on-write), and the copy gets its own root
# marker. The copy is mode 0700, served on loopback with every port in the
# scratch range, and removed by `cleanup`.
#
#   tools/shadow.sh copy <source-root>      # make the copy (idle preflight first for the live root)
#   tools/shadow.sh serve go|ts             # start a gateway on the copy (background)
#   tools/shadow.sh stop                    # stop it
#   tools/shadow.sh schema <label>          # snapshot the journal and schema
#   tools/shadow.sh sessions                # TS-vs-Go verdicts on the copy's sessions
#   tools/shadow.sh cleanup                 # stop and delete the copy
#
# LBGO_SHADOW (default /private/tmp/lbgo-shadow) is the work directory.
# LBGO_TS_SRC is a TS source tree for `serve ts` (the live build's commit).
set -uo pipefail
cd "$(dirname "$0")/.."
work=${LBGO_SHADOW:-/private/tmp/lbgo-shadow}
root="$work/root"
live="$HOME/.local/share/local-base"
die() { echo "shadow: $*" >&2; exit 1; }

case "$work" in /private/tmp/*|/tmp/*) ;; *) die "the work directory must be under /tmp" ;; esac

idle_preflight() { # no inference.started without an end in the live event log's last 60 s (read-only)
  local since out
  since=$(date -u -v-60S +%Y-%m-%dT%H:%M:%S)
  out=$(tail -n 2000 "$live/logs/events.jsonl" 2>/dev/null | awk -v s="$since" '
    match($0, /"timestamp":"[^"]*"/) { t=substr($0, RSTART+13, 19); if (t < s) next }
    /"eventName":"inference\.started"/ {st++} /"eventName":"inference\.(completed|failed|cancelled|aborted)"/ {en++}
    END {printf "started=%d finished=%d", st, en; exit !(st <= en)}') || { echo "live inference in the last 60 s: $out"; die "the live gateway is not idle; retry later"; }
  echo "live inference in the last 60 s: $out"
}

clone() { # clone <src> <dst>: an APFS clone, never a link
  cp -c -R "$1" "$2" || die "clone failed: $1"
}

verify_copy() {
  local bad=0
  if [ -n "$(find "$root/models" "$root/catalog.d" -type l 2>/dev/null)" ]; then
    echo "a model or catalog file is a symlink"; bad=1
  fi
  while IFS= read -r l; do
    t=$(readlink "$l")
    case "$t" in /*) echo "absolute link: $l"; bad=1 ;; esac
    real=$(cd "$(dirname "$l")" && cd "$(dirname "$t")" 2>/dev/null && pwd -P)
    case "$real" in "$root"*) ;; *) echo "link leaves the copy: $l"; bad=1 ;; esac
  done < <(find "$root" -type l)
  if [ -n "$(find "$root" -type f -links +1)" ]; then echo "a file in the copy has more than one link"; bad=1; fi
  [ $bad = 0 ] || die "the copy is not detached"
  echo "copy verified: no links into the source, every file has one link"
}

cmd=${1:-}
case "$cmd" in
copy)
  src=${2:-}; [ -d "$src" ] || die "usage: copy <source-root>"
  src=$(cd "$src" && pwd -P)
  [ -e "$work" ] && die "$work exists; run cleanup first"
  [ "$src" = "$(cd "$live" 2>/dev/null && pwd -P)" ] && idle_preflight
  umask 077
  mkdir -m 700 -p "$root" || die "mkdir"
  # The database: online backup through a read-only connection.
  sqlite3 "file:$src/local-base.db?mode=ro" ".backup '$root/local-base.db'" || die "database backup failed"
  # Everything else the gateway reads: clones. Not copied: service/ and
  # runtime/ (they name the source's label and lock), backups and tmp.
  for x in catalog.d models ui-access.json voices credentials cache logs auth-profiles.json; do
    [ -e "$src/$x" ] && clone "$src/$x" "$root/$x"
  done
  if [ -d "$src/bin/runtimes" ]; then
    mkdir -m 700 -p "$root/bin"
    clone "$src/bin/runtimes" "$root/bin/runtimes"
  fi
  # The copy's own root marker (both implementations refuse a moved root).
  real=$(cd "$root" && pwd -P)
  hash=$(printf '%s' "$real" | shasum -a 256 | cut -d' ' -f1)
  printf '{"version":1,"root":"%s","rootHash":"%s","identity":"%s"}' "$real" "$hash" "$(uuidgen | tr 'A-Z' 'a-z')" > "$root/.localbase-root.json"
  # The config row records its root too; only the copy's row changes.
  sqlite3 "$root/local-base.db" "UPDATE config SET root = '$real' WHERE id = 'default'" || die "config root"
  chmod -R go-rwx "$work"
  verify_copy
  echo "copy ready at $root"
  ;;
serve)
  impl=${2:-}; [ -d "$root" ] || die "no copy"
  [ -e "$work/gateway.pid" ] && die "a gateway is already running (stop first)"
  pick() { while :; do p=$((24000 + RANDOM % 6000)); [ $p = 24173 ] && continue; [ $p -ge 25611 ] && [ $p -le 25613 ] && continue
    lsof -nP -iTCP:$p -sTCP:LISTEN >/dev/null 2>&1 || { echo $p; return; }; done; }
  port=$(pick)
  args=(--root "$root" serve --host 127.0.0.1 --port "$port"
    --llm-host 127.0.0.1 --llm-port "$(pick)" --stt-host 127.0.0.1 --stt-port "$(pick)"
    --tts-host 127.0.0.1 --tts-port "$(pick)" --image-host 127.0.0.1 --image-port "$(pick)"
    --video-host 127.0.0.1 --video-port "$(pick)")
  case "$impl" in
  go)
    go build -o "$work/local-base-go" ./cmd/localbase || die "build"
    (cd "$work" && nohup "$work/local-base-go" "${args[@]}" > "$work/serve-go.log" 2>&1 & echo $! > "$work/gateway.pid")
    ;;
  ts)
    [ -d "${LBGO_TS_SRC:-}" ] || die "set LBGO_TS_SRC to the TS source tree"
    (cd "$LBGO_TS_SRC" && nohup bun run src/cli.ts "${args[@]}" > "$work/serve-ts.log" 2>&1 & echo $! > "$work/gateway.pid")
    ;;
  *) die "usage: serve go|ts" ;;
  esac
  echo "$port" > "$work/gateway.port"
  for i in $(seq 1 90); do
    curl -s -m 2 "http://127.0.0.1:$port/health" >/dev/null && { echo "$impl gateway on 127.0.0.1:$port (pid $(cat "$work/gateway.pid"))"; exit 0; }
    kill -0 "$(cat "$work/gateway.pid")" 2>/dev/null || { tail -20 "$work/serve-$impl.log"; rm -f "$work/gateway.pid"; die "$impl gateway exited"; }
    sleep 1
  done
  die "$impl gateway not healthy"
  ;;
stop)
  [ -e "$work/gateway.pid" ] || { echo "no gateway"; exit 0; }
  pid=$(cat "$work/gateway.pid")
  # The gateway and the backends it started (their command lines name the copy).
  kill -TERM "$pid" 2>/dev/null
  for i in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -KILL "$pid" 2>/dev/null
  for p in $(pgrep -f "$root" 2>/dev/null); do kill -TERM "$p" 2>/dev/null; done
  sleep 2
  for p in $(pgrep -f "$root" 2>/dev/null); do kill -KILL "$p" 2>/dev/null; done
  rm -f "$work/gateway.pid" "$work/gateway.port"
  echo "stopped"
  ;;
schema)
  label=${2:-}; [ -n "$label" ] || die "usage: schema <label>"
  sqlite3 "file:$root/local-base.db?mode=ro" "SELECT id, hash, created_at FROM __drizzle_migrations ORDER BY id" > "$work/journal-$label.txt"
  sqlite3 "file:$root/local-base.db?mode=ro" ".schema" > "$work/schema-$label.sql"
  echo "journal rows: $(wc -l < "$work/journal-$label.txt" | tr -d ' '), schema lines: $(wc -l < "$work/schema-$label.sql" | tr -d ' ')"
  ;;
sessions)
  [ -d "$root" ] || die "no copy"
  [ -e "$work/gateway.pid" ] && die "stop the gateway first"
  # Both sides read a second copy of the database, so neither can change
  # the shadow copy; the same instant is used for both verdicts.
  check="$work/session-check"
  rm -rf "$check"; mkdir -m 700 "$check"
  sqlite3 "file:$root/local-base.db?mode=ro" ".backup '$check/local-base.db'"
  cp "$root/ui-access.json" "$check/" 2>/dev/null
  now=$(($(date +%s) * 1000))
  # TS from the live build's source when given (LBGO_TS_SRC), else this tree.
  ts=..
  if [ -d "${LBGO_TS_SRC:-}" ]; then
    mkdir -p "$LBGO_TS_SRC/go/tools/golden" && cp tools/golden/session-check.ts "$LBGO_TS_SRC/go/tools/golden/"
    ts=$LBGO_TS_SRC
  fi
  (cd "$ts" && bun run go/tools/golden/session-check.ts "$check" "$now") > "$work/sessions-ts.json" || die "TS session check"
  LOCALBASE_SESSION_ROOT="$check" LOCALBASE_SESSION_NOW="$now" LOCALBASE_SESSION_TS="$work/sessions-ts.json" \
    go test ./internal/auth -run TestSessionRowsMatchTS -count=1 -v 2>&1 | grep -E "session |--- |^ok|FAIL"
  rm -rf "$check"
  ;;
cleanup)
  "$0" stop >/dev/null
  rm -rf "$work"
  [ -e "$work" ] && die "could not remove $work"
  echo "removed $work"
  ;;
*) die "usage: copy|serve|stop|schema|sessions|cleanup" ;;
esac
